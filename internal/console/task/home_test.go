package task

import (
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/admintest"
	"github.com/jorlaru/stargaite/internal/cli"
	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/rules"
	"github.com/jorlaru/stargaite/internal/console/ui"
	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// homeWorld is the world on admintest.Plane with one provider, mock, and
// alias a toward mock/m: ready, read through the real CLI.
func homeWorld(t *testing.T) (*world, string) {
	t.Helper()
	env, port := admintest.Plane(t)
	w := worldOn(t, env)
	writeFile(t, filepath.Join(w.h.Dir, ".env"), "STARGAITE_PORT="+port+"\n", 0o600)
	srv := httptest.NewServer(mockprovider.New())
	t.Cleanup(srv.Close)
	w.must("providers", "add", "mock", "--protocol", "openai", "--base-url", srv.URL, "--allow-private-network", "--billing", "none")
	w.target("mock/m", "--kind", "chat")
	w.must("models", "add", "a", "--target", "mock/m")
	return w, port
}

// closedPort is a loopback port nothing listens on.
func closedPort(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	u, _ := url.Parse(srv.URL)
	srv.Close()
	return u.Port()
}

// serial runs next one call at a time, for a canned runner, which is not
// safe for the home's concurrent reads.
func serial(next proxy.Runner) proxy.Runner {
	var mu sync.Mutex
	return func(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return next(ctx, args, stdin, stdout, stderr)
	}
}

// homeReads are canned answers to the home's three reads.
func homeReads() map[string]reply {
	return map[string]reply{"export --json": {out: `{"version":1}`}, "validate --json": {out: `{"findings":[]}`},
		"version --json": {out: `{"version":"v1"}`}}
}

// hangExport answers the home's reads from homeReads, but export only once
// its context ends.
func hangExport(ctx context.Context, args []string, _ io.Reader, stdout, _ io.Writer) (int, error) {
	if args[0] == "export" {
		<-ctx.Done()
		return 1, nil
	}
	_, _ = io.WriteString(stdout, homeReads()[strings.Join(args, " ")].out)
	return 0, nil
}

// labels is the suggestions' labels.
func labels(s []rules.Suggestion) []string {
	var out []string
	for _, x := range s {
		out = append(out, x.Label)
	}
	return out
}

func TestHomeRead(t *testing.T) {
	ctx := context.Background()
	const fallback = "Give alias a a fallback: it has one model"
	w, port := homeWorld(t)
	base := "http://127.0.0.1:" + port + "/v1"
	hm := readHome(ctx, w.proxy(), w.h)
	if want := base + ": ready · proxy " + cli.Version + " · clone dev · 1 provider(s), 1 alias(es), 0 key(s) · spend not capped"; hm.summary != want {
		t.Errorf("summary %q; want %q", hm.summary, want)
	}
	if got := slices.Sorted(slices.Values(w.cli())); !slices.Equal(got, []string{"export --json", "validate --json", "version --json"}) {
		t.Errorf("CLI calls %q", got)
	}
	if got := labels(hm.suggested); !slices.Equal(got, []string{"Give an app or agent its key: none exists yet", fallback}) {
		t.Errorf("suggested %q", got)
	}

	// An instance budget and a key: no "spend not capped", no key row.
	w.must("settings", "set", "--budget", "limit_usd=10,reset_interval=monthly")
	w.must("keys", "create", "--model", "a", "--", "k")
	hm = readHome(ctx, w.proxy(), w.h)
	if want := base + ": ready · proxy " + cli.Version + " · clone dev · 1 provider(s), 1 alias(es), 1 key(s)"; hm.summary != want {
		t.Errorf("summary %q; want %q", hm.summary, want)
	}
	if got := labels(hm.suggested); !slices.Equal(got, []string{fallback}) {
		t.Errorf("suggested %q", got)
	}

	// Rows that make no CLI call.
	for _, row := range []struct {
		name, ps, env, want string
	}{
		{"stopped", "printf ''", "", "http://127.0.0.1:4000/v1: stopped (./setup.sh install starts it) · clone dev"},
		{"compose ps fails", "exit 1", "",
			"http://127.0.0.1:4000/v1: not checked · clone dev · docker compose ps: docker compose ps failed with exit 1"},
		{"not ready", "echo c0ffee", "STARGAITE_PORT=" + closedPort(t), "http://127.0.0.1:<port>/v1: not ready · clone dev"},
		{"settings", "echo c0ffee", "STARGAITE_PORT=x",
			"this clone's settings cannot be read: STARGAITE_PORT in .env is not a port number, 1 to 65535: write it as STARGAITE_PORT=<n>"},
	} {
		h := psClone(t, row.ps)
		want := row.want
		if row.env != "" {
			writeFile(t, filepath.Join(h.Dir, ".env"), row.env+"\n", 0o600)
			want = strings.Replace(want, "<port>", strings.TrimPrefix(row.env, "STARGAITE_PORT="), 1)
		}
		var calls []string
		hm := readHome(ctx, proxy.New(cannedRunner(t, nil, &calls)), h)
		if hm.summary != want || hm.suggested != nil || len(calls) != 0 {
			t.Errorf("%s: summary %q, suggested %q, calls %q; want %q", row.name, hm.summary, labels(hm.suggested), calls, want)
		}
	}

	// A read that fails: the proxy was not read, and nothing is suggested.
	w = newWorld(t)
	reads := homeReads()
	reads["validate --json"] = reply{code: 1}
	w.runner = serial(cannedRunner(t, reads, new([]string)))
	hm = readHome(ctx, w.proxy(), w.h)
	if want := "http://127.0.0.1:" + w.models.port() + "/v1: ready · clone dev · the proxy was not read: stargaite validate: failed with exit 1"; hm.summary != want || hm.suggested != nil {
		t.Errorf("summary %q, suggested %q; want %q", hm.summary, labels(hm.suggested), want)
	}
}

// psClone is a host whose docker runs sh for compose ps -q, with no git on
// PATH, so the clone's version is dev.
func psClone(t *testing.T, sh string) *host.Host {
	t.Helper()
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "docker"), "#!/bin/sh\n[ \"$*\" = \"compose ps -q\" ] || exit 9\n"+sh+"\n", 0o755)
	t.Setenv("PATH", bin)
	return host.New(t.TempDir())
}

func TestHomeReadConcurrent(t *testing.T) {
	w := newWorld(t)
	var (
		mu      sync.Mutex
		started int
	)
	w.runner = func(ctx context.Context, args []string, _ io.Reader, stdout, _ io.Writer) (int, error) {
		mu.Lock()
		started++
		mu.Unlock()
		for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			mu.Lock()
			all := started == 3
			mu.Unlock()
			if all {
				break
			}
			if time.Now().After(deadline) {
				return 1, nil // the reads did not run together
			}
		}
		_, _ = io.WriteString(stdout, homeReads()[strings.Join(args, " ")].out)
		return 0, nil
	}
	hm := readHome(context.Background(), w.proxy(), w.h)
	if want := "http://127.0.0.1:" + w.models.port() + "/v1: ready · proxy v1 · clone dev · 0 provider(s), 0 alias(es), 0 key(s) · spend not capped"; hm.summary != want {
		t.Errorf("summary %q; want %q", hm.summary, want)
	}
	if got := labels(hm.suggested); !slices.Equal(got, []string{"Add a provider: none is added yet"}) {
		t.Errorf("suggested %q", got)
	}
}

// homeFront is scripted, but answers the home's pick with Ctrl-C once its
// answers for it are used up.
type homeFront struct{ *scripted }

func (f homeFront) Ask(ctx context.Context, q ui.Question) (string, error) {
	f.mu.Lock()
	quit := q.ID == "home" && len(f.answers["home"]) == 0
	if quit {
		f.asked, f.questions = append(f.asked, q.Prompt), append(f.questions, q)
	}
	f.mu.Unlock()
	if quit {
		return "", ui.ErrQuit
	}
	return f.scripted.Ask(ctx, q)
}

// homes is the home's picks the front was asked.
func (s *scripted) homes() []ui.Question {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(s.questions), func(q ui.Question) bool { return q.ID != "home" })
}

func TestHomeBound(t *testing.T) {
	w := newWorld(t)
	w.runner = hangExport
	s := &scripted{answers: map[string][]string{}}
	start := time.Now()
	if err := Home(context.Background(), ui.New(homeFront{s}, nil), w.proxy(), w.h, 200*time.Millisecond); !errors.Is(err, ui.ErrQuit) {
		t.Fatalf("Home: %v", err)
	}
	if took := time.Since(start); took > 1200*time.Millisecond {
		t.Errorf("the home's read took %s; bound 200ms", took)
	}
	want := "http://127.0.0.1:" + w.models.port() + "/v1: ready · clone dev · the proxy was not read: no answer in time\n"
	if s.output() != want {
		t.Errorf("output %q; want %q", s.output(), want)
	}
	if homes := s.homes(); len(homes) != 1 || slices.ContainsFunc(homes[0].Options, func(o ui.Option) bool { return o.Group == "Suggested" }) {
		t.Errorf("home asked %+v", homes)
	}
}

func TestHomeEscDuringRead(t *testing.T) {
	w := newWorld(t)
	w.runner = hangExport
	s := &scripted{answers: map[string][]string{}, leaveWork: func(q ui.Question) bool { return q.ID == "home-read" && len(w.cli()) == 3 }}
	if err := Home(context.Background(), ui.New(homeFront{s}, nil), w.proxy(), w.h, 5*time.Second); !errors.Is(err, ui.ErrQuit) {
		t.Fatalf("Home: %v", err)
	}
	if s.output() != "what exists was not read: stopped\n" {
		t.Errorf("output %q", s.output())
	}
	homes := s.homes()
	if len(homes) != 1 || homes[0].Prompt != "What do you want to do?" ||
		slices.ContainsFunc(homes[0].Options, func(o ui.Option) bool { return o.Group == "Suggested" }) {
		t.Errorf("home asked %+v", homes)
	}
}

func TestHomeQuestion(t *testing.T) {
	hm := home{suggested: []rules.Suggestion{{Label: "Give an app or agent its key: none exists yet", Task: "Give an app or agent its key"},
		{Label: "Give alias a a fallback: it has one model", Task: "Change an alias"}}}
	q := homeQuestion(hm, Menu())
	var got []string
	for _, o := range q.Options {
		got = append(got, o.Group+"|"+o.Value+"|"+o.Label)
	}
	want := []string{
		"Suggested|suggested:0|Give an app or agent its key: none exists yet",
		"Suggested|suggested:1|Give alias a a fallback: it has one model",
		"Providers|Add providers|Add providers",
		"Providers|Change a provider|Change a provider",
		"Models your apps call|Make an alias|Make an alias",
		"Models your apps call|Change an alias|Change an alias",
		"Apps and agents|Give an app or agent its key|Give an app or agent its key",
		"Apps and agents|Change a key|Change a key",
		"This machine|See what exists|See what exists",
	}
	if q.ID != "home" || q.Prompt != "What do you want to do?" || !slices.Equal(got, want) {
		t.Fatalf("%s %q: %q", q.ID, q.Prompt, got)
	}
	if q := homeQuestion(home{}, Menu()); len(q.Options) != 7 || q.Options[0].Group != "Providers" {
		t.Fatalf("without suggestions: %+v", q.Options)
	}
}

func TestHomeSuggestionPresets(t *testing.T) {
	w, port := homeWorld(t)
	s := &scripted{answers: map[string][]string{"home": {"suggested:1"}, "fallback-provider": {leave}}}
	if err := Home(context.Background(), ui.New(homeFront{s}, nil), w.proxy(), w.h, 15*time.Second); !errors.Is(err, ui.ErrQuit) {
		t.Fatalf("Home: %v\n%s", err, s.output())
	}
	var ids []string
	for _, q := range s.questions {
		ids = append(ids, q.ID)
	}
	if !slices.Equal(ids, []string{"home", "fallback-provider", "home"}) {
		t.Errorf("asked %q", ids)
	}
	summary := "http://127.0.0.1:" + port + "/v1: ready · proxy " + cli.Version + " · clone dev · 1 provider(s), 1 alias(es), 0 key(s) · spend not capped\n"
	if out := s.output(); out != summary+"Left Change an alias.\n"+summary {
		t.Errorf("output:\n%s", out)
	}
}

// TestHomeUnresolved: validate's credential_unresolved, read through the real
// CLI, suggests replacing that provider's key, and picking it opens Change a
// provider at its key question.
func TestHomeUnresolved(t *testing.T) {
	w, _ := homeWorld(t)
	w.onMock("acme", "--protocol", "openai", "--api-key-env", "NOPE_KEY", "--billing", "pay_per_use")
	hm := readHome(context.Background(), w.proxy(), w.h)
	const label = "Replace the key of acme: NOPE_KEY does not resolve"
	i := slices.IndexFunc(hm.suggested, func(s rules.Suggestion) bool { return s.Label == label })
	if i < 0 || !maps.Equal(hm.suggested[i].Presets, map[string]string{"provider": "acme", "change": "key"}) {
		t.Fatalf("suggested %+v", hm.suggested)
	}
	s := &scripted{answers: map[string][]string{"home": {"suggested:" + strconv.Itoa(i)}, "key-env": {leave}}}
	if err := Home(context.Background(), ui.New(homeFront{s}, nil), w.proxy(), w.h, 15*time.Second); !errors.Is(err, ui.ErrQuit) {
		t.Fatalf("Home: %v\n%s", err, s.output())
	}
	var ids []string
	for _, q := range s.questions {
		ids = append(ids, q.ID)
	}
	if !slices.Equal(ids, []string{"home", "key-env", "home"}) || !strings.Contains(s.output(), "Left Change a provider.\n") {
		t.Errorf("asked %q\n%s", ids, s.output())
	}
}

// TestSuggestionTasks: every task a suggestion names is a menu entry.
func TestSuggestionTasks(t *testing.T) {
	for _, f := range []rules.Facts{{}, {Providers: 1},
		{Providers: 1, Aliases: map[string]int{"a": 1}, Unresolved: map[string]string{"p": "V"}}} {
		for _, s := range rules.Suggest(f) {
			if !slices.ContainsFunc(Menu(), func(e Entry) bool { return e.Label == s.Task }) {
				t.Errorf("%q opens %q, which is no menu entry", s.Label, s.Task)
			}
		}
	}
}

func TestHomeTaskLines(t *testing.T) {
	for _, row := range []struct {
		res  string
		err  error
		want string
	}{
		{"Done: key k revoked.", nil, "Done: key k revoked."},
		{"", errors.New("stargaite keys list: failed with exit 1"), "Change a key: stargaite keys list: failed with exit 1"},
		{"", ui.ErrLeave, "Left Change a key."},
		{"", nil, ""},
	} {
		if got := taskLine("Change a key", row.res, row.err); got != row.want {
			t.Errorf("taskLine(%q, %v) = %q; want %q", row.res, row.err, got, row.want)
		}
	}
}
