// Package benchmark measures the built proxy against the targets of kick-off
// section 18 (AC-52), by the method of phase-4 design section 9. It runs
// only with STARGAITE_BENCH=1 and on Linux (RSS is read from /proc), which
// make bench sets: the binaries stargaite and mockprovider are read from
// STARGAITE_BENCH_DIR, else the temporary directory, and both run as
// processes on loopback, the proxy with GOMAXPROCS=1 and, when taskset is
// present, pinned to CPU 0 and the mock to the other CPUs. The figures go to
// the file STARGAITE_BENCH_OUT names as one JSON document, else to standard
// output, then as a table. Each missed target fails the test unless
// docs/benchmark.md lists it as an accepted deviation, up to its figure.
package benchmark

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"text/tabwriter"
	"time"

	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// The load of each measurement (phase-4 design section 15, point 28).
const (
	latencyRate    = 200             // requests per second of each latency run
	latencyRun     = 5 * time.Second // the length of each run
	latencyRuns    = 10              // runs per scenario, each straight to the mock and through the proxy
	throughputRate = 1000
	throughputRun  = 30 * time.Second
	cancelTrials   = 10
)

// figure is one measured value against its kick-off section 18 target,
// Limit, which every gated figure must not exceed; a figure that is not
// gated is reported only.
type figure struct {
	ID      string  `json:"id"`
	Value   float64 `json:"value"`
	Unit    string  `json:"unit"`
	Limit   float64 `json:"limit,omitempty"`
	Gated   bool    `json:"gated"`
	Verdict string  `json:"verdict"` // met, missed, accepted (a listed deviation) or reported
}

// report is the JSON document: the host, the method and the figures.
type report struct {
	Commit            string   `json:"commit"`
	Go                string   `json:"go"`
	CPU               string   `json:"cpu"`
	Kernel            string   `json:"kernel"`
	Cores             int      `json:"cores"`
	Taskset           bool     `json:"taskset"`
	LatencyRate       int      `json:"latency_rate_rps"`
	LatencyRunSeconds float64  `json:"latency_run_seconds"`
	LatencyRuns       int      `json:"latency_runs"`
	ThroughputRate    int      `json:"throughput_rate_rps"`
	ThroughputSeconds float64  `json:"throughput_seconds"`
	Availability      string   `json:"availability"`
	Figures           []figure `json:"figures"`
}

func (r *report) add(id string, value float64, unit string, limit float64, gated bool) {
	r.Figures = append(r.Figures, figure{ID: id, Value: math.Round(value*1000) / 1000, Unit: unit, Limit: limit, Gated: gated})
}

func TestBenchmark(t *testing.T) {
	if os.Getenv("STARGAITE_BENCH") != "1" {
		t.Skip("the benchmark runs with STARGAITE_BENCH=1: make bench")
	}
	if runtime.GOOS != "linux" {
		t.Skip("the benchmark reads RSS from /proc: Linux only")
	}
	dir := cmp.Or(os.Getenv("STARGAITE_BENCH_DIR"), os.TempDir())
	bin, mockBin := filepath.Join(dir, "stargaite"), filepath.Join(dir, "mockprovider")
	for _, f := range []string{bin, mockBin} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("%v: make bench builds it", err)
		}
	}
	_, err := exec.LookPath("taskset")
	rep := &report{Commit: commit(), Go: runtime.Version(), CPU: cpuModel(), Kernel: firstLine("/proc/sys/kernel/osrelease"),
		Cores: runtime.NumCPU(), Taskset: err == nil, LatencyRate: latencyRate, LatencyRunSeconds: latencyRun.Seconds(),
		LatencyRuns: latencyRuns, ThroughputRate: throughputRate, ThroughputSeconds: throughputRun.Seconds(),
		Availability: "not measured: a benchmark cannot measure monthly availability (phase-4 design section 15, point 28)"}
	mockCPUs := ""
	if runtime.NumCPU() > 1 {
		mockCPUs = fmt.Sprintf("1-%d", runtime.NumCPU()-1)
	}
	_, mockReady, _ := start(t, []string{}, mockCPUs, mockBin, "-listen", "127.0.0.1:0")
	mockURL := "http://" + mockReady.Addr

	// The proxy on an empty data directory: start-up is the first start.
	data := t.TempDir()
	proxyEnv := []string{"GOMAXPROCS=1"}
	proxy, ready, startup := start(t, proxyEnv, "0", bin, "serve", "--data-dir", data, "--listen", "127.0.0.1:0", "--admin-listen", "127.0.0.1:0")
	rep.add("startup_ms", ms(startup), "ms", 2000, true)
	cfg := filepath.Join(t.TempDir(), "bench.yaml")
	writeFile(t, cfg, fmt.Sprintf(benchConfig, mockURL))
	var applied struct{ Secrets map[string]string }
	if err := json.Unmarshal(cli(t, bin, ready.AdminAddr, data, "apply", "-f", cfg, "--json"), &applied); err != nil || applied.Secrets["bench"] == "" {
		t.Fatalf("apply: %v %v", err, applied)
	}
	b := &bench{t: t, mockURL: mockURL, proxyURL: "http://" + ready.DataAddr, token: applied.Secrets["bench"],
		client: &http.Client{Timeout: time.Minute, Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 1000, DisableCompression: true}}}
	scenarios := b.scenarios()

	// A warm-up of every path.
	for _, sc := range scenarios {
		for _, proxied := range []bool{false, true} {
			b.check(sc.id+" warm-up", b.run(sc, proxied, latencyRate, time.Second))
		}
	}

	// Cancellation: the client closes a stream that the mock holds open; the
	// mock's Ended is when the proxy's upstream call ended (RES-5).
	var cancels []float64
	for range cancelTrials {
		b.script(mockprovider.Step{Chunks: [][]byte{[]byte(chunk("mock "))}, Hang: true})
		resp := b.openStream()
		if resp == nil {
			t.FailNow()
		}
		closed := time.Now()
		_ = resp.Body.Close()
		cancels = append(cancels, ms(b.lastEnded(closed.Add(5*time.Second)).Sub(closed)))
	}
	rep.add("cancel_max_ms", slices.Max(cancels), "ms", 1000, true)
	rep.add("cancel_median_ms", median(cancels), "ms", 0, false)

	// At rest: a minute after the last request, which is also how long a
	// closed connection stays in TIME_WAIT, so the count after the
	// throughput run is that run's: each is an outbound connection the
	// proxy closed (phase-4 design section 15, point 29).
	cpu := cpuTime(t, proxy)
	time.Sleep(61 * time.Second)
	rep.add("rss_rest_mb", rss(t, proxy), "MB", 40, true)
	rep.add("cpu_rest_cores", (cpuTime(t, proxy)-cpu).Seconds()/61, "CPU", 0, false)
	cpu = cpuTime(t, proxy)
	began := time.Now()
	samples := load(b.client, throughputRate, throughputRun, b.request(b.proxyURL+"/v1/chat/completions", chatBody("fast", false), true))
	var totals []time.Duration
	failed, end := 0, began
	for _, s := range samples {
		if s.err != nil {
			failed++
			continue
		}
		totals = append(totals, s.total)
		if s.end.After(end) {
			end = s.end
		}
	}
	rep.add("throughput_errors", float64(failed), "requests", 0, true)
	if len(totals) > 0 {
		rep.add("throughput_max_ms", ms(slices.Max(totals)), "ms", 1000, true)
		rep.add("throughput_p50_ms", pct(totals, 0.5), "ms", 0, false)
		rep.add("throughput_p99_ms", pct(totals, 0.99), "ms", 0, false)
		rep.add("throughput_rps", float64(len(totals))/end.Sub(began).Seconds(), "rps", 0, false)
	}
	rep.add("throughput_cpu_cores", (cpuTime(t, proxy)-cpu).Seconds()/end.Sub(began).Seconds(), "CPU", 0, false)
	rep.add("throughput_time_wait", float64(timeWait(mockReady.Addr)), "sockets", 0, false)

	// RSS with open streams; all 500 must open (kick-off section 18,
	// concurrency).
	for _, n := range []int{100, 500} {
		steps := make([]mockprovider.Step, n)
		for i := range steps {
			steps[i] = mockprovider.Step{Chunks: [][]byte{[]byte(chunk("mock "))}, Hang: true}
		}
		b.script(steps...)
		bodies := make([]io.ReadCloser, n)
		var wg sync.WaitGroup
		for i := range bodies {
			wg.Go(func() {
				if resp := b.openStream(); resp != nil {
					bodies[i] = resp.Body
				}
			})
		}
		wg.Wait()
		time.Sleep(time.Second)
		open := 0
		for _, body := range bodies {
			if body != nil {
				open++
			}
		}
		if n == 100 {
			rep.add("rss_streams_100_mb", rss(t, proxy), "MB", 100, true)
		} else {
			rep.add("rss_streams_500_mb", rss(t, proxy), "MB", 0, false)
			rep.add("streams_500_failed", float64(n-open), "streams", 0, true)
		}
		for _, body := range bodies {
			if body != nil {
				_ = body.Close()
			}
		}
		time.Sleep(2 * time.Second)
	}

	// Added latency: ten alternating runs straight to the mock and through
	// the proxy; the median of the per-run differences of each percentile.
	for _, sc := range scenarios {
		var diffs [4][]float64 // total P50, P99, first byte P50, P99
		var direct [2][]float64
		for r := range latencyRuns {
			var res [2][]sample
			for k := range 2 {
				proxied := k^(r%2) == 1
				res[k^(r%2)] = b.run(sc, proxied, latencyRate, latencyRun)
				b.check(fmt.Sprintf("%s run %d proxied %v", sc.id, r, proxied), res[k^(r%2)])
			}
			for i, q := range []float64{0.5, 0.99} {
				d, p := durations(res[0], false), durations(res[1], false)
				diffs[i] = append(diffs[i], pct(p, q)-pct(d, q))
				direct[i] = append(direct[i], pct(d, q))
				d, p = durations(res[0], true), durations(res[1], true)
				diffs[2+i] = append(diffs[2+i], pct(p, q)-pct(d, q))
			}
		}
		rep.add("latency_"+sc.id+"_p50_ms", median(diffs[0]), "ms", 0.5, true)
		rep.add("latency_"+sc.id+"_p99_ms", median(diffs[1]), "ms", sc.p99, true)
		if sc.stream {
			rep.add("first_byte_"+sc.id+"_p50_ms", median(diffs[2]), "ms", 0.5, true)
			rep.add("first_byte_"+sc.id+"_p99_ms", median(diffs[3]), "ms", sc.p99, true)
		}
		rep.add("direct_"+sc.id+"_p50_ms", median(direct[0]), "ms", 0, false)
		rep.add("direct_"+sc.id+"_p99_ms", median(direct[1]), "ms", 0, false)
	}
	time.Sleep(5 * time.Second)
	rep.add("rss_rest_after_load_mb", rss(t, proxy), "MB", 40, true)

	// RTO on a store of kick-off section 18 scale: its configuration applied
	// to the proxy's store, which holds every usage record of the run; then
	// the backup, the procedure restore follows, by hand, and the start to
	// ready.
	scale := filepath.Join(t.TempDir(), "scale18.yaml")
	writeFile(t, scale, scaleConfig(mockURL))
	began = time.Now()
	cli(t, bin, ready.AdminAddr, data, "apply", "-f", scale, "--json")
	rep.add("apply_scale18_ms", ms(time.Since(began)), "ms", 0, false)
	backup := filepath.Join(t.TempDir(), "backup.db")
	began = time.Now()
	cli(t, bin, ready.AdminAddr, data, "backup", backup)
	backupTook := time.Since(began)
	info, err := os.Stat(backup)
	if err != nil {
		t.Fatal(err)
	}
	stopTook := stop(t, proxy)
	began = time.Now()
	moved := t.TempDir()
	for _, f := range []string{"stargaite.db", "stargaite.db-wal", "stargaite.db-shm"} {
		if err := os.Rename(filepath.Join(data, f), filepath.Join(moved, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command("install", "-m", "600", backup, filepath.Join(data, "stargaite.db")).CombinedOutput(); err != nil {
		t.Fatalf("install: %v: %s", err, out)
	}
	restoreTook := time.Since(began)
	proxy, ready, startScale := start(t, proxyEnv, "0", bin, "serve", "--data-dir", data, "--listen", "127.0.0.1:0", "--admin-listen", "127.0.0.1:0")
	b.proxyURL = "http://" + ready.DataAddr
	b.check("after the restore", load(b.client, 10, time.Second, b.request(b.proxyURL+"/v1/chat/completions", chatBody("fast", false), true)))
	rep.add("store_mb", float64(info.Size())/1e6, "MB", 0, false)
	rep.add("backup_ms", ms(backupTook), "ms", 0, false)
	rep.add("stop_ms", ms(stopTook), "ms", 0, false)
	rep.add("restore_ms", ms(restoreTook), "ms", 0, false)
	rep.add("startup_scale18_ms", ms(startScale), "ms", 2000, true)
	rep.add("rto_s", (backupTook + stopTook + restoreTook + startScale).Seconds(), "s", 900, true)
	stop(t, proxy)

	// A configuration change at kick-off section 18 scale, on one CPU:
	// internal/config's BenchmarkCommitScale18.
	rep.add("commit_scale18_ms", commitScale18(t), "ms", 1000, true)

	// The image size is reported, not gated (kick-off section 18).
	if out, err := exec.Command("docker", "image", "inspect", "-f", "{{.Size}}", "stargaite:dev").Output(); err == nil {
		if size, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64); err == nil {
			rep.add("image_mb", size/1e6, "MB", 30, false)
		}
	}

	verdicts(t, rep, deviations(t, "../../docs/benchmark.md"))
	doc, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if out := os.Getenv("STARGAITE_BENCH_OUT"); out != "" {
		writeFile(t, out, string(doc)+"\n")
	} else {
		fmt.Println(string(doc))
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "figure\tvalue\tunit\ttarget\tverdict")
	for _, f := range rep.Figures {
		target := "-"
		if f.Limit != 0 || f.Gated {
			target = "<= " + strconv.FormatFloat(f.Limit, 'f', -1, 64)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%v\t%s\t%s\t%s\n", f.ID, f.Value, f.Unit, target, f.Verdict)
	}
	_ = tw.Flush()
}

// verdicts marks each figure, and fails the test for every gated figure
// over its target that accepted does not list, or lists below its value.
func verdicts(t *testing.T, rep *report, accepted map[string]float64) {
	t.Helper()
	for i := range rep.Figures {
		f := &rep.Figures[i]
		bound, listed := accepted[f.ID]
		switch {
		case !f.Gated:
			f.Verdict = "reported"
		case f.Value <= f.Limit:
			f.Verdict = "met"
		case listed && f.Value <= bound:
			f.Verdict = "accepted"
		default:
			f.Verdict = "missed"
			t.Errorf("%s is %v %s, over its target of %v, and docs/benchmark.md lists no accepted deviation up to it", f.ID, f.Value, f.Unit, f.Limit)
		}
	}
}

// deviations reads the table under "## Accepted deviations" in the file at
// path: | `id` | figure | reason | date |. A row counts only with an id, a
// figure that starts with a number (in the figure's unit), a reason and a
// date; the harness accepts a miss up to that figure.
func deviations(t *testing.T, path string) map[string]float64 {
	t.Helper()
	text, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, section, _ := strings.Cut(string(text), "\n## Accepted deviations\n")
	section, _, _ = strings.Cut(section, "\n## ")
	accepted := map[string]float64{}
	for line := range strings.SplitSeq(section, "\n") {
		cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
		if len(cells) != 4 {
			continue
		}
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		id, figure := strings.Trim(cells[0], "`"), strings.Fields(cells[1])
		if id == "" || len(figure) == 0 || cells[2] == "" {
			continue
		}
		value, err := strconv.ParseFloat(figure[0], 64)
		if _, derr := time.Parse(time.DateOnly, cells[3]); err == nil && derr == nil {
			accepted[id] = value
		}
	}
	return accepted
}

// bench holds what the measurements share.
type bench struct {
	t                        *testing.T
	mockURL, proxyURL, token string
	client                   *http.Client
}

// scenario is one added-latency measurement: the same rate and payload
// straight to the mock and through the proxy. think scripts each request's
// answer as an inline think stream.
type scenario struct {
	id              string
	p99             float64 // the P99 target: 2 ms relayed, 5 ms when SSE is inspected or adapted
	stream, think   bool
	direct, proxied func() *http.Request
}

func (b *bench) scenarios() []scenario {
	chat, msgs := b.mockURL+"/chat/completions", b.mockURL+"/v1/messages"
	proxy := b.proxyURL + "/v1/chat/completions"
	return []scenario{
		{id: "sync", p99: 2, direct: b.request(chat, chatBody("mock", false), false), proxied: b.request(proxy, chatBody("fast", false), true)},
		{id: "native_stream", p99: 2, stream: true, direct: b.request(chat, chatBody("mock", true), false), proxied: b.request(proxy, chatBody("fast", true), true)},
		// The direct call of a translated stream is the Messages call the
		// proxy makes for it.
		{id: "translated_stream", p99: 5, stream: true,
			direct:  b.request(msgs, `{"model":"mock","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, false),
			proxied: b.request(proxy, chatBody("translated", true), true)},
		{id: "think_stream", p99: 5, stream: true, think: true, direct: b.request(chat, chatBody("mock", true), false), proxied: b.request(proxy, chatBody("think", true), true)},
	}
}

// run is one open-loop run of sc, straight to the mock or through the proxy.
func (b *bench) run(sc scenario, proxied bool, rate int, d time.Duration) []sample {
	if sc.think {
		steps := make([]mockprovider.Step, int(d.Seconds()*float64(rate)))
		for i := range steps {
			steps[i] = thinkStep
		}
		b.script(steps...)
	}
	req := sc.direct
	if proxied {
		req = sc.proxied
	}
	return load(b.client, rate, d, req)
}

// check fails the test on any failed request of a run.
func (b *bench) check(what string, samples []sample) {
	b.t.Helper()
	failed := 0
	var first error
	for _, s := range samples {
		if s.err != nil {
			failed++
			first = cmp.Or(first, s.err)
		}
	}
	if failed > 0 {
		b.t.Errorf("%s: %d of %d requests failed, the first: %v", what, failed, len(samples), first)
	}
}

func (b *bench) request(url, body string, proxied bool) func() *http.Request {
	return func() *http.Request {
		req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
		if err != nil {
			panic(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if proxied {
			req.Header.Set("Authorization", "Bearer "+b.token)
		}
		return req
	}
}

// openStream opens a stream through the proxy and reads its first byte;
// nil, with the test failed, when it does not open.
func (b *bench) openStream() *http.Response {
	resp, err := b.client.Do(b.request(b.proxyURL+"/v1/chat/completions", chatBody("fast", true), true)())
	if err == nil && resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		err = fmt.Errorf("status %d", resp.StatusCode)
	}
	if err == nil {
		_, err = io.ReadFull(resp.Body, make([]byte, 1))
	}
	if err != nil {
		b.t.Errorf("a stream did not open: %v", err)
		return nil
	}
	return resp
}

// script queues steps on the mock process.
func (b *bench) script(steps ...mockprovider.Step) {
	b.t.Helper()
	body, err := json.Marshal(steps)
	if err != nil {
		b.t.Fatal(err)
	}
	resp, err := b.client.Post(b.mockURL+"/mock/script", "application/json", bytes.NewReader(body))
	if err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			err = fmt.Errorf("status %d", resp.StatusCode)
		}
	}
	if err != nil {
		b.t.Fatalf("script the mock: %v", err)
	}
}

// lastEnded waits until the mock's last call has ended and returns when;
// the call must have been cancelled.
func (b *bench) lastEnded(deadline time.Time) time.Time {
	b.t.Helper()
	for time.Now().Before(deadline) {
		resp, err := b.client.Get(b.mockURL + "/mock/calls")
		if err != nil {
			b.t.Fatal(err)
		}
		var calls []mockprovider.Call
		err = json.NewDecoder(resp.Body).Decode(&calls)
		_ = resp.Body.Close()
		if err != nil || len(calls) == 0 {
			b.t.Fatalf("the mock's calls: %v", err)
		}
		if last := calls[len(calls)-1]; !last.Ended.IsZero() {
			if !last.Cancelled {
				b.t.Error("the mock's last call was not cancelled")
			}
			return last.Ended
		}
		time.Sleep(10 * time.Millisecond)
	}
	b.t.Fatal("the upstream call did not end within 5 s of the client's close")
	return time.Time{}
}

// sample is one request: its first body byte and its end, measured from its
// intended send time.
type sample struct {
	first, total time.Duration
	end          time.Time
	err          error
}

// load sends rate requests per second for d, open loop: request i has the
// intended send time begin + i/rate, and its durations run from that time,
// so a stall counts against every request it delays (no coordinated
// omission).
func load(c *http.Client, rate int, d time.Duration, req func() *http.Request) []sample {
	out := make([]sample, int(d.Seconds()*float64(rate)))
	var wg sync.WaitGroup
	begin := time.Now()
	for i := range out {
		at := begin.Add(time.Duration(i) * time.Second / time.Duration(rate))
		time.Sleep(time.Until(at))
		wg.Go(func() { out[i] = send(c, at, req()) })
	}
	wg.Wait()
	return out
}

func send(c *http.Client, at time.Time, req *http.Request) sample {
	resp, err := c.Do(req)
	if err != nil {
		return sample{err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return sample{err: fmt.Errorf("status %d: %s", resp.StatusCode, body)}
	}
	if _, err := io.ReadFull(resp.Body, make([]byte, 1)); err != nil {
		return sample{err: err}
	}
	first := time.Since(at)
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return sample{err: err}
	}
	end := time.Now()
	return sample{first: first, total: end.Sub(at), end: end}
}

func durations(samples []sample, first bool) []time.Duration {
	out := make([]time.Duration, 0, len(samples))
	for _, s := range samples {
		switch {
		case s.err != nil:
		case first:
			out = append(out, s.first)
		default:
			out = append(out, s.total)
		}
	}
	return out
}

// pct is the nearest-rank percentile q of ds, in milliseconds.
func pct(ds []time.Duration, q float64) float64 {
	if len(ds) == 0 {
		return math.NaN()
	}
	s := slices.Sorted(slices.Values(ds))
	return ms(s[int(math.Ceil(q*float64(len(s))))-1])
}

func median(v []float64) float64 {
	s := slices.Sorted(slices.Values(v))
	return (s[(len(s)-1)/2] + s[len(s)/2]) / 2
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// chunk is one OpenAI chat chunk carrying content, as SSE.
func chunk(content string) string {
	text, _ := json.Marshal(content)
	return `data: {"id":"chatcmpl-mock","object":"chat.completion.chunk","created":0,"model":"mock","choices":[{"index":0,"delta":{"content":` +
		string(text) + `},"finish_reason":null}]}` + "\n\n"
}

// thinkStep is the answer of an inline think stream: the tags split across
// chunks, then the visible text, the finish, the usage and [DONE].
var thinkStep = mockprovider.Step{Chunks: [][]byte{
	[]byte(chunk("<thi")), []byte(chunk("nk>weighing the answer</th")), []byte(chunk("ink>mock reply")),
	[]byte(`data: {"id":"chatcmpl-mock","object":"chat.completion.chunk","created":0,"model":"mock","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"),
	[]byte(`data: {"id":"chatcmpl-mock","object":"chat.completion.chunk","created":0,"model":"mock","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}` + "\n\n"),
	[]byte("data: [DONE]\n\n"),
}}

func chatBody(model string, stream bool) string {
	return fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"stream":%v}`, model, stream)
}

// benchConfig is the proxy's configuration for the measurements: the mock
// as an openai and as an anthropic provider, billed per use and priced, so
// every call writes a priced usage record; a key that may hold 1,000
// requests in flight; no telemetry.
const benchConfig = `version: 1
providers:
  mock: { protocol: openai, base_url: "%[1]s", billing: pay_per_use, allow_private_network: true }
  mocka: { protocol: anthropic, base_url: "%[1]s", billing: pay_per_use, allow_private_network: true }
targets:
  mock/mock: { kind: chat, pricing: { input_per_million: 1, output_per_million: 2, as_of: 2026-11-01 } }
  mock/think: { kind: chat, inline_think_tags: true, pricing: { input_per_million: 1, output_per_million: 2, as_of: 2026-11-01 } }
  mocka/mock: { kind: chat, pricing: { input_per_million: 1, output_per_million: 2, as_of: 2026-11-01 } }
models:
  fast: { targets: [mock/mock] }
  think: { targets: [mock/think] }
  translated: { targets: [mocka/mock], max_output_tokens: 1024 }
keys:
  bench: { models: [fast, think, translated], max_parallel_requests: 1000 }
`

// scaleConfig is a configuration at the sizes of kick-off section 18: 100
// providers, 1,000 priced targets, 1,000 aliases and 10,000 keys, each key
// allowed one alias.
func scaleConfig(mockURL string) string {
	var s strings.Builder
	s.WriteString("version: 1\nproviders:\n")
	for p := range 100 {
		fmt.Fprintf(&s, "  p%02d: { protocol: openai, base_url: %q, billing: pay_per_use, allow_private_network: true }\n", p, mockURL)
	}
	s.WriteString("targets:\n")
	for i := range 1000 {
		fmt.Fprintf(&s, "  p%02d/m%04d: { kind: chat, pricing: { input_per_million: 1, output_per_million: 2, as_of: 2026-11-01 } }\n", i/10, i)
	}
	s.WriteString("models:\n")
	for i := range 1000 {
		fmt.Fprintf(&s, "  a%04d: { targets: [p%02d/m%04d] }\n", i, i/10, i)
	}
	s.WriteString("keys:\n")
	for i := range 10000 {
		fmt.Fprintf(&s, "  k%05d: { models: [a%04d] }\n", i, i%1000)
	}
	return s.String()
}

// commitScale18 runs internal/config's BenchmarkCommitScale18 on one CPU
// and returns its time per change.
func commitScale18(t *testing.T) float64 {
	t.Helper()
	out, err := exec.Command("go", "test", "-count=1", "-run", "^$", "-bench", "^BenchmarkCommitScale18$", "-benchtime", "10x", "-cpu", "1",
		"../../internal/config").CombinedOutput()
	if err != nil {
		t.Fatalf("BenchmarkCommitScale18: %v: %s", err, out)
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if f := strings.Fields(line); len(f) >= 4 && strings.HasPrefix(f[0], "BenchmarkCommitScale18") && f[3] == "ns/op" {
			if v, err := strconv.ParseFloat(f[2], 64); err == nil {
				return v / 1e6
			}
		}
	}
	t.Fatalf("BenchmarkCommitScale18 printed no ns/op: %s", out)
	return 0
}

// proc is a running process, killed at the end of the test if still
// running. notes keeps its log lines above INFO, for a failure message.
type proc struct {
	cmd    *exec.Cmd
	exited chan struct{}
	mu     sync.Mutex
	notes  []string
}

// readyLine is a JSON log line; the ready line of the mock holds Addr, the
// one of serve DataAddr and AdminAddr.
type readyLine struct {
	Msg, Level, Addr string
	DataAddr         string `json:"data_addr"`
	AdminAddr        string `json:"admin_addr"`
}

// start runs bin, pinned to cpus when taskset is present and cpus is set,
// and waits for its ready line; the duration runs from the exec to it.
func start(t *testing.T, env []string, cpus, bin string, args ...string) (*proc, readyLine, time.Duration) {
	t.Helper()
	if ts, err := exec.LookPath("taskset"); err == nil && cpus != "" {
		args, bin = append([]string{"-c", cpus, bin}, args...), ts
	}
	p := &proc{cmd: exec.Command(bin, args...), exited: make(chan struct{})}
	p.cmd.Env = env
	logs, err := p.cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan readyLine, 1)
	readyAt := make(chan time.Time, 1)
	go func() {
		lines, seen := bufio.NewScanner(logs), false
		lines.Buffer(nil, 1<<20)
		for lines.Scan() {
			var l readyLine
			if json.Unmarshal(lines.Bytes(), &l) == nil && l.Msg == "ready" && !seen {
				seen = true
				readyAt <- time.Now()
				ready <- l
			}
			p.mu.Lock()
			if l.Level != "INFO" && len(p.notes) < 100 {
				p.notes = append(p.notes, lines.Text())
			}
			p.mu.Unlock()
		}
		_, _ = io.Copy(io.Discard, logs)
		_ = p.cmd.Wait()
		close(p.exited)
	}()
	t.Cleanup(func() {
		select {
		case <-p.exited:
		default:
			_ = p.cmd.Process.Kill()
			<-p.exited
		}
	})
	select {
	case l := <-ready:
		return p, l, (<-readyAt).Sub(began)
	case <-p.exited:
		t.Fatalf("%s exited before its ready line: %s", bin, p.log())
	case <-time.After(time.Minute):
		t.Fatalf("%s was not ready within a minute: %s", bin, p.log())
	}
	return nil, readyLine{}, 0
}

func (p *proc) log() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.notes, "\n")
}

// stop sends SIGTERM and waits for exit 0; it returns how long that took.
func stop(t *testing.T, p *proc) time.Duration {
	t.Helper()
	began := time.Now()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.exited:
	case <-time.After(70 * time.Second):
		t.Fatalf("no exit within 70 s of SIGTERM: %s", p.log())
	}
	if code := p.cmd.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("exit %d after SIGTERM: %s", code, p.log())
	}
	return time.Since(began)
}

// cli runs one stargaite command against the admin listener at admin, with
// the admin key of data, and returns its standard output.
func cli(t *testing.T, bin, admin, data string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = []string{"STARGAITE_ADMIN_URL=http://" + admin, "STARGAITE_DATA_DIR=" + data}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("stargaite %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.Bytes()
}

// rss is the process's resident set in MB (10^6 bytes), from VmRSS.
func rss(t *testing.T, p *proc) float64 {
	t.Helper()
	text, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", p.cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(text), "\n") {
		if f := strings.Fields(line); len(f) == 3 && f[0] == "VmRSS:" && f[2] == "kB" {
			if kb, err := strconv.ParseFloat(f[1], 64); err == nil {
				return kb * 1024 / 1e6
			}
		}
	}
	t.Fatalf("no VmRSS in /proc/%d/status", p.cmd.Process.Pid)
	return 0
}

// cpuTime is the CPU time the process has used, user and system, from
// /proc/<pid>/stat in clock ticks of 10 ms (USER_HZ is 100 on Linux).
func cpuTime(t *testing.T, p *proc) time.Duration {
	t.Helper()
	text, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", p.cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	// The fields after the command name, which is in parentheses: utime and
	// stime are the 12th and 13th.
	f := strings.Fields(string(text[bytes.LastIndexByte(text, ')')+1:]))
	user, uerr := strconv.ParseInt(f[11], 10, 64)
	system, serr := strconv.ParseInt(f[12], 10, 64)
	if uerr != nil || serr != nil {
		t.Fatalf("/proc/%d/stat: %s", p.cmd.Process.Pid, text)
	}
	return time.Duration(user+system) * 10 * time.Millisecond
}

// timeWait counts the TCP sockets in TIME_WAIT toward addr's port: on the
// proxy's side, one per connection to the mock that it closed in the last
// minute.
func timeWait(addr string) int {
	_, port, _ := strings.Cut(addr, ":")
	p, _ := strconv.Atoi(port)
	suffix, n := fmt.Sprintf(":%04X", p), 0
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		text, _ := os.ReadFile(f)
		for line := range strings.SplitSeq(string(text), "\n") {
			if f := strings.Fields(line); len(f) > 3 && f[3] == "06" && strings.HasSuffix(f[2], suffix) {
				n++
			}
		}
	}
	return n
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func firstLine(path string) string {
	text, _ := os.ReadFile(path)
	line, _, _ := strings.Cut(string(text), "\n")
	return strings.TrimSpace(line)
}

func cpuModel() string {
	text, _ := os.ReadFile("/proc/cpuinfo")
	for line := range strings.SplitSeq(string(text), "\n") {
		if name, value, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(name) == "model name" {
			return strings.TrimSpace(value)
		}
	}
	return "unknown"
}

func commit() string {
	out, err := exec.Command("git", "describe", "--always", "--dirty").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}
