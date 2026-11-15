// Package e2e runs the built binary as a user does: the quick start of
// README.md line by line, then a restart, against the mock provider. It dials
// loopback only, resolves no name, binds no fixed port and leaves no process
// behind.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/tests/mockprovider"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

const (
	// The values the quick start writes, which the test replaces with its own.
	mockDefault = "http://127.0.0.1:4010"
	tokenShown  = "sk-stargaite-..."
	dataDefault = "http://127.0.0.1:4000"
)

var (
	tokenLine  = regexp.MustCompile(`(?m)^sk-stargaite-\S+$`)
	curlURL    = regexp.MustCompile(regexp.QuoteMeta(dataDefault) + `(/\S*)`)
	curlHeader = regexp.MustCompile(`-H "([^"]+)"`)
	curlData   = regexp.MustCompile(`-d '([^']*)'`)
	traceID    = regexp.MustCompile(`^[0-9a-f]{32}$`)
	// The line of kick-off section 3.2 for an accepted test span.
	acceptedLine = regexp.MustCompile(`^credentials: resolved · reachable: yes · authorized: yes · span: accepted \(200\) · trace_id=([0-9a-f]{32})\n$`)
)

// TestQuickStart runs the quick start of README.md with the built binary,
// then checks what it left: the first answer of the chat completion with its
// PAR-9 headers and the one call the provider got (AC-04), the embeddings
// answer byte for byte with its call (AC-46), /v1/models with the new key,
// usage show with the two requests by alias, their tokens and their
// price_table costs (AC-33, AC-46), telemetry test accepted by the mock as
// OTLP receiver, which got the test span (AC-24), then the request spans of
// the chat completion and of the embeddings request with the key my-agent
// (AC-25), and nothing else, a backup with mode 0600 that opens with
// integrity_check ok and the records (ADR-018), the key's parallel limit shown as 5 and exported as declared
// (LIM-7), a second serve on the same directory refused (AC-42), and the
// same export, the same key and the same usage after a restart (RES-9). The guide's commands use
// the default data directory and addresses; here the environment points them
// at a temporary directory and at ports the system chose.
func TestQuickStart(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "stargaite")
	dir := filepath.Join(t.TempDir(), "data")
	env := []string{"STARGAITE_DATA_DIR=" + dir, "STARGAITE_LISTEN=127.0.0.1:0", "STARGAITE_ADMIN_LISTEN=127.0.0.1:0"}
	var (
		mock     *mockprovider.Server
		mockURL  string
		srv      *server
		token    string
		answered bool
		embedded bool   // the embeddings call got the mock's bytes
		shown    string // what usage show printed
		tested   string // what telemetry test printed
		trace    string // the trace id of the chat completion
		embTrace string // the trace id of the embeddings request
		backedUp bool   // the backup opened with integrity_check ok
	)
	cli := func(args ...string) string {
		t.Helper()
		code, stdout, stderr := run(t, "", slices.Concat(env, []string{"STARGAITE_ADMIN_URL=http://" + srv.admin}), bin, args...)
		if code != 0 {
			t.Fatalf("stargaite %q: exit %d; stdout %q stderr %q", args, code, stdout, stderr)
		}
		return stdout
	}

	for _, line := range quickStart(t) {
		if mockURL != "" {
			line = strings.ReplaceAll(line, mockDefault, mockURL)
		}
		line = strings.ReplaceAll(line, tokenShown, token)
		fields := strings.Fields(line)
		switch {
		case line == "go build -o stargaite ./cmd/stargaite":
			if code, _, stderr := run(t, "../..", nil, "go", "build", "-o", bin, "./cmd/stargaite"); code != 0 {
				t.Fatalf("%s: exit %d: %s", line, code, stderr)
			}
		case line == "go run ./tests/mockprovider/cmd/mockprovider &":
			mock = mockprovider.New()
			s := httptest.NewServer(mock)
			t.Cleanup(s.Close)
			mockURL = s.URL
		case fields[0] == "./stargaite" && fields[len(fields)-1] == "&":
			srv = start(t, bin, env, fields[1:len(fields)-1]...)
		case fields[0] == "./stargaite" && fields[1] == "usage" && srv != nil:
			// A record reaches the store within a batch's 50 ms: the line runs
			// again until it shows both requests.
			for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
				if shown = cli(fields[1:]...); strings.Count(shown, "pay_per_use") == 2 || time.Now().After(deadline) {
					break
				}
			}
		case fields[0] == "./stargaite" && fields[1] == "backup" && srv != nil:
			// The copy goes to a directory of the test's, not to the clone.
			file := filepath.Join(t.TempDir(), fields[2])
			if out := cli("backup", file); !strings.HasPrefix(out, file+": ") {
				t.Errorf("%s printed %q", line, out)
			}
			backedUp = checkBackup(t, file)
		case fields[0] == "./stargaite" && fields[1] == "telemetry" && fields[2] == "test" && srv != nil:
			// AC-24: the mock, as OTLP receiver, accepts the test span.
			if tested = cli(fields[1:]...); !acceptedLine.MatchString(tested) {
				t.Errorf("%s printed %q", line, tested)
			}
		case fields[0] == "./stargaite" && srv != nil:
			if m := tokenLine.FindString(cli(fields[1:]...)); m != "" {
				token = m
			}
		case fields[0] == "curl" && srv != nil:
			m := curlURL.FindStringSubmatch(line)
			if m == nil {
				t.Fatalf("%s: no URL on %s", line, dataDefault)
			}
			var header []string
			if h := curlHeader.FindStringSubmatch(line); h != nil {
				header = []string{h[1]}
			}
			// curl -d sends a POST with a form content type, which the proxy ignores.
			method, data := http.MethodGet, ""
			if d := curlData.FindStringSubmatch(line); d != nil {
				method, data = http.MethodPost, d[1]
				header = append(header, "Content-Type: application/x-www-form-urlencoded")
			}
			status, h, body := send(t, method, "http://"+srv.data+m[1], data, header...)
			if status != http.StatusOK {
				t.Fatalf("%s: status %d: %s", line, status, body)
			}
			if method == http.MethodPost && m[1] == "/v1/embeddings" {
				// The mock's default answer, relayed byte for byte.
				embedded, embTrace = true, h.Get("X-Stargaite-Trace-Id")
				if string(body) != `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0,0.5,1]}],"model":"mock","usage":{"prompt_tokens":1,"total_tokens":1}}` {
					t.Errorf("%s: body %s", line, body)
				}
				if !traceID.MatchString(embTrace) || h.Get("X-Stargaite-Target") != "mock/mock-embed" || h.Get("Content-Type") != "application/json" {
					t.Errorf("%s: headers %v", line, h)
				}
			} else if method == http.MethodPost {
				answered, trace = true, h.Get("X-Stargaite-Trace-Id")
				var completion struct {
					Choices []struct{ Message struct{ Content string } }
				}
				if err := json.Unmarshal(body, &completion); err != nil || len(completion.Choices) != 1 || completion.Choices[0].Message.Content == "" {
					t.Errorf("%s: body %s", line, body)
				}
				if h.Get("X-Stargaite-Request-Id") == "" || !traceID.MatchString(h.Get("X-Stargaite-Trace-Id")) || h.Get("X-Stargaite-Target") != "mock/mock" ||
					h.Values("X-Stargaite-Reasoning-Effort") != nil || h.Values("X-Stargaite-Params-Changed") != nil {
					t.Errorf("%s: headers %v", line, h)
				}
			}
		default:
			t.Fatalf("a quick start line this test does not run: %q", line)
		}
	}
	if srv == nil || mock == nil || token == "" || !answered || !embedded || shown == "" || tested == "" || !backedUp {
		t.Fatalf("the quick start did not start the proxy and the mock provider, create a key, test the exporter, get both answers, show the usage and back up")
	}
	// By alias: the chat completion's usage, 1 input and 2 output tokens, at 1
	// and 2 USD per million, an output median and p90 of 2; the embeddings
	// request's, 1 input token and 0 output, at 1 USD per million; no length
	// finish, no empty answer and no reasoning reported.
	if !regexp.MustCompile(`(?m)\Awindow \S+ to \S+, from the records\n.*\npay_per_use +embed +1 +0 +1 +0 +0 +0 +0 +0 +0\.000001 +0 +0 +0 +0 +0 +- +- *\n` +
		`pay_per_use +fast +1 +0 +1 +0 +0 +2 +0 +0 +0\.000005 +0 +0 +0 +2 +2 +- +- *$`).MatchString(shown) {
		t.Errorf("usage show printed:\n%s", shown)
	}
	usage := func() {
		t.Helper()
		var rep struct {
			Source string
			Groups []struct {
				Billing                               string
				Requests, Errors, Estimated, Unpriced int64
				Input                                 int64    `json:"input_tokens"`
				Output                                int64    `json:"output_tokens"`
				CostUSD                               *float64 `json:"cost_usd"`
			}
		}
		out := cli("usage", "show", "--since", "1h", "--json")
		if err := json.Unmarshal([]byte(out), &rep); err != nil || rep.Source != "records" || len(rep.Groups) != 1 {
			t.Fatalf("usage show --json: %s", out)
		}
		if g := rep.Groups[0]; g.Billing != "pay_per_use" || g.Requests != 2 || g.Errors != 0 || g.Estimated != 0 || g.Unpriced != 0 ||
			g.Input != 2 || g.Output != 2 || g.CostUSD == nil || *g.CostUSD != 6e-6 {
			t.Errorf("usage show --json: %s", out)
		}
		// AC-35: the medians of two windows, by key, alias and effort sent,
		// from the local store alone.
		type window struct {
			Source string `json:"source"`
			Groups []struct {
				Key        string `json:"key"`
				Model      string `json:"model"`
				EffortSent string `json:"effort_sent"`
				OutputP50  *int64 `json:"output_tokens_p50"`
			} `json:"groups"`
		}
		var compared struct {
			window
			Previous *window `json:"previous"`
		}
		out = cli("usage", "show", "--since", "1h", "--by", "key,model,effort-sent", "--compare-previous", "--json")
		if err := json.Unmarshal([]byte(out), &compared); err != nil || compared.Source != "records" || len(compared.Groups) != 2 ||
			compared.Previous == nil || compared.Previous.Source != "records" || len(compared.Previous.Groups) != 0 {
			t.Fatalf("usage show --compare-previous --json: %s", out)
		}
		for i, want := range []struct {
			model string
			p50   int64
		}{{"embed", 0}, {"fast", 2}} {
			if g := compared.Groups[i]; g.Key != "my-agent" || g.Model != want.model || g.EffortSent != "-" || g.OutputP50 == nil || *g.OutputP50 != want.p50 {
				t.Errorf("usage show --compare-previous --json: %s", out)
			}
		}
	}
	usage()

	models := func() {
		t.Helper()
		status, _, body := send(t, http.MethodGet, "http://"+srv.data+"/v1/models", "", "Authorization: Bearer "+token)
		var list struct {
			Object string
			Data   []struct{ ID string }
		}
		if err := json.Unmarshal(body, &list); err != nil || status != http.StatusOK || list.Object != "list" || len(list.Data) != 2 ||
			list.Data[0].ID != "embed" || list.Data[1].ID != "fast" {
			t.Errorf("/v1/models: status %d, body %s", status, body)
		}
	}
	models()
	// The chat call and the embeddings call of the quick start, each with
	// the upstream model id and no credential: the mock provider declares
	// none. The mock is also the OTLP receiver: telemetry test sent it one
	// export, the test span alone, and the request spans of the two calls
	// follow in the next batches, within the 2 s of a batch.
	var calls, exports []mockprovider.Call
	var spans []span
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		calls, exports, spans = nil, nil, nil
		for _, c := range mock.Calls() {
			if c.Path == "/v1/traces" {
				exports = append(exports, c)
			} else {
				calls = append(calls, c)
			}
		}
		for _, e := range exports[min(1, len(exports)):] {
			spans = append(spans, exported(t, e.Body)...)
		}
		if len(spans) >= 2 || time.Now().After(deadline) {
			break
		}
	}
	if len(calls) != 2 || calls[0].Path != "/chat/completions" ||
		string(calls[0].Body) != `{"model":"mock","messages":[{"role":"user","content":"hi"}]}` || calls[0].Header.Values("Authorization") != nil ||
		calls[1].Path != "/embeddings" || string(calls[1].Body) != `{"model":"mock-embed","input":"hi"}` || calls[1].Header.Values("Authorization") != nil {
		t.Fatalf("the mock received %+v", calls)
	}
	if len(exports) < 2 || exports[0].Header.Get("Content-Type") != "application/json" || !json.Valid(exports[0].Body) ||
		!strings.Contains(string(exports[0].Body), `"name":"stargaite telemetry test"`) || !strings.Contains(string(exports[0].Body), `{"stringValue":"_telemetry"}`) ||
		!strings.Contains(string(exports[0].Body), `"traceId":"`+acceptedLine.FindStringSubmatch(tested)[1]+`"`) {
		t.Fatalf("the receiver got %+v", exports)
	}
	if len(spans) != 2 {
		t.Fatalf("%d request spans after the test span, want 2: %+v", len(spans), spans)
	}
	requestSpan(t, spans[0], trace, "chat fast", map[string]any{"stargaite.model": "fast", "stargaite.target": "mock/mock",
		"gen_ai.operation.name": "chat", "gen_ai.usage.input_tokens": "1", "gen_ai.usage.output_tokens": "2"})
	requestSpan(t, spans[1], embTrace, "embeddings embed", map[string]any{"stargaite.model": "embed", "stargaite.target": "mock/mock-embed",
		"gen_ai.operation.name": "embeddings", "gen_ai.usage.input_tokens": "1", "gen_ai.usage.output_tokens": "0"})
	if status := cli("telemetry", "status"); !strings.Contains(status, "exporter.sent: 3\n") || !strings.Contains(status, "telemetry.otlp_endpoint: "+mockURL+"\n") {
		t.Errorf("telemetry status:\n%s", status)
	}
	// models explain, run above without a call, shows the bytes the provider
	// received for the same request (AC-17).
	var explained struct {
		Targets []struct{ Target, Body string }
	}
	if out := cli("models", "explain", "fast", "--json"); json.Unmarshal([]byte(out), &explained) != nil || len(explained.Targets) != 1 ||
		explained.Targets[0].Target != "mock/mock" || explained.Targets[0].Body != string(calls[0].Body) {
		t.Errorf("models explain answered %s", out)
	}
	before := cli("export")
	if !strings.Contains(before, "my-agent") || strings.Contains(before, token) {
		t.Errorf("export lacks the key or carries its token:\n%s", before)
	}
	// LIM-7: the key's parallel limit is published as 5 and exported as
	// declared, that is not at all.
	var shownKey struct {
		RPMLimit            int64 `json:"rpm_limit"`
		MaxParallelRequests int64 `json:"max_parallel_requests"`
	}
	if out := cli("keys", "show", "my-agent", "--json"); json.Unmarshal([]byte(out), &shownKey) != nil || shownKey.RPMLimit != 60 || shownKey.MaxParallelRequests != 5 {
		t.Errorf("keys show: %s", out)
	}
	if !strings.Contains(before, "rpm_limit: 60") || strings.Contains(before, "max_parallel_requests") {
		t.Errorf("export of the key's limits:\n%s", before)
	}

	// A second serve on the same data directory refuses to start (AC-42).
	if code, _, stderr := run(t, "", env, bin, "serve"); code != 1 || !strings.Contains(stderr, `"msg":"store failed"`) {
		t.Errorf("second serve: exit %d, log %q", code, stderr)
	}

	code, log := srv.stop(t)
	if code != 0 || !strings.Contains(log, `"msg":"stopped"`) {
		t.Fatalf("stop: exit %d, log:\n%s", code, log)
	}
	// The stop flushes the exporter: nothing was left to send.
	if n := len(mock.Calls()); n != len(calls)+len(exports) {
		t.Errorf("%d calls to the mock after the stop, %d before", n, len(calls)+len(exports))
	}
	// OPS-9: each provider call and each request logged once, with the key and the alias.
	if strings.Count(log, `"msg":"attempt"`) != 2 || !strings.Contains(log, `"key":"my-agent","alias":"fast","attempts":1,"status":200`) ||
		!strings.Contains(log, `"key":"my-agent","alias":"embed","attempts":1,"status":200`) {
		t.Errorf("log:\n%s", log)
	}
	srv = start(t, bin, env, "serve")
	models()
	usage()
	if after := cli("export"); after != before {
		t.Errorf("export after the restart:\n%s\nbefore:\n%s", after, before)
	}
	code, restarted := srv.stop(t)
	if code != 0 {
		t.Errorf("stop after the restart: exit %d", code)
	}

	// Logs are JSON lines and never carry the admin key or a client key.
	adminKey, err := os.ReadFile(filepath.Join(dir, "admin.key"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(log+restarted), "\n") {
		if !json.Valid([]byte(line)) || strings.Contains(line, strings.TrimSpace(string(adminKey))) || strings.Contains(line, token) {
			t.Errorf("log line is not JSON or carries a key: %q", line)
		}
	}
}

// checkBackup reports whether the backup at file has mode 0600 and opens
// read-only with integrity_check ok, an empty foreign_key_check and the two
// usage records of the quick start (ADR-018).
func checkBackup(t *testing.T, file string) bool {
	t.Helper()
	if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the backup: %v, error %v", info, err)
		return false
	}
	db, err := sql.Open("sqlite", "file:"+file+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var integrity string
	var violations, records int
	if err := db.QueryRow("SELECT (SELECT integrity_check FROM pragma_integrity_check), (SELECT COUNT(*) FROM pragma_foreign_key_check), (SELECT COUNT(*) FROM usage)").
		Scan(&integrity, &violations, &records); err != nil || integrity != "ok" || violations != 0 || records != 2 {
		t.Errorf("the backup: integrity_check %q, %d foreign key violations, %d records, error %v", integrity, violations, records, err)
		return false
	}
	return true
}

// span is one exported span, its attributes by key.
type span struct {
	TraceID string
	Name    string
	Kind    int
	Attrs   map[string]any
}

// exported is the spans of one OTLP export, in order.
func exported(t *testing.T, export []byte) []span {
	t.Helper()
	var req struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []struct {
					TraceID    string `json:"traceId"`
					Name       string
					Kind       int
					Attributes []struct {
						Key   string
						Value map[string]any
					}
				}
			}
		}
	}
	if err := json.Unmarshal(export, &req); err != nil {
		t.Fatalf("the export %s", export)
	}
	var out []span
	for _, rs := range req.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			for _, s := range ss.Spans {
				attrs := map[string]any{}
				for _, a := range s.Attributes {
					for _, v := range a.Value {
						attrs[a.Key] = v
					}
				}
				out = append(out, span{s.TraceID, s.Name, s.Kind, attrs})
			}
		}
	}
	return out
}

// requestSpan checks that s is the request span of a quick start call:
// SERVER, named after the operation and the alias, in the trace the
// response named, with the key my-agent, its compatibility attribute, status
// 200 and the attributes of want (AC-25, kick-off annex D).
func requestSpan(t *testing.T, s span, trace, name string, want map[string]any) {
	t.Helper()
	want["stargaite.key"], want["langfuse.observation.metadata.stargaite_key"], want["http.response.status_code"] = "my-agent", "my-agent", "200"
	for k, v := range want {
		if s.Attrs[k] != v {
			t.Errorf("the request span %q: %s is %v, want %v", s.Name, k, s.Attrs[k], v)
		}
	}
	if s.TraceID != trace || s.Name != name || s.Kind != 2 {
		t.Errorf("the request span %+v, the trace %s", s, trace)
	}
}

// TestListenerOutsideLoopback: without TLS or --behind-tls-proxy, serve exits
// 1 before it opens the store (AC-45), and keeps its output JSON with --json.
func TestListenerOutsideLoopback(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "stargaite")
	if code, _, stderr := run(t, "../..", nil, "go", "build", "-o", bin, "./cmd/stargaite"); code != 0 {
		t.Fatalf("build: exit %d: %s", code, stderr)
	}
	dir := filepath.Join(t.TempDir(), "data")
	code, stdout, stderr := run(t, "", []string{"STARGAITE_DATA_DIR=" + dir}, bin, "serve", "--json", "--listen", "0.0.0.0:0", "--admin-listen", "127.0.0.1:0")
	var line struct{ Msg, Listener, Rule string }
	if code != 1 || stdout != "" || json.Unmarshal([]byte(stderr), &line) != nil || line != (struct{ Msg, Listener, Rule string }{"listener refused", "data", "SEC-6"}) {
		t.Errorf("exit %d, stdout %q, log %q", code, stdout, stderr)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused serve created its data directory: %v", err)
	}
}

// quickStart returns the command lines of the first sh block after the
// "## Quick start" heading of README.md, exactly as written. A line with "#"
// fails: stock interactive zsh does not treat it as a comment.
func quickStart(t *testing.T) []string {
	t.Helper()
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(readme), "\n## Quick start\n")
	_, block, ok2 := strings.Cut(section, "\n```sh\n")
	block, _, ok3 := strings.Cut(block, "\n```")
	if !ok || !ok2 || !ok3 {
		t.Fatal("README.md has no sh block under ## Quick start")
	}
	var lines []string
	for _, line := range strings.Split(block, "\n") {
		if strings.Contains(line, "#") {
			t.Fatalf("a quick start line with #, which zsh passes on as arguments: %q", line)
		}
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// run runs a command to its end, with standard input empty and no terminal,
// within a bounded time. A nil env inherits the test's environment.
func run(t *testing.T, dir string, env []string, name string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	timeout := 30 * time.Second
	if name == "go" {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Env, cmd.WaitDelay = dir, env, 5*time.Second
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil && !errors.As(err, new(*exec.ExitError)) {
		t.Fatalf("%s %q: %v", name, args, err)
	}
	return cmd.ProcessState.ExitCode(), out.String(), errOut.String()
}

// send sends one request with the given body and "Name: value" headers.
func send(t *testing.T, method, url, body string, headers ...string) (int, http.Header, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range headers {
		name, value, _ := strings.Cut(h, ": ")
		req.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, b
}

// server is a running serve, killed at the end of the test if still running.
type server struct {
	cmd         *exec.Cmd
	data, admin string // the bound addresses, from the ready line
	exited      chan struct{}
	code        int
	mu          sync.Mutex
	log         strings.Builder
}

// start runs serve and waits for its ready line.
func start(t *testing.T, bin string, env []string, args ...string) *server {
	t.Helper()
	s := &server{cmd: exec.Command(bin, args...), exited: make(chan struct{})}
	logs, w := io.Pipe()
	s.cmd.Env, s.cmd.Stdout, s.cmd.Stderr = env, w, w
	if err := s.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan [2]string, 1)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for lines := bufio.NewScanner(logs); lines.Scan(); {
			s.mu.Lock()
			s.log.WriteString(lines.Text() + "\n")
			s.mu.Unlock()
			var line struct {
				Msg       string
				DataAddr  string `json:"data_addr"`
				AdminAddr string `json:"admin_addr"`
			}
			if json.Unmarshal(lines.Bytes(), &line) == nil && line.Msg == "ready" {
				select {
				case ready <- [2]string{line.DataAddr, line.AdminAddr}:
				default:
				}
			}
		}
	}()
	go func() {
		_ = s.cmd.Wait() // the exit code is read from ProcessState
		_ = w.Close()
		<-drained
		s.code = s.cmd.ProcessState.ExitCode()
		close(s.exited)
	}()
	t.Cleanup(func() {
		select {
		case <-s.exited:
		default:
			_ = s.cmd.Process.Kill()
			<-s.exited
		}
	})

	select {
	case addrs := <-ready:
		s.data, s.admin = addrs[0], addrs[1]
	case <-s.exited:
		t.Fatalf("serve exited %d before it was ready; log:\n%s", s.code, s.logs())
	case <-time.After(30 * time.Second):
		t.Fatalf("serve was not ready within 30 s; log:\n%s", s.logs())
	}
	return s
}

// stop sends SIGTERM and returns the exit code and the whole log.
func (s *server) stop(t *testing.T) (int, string) {
	t.Helper()
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.exited:
	case <-time.After(15 * time.Second):
		t.Fatalf("serve did not stop within 15 s; log:\n%s", s.logs())
	}
	return s.code, s.logs()
}

func (s *server) logs() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.log.String()
}
