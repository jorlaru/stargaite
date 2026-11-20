package guided

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
)

// The guided runs of Cap spending, See usage and cost and Send traces:
// successors of setup_test.go's TestSetupPrompts budget and telemetry rows
// at layer 3.

// changes is argvs without the reads.
func changes(argvs []string) []string {
	return slices.DeleteFunc(slices.Clone(argvs), func(a string) bool {
		return strings.Contains(a, " list ") || strings.Contains(a, " show ") || strings.HasPrefix(a, "telemetry status")
	})
}

// makeKey makes key name toward alias a through the CLI, unrecorded, and
// returns its secret.
func (w *aliasWorld) makeKey(t *testing.T, name string) string {
	t.Helper()
	var made proxy.KeyChange
	if err := json.Unmarshal([]byte(w.must(t, "keys", "create", "--json", "--model", "a", "--", name)), &made); err != nil {
		t.Fatal(err)
	}
	return made.Secrets[name]
}

// chat sends one chat completion toward alias a with secret to the data
// plane at port, and fails the test unless it is answered.
func chat(t *testing.T, port, secret string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+port+"/v1/chat/completions",
		strings.NewReader(`{"model":"a","max_tokens":16,"messages":[{"role":"user","content":"Hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat: %d", resp.StatusCode)
	}
}

// usage waits, 5 s at most, until usage show by key over 24 h holds n
// groups (a record is written after its call returns), and returns it.
func (w *aliasWorld) usage(t *testing.T, n int) proxy.UsageReply {
	t.Helper()
	var rep proxy.UsageReply
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if err := json.Unmarshal([]byte(w.must(t, "usage", "show", "--json", "--since", "24h", "--by", "key")), &rep); err != nil {
			t.Fatal(err)
		}
		if len(rep.Groups) == n {
			return rep
		}
	}
	t.Fatalf("usage show holds %d group(s), not %d", len(rep.Groups), n)
	return rep
}

// noFrameHolds fails the test when a frame holds s.
func (h *harness) noFrameHolds(s string) {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, f := range h.frames {
		if strings.Contains(f.Content, s) {
			h.t.Fatalf("a frame holds %q:\n%s", s, f.Content)
		}
	}
}

// yes waits for a confirm frame that also holds s, and answers Yes.
func (h *harness) yes(s string) {
	h.t.Helper()
	h.waitFrame(func(v tea.View) bool {
		return strings.Contains(v.Content, "continue?") && strings.Contains(v.Content, s)
	})
	h.p.Send(key(tea.KeyUp))
	h.waitFrame(func(v tea.View) bool { return strings.Contains(v.Content, "> Yes") && strings.Contains(v.Content, s) })
	h.enter()
}

func TestGuidedCapSpending(t *testing.T) {
	h := start(t)
	w, _, srvURL := planeWorld(t)
	w.must(t, "providers", "add", "acme", "--protocol", "openai", "--base-url", srvURL, "--allow-private-network", "--billing", "pay_per_use")
	w.must(t, "targets", "add", "--kind", "chat", "--", "acme/m1")
	w.must(t, "targets", "verify", "--", "acme/m1")
	w.must(t, "models", "add", "--target", "acme/m1", "--", "a")
	w.makeKey(t, "k")
	before := len(w.mock.Calls())
	done := w.run(h, "Cap spending", nil)
	f := h.waitShown("What is the cap on?")
	for _, s := range []string{"> everything (the instance)\n", "── Aliases\n", "  alias a\n", "── Apps and agents\n", "  key k\n"} {
		if !strings.Contains(f.Content, s) {
			t.Errorf("the scope pick lacks %q:\n%s", s, f.Content)
		}
	}
	h.choose("alias a")
	h.waitShown("the limit in USD")
	h.choose("5")
	f = h.waitShown("When does it reset?")
	if !strings.Contains(f.Content, "> monthly\n") {
		t.Errorf("monthly is not first:\n%s", f.Content)
	}
	h.enter()
	h.yes("● limit and reset")
	h.waitShown("the price of acme/m1 in USD per million input tokens")
	h.choose("2")
	h.waitShown("the price of acme/m1 in USD per million output tokens")
	h.choose("8")
	f = h.waitShown("1 target(s) must be checked again: 1 paid call(s)")
	if !strings.Contains(f.Content, "> Check it") || !strings.Contains(f.Content, "Not now") {
		t.Errorf("paid frame:\n%s", f.Content)
	}
	h.p.Send(key(tea.KeyEscape))
	if r := ended(t, done, 5*time.Second); !errors.Is(r.err, ui.ErrLeave) {
		t.Fatalf("%+v; want ErrLeave", r)
	}
	h.waitOutput("already changed and kept: prices of acme/m1 set; alias a: budget 5 USD, reset monthly")
	if n := len(w.mock.Calls()) - before; n != 0 {
		t.Errorf("%d mock calls, want 0", n)
	}
	budget := "models set --json --budget limit_usd=5,reset_interval=monthly -- a"
	today := time.Now().Format(time.DateOnly)
	want := []string{budget, "targets set --json --pricing input_per_million=2,output_per_million=8,as_of=" + today + " -- acme/m1", budget}
	if got := changes(w.cli()); !slices.Equal(got, want) {
		t.Errorf("changes %q\nwant %q", got, want)
	}
	h.front.Done()
	h.ended()
	h.fits()
	if bars := h.steps("on what", "limit and reset", "check"); !slices.Equal(bars, []string{"on what", "limit and reset", "check"}) {
		t.Errorf("the step bar went %q", bars)
	}
}

func TestGuidedSeeUsage(t *testing.T) {
	h := start(t)
	w, port := newKeyWorld(t, "mock/m")
	for _, k := range []string{"app-1", "app-2"} {
		chat(t, port, w.makeKey(t, k))
	}
	rep := w.usage(t, 2)
	done := w.run(h, "See usage and cost", nil)
	h.waitShown("Which period?")
	h.choose("24 hours")
	h.waitShown("Grouped by?")
	h.choose("key (an")
	if r := ended(t, done, 5*time.Second); r.err != nil || r.res != "Done: usage of the last 24 hours by key." {
		t.Fatalf("%+v\n%s", r, h.out.String())
	}
	h.waitOutput("Done: usage of the last 24 hours by key.")
	h.front.Done()
	h.ended()
	h.fits()
	lines := strings.Split(strings.ReplaceAll(ansi.Strip(h.out.String()), "\r", ""), "\n")
	if !slices.ContainsFunc(lines, func(l string) bool {
		return strings.Contains(l, "key    billing  requests  errors  input  output  cost USD")
	}) {
		t.Errorf("no header in the output:\n%q", lines)
	}
	for _, g := range rep.Groups {
		row := []string{g.Key, g.Billing, strconv.FormatInt(g.Requests, 10), strconv.FormatInt(g.Errors, 10),
			strconv.FormatInt(g.Input, 10), strconv.FormatInt(g.Output, 10), "-"}
		if !slices.ContainsFunc(lines, func(l string) bool { return slices.Equal(strings.Fields(l), row) }) {
			t.Errorf("no row %q in the output:\n%q", row, lines)
		}
	}
	h.noFrameHolds("billing  requests")
	h.noFrameHolds("app-1")
	if got := w.cli(); !slices.Equal(got, []string{"usage show --json --since 24h --by key"}) {
		t.Errorf("CLI calls %q", got)
	}
}

// spans is an OTLP receiver on 127.0.0.1 that accepts every span and
// records each one's headers; the plane's exporter dials 127.0.0.1 for
// every name, so host.docker.internal:<port> reaches it.
type spans struct {
	port string
	mu   sync.Mutex
	got  []http.Header
}

func receive(t *testing.T) *spans {
	t.Helper()
	s := &spans{}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v1/traces") {
			s.mu.Lock()
			s.got = append(s.got, r.Header.Clone())
			s.mu.Unlock()
		}
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	s.port = u.Port()
	return s
}

func (s *spans) headers() []http.Header { s.mu.Lock(); defer s.mu.Unlock(); return slices.Clone(s.got) }

// leaks fails the test when secret is in a frame, in the output (every
// said line) or in an argv of the CLI.
func leaks(t *testing.T, h *harness, w *aliasWorld, secret string) {
	t.Helper()
	h.noFrameHolds(secret)
	if strings.Contains(h.out.String(), secret) {
		t.Errorf("the output holds %q", secret)
	}
	if slices.ContainsFunc(w.cli(), func(a string) bool { return strings.Contains(a, secret) }) {
		t.Errorf("an argv holds %q: %q", secret, w.cli())
	}
}

const accepted = "credentials: resolved · reachable: yes · authorized: yes · span: accepted (200) · trace_id="

func TestGuidedSendTraces(t *testing.T) {
	const header = `Bearer s3cr3t $HOME #x y\z`
	h := start(t)
	w, _ := newKeyWorld(t)
	langfuse, otlp := receive(t), receive(t)
	old := "http://host.docker.internal:" + langfuse.port + "/api/public/otel"
	w.must(t, "telemetry", "set", "--otlp-endpoint", old, "--preset", "langfuse", "--allow-private-network")
	done := w.run(h, "Send traces", nil)
	h.waitShown("Where do traces go?")
	h.choose("OTLP")
	h.waitShown("spans go to <endpoint>/v1/traces")
	h.choose("http://localhost:" + otlp.port)
	f := h.waitShown("empty for none")
	if !strings.Contains(f.Content, "Authorization") {
		t.Errorf("header frame:\n%s", f.Content)
	}
	h.choose("Authorization")
	h.yes("● endpoint")
	h.waitOutput("telemetry toward " + old + " is replaced whole")
	h.waitShown("typed hidden")
	h.p.Send(tea.PasteMsg{Content: header})
	h.enter()
	h.yes("● write")
	ep := "http://host.docker.internal:" + otlp.port
	if r := ended(t, done, 10*time.Second); r.err != nil || r.res != "Done: traces go to "+ep+"." {
		t.Fatalf("%+v\n%s", r, h.out.String())
	}
	h.waitOutput(accepted)
	h.front.Done()
	h.ended()
	h.fits()
	env, err := os.ReadFile(filepath.Join(w.h.Dir, ".env"))
	if err != nil || !strings.Contains(string(env), "\nOTLP_AUTHORIZATION='"+header+"'\n") {
		t.Errorf(".env %q, %v", env, err)
	}
	leaks(t, h, w, "s3cr3t")
	want := []string{"telemetry status --json", "telemetry unset --json --yes",
		"telemetry set --json --otlp-endpoint " + ep + " --header-env Authorization=OTLP_AUTHORIZATION --allow-private-network",
		"telemetry status --json", "telemetry test --json"}
	if got := w.cli(); !slices.Equal(got, want) {
		t.Errorf("CLI calls %q\nwant %q", got, want)
	}
	if got := otlp.headers(); len(got) != 1 || got[0].Get("Authorization") != "x" {
		t.Errorf("spans at the OTLP receiver: %v", got)
	}
	if bars := h.steps("where", "endpoint", "keys", "write", "recreate"); !slices.Equal(bars, []string{"where", "endpoint", "keys", "write", "recreate"}) {
		t.Errorf("the step bar went %q", bars)
	}

	// Langfuse replaces it: two keys pasted, both written, neither shown.
	h = start(t)
	done = w.run(h, "Send traces", nil)
	h.waitShown("Where do traces go?")
	h.choose("Langfuse")
	h.waitShown("the Langfuse endpoint")
	h.choose("http://localhost:" + langfuse.port + "/api/public/otel")
	h.yes("● endpoint")
	h.waitShown("LANGFUSE_PUBLIC_KEY")
	h.p.Send(tea.PasteMsg{Content: "pk-lf-pasted-1"})
	h.enter()
	h.waitShown("LANGFUSE_SECRET_KEY")
	h.p.Send(tea.PasteMsg{Content: "sk-lf-pasted-2"})
	h.enter()
	h.yes("● write")
	if r := ended(t, done, 10*time.Second); r.err != nil || r.res != "Done: traces go to "+old+"." {
		t.Fatalf("%+v\n%s", r, h.out.String())
	}
	h.waitOutput("span: accepted (200)")
	h.front.Done()
	h.ended()
	h.fits()
	env, err = os.ReadFile(filepath.Join(w.h.Dir, ".env"))
	if err != nil || !strings.Contains(string(env), "\nLANGFUSE_PUBLIC_KEY='pk-lf-pasted-1'\n") ||
		!strings.Contains(string(env), "\nLANGFUSE_SECRET_KEY='sk-lf-pasted-2'\n") {
		t.Errorf(".env %q, %v", env, err)
	}
	for _, s := range []string{"pk-lf-pasted-1", "sk-lf-pasted-2"} {
		leaks(t, h, w, s)
	}
	if got := langfuse.headers(); len(got) != 1 || got[0].Get("X-Langfuse-Ingestion-Version") != "4" {
		t.Errorf("spans at the Langfuse receiver: %v", got)
	}
}
