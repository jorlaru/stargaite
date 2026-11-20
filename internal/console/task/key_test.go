package task

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/admintest"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
	"github.com/jorlaru/stargaite/internal/console/ui/plain"
	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// TestPlaneServes: admintest.Plane serves the data plane beside the admin
// API, on one configuration, and a key the CLI makes authenticates on it at
// once (the probe of research-console section 6).
func TestPlaneServes(t *testing.T) {
	env, port := admintest.Plane(t)
	mock := mockprovider.New()
	msrv := httptest.NewServer(mock)
	t.Cleanup(msrv.Close)
	px, ctx := proxy.New(cliRunner(env)), context.Background()
	for _, c := range []struct {
		words string
		flags []string
		name  string
	}{
		{"providers add", []string{"--protocol", "openai", "--base-url", msrv.URL, "--allow-private-network", "--billing", "none"}, "mock"},
		{"targets add", []string{"--kind", "chat"}, "mock/m"},
		{"models add", []string{"--target", "mock/m"}, "a"},
	} {
		if err := px.Call(ctx, &proxy.Change{}, c.words, c.flags, c.name); err != nil {
			t.Fatal(err)
		}
	}
	secret, _, err := px.NewSecret(ctx, "keys create", []string{"--model", "a"}, "k")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://127.0.0.1:" + port
	resp, err := http.Get(base + "/health/ready")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("/health/ready answered %d", resp.StatusCode)
	}
	req, err := http.NewRequest(http.MethodPost, base+"/v1/chat/completions",
		strings.NewReader(`{"model":"a","max_tokens":256,"messages":[{"role":"user","content":"Hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "mock reply") || resp.Header.Get("X-Stargaite-Target") != "mock/m" {
		t.Fatalf("chat completion: %d, target %q, body %s", resp.StatusCode, resp.Header.Get("X-Stargaite-Target"), body)
	}
	var upstream struct{ Model string }
	if calls := mock.Calls(); len(calls) != 1 || json.Unmarshal(calls[0].Body, &upstream) != nil || upstream.Model != "m" {
		t.Fatalf("%d mock calls, the first toward %q", len(calls), upstream.Model)
	}
}

// keyWorld is the world on admintest.Plane: .env names the data plane's
// port, so readiness and every try reach it; providers mock and mock2 on one
// mock, without a credential; alias a toward targets, when given, made
// through the CLI. It returns the mock and the data plane's port.
func keyWorld(t *testing.T, targets ...string) (*world, *mockprovider.Server, string) {
	t.Helper()
	env, port := admintest.Plane(t)
	w := worldOn(t, env)
	writeFile(t, filepath.Join(w.h.Dir, ".env"), "STARGAITE_PORT="+port+"\n", 0o600)
	mock := mockprovider.New()
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	for _, p := range []string{"mock", "mock2"} {
		w.must("providers", "add", p, "--protocol", "openai", "--base-url", srv.URL, "--allow-private-network", "--billing", "none")
	}
	if len(targets) > 0 {
		args := []string{"models", "add", "a"}
		for _, id := range targets {
			w.target(id, "--kind", "chat")
			args = append(args, "--target", id)
		}
		w.must(args...)
	}
	return w, mock, port
}

// giveKey runs Give an app or agent its key with u under ctx.
func (w *world) giveKey(ctx context.Context, u *ui.UI) (string, error) {
	return u.Run(ctx, giveSteps(), func(ctx context.Context) (string, error) { return GiveKey(ctx, u, w.proxy(), w.h) })
}

// givePlain runs it with the plain front and answers; it returns the
// result, the output and the error.
func (w *world) givePlain(answers map[string]string) (string, string, error) {
	var out bytes.Buffer
	res, err := w.giveKey(context.Background(), ui.New(plain.New(&out), answers))
	return res, out.String(), err
}

// secretLine matches a client key's secret alone on its line.
var secretLine = regexp.MustCompile(`(?m)^sk-stargaite-[a-z2-7]{12}-[a-z2-7]{52}$`)

// keySaid is the said lines of a key made or rotated, before its reveal.
func keySaid(n, how string) string {
	return "key " + n + " is " + how + ": its secret is shown next, once"
}

// mayLose is the line said before keys create or keys rotate runs.
func mayLose(n, how string) string {
	return "if this stops before you store its secret, key " + n + " may be " + how + " without it: Change a key rotates it"
}

// example is T9's two lines for alias a on the plane at port.
func example(port, a string) []string {
	base := "http://127.0.0.1:" + port + "/v1"
	return []string{"Point an OpenAI-compatible client at base URL " + base + ", with model " + a + " and this key, such as:",
		"  curl " + base + `/chat/completions -H "Authorization: Bearer $STARGAITE_KEY" -H "Content-Type: application/json" -d '{"model":"` + a +
			`","max_tokens":256,"messages":[{"role":"user","content":"Hello"}]}'`}
}

const (
	tryWarning = "Esc or Ctrl-C stops the try at the server: no further target is called, and a call already sent may still be billed"
	tryStopped = "the try through alias a was stopped: the call in flight may still be billed"
)

func TestGiveKeyFlags(t *testing.T) {
	w, mock, port := keyWorld(t, "mock/m")
	res, out, err := w.givePlain(map[string]string{"alias": "a", "key-name": "second", "rpm-limit": "10"})
	if err != nil || res != "Done: key second, which may call a." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	want := []string{"models list --json", "keys list --json", "keys create --json --model a --rpm-limit 10 -- second"}
	if got := w.cli(); !slices.Equal(got, want) {
		t.Errorf("CLI calls %q", got)
	}
	secrets := secretLine.FindAllString(out, -1)
	if len(secrets) != 1 {
		t.Fatalf("%d secrets alone on a line:\n%s", len(secrets), out)
	}
	// In this order: what a stop may leave is said before the key is made and
	// before it is shown (owner decision 2), and the count before the paid
	// question.
	order := []string{"creating key second, which may call a", mayLose("second", "made"), keySaid("second", "made"), secrets[0],
		"try alias a: up to 1 paid call(s), one per target if the earlier fail, each capped at 256 output tokens, never retried",
		tryWarning, "try it: up to 1 paid call(s)", "answer: mock reply"}
	hasLines(t, out, order...)
	for i := 1; i < len(order); i++ {
		if !before(out, order[i-1], order[i]) {
			t.Errorf("%q is not before %q:\n%s", order[i-1], order[i], out)
		}
	}
	hasLines(t, out, example(port, "a")...)
	if !regexp.MustCompile(`(?m)^finish_reason: stop, [0-9]+\.[0-9]{3} s, from mock/m$`).MatchString(out) {
		t.Errorf("no finish_reason line:\n%s", out)
	}
	if got := models(mock); !slices.Equal(got, []string{"m"}) {
		t.Errorf("mock calls toward %q", got)
	}
	if where := w.leaks(secrets[0], strings.Replace(out, secrets[0]+"\n", "", 1)); len(where) > 0 {
		t.Errorf("the secret leaked to %q", where)
	}
}

func TestGiveKeyExists(t *testing.T) {
	w, mock, _ := keyWorld(t, "mock/m")
	w.must("keys", "create", "--model", "a", "--", "second")
	res, out, err := w.givePlain(map[string]string{"alias": "a", "key-name": "second"})
	if err != nil || res != "key second exists, and its secret cannot be shown again: no key made and no test call; Change a key rotates it" {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	if got := w.cli(); !slices.Equal(got, []string{"models list --json", "keys list --json"}) {
		t.Errorf("CLI calls %q", got)
	}
	if len(mock.Calls()) != 0 || secretLine.MatchString(out) {
		t.Errorf("%d mock calls, output:\n%s", len(mock.Calls()), out)
	}

	// At a terminal, a name from the flag ends the run as well: never asked again.
	s := &scripted{answers: map[string][]string{}}
	res, err = w.giveKey(context.Background(), ui.New(s, map[string]string{"alias": "a", "key-name": "second"}))
	if err != nil || res != "key second exists, and its secret cannot be shown again: no key made and no test call; Change a key rotates it" ||
		s.asks("key-name-again") {
		t.Fatalf("at a terminal: %q, %v, asked %q\n%s", res, err, s.asked, s.output())
	}
}

func TestGiveKeyNoAlias(t *testing.T) {
	for _, row := range []struct {
		answers map[string]string
		alias   string
	}{
		{map[string]string{"alias": "nope", "key-name": "third"}, "nope"},
		{map[string]string{"alias": "a,nope"}, "nope"},
		{nil, "main"},
	} {
		w, _, _ := keyWorld(t, "mock/m")
		_, out, err := w.givePlain(row.answers)
		if err == nil || err.Error() != "alias "+row.alias+" does not exist: ./setup.sh alias makes it" {
			t.Fatalf("%v: err = %v\n%s", row.answers, err, out)
		}
		if got := w.cli(); !slices.Equal(got, []string{"models list --json"}) {
			t.Errorf("%v: CLI calls %q", row.answers, got)
		}
	}
	w, _, _ := keyWorld(t)
	s := &scripted{answers: map[string][]string{}}
	if res, err := w.giveKey(context.Background(), ui.New(s, nil)); err != nil || res != "No alias yet: Make an alias makes one." {
		t.Fatalf("guided, no alias: %q, %v", res, err)
	}
}

func TestGiveKeyAsked(t *testing.T) {
	w, mock, _ := keyWorld(t, "mock/m")
	w.must("models", "add", "b", "--target", "mock/m")
	w.must("keys", "create", "--model", "a", "--", "my-app")
	s := &scripted{answers: map[string][]string{"alias": {"a,b"}, "key-name": {""}, "key-name-again": {"app2"}, "rpm-limit": {""},
		"secret": {""}, "try": {"check"}}}
	res, err := w.giveKey(context.Background(), ui.New(s, nil))
	if err != nil || res != "Done: key app2, which may call a, b." {
		t.Fatalf("%q, %v\n%s", res, err, s.output())
	}
	q, _ := find(s.questions, func(q ui.Question) bool { return q.ID == "alias" })
	if q.Kind != ui.KindPickMany || !slices.Equal(options(q), []string{"a|", "b|"}) {
		t.Errorf("alias question %+v", q)
	}
	hasLines(t, s.output(), "key my-app exists, and its secret cannot be shown again", "try alias a: up to 1 paid call(s), one per target if the earlier fail, each capped at 256 output tokens, never retried")
	if i := indexOf(w.cli(), "keys create "); i < 0 || w.cli()[i] != "keys create --json --model a --model b --rpm-limit 60 -- app2" {
		t.Errorf("CLI calls %q", w.cli())
	}
	if len(mock.Calls()) != 1 {
		t.Errorf("%d mock calls", len(mock.Calls()))
	}
}

func TestGiveKeyExistsAtTerminal(t *testing.T) {
	const line = "key my-app exists, and its secret cannot be shown again"
	const result = "No key made: Change a key rotates a key that exists."
	for _, again := range [][]string{{""}, {"my-app", "x", "y"}} {
		w, mock, _ := keyWorld(t, "mock/m")
		for _, n := range []string{"my-app", "x", "y"} {
			w.must("keys", "create", "--model", "a", "--", n)
		}
		s := &scripted{answers: map[string][]string{"alias": {"a"}, "key-name": {"my-app"}, "key-name-again": again}}
		res, err := w.giveKey(context.Background(), ui.New(s, nil))
		if err != nil || res != result {
			t.Fatalf("%q: %q, %v\n%s", again, res, err, s.output())
		}
		said := strings.Count(s.output(), "exists, and its secret cannot be shown again\n")
		asked := len(slices.DeleteFunc(slices.Clone(s.questions), func(q ui.Question) bool { return q.ID != "key-name-again" }))
		if len(again) == 1 && (said != 1 || asked != 1 || !strings.Contains(s.output(), line+"\n")) ||
			len(again) == 3 && (said != 4 || asked != 3) {
			t.Errorf("%q: said %d times, asked %d times\n%s", again, said, asked, s.output())
		}
		if got := w.cli(); !slices.Equal(got, []string{"models list --json", "keys list --json"}) || len(mock.Calls()) != 0 {
			t.Errorf("%q: CLI calls %q, %d mock calls", again, got, len(mock.Calls()))
		}
	}
}

// giveAnswers is a scripted Give a key of key app toward alias a, the try
// answered try.
func giveAnswers(try string) map[string][]string {
	return map[string][]string{"alias": {"a"}, "key-name": {"app"}, "rpm-limit": {""}, "secret": {""}, "try": {try}}
}

func TestGiveKeyFallbackCount(t *testing.T) {
	w, mock, _ := keyWorld(t, "mock/m", "mock2/m")
	s := &scripted{answers: giveAnswers("check")}
	u := ui.New(s, nil)
	if _, err := w.giveKey(context.Background(), u); err != nil {
		t.Fatalf("%v\n%s", err, s.output())
	}
	hasLines(t, s.output(), "try alias a: up to 2 paid call(s), one per target if the earlier fail, each capped at 256 output tokens, never retried")
	if !slices.Contains(s.asked, "try it: up to 2 paid call(s)") {
		t.Errorf("asked %q", s.asked)
	}
	if len(mock.Calls()) != 1 {
		t.Errorf("%d mock calls", len(mock.Calls()))
	}
	s.answers["try"] = []string{"check", "check"}
	ctx := context.Background()
	if _, err := u.Paid(ctx, ui.Question{ID: "try"}, 11); err == nil {
		t.Error("a step of 11 opened after a try of 2")
	}
	if b, err := u.Paid(ctx, ui.Question{ID: "try"}, 10); err != nil || b == nil {
		t.Errorf("a step of 10 did not open: %v", err)
	}
}

func TestGiveKeyNotNow(t *testing.T) {
	w, mock, port := keyWorld(t, "mock/m")
	s := &scripted{answers: giveAnswers("not now")}
	u := ui.New(s, nil)
	res, err := w.giveKey(context.Background(), u)
	if err != nil || res != "Done: key app, which may call a; not tried." {
		t.Fatalf("%q, %v\n%s", res, err, s.output())
	}
	hasLines(t, s.output(), example(port, "a")...)
	if len(mock.Calls()) != 0 {
		t.Errorf("%d mock calls", len(mock.Calls()))
	}
	s.answers["try"] = []string{"check"}
	if b, err := u.Paid(context.Background(), ui.Question{ID: "try"}, 12); err != nil || b == nil {
		t.Errorf("a step of 12 did not open after Not now: %v", err)
	}
}

// TestHandOverExampleTLS: on a plane behind TLS (the override's tls_name
// and tls_ca), the try reaches it over HTTPS and T9 names the https base URL
// and the CA in tls/ (owner question C62-Q2: the example comes with a new
// key).
func TestHandOverExampleTLS(t *testing.T) {
	w, _, port := keyWorld(t, "mock/m")
	plane, _ := url.Parse("http://127.0.0.1:" + port)
	srv := httptest.NewTLSServer(httputil.NewSingleHostReverseProxy(plane))
	t.Cleanup(srv.Close)
	tlsPort := strings.TrimPrefix(srv.URL, "https://127.0.0.1:")
	writeFile(t, filepath.Join(w.h.Dir, ".env"), "STARGAITE_PORT="+tlsPort+"\n", 0o600)
	writeFile(t, filepath.Join(w.h.Dir, "compose.override.yaml"), "# setup: tls_name=example.com\n# setup: tls_ca=ca.pem\nservices: {}\n", 0o600)
	writeFile(t, filepath.Join(w.h.Dir, "tls", "ca.pem"), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})), 0o600)
	res, out, err := w.givePlain(map[string]string{"alias": "a", "key-name": "app"})
	if err != nil || res != "Done: key app, which may call a." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	base := "https://example.com:" + tlsPort + "/v1"
	hasLines(t, out, "answer: mock reply", "Point an OpenAI-compatible client at base URL "+base+", with model a and this key, such as:",
		"  curl --cacert tls/ca.pem "+base+`/chat/completions -H "Authorization: Bearer $STARGAITE_KEY" -H "Content-Type: application/json" -d '{"model":"a","max_tokens":256,"messages":[{"role":"user","content":"Hello"}]}'`)
}

func TestGiveKeyEscBeforeTry(t *testing.T) {
	w, mock, _ := keyWorld(t, "mock/m")
	s := &scripted{answers: giveAnswers(leave)}
	if _, err := w.giveKey(context.Background(), ui.New(s, nil)); !errors.Is(err, ui.ErrLeave) {
		t.Fatalf("err = %v\n%s", err, s.output())
	}
	hasLines(t, s.output(), "already changed and kept: key app made")
	if len(mock.Calls()) != 0 {
		t.Errorf("%d mock calls", len(mock.Calls()))
	}
}

func TestGiveKeyEscDuringTry(t *testing.T) {
	// Esc leaves the work, Ctrl-C quits it, and nil ends the root context, as
	// a signal does.
	for _, key := range []error{ui.ErrLeave, ui.ErrQuit, nil} {
		esc := key != nil
		w, mock, _ := keyWorld(t, "mock/m", "mock2/m")
		mock.Script(mockprovider.Step{Hang: true})
		ctx, cancel := context.WithCancel(context.Background())
		s := &scripted{answers: giveAnswers("check"), workErr: key, leaveWork: func(q ui.Question) bool {
			if q.ID != "trying" || len(mock.Calls()) != 1 {
				return false
			}
			if !esc {
				cancel()
			}
			return esc
		}}
		done := make(chan error, 1)
		go func() { _, err := w.giveKey(ctx, ui.New(s, nil)); done <- err }()
		var err error
		select {
		case err = <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%v: the try was not stopped within 5 s", key)
		}
		cancel()
		out := s.output()
		if esc && !errors.Is(err, key) || !esc && (err == nil || err.Error() != tryStopped || !errors.Is(err, context.Canceled)) {
			t.Fatalf("%v: err = %v\n%s", key, err, out)
		}
		if !strings.Contains(out, tryStopped+"\n") || strings.Contains(out, "context canceled") || strings.Contains(out, "the last 30 log lines:") ||
			strings.Contains(out, "Point an OpenAI-compatible client") || !strings.Contains(out, "already changed and kept: key app made\n") {
			t.Errorf("%v: output:\n%s", key, out)
		}
		for deadline := time.Now().Add(5 * time.Second); !mock.Calls()[0].Cancelled; time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%v: the call in flight was never cancelled at the mock", key)
			}
		}
		time.Sleep(500 * time.Millisecond)
		if n := len(mock.Calls()); n != 1 {
			t.Errorf("%v: %d mock calls; the second target was called", key, n)
		}
		if strings.Contains(w.dockerLog(), "compose logs") {
			t.Errorf("%v: docker log %q", key, w.dockerLog())
		}
	}
}

func TestGiveKeyTryFails(t *testing.T) {
	w, mock, _ := keyWorld(t, "mock/m")
	mock.Script(mockprovider.Step{Status: http.StatusBadGateway, Body: []byte(`{"error":{"message":"upstream down"}}`)})
	s := &scripted{answers: giveAnswers("check")}
	_, err := w.giveKey(context.Background(), ui.New(s, nil))
	if err == nil || !strings.HasPrefix(err.Error(), "the test call answered 502: ") {
		t.Fatalf("err = %v\n%s", err, s.output())
	}
	if len(mock.Calls()) != 1 || !strings.Contains(s.output(), "already changed and kept: key app made\n") {
		t.Errorf("%d mock calls, output:\n%s", len(mock.Calls()), s.output())
	}

	// The plane is gone once the key is made: the logs, then T8.
	w, mock, _ = keyWorld(t, "mock/m")
	closed := httptest.NewServer(http.NotFoundHandler())
	u, _ := url.Parse(closed.URL)
	closed.Close()
	next := w.runner
	w.runner = func(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		code, err := next(ctx, args, stdin, stdout, stderr)
		if len(args) > 1 && args[0] == "keys" && args[1] == "create" {
			writeFile(t, filepath.Join(w.h.Dir, ".env"), "STARGAITE_PORT="+u.Port()+"\n", 0o600)
		}
		return code, err
	}
	s = &scripted{answers: giveAnswers("check")}
	_, err = w.giveKey(context.Background(), ui.New(s, nil))
	out := s.output()
	if err == nil || !strings.HasPrefix(err.Error(), "the test call did not complete: ") {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if !before(out, "the last 30 log lines:", "a log line") || strings.Contains(out, "Point an OpenAI-compatible client") || len(mock.Calls()) != 0 {
		t.Errorf("%d mock calls, output:\n%s", len(mock.Calls()), out)
	}
}

func TestGiveKeyRefusedName(t *testing.T) {
	w, mock, _ := keyWorld(t, "mock/m")
	w.must("keys", "create", "--model", "a", "--", "old")
	w.must("keys", "revoke", "--yes", "--", "old")
	_, out, err := w.givePlain(map[string]string{"alias": "a", "key-name": "old"})
	if err == nil || !strings.Contains(err.Error(), "is revoked, and a revoked name is never reused") {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if strings.Contains(out, "is made:") || secretLine.MatchString(out) || strings.Contains(out, "try it:") || strings.Contains(out, "already changed") ||
		len(mock.Calls()) != 0 {
		t.Errorf("%d mock calls, output:\n%s", len(mock.Calls()), out)
	}
}

func TestTryAlias(t *testing.T) {
	r := &run{models: []proxy.Model{{Name: "a", Targets: []string{"x/1"}}, {Name: "b", Targets: []string{"x/1", "x/2"}}}}
	for _, row := range []struct {
		aliases []string
		alias   string
		n       int
		ok      bool
	}{
		{[]string{"a"}, "a", 1, true},
		{[]string{"nope", "b"}, "b", 2, true},
		{[]string{"*"}, "a", 1, true},
		{[]string{"nope"}, "", 0, false},
	} {
		if alias, n, ok := r.tryAlias(row.aliases); alias != row.alias || n != row.n || ok != row.ok {
			t.Errorf("tryAlias(%q) = %q, %d, %v", row.aliases, alias, n, ok)
		}
	}
	if _, _, ok := (&run{}).tryAlias([]string{"*"}); ok {
		t.Error(`tryAlias(["*"]) with no alias is ok`)
	}
}
