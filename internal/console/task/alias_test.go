package task

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/cli"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
	"github.com/jorlaru/stargaite/internal/console/ui/plain"
	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// listing is a model list on 127.0.0.1 that answers every request with
// answer and records each one.
type listing struct {
	port string
	mu   sync.Mutex
	reqs []*http.Request
}

func serveList(t *testing.T, answer http.HandlerFunc) *listing {
	t.Helper()
	l := &listing{}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		l.reqs = append(l.reqs, r.Clone(context.Background()))
		l.mu.Unlock()
		answer(rw, r)
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	l.port = u.Port()
	return l
}

func (l *listing) requests() []*http.Request {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.reqs)
}

// answers is a handler that writes body.
func answers(body string) http.HandlerFunc {
	return func(rw http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(rw, body) }
}

// modelIDs is a model list's body holding ids.
func modelIDs(ids ...string) string {
	items := make([]string, len(ids))
	for i, id := range ids {
		items[i] = `{"id":"` + id + `"}`
	}
	return `{"data":[` + strings.Join(items, ",") + `]}`
}

// must runs the CLI before the run, failing the test on a non-zero exit.
func (w *world) must(args ...string) {
	w.t.Helper()
	_ = w.out(args...)
}

// out runs the CLI outside the run, unrecorded, failing the test on a
// non-zero exit, and returns what it printed.
func (w *world) out(args ...string) string {
	w.t.Helper()
	var out, errOut bytes.Buffer
	if code := cli.Main(args, func(k string) string { return w.env[k] }, strings.NewReader(""), &out, &errOut); code != 0 {
		w.t.Fatalf("%q: exit %d: %s%s", args, code, out.String(), errOut.String())
	}
	return out.String()
}

// decoded is what the CLI prints for args, decoded into v.
func (w *world) decoded(v any, args ...string) {
	w.t.Helper()
	if err := json.Unmarshal([]byte(w.out(append(args, "--json")...)), v); err != nil {
		w.t.Fatalf("%q: %v", args, err)
	}
}

// keyless adds an openai-compatible provider without a credential, on
// host.docker.internal at port, as the task stores a service on the host.
func (w *world) keyless(name, port string) {
	w.t.Helper()
	w.must("providers", "add", "--base-url", "http://host.docker.internal:"+port+"/v1", "--allow-private-network",
		"--protocol", "openai", "--billing", "none", "--", name)
}

// inAlias runs fn on a run of the world's doors, inside Make an alias's steps.
func (w *world) inAlias(u *ui.UI, fn func(context.Context, *run) (string, error)) (string, error) {
	r := &run{u: u, px: w.proxy(), h: w.h}
	return u.Run(context.Background(), makeSteps(), func(ctx context.Context) (string, error) { return fn(ctx, r) })
}

// pickModelOf lists the providers and picks a model of name at the step model.
func (w *world) pickModelOf(u *ui.UI, name string) (string, error) {
	return w.inAlias(u, func(ctx context.Context, r *run) (string, error) {
		if err := r.listProviders(ctx); err != nil {
			return "", err
		}
		i := slices.IndexFunc(r.providers, func(p proxy.Provider) bool { return p.Name == name })
		if i < 0 {
			return "", errors.New(name + " is not listed")
		}
		return r.pickModel(ctx, ui.Question{ID: "model", Flag: "model", Step: "model"}, r.providers[i])
	})
}

// options is the values and groups of q's options, as "value|group".
func options(q ui.Question) []string {
	var out []string
	for _, o := range q.Options {
		out = append(out, o.Value+"|"+o.Group)
	}
	return out
}

func TestPickModelGrouped(t *testing.T) {
	const key = `k"1\2`
	w := newWorld(t)
	l := serveList(t, answers(modelIDs("gpt-b", "gpt-a", "other-model", "text-embedding-3-small", "claude-x")))
	w.listed("acme", "ANTHROPIC_API_KEY", l.port)
	if err := w.h.EnvSet("ANTHROPIC_API_KEY", key); err != nil {
		t.Fatal(err)
	}
	s := &scripted{answers: map[string][]string{"model": {"gpt-b"}}}
	got, err := w.pickModelOf(ui.New(s, nil), "acme")
	if err != nil || got != "gpt-b" {
		t.Fatalf("%q, %v\n%s", got, err, s.output())
	}
	if len(s.questions) != 1 || s.questions[0].Prompt != "Which model of acme?" {
		t.Fatalf("questions %+v", s.questions)
	}
	q := s.questions[0]
	want := []string{"gpt-a|OpenAI · GPT", "gpt-b|OpenAI · GPT", "claude-x|Anthropic · Claude", "other-model|Other", "+all|", "+type|"}
	if !slices.Equal(options(q), want) || q.Options[4].Label != "show every model (1 not chat)" || q.Options[5].Label != "type a model id" {
		t.Errorf("options %+v", q.Options)
	}
	reqs := l.requests()
	if len(reqs) != 1 || reqs[0].Header.Get("Authorization") != "Bearer "+key {
		t.Errorf("requests %+v", reqs)
	}
	if where := w.leaks(key, s.output()); len(where) > 0 {
		t.Errorf("the key leaked to %q", where)
	}

	// A list of 2 MiB, over the free check's 1 MiB, is read and grouped.
	big := make([]string, 0, 9000)
	for i := range cap(big) {
		big = append(big, fmt.Sprintf("openai/gpt-%s%05d", strings.Repeat("x", 220), i))
	}
	body := modelIDs(big...)
	if len(body) < 2<<20 {
		t.Fatalf("the big list is %d bytes", len(body))
	}
	w = newWorld(t)
	l = serveList(t, answers(body))
	w.keyless("acme", l.port)
	s = &scripted{answers: map[string][]string{"model": {big[42]}}}
	if got, err := w.pickModelOf(ui.New(s, nil), "acme"); err != nil || got != big[42] {
		t.Fatalf("2 MiB: %q, %v\n%s", got, err, s.output())
	}
	if len(s.questions) != 1 || len(s.questions[0].Options) != len(big)+1 || s.questions[0].Options[0].Group != "OpenAI · GPT" {
		t.Errorf("2 MiB: not one pick of every id, grouped:\n%s", s.output())
	}
}

func TestPickModelAll(t *testing.T) {
	w := newWorld(t)
	l := serveList(t, answers(modelIDs("gpt-a", "text-embedding-3-small")))
	w.keyless("acme", l.port)
	s := &scripted{answers: map[string][]string{"model": {"+all", "text-embedding-3-small"}}}
	got, err := w.pickModelOf(ui.New(s, nil), "acme")
	if err != nil || got != "text-embedding-3-small" {
		t.Fatalf("%q, %v\n%s", got, err, s.output())
	}
	if len(s.questions) != 2 || slices.ContainsFunc(s.questions[1].Options, func(o ui.Option) bool { return o.Value == "+all" }) ||
		!slices.ContainsFunc(s.questions[1].Options, func(o ui.Option) bool { return o.Value == "text-embedding-3-small" }) {
		t.Errorf("questions %+v", s.questions)
	}
	if len(l.requests()) != 1 {
		t.Errorf("%d list requests, want 1", len(l.requests()))
	}

	// "type a model id" asks the id as text.
	s = &scripted{answers: map[string][]string{"model": {"+type", "typed-id"}}}
	if got, err := w.pickModelOf(ui.New(s, nil), "acme"); err != nil || got != "typed-id" {
		t.Fatalf("typed: %q, %v\n%s", got, err, s.output())
	}
	if !slices.Equal(s.asked, []string{"Which model of acme?", "the model id at acme"}) {
		t.Errorf("typed: asked %q", s.asked)
	}
}

func TestPickModelAnthropic(t *testing.T) {
	w := newWorld(t)
	l := serveList(t, answers(modelIDs("claude-x")))
	w.must("providers", "add", "--base-url", "http://host.docker.internal:"+l.port, "--allow-private-network", "--protocol", "anthropic",
		"--api-key-env", "ANTHROPIC_API_KEY", "--billing", "pay_per_use", "--", "acme")
	if err := w.h.EnvSet("ANTHROPIC_API_KEY", "sk-a"); err != nil {
		t.Fatal(err)
	}
	s := &scripted{answers: map[string][]string{"model": {"claude-x"}}}
	if got, err := w.pickModelOf(ui.New(s, nil), "acme"); err != nil || got != "claude-x" {
		t.Fatalf("%q, %v\n%s", got, err, s.output())
	}
	reqs := l.requests()
	if len(reqs) != 1 || reqs[0].URL.String() != "/v1/models?limit=1000" || reqs[0].Header.Get("x-api-key") != "sk-a" ||
		reqs[0].Header.Get("anthropic-version") != "2023-06-01" || reqs[0].Header.Get("Authorization") != "" {
		t.Errorf("requests %+v", reqs)
	}
}

func TestPickModelLocal(t *testing.T) {
	w := newWorld(t)
	l := serveList(t, answers(modelIDs("llama3.1:8b")))
	w.keyless("local", l.port)
	s := &scripted{answers: map[string][]string{"model": {"llama3.1:8b"}}}
	if got, err := w.pickModelOf(ui.New(s, nil), "local"); err != nil || got != "llama3.1:8b" {
		t.Fatalf("%q, %v\n%s", got, err, s.output())
	}
	reqs := l.requests()
	if len(reqs) != 1 || reqs[0].Host != "localhost:"+l.port || reqs[0].URL.Path != "/v1/models" ||
		reqs[0].Header.Get("Authorization") != "" || reqs[0].Header.Get("x-api-key") != "" {
		t.Errorf("requests %+v", reqs)
	}
	if strings.Contains(s.output(), "is not in .env") {
		t.Errorf("a provider without a variable was said to miss it:\n%s", s.output())
	}
}

func TestPickModelListFails(t *testing.T) {
	closed := &listing{port: closedPort(t)}
	for name, l := range map[string]*listing{
		"500": serveList(t, func(rw http.ResponseWriter, _ *http.Request) {
			rw.WriteHeader(http.StatusInternalServerError)
		}),
		"empty":       serveList(t, answers(`{"data":[]}`)),
		"not json":    serveList(t, answers("not json")),
		"closed port": closed,
		"over 8 MiB":  serveList(t, answers(strings.Repeat("x", 8<<20+1))),
	} {
		w := newWorld(t)
		w.listed("acme", "ANTHROPIC_API_KEY", l.port)
		if err := w.h.EnvSet("ANTHROPIC_API_KEY", "sk-list"); err != nil {
			t.Fatal(err)
		}
		s := &scripted{answers: map[string][]string{"model": {"typed-id"}}}
		got, err := w.pickModelOf(ui.New(s, nil), "acme")
		if err != nil || got != "typed-id" {
			t.Fatalf("%s: %q, %v\n%s", name, got, err, s.output())
		}
		lines := strings.Split(strings.TrimSuffix(s.output(), "\n"), "\n")
		if len(lines) != 1 || !strings.Contains(lines[0], "the model list at http://localhost:"+l.port+"/v1") ||
			!strings.HasSuffix(lines[0], "; type the model id") {
			t.Errorf("%s: output:\n%s", name, s.output())
		}
		if !slices.Equal(s.asked, []string{"the model id at acme"}) {
			t.Errorf("%s: asked %q", name, s.asked)
		}
		if where := w.leaks("sk-list", s.output()); len(where) > 0 {
			t.Errorf("%s: the key leaked to %q", name, where)
		}
	}
}

func TestPickModelFlags(t *testing.T) {
	w := newWorld(t)
	l := serveList(t, answers(modelIDs("m1")))
	w.keyless("acme", l.port)
	var out bytes.Buffer
	if got, err := w.pickModelOf(ui.New(plain.New(&out), map[string]string{"model": "m2"}), "acme"); err != nil || got != "m2" {
		t.Fatalf("%q, %v\n%s", got, err, out.String())
	}
	_, err := w.pickModelOf(ui.New(plain.New(&out), nil), "acme")
	var usage *ui.UsageError
	if !errors.As(err, &usage) || err.Error() != "missing --model: the model id at acme" {
		t.Fatalf("err = %v", err)
	}
	if n := len(l.requests()); n != 0 {
		t.Errorf("%d list requests, want 0", n)
	}
}

func TestPickModelEsc(t *testing.T) {
	w := newWorld(t)
	l := serveList(t, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	w.keyless("acme", l.port)
	s := &scripted{leaveWork: func(q ui.Question) bool {
		return q.Prompt == "reading the models of acme" && q.ID == "models" && q.Step == "model"
	}}
	done := make(chan error, 1)
	go func() { _, err := w.pickModelOf(ui.New(s, nil), "acme"); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, ui.ErrLeave) {
			t.Fatalf("err = %v\n%s", err, s.output())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Esc did not stop the model list within 2 s")
	}
}

func TestPickProviderFlags(t *testing.T) {
	q := ui.Question{ID: "provider", Flag: "provider", Step: "model", Prompt: "Which provider has the model?"}
	w := newWorld(t)
	w.keyless("acme", w.models.port())
	var out bytes.Buffer
	_, err := w.inAlias(ui.New(plain.New(&out), map[string]string{"provider": "acme"}), func(ctx context.Context, r *run) (string, error) {
		if err := r.listProviders(ctx); err != nil {
			return "", err
		}
		p, err := r.pickProvider(ctx, q, true)
		if err == nil && p.Name != "acme" {
			err = fmt.Errorf("picked %+v", p)
		}
		return "", err
	})
	if err != nil || !strings.Contains(out.String(), "provider acme exists; kept\n") {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if got := w.cli(); !slices.Equal(got, []string{"providers list --json"}) {
		t.Errorf("CLI calls %q", got)
	}

	w = newWorld(t)
	out.Reset()
	_, err = w.inAlias(ui.New(plain.New(&out), map[string]string{"provider": "ollama", "yes": "yes"}), func(ctx context.Context, r *run) (string, error) {
		if err := r.listProviders(ctx); err != nil {
			return "", err
		}
		p, err := r.pickProvider(ctx, q, true)
		if err == nil && (p.Name != "ollama" || p.BaseURL != "http://host.docker.internal:11434/v1") {
			err = fmt.Errorf("picked %+v", p)
		}
		return "", err
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	want := []string{"providers list --json", "providers presets --json", "providers list --json",
		"providers add --json --base-url http://host.docker.internal:11434/v1 --allow-private-network -- ollama",
		"providers show --json -- ollama", "providers list --json"}
	if got := w.cli(); !slices.Equal(got, want) {
		t.Errorf("CLI calls:\n%q\nwant:\n%q", got, want)
	}
	if !strings.Contains(out.String(), "Done: ollama added, no key.\n") {
		t.Errorf("the nested result was not said:\n%s", out.String())
	}
}

// fallbacks runs pickFallbacks at the step fallbacks, after listing the
// providers.
func (w *world) fallbacks(u *ui.UI) ([]string, error) {
	var got []string
	_, err := w.inAlias(u, func(ctx context.Context, r *run) (string, error) {
		if err := r.listProviders(ctx); err != nil {
			return "", err
		}
		var err error
		got, err = r.pickFallbacks(ctx, "fallbacks")
		return "", err
	})
	return got, err
}

func TestPickFallbacksFlags(t *testing.T) {
	w := newWorld(t)
	w.keyless("acme", w.models.port())
	w.keyless("acme2", w.models.port())
	var out bytes.Buffer
	got, err := w.fallbacks(ui.New(plain.New(&out), map[string]string{"fallback": "acme/m2,acme2/m3"}))
	if err != nil || !slices.Equal(got, []string{"acme/m2", "acme2/m3"}) {
		t.Fatalf("%q, %v", got, err)
	}
	for _, given := range []string{"nope/m", "acme", "acme/"} {
		_, err := w.fallbacks(ui.New(plain.New(&out), map[string]string{"fallback": given}))
		var usage *ui.UsageError
		want := "--fallback: " + given + " is not <provider>/<model> of a provider added here; ./setup.sh provider adds one"
		if !errors.As(err, &usage) || err.Error() != want {
			t.Errorf("%s: err = %v", given, err)
		}
	}
	if got, err := w.fallbacks(ui.New(plain.New(&out), nil)); err != nil || got != nil {
		t.Errorf("none given without a terminal: %q, %v", got, err)
	}
	if len(w.models.requests()) != 0 {
		t.Errorf("list requests %q", w.models.requests())
	}
}

func TestPickFallbacksAsked(t *testing.T) {
	w := newWorld(t)
	w.listed("acme", "ANTHROPIC_API_KEY", w.models.port())
	s := &scripted{answers: map[string][]string{"fallback-provider": {"acme", ""}, "fallback-model": {"m2"}}}
	got, err := w.fallbacks(ui.New(s, nil))
	if err != nil || !slices.Equal(got, []string{"acme/m2"}) {
		t.Fatalf("%q, %v\n%s", got, err, s.output())
	}
	var picks []string
	for _, q := range s.questions {
		if q.ID == "fallback-provider" {
			picks = append(picks, q.Prompt)
			if len(q.Options) == 0 || q.Options[0] != (ui.Option{Value: "", Label: "no more fallbacks"}) ||
				slices.ContainsFunc(q.Options, func(o ui.Option) bool { return o.Value == "+add" }) {
				t.Errorf("options %+v", q.Options)
			}
		}
	}
	if !slices.Equal(picks, []string{"Fallback 1 of 2: from which provider?", "Fallback 2 of 2: from which provider?"}) {
		t.Errorf("asked %q", s.asked)
	}

	// Two fallbacks answered: a third is never asked.
	s = &scripted{answers: map[string][]string{"fallback-provider": {"acme", "acme"}, "fallback-model": {"m2", "m3"}}}
	if got, err := w.fallbacks(ui.New(s, nil)); err != nil || !slices.Equal(got, []string{"acme/m2", "acme/m3"}) {
		t.Fatalf("two: %q, %v\n%s", got, err, s.output())
	}
	if n := len(slices.DeleteFunc(slices.Clone(s.questions), func(q ui.Question) bool { return q.ID != "fallback-provider" })); n != 2 {
		t.Errorf("two: %d fallback-provider questions, want 2", n)
	}
}

// checkWorld is the world with provider mock on the mock provider, on
// 127.0.0.1, without a credential; each test adds its targets.
func checkWorld(t *testing.T) (*world, *mockprovider.Server) {
	t.Helper()
	w := newWorld(t)
	mock := mockprovider.New()
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	w.must("providers", "add", "mock", "--protocol", "openai", "--base-url", srv.URL, "--allow-private-network", "--billing", "none")
	return w, mock
}

// onMock adds provider name on mock's URL with flags: a compatible service
// on the same mock, as design section 8's two entries on one key.
func (w *world) onMock(name string, flags ...string) {
	w.t.Helper()
	var mock proxy.Provider
	w.decoded(&mock, "providers", "show", "mock")
	w.must(append([]string{"providers", "add", name, "--base-url", mock.BaseURL, "--allow-private-network"}, flags...)...)
}

// threeLevels is the flags of a target whose check is 3 calls: low, high, none.
var threeLevels = []string{"--kind", "chat", "--level", "low", "--level", "high", "--can-disable"}

// target adds target id with flags.
func (w *world) target(id string, flags ...string) {
	w.t.Helper()
	w.must(append(append([]string{"targets", "add"}, flags...), "--", id)...)
}

// checkIn lists the providers and targets, then checks ids at the step check.
func (w *world) checkIn(u *ui.UI, ids ...string) (bool, error) {
	var checked bool
	_, err := w.inAlias(u, func(ctx context.Context, r *run) (string, error) {
		if err := r.listProviders(ctx); err != nil {
			return "", err
		}
		if err := r.listTargets(ctx); err != nil {
			return "", err
		}
		var err error
		checked, err = r.check(ctx, "check", inAll, ids)
		return "", err
	})
	return checked, err
}

// verifies is every verify argv the run gave the CLI.
func (w *world) verifies() []string {
	var out []string
	for _, a := range w.cli() {
		if strings.HasPrefix(a, "targets verify ") {
			out = append(out, a)
		}
	}
	return out
}

// verifyOf is the verify argv of target id.
func verifyOf(id string) string {
	return "targets verify --json --max-output-tokens 256 --until-stdin-closes -- " + id
}

// models is the upstream model of each mock call.
func models(mock *mockprovider.Server) []string {
	var out []string
	for _, c := range mock.Calls() {
		var body struct{ Model string }
		_ = json.Unmarshal(c.Body, &body)
		out = append(out, body.Model)
	}
	return out
}

func TestCalls(t *testing.T) {
	yes, no := true, false
	for _, row := range []struct {
		levels  []string
		disable *bool
		none    bool
	}{{none: true}, {levels: []string{"low", "high"}}, {levels: []string{"low", "high"}, disable: &yes}, {disable: &yes}, {levels: []string{"low"}, disable: &no}} {
		var target proxy.Target
		var spec *domain.Reasoning
		if !row.none {
			target.Reasoning = &proxy.Reasoning{Levels: row.levels, CanDisable: row.disable}
			spec = &domain.Reasoning{Levels: row.levels, CanDisable: row.disable}
		}
		if got, want := calls(target), len(domain.VerifyLevels(spec)); got != want {
			t.Errorf("%+v: calls %d, want %d", row, got, want)
		}
	}
}

func TestCheckOneTarget(t *testing.T) {
	w, mock := checkWorld(t)
	w.target("mock/m", threeLevels...)
	var out bytes.Buffer
	checked, err := w.checkIn(ui.New(plain.New(&out), nil), "mock/m")
	if err != nil || !checked {
		t.Fatalf("%v, %v\n%s", checked, err, out.String())
	}
	if got := w.verifies(); !slices.Equal(got, []string{verifyOf("mock/m")}) {
		t.Errorf("verify argvs %q", got)
	}
	if n := len(mock.Calls()); n != 3 {
		t.Errorf("%d mock calls, want 3", n)
	}
	want := "check mock/m: 3 paid call(s) to mock, each capped at 256 output tokens, never retried\n" +
		"Esc or Ctrl-C stops the check at the server: no further call starts, and a call already sent may still be billed\n" +
		"3 paid call(s) in all\ntarget mock/m is checked\n"
	if out.String() != want {
		t.Errorf("output:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestCheckVerifiedSkipped(t *testing.T) {
	w, mock := checkWorld(t)
	w.target("mock/m", threeLevels...)
	var out bytes.Buffer
	if _, err := w.checkIn(ui.New(plain.New(&out), nil), "mock/m"); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	out.Reset()
	checked, err := w.checkIn(ui.New(plain.New(&out), nil), "mock/m")
	if err != nil || !checked || out.String() != "target mock/m is verified; not checked again (--reverify asks for it)\n" {
		t.Fatalf("again: %v, %v\n%s", checked, err, out.String())
	}
	if len(w.verifies()) != 1 || len(mock.Calls()) != 3 {
		t.Errorf("again: verify argvs %q, %d mock calls", w.verifies(), len(mock.Calls()))
	}
	// At a terminal, where no flag can be given, the flag is not named.
	s := &scripted{}
	if checked, err := w.checkIn(ui.New(s, nil), "mock/m"); err != nil || !checked || s.output() != "target mock/m is verified; not checked again\n" {
		t.Fatalf("terminal: %v, %v\n%s", checked, err, s.output())
	}
	if _, err := w.checkIn(ui.New(plain.New(&out), map[string]string{"reverify": "yes"}), "mock/m"); err != nil {
		t.Fatalf("reverify: %v\n%s", err, out.String())
	}
	if len(w.verifies()) != 2 || len(mock.Calls()) != 6 {
		t.Errorf("reverify: verify argvs %q, %d mock calls", w.verifies(), len(mock.Calls()))
	}
}

func TestCheckNotNow(t *testing.T) {
	w, mock := checkWorld(t)
	w.target("mock/m", threeLevels...)
	s := &scripted{answers: map[string][]string{"check": {"not now", "check"}}}
	_, err := w.inAlias(ui.New(s, nil), func(ctx context.Context, r *run) (string, error) {
		if err := r.listTargets(ctx); err != nil {
			return "", err
		}
		checked, err := r.check(ctx, "check", inAll, []string{"mock/m"})
		if err != nil || checked {
			return "", fmt.Errorf("not now: %v, %v", checked, err)
		}
		if b, err := r.u.Paid(ctx, ui.Question{ID: "check", Step: "check"}, 12); b == nil || err != nil {
			return "", fmt.Errorf("a later step of 12 was not opened: %v", err)
		}
		return "", nil
	})
	if err != nil || len(w.verifies()) != 0 || len(mock.Calls()) != 0 {
		t.Fatalf("%v, verify argvs %q, %d mock calls\n%s", err, w.verifies(), len(mock.Calls()), s.output())
	}
}

func TestCheckEscBeforeEnter(t *testing.T) {
	w, mock := checkWorld(t)
	w.target("mock/m", threeLevels...)
	s := &scripted{answers: map[string][]string{"check": {leave}}}
	if _, err := w.checkIn(ui.New(s, nil), "mock/m"); !errors.Is(err, ui.ErrLeave) {
		t.Fatalf("err = %v", err)
	}
	if len(w.verifies()) != 0 || len(mock.Calls()) != 0 {
		t.Errorf("verify argvs %q, %d mock calls", w.verifies(), len(mock.Calls()))
	}
}

func TestCheckStopsAtFirstFailure(t *testing.T) {
	w, mock := checkWorld(t)
	w.target("mock/a", "--kind", "chat")
	w.target("mock/b", "--kind", "chat")
	mock.Script(mockprovider.Step{Status: http.StatusNotFound, Body: []byte(`{"error":{"message":"no such model"}}`)})
	var out bytes.Buffer
	_, err := w.checkIn(ui.New(plain.New(&out), nil), "mock/a", "mock/b")
	if err == nil || !strings.Contains(err.Error(), "status 404") || !strings.Contains(err.Error(), "target mock/a failed its check") {
		t.Fatalf("err = %v\n%s", err, out.String())
	}
	if got := w.verifies(); !slices.Equal(got, []string{verifyOf("mock/a")}) {
		t.Errorf("verify argvs %q", got)
	}
	if got := models(mock); !slices.Equal(got, []string{"a"}) {
		t.Errorf("mock calls toward %q", got)
	}
}

func TestMakeAliasEscDuringCheck(t *testing.T) {
	w, mock := checkWorld(t)
	w.target("mock/a", "--kind", "chat")
	w.target("mock/b", "--kind", "chat")
	mock.Script(mockprovider.Step{Hang: true})
	s := &scripted{answers: map[string][]string{"check": {"check"}},
		leaveWork: func(q ui.Question) bool { return q.ID == "checking" && len(mock.Calls()) == 1 }}
	done := make(chan error, 1)
	go func() { _, err := w.checkIn(ui.New(s, nil), "mock/a", "mock/b"); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, ui.ErrLeave) {
			t.Fatalf("err = %v\n%s", err, s.output())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Esc did not stop the check within 5 s")
	}
	if !strings.Contains(s.output(), "the check of mock/a was stopped: the call in flight may still be billed") {
		t.Errorf("output:\n%s", s.output())
	}
	for deadline := time.Now().Add(5 * time.Second); !mock.Calls()[0].Cancelled; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the call in flight was never cancelled at the mock")
		}
	}
	time.Sleep(300 * time.Millisecond)
	if got := models(mock); !slices.Equal(got, []string{"a"}) {
		t.Errorf("mock calls toward %q", got)
	}
	if got := w.verifies(); !slices.Equal(got, []string{verifyOf("mock/a")}) {
		t.Errorf("verify argvs %q", got)
	}
}

func TestCheckCredentialUnresolved(t *testing.T) {
	w, _ := checkWorld(t)
	w.target("mock/m", threeLevels...)
	s := &scripted{answers: map[string][]string{"check": {"check"}}}
	_, err := w.inAlias(ui.New(s, nil), func(ctx context.Context, r *run) (string, error) {
		if err := r.listTargets(ctx); err != nil {
			return "", err
		}
		r.providers = []proxy.Provider{{Name: "mock", Credential: "unresolved", APIKeyEnv: "MOCK_KEY"}}
		_, err := r.check(ctx, "check", inAll, []string{"mock/m"})
		want := "the credential of mock does not resolve: MOCK_KEY is not in the container's environment"
		if err == nil || err.Error() != want {
			return "", fmt.Errorf("err = %v, want %q", err, want)
		}
		if len(s.asked) != 0 {
			return "", fmt.Errorf("asked %q", s.asked)
		}
		if b, err := r.u.Paid(ctx, ui.Question{ID: "check", Step: "check"}, 12); b == nil || err != nil {
			return "", fmt.Errorf("a later step of 12 was not opened: %v", err)
		}
		return "", nil
	})
	if err != nil || len(w.verifies()) != 0 {
		t.Fatalf("%v, verify argvs %q", err, w.verifies())
	}
}

func TestCheckNotListed(t *testing.T) {
	w, _ := checkWorld(t)
	s := &scripted{}
	_, err := w.checkIn(ui.New(s, nil), "mock/nope")
	if err == nil || err.Error() != "target mock/nope is not listed, so its paid calls cannot be counted" || len(s.asked) != 0 {
		t.Fatalf("err = %v, asked %q", err, s.asked)
	}
}

// TestCheckCutAtBound: the check passes its bound to the verify, which is
// cut there: a CLI that hangs until its standard input closes, the bound's
// margins shortened.
func TestCheckCutAtBound(t *testing.T) {
	w, _ := checkWorld(t)
	w.target("mock/m", "--kind", "chat")
	w.must("providers", "set", "--total-ms", "50", "--", "mock")
	next := w.runner
	w.runner = func(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		if strings.HasPrefix(strings.Join(args, " "), "targets verify ") {
			_, _ = io.Copy(io.Discard, stdin)
			return 1, nil
		}
		return next(ctx, args, stdin, stdout, stderr)
	}
	s := &scripted{answers: map[string][]string{"check": {"check"}}}
	u := ui.New(s, nil)
	r := &run{u: u, px: w.proxy(), h: w.h, slack: time.Millisecond, margin: time.Millisecond}
	done := make(chan error, 1)
	go func() {
		_, err := u.Run(context.Background(), makeSteps(), func(ctx context.Context) (string, error) {
			if err := r.listProviders(ctx); err != nil {
				return "", err
			}
			if err := r.listTargets(ctx); err != nil {
				return "", err
			}
			_, err := r.check(ctx, "check", inAll, []string{"mock/m"})
			return "", err
		})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.HasPrefix(err.Error(), "the check of mock/m did not end within 52ms") {
			t.Fatalf("err = %v\n%s", err, s.output())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the check was not cut at its bound within 5 s")
	}
}

func TestCheckPromptCounts(t *testing.T) {
	w, _ := checkWorld(t)
	w.target("mock/v", "--kind", "chat")
	w.target("mock/n", "--kind", "chat", "--level", "low", "--level", "high")
	var out bytes.Buffer
	if _, err := w.checkIn(ui.New(plain.New(&out), nil), "mock/v"); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	var k, total int
	_, err := w.inAlias(ui.New(plain.New(&out), nil), func(ctx context.Context, r *run) (string, error) {
		if err := r.listTargets(ctx); err != nil {
			return "", err
		}
		prompt := func(kk, tt int) string { k, total = kk, tt; return inAll(kk, tt) }
		if _, err := r.check(ctx, "check", prompt, []string{"mock/v", "mock/n"}); err != nil {
			return "", err
		}
		if b, err := r.u.Paid(ctx, ui.Question{ID: "check", Step: "check"}, 11); b != nil || err == nil {
			return "", errors.New("a step of 11 was opened after 2 of 12")
		}
		if b, err := r.u.Paid(ctx, ui.Question{ID: "check", Step: "check"}, 10); b == nil || err != nil {
			return "", fmt.Errorf("a step of 10 was not opened after 2 of 12: %v", err)
		}
		return "", nil
	})
	if err != nil || k != 1 || total != 2 {
		t.Fatalf("%v, k %d, total %d\n%s", err, k, total, out.String())
	}
}

func TestCheckOverRunCeiling(t *testing.T) {
	w, mock := checkWorld(t)
	five := []string{"--kind", "chat", "--level", "minimal", "--level", "low", "--level", "medium", "--level", "high", "--level", "xhigh", "--can-disable"}
	for _, id := range []string{"mock/a", "mock/b", "mock/c"} {
		w.target(id, five...)
	}
	s := &scripted{}
	_, err := w.checkIn(ui.New(s, nil), "mock/a", "mock/b", "mock/c")
	if err == nil || err.Error() != "this step states 18 paid calls and this console run has 12 left of 12" {
		t.Fatalf("err = %v", err)
	}
	if len(s.asked) != 0 || len(w.verifies()) != 0 || len(mock.Calls()) != 0 {
		t.Errorf("asked %q, verify argvs %q, %d mock calls", s.asked, w.verifies(), len(mock.Calls()))
	}
}

func TestCheckBound(t *testing.T) {
	w, _ := checkWorld(t)
	w.must("providers", "set", "--total-ms", "9000", "--", "mock")
	w.keyless("plain", w.models.port())
	var settings proxy.SettingsReply
	if err := w.proxy().Call(context.Background(), &settings, "settings show", nil); err != nil || settings.Timeouts.TotalMS <= 0 {
		t.Fatalf("settings show: %+v, %v", settings, err)
	}
	before := len(w.cli())
	var got []time.Duration
	_, err := w.inAlias(ui.New(plain.New(io.Discard), nil), func(ctx context.Context, r *run) (string, error) {
		if err := r.listProviders(ctx); err != nil {
			return "", err
		}
		for _, c := range []struct {
			id string
			n  int
		}{{"mock/m", 3}, {"plain/a", 1}, {"plain/b", 2}} {
			d, err := r.bound(ctx, c.id, c.n)
			if err != nil {
				return "", err
			}
			got = append(got, d)
		}
		return "", nil
	})
	total := time.Duration(settings.Timeouts.TotalMS) * time.Millisecond
	want := []time.Duration{52 * time.Second, total + 15*time.Second, 2*(total+5*time.Second) + 10*time.Second}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("%v, bounds %v, want %v", err, got, want)
	}
	if calls := w.cli()[before:]; !slices.Equal(calls, []string{"providers list --json", "settings show --json"}) {
		t.Errorf("CLI calls %q", calls)
	}
}

func TestKept(t *testing.T) {
	for _, row := range []struct {
		made []string
		err  error
		want string
	}{
		{[]string{"target a added"}, errors.New("x"), "already changed and kept: target a added\n"},
		{[]string{"target a added", "target b added"}, errors.New("x"), "already changed and kept: target a added; target b added\n"},
		{nil, errors.New("x"), ""},
		{[]string{"target a added"}, nil, ""},
	} {
		var out bytes.Buffer
		r := &run{u: ui.New(plain.New(&out), nil), made: row.made}
		if err := r.kept(context.Background(), row.err); err != row.err || out.String() != row.want {
			t.Errorf("%+v: %v, output %q", row, err, out.String())
		}
	}
}
