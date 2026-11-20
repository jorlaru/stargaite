package task

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
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
	"github.com/jorlaru/stargaite/internal/console/ui/plain"
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
	const check = "Check alias a: no target of mock is checked yet" // H1: mock/m is never checked
	w, port := homeWorld(t)
	base := "http://127.0.0.1:" + port + "/v1"
	hm := readHome(ctx, w.proxy(), w.h)
	if want := base + ": ready · proxy " + cli.Version + " · clone dev · 1 provider(s), 1 alias(es), 0 key(s) · spend not capped"; hm.summary != want {
		t.Errorf("summary %q; want %q", hm.summary, want)
	}
	if got := slices.Sorted(slices.Values(w.cli())); !slices.Equal(got, []string{"export --json", "validate --json", "version --json"}) {
		t.Errorf("CLI calls %q", got)
	}
	if got := labels(hm.suggested); !slices.Equal(got, []string{"Give an app or agent its key: none exists yet", fallback, "Cap spending: spend is not capped", check}) {
		t.Errorf("suggested %q", got)
	}

	// An instance budget and a key: no "spend not capped", no key row.
	w.must("settings", "set", "--budget", "limit_usd=10,reset_interval=monthly")
	w.must("keys", "create", "--model", "a", "--", "k")
	hm = readHome(ctx, w.proxy(), w.h)
	if want := base + ": ready · proxy " + cli.Version + " · clone dev · 1 provider(s), 1 alias(es), 1 key(s)"; hm.summary != want {
		t.Errorf("summary %q; want %q", hm.summary, want)
	}
	if got := labels(hm.suggested); !slices.Equal(got, []string{fallback, check}) {
		t.Errorf("suggested %q", got)
	}

	// Rows that make no CLI call.
	start := []rules.Suggestion{{Label: "Start the container: it is stopped", Task: "Stop or start", Presets: map[string]string{"action": "start"}}}
	restart := []rules.Suggestion{{Label: "Restart the container: it is not ready", Task: "Stop or start", Presets: map[string]string{"action": "restart"}}}
	for _, row := range []struct {
		name, ps, env, want string
		suggested           []rules.Suggestion
	}{
		{"stopped", "printf ''", "", "http://127.0.0.1:4000/v1: stopped · clone dev", start},
		{"compose ps fails", "exit 1", "",
			"http://127.0.0.1:4000/v1: not checked · clone dev · docker compose ps: docker compose ps failed with exit 1", nil},
		{"not ready", "echo c0ffee", "STARGAITE_PORT=" + closedPort(t), "http://127.0.0.1:<port>/v1: not ready · clone dev", restart},
		{"settings", "echo c0ffee", "STARGAITE_PORT=x",
			"this clone's settings cannot be read: STARGAITE_PORT in .env is not a port number, 1 to 65535: write it as STARGAITE_PORT=<n>", nil},
	} {
		h := psClone(t, row.ps)
		want := row.want
		if row.env != "" {
			writeFile(t, filepath.Join(h.Dir, ".env"), row.env+"\n", 0o600)
			want = strings.Replace(want, "<port>", strings.TrimPrefix(row.env, "STARGAITE_PORT="), 1)
		}
		var calls []string
		hm := readHome(ctx, proxy.New(cannedRunner(t, nil, &calls)), h)
		if hm.summary != want || !slices.EqualFunc(hm.suggested, row.suggested, sameSuggestion) || (hm.suggested == nil) != (row.suggested == nil) ||
			len(calls) != 0 {
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

// psClone is a host whose docker runs sh for compose ps -q and answers
// compose ps -a -q with a container, with no git on PATH, so the clone's
// version is dev.
func psClone(t *testing.T, sh string) *host.Host {
	t.Helper()
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "docker"), "#!/bin/sh\ncase \"$*\" in\n'compose ps -q') "+sh+" ;;\n'compose ps -a -q') echo c0ffee ;;\n*) exit 9 ;;\nesac\n", 0o755)
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
		"Spend and traces|Cap spending|Cap spending",
		"Spend and traces|See usage and cost|See usage and cost",
		"Spend and traces|Send traces|Send traces",
		"This machine|See what exists|See what exists",
		"This machine|Open to the network|Open to the network",
		"This machine|Upgrade|Upgrade",
		"This machine|Back up or restore|Back up or restore",
		"This machine|See logs|See logs",
		"This machine|Stop or start|Stop or start",
		"This machine|Check everything|Check everything",
		"This machine|Uninstall|Uninstall",
	}
	if q.ID != "home" || q.Prompt != "What do you want to do?" || !slices.Equal(got, want) {
		t.Fatalf("%s %q: %q", q.ID, q.Prompt, got)
	}
	if q := homeQuestion(home{}, Menu()); len(q.Options) != 17 || q.Options[0].Group != "Providers" {
		t.Fatalf("without suggestions: %+v", q.Options)
	}
	if slices.ContainsFunc(q.Options, func(o ui.Option) bool { return o.Value == "First run" || o.Label == "First run" }) {
		t.Fatalf("the first run is listed: %+v", q.Options)
	}
}

func TestHomeSuggestionPresets(t *testing.T) {
	w, port := homeWorld(t)
	w.must("keys", "create", "--model", "a", "--", "k")
	hm := readHome(context.Background(), w.proxy(), w.h)
	i := slices.IndexFunc(hm.suggested, func(s rules.Suggestion) bool { return s.Label == "Give alias a a fallback: it has one model" })
	if i < 0 {
		t.Fatalf("suggested %+v", hm.suggested)
	}
	s := &scripted{answers: map[string][]string{"home": {"suggested:" + strconv.Itoa(i)}, "fallback-provider": {leave}}}
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
	summary := "http://127.0.0.1:" + port + "/v1: ready · proxy " + cli.Version + " · clone dev · 1 provider(s), 1 alias(es), 1 key(s) · spend not capped\n"
	if out := s.output(); out != summary+"Left Change an alias.\n"+summary {
		t.Errorf("output:\n%s", out)
	}
}

// TestHomeCapSuggestion: picking "Cap spending: spend is not capped" opens
// Cap spending on the instance, as --on gives it: what exists is not read
// for a pick of the scope.
func TestHomeCapSuggestion(t *testing.T) {
	w, _ := homeWorld(t)
	w.must("keys", "create", "--model", "a", "--", "k")
	hm := readHome(context.Background(), w.proxy(), w.h)
	i := slices.IndexFunc(hm.suggested, func(s rules.Suggestion) bool { return s.Label == "Cap spending: spend is not capped" })
	if i < 0 {
		t.Fatalf("suggested %+v", hm.suggested)
	}
	s := &scripted{answers: map[string][]string{"home": {"suggested:" + strconv.Itoa(i)}, "limit-usd": {"5"}, "reset": {"monthly"}, "yes": {"yes"}}}
	if err := Home(context.Background(), ui.New(homeFront{s}, nil), w.proxy(), w.h, 15*time.Second); !errors.Is(err, ui.ErrQuit) {
		t.Fatalf("Home: %v\n%s", err, s.output())
	}
	if slices.Contains(w.cli(), "models list --json") || slices.Contains(w.cli(), "keys list --json") {
		t.Errorf("the scope pick was read: CLI %q", w.cli())
	}
	reads := []string{"version --json", "export --json", "validate --json"} // the home's own
	got := slices.DeleteFunc(changing(w.cli()), func(c string) bool { return slices.Contains(reads, c) })
	if !slices.Equal(got, []string{"settings set --json --budget limit_usd=5,reset_interval=monthly"}) {
		t.Errorf("changes %q\n%s", got, s.output())
	}
}

// TestHomeUnresolved: validate's credential_unresolved, read through the real
// CLI, suggests replacing that provider's key, and picking it opens Change a
// provider at its key question.
func TestHomeUnresolved(t *testing.T) {
	w, _ := homeWorld(t)
	w.must("keys", "create", "--model", "a", "--", "k")
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

// TestSuggestionTasks: every task a suggestion, the home's Start and
// Restart, or a fix of Check everything names is a menu entry.
func TestSuggestionTasks(t *testing.T) {
	var all []rules.Suggestion
	for _, f := range []rules.Facts{{}, {Providers: 1},
		{Providers: 1, Aliases: map[string]int{"a": 1}, Unresolved: map[string]string{"p": "V"}},
		{Providers: 1, Aliases: map[string]int{"a": 1}, Unverified: map[string]string{"p": "a"}}} {
		all = append(all, rules.Suggest(f)...)
	}
	for _, hm := range []home{readHome(context.Background(), nil, psClone(t, "printf ''")), notReady(t)} {
		all = append(all, hm.suggested...)
	}
	for _, f := range [][3]string{{"credential_unresolved", "providers/p", ""}, {"credential_unresolved", "settings", ""},
		{"target_unverified", "targets/p/m", "a"}, {"unknown_alias", "keys/k", ""}, {"effort_dropped", "models/a", ""},
		{"endpoint_unservable", "models/a", ""}} {
		s, ok := rules.Fix(f[0], f[1], f[2])
		if !ok {
			t.Errorf("no fix for %q", f)
		}
		all = append(all, s)
	}
	if len(all) != 16 {
		t.Errorf("%d suggestions, want 16: %+v", len(all), all)
	}
	for _, s := range all {
		if !slices.ContainsFunc(Menu(), func(e Entry) bool { return e.Label == s.Task }) {
			t.Errorf("%q opens %q, which is no menu entry", s.Label, s.Task)
		}
	}
}

// notReady is the home of a running container that is not ready.
func notReady(t *testing.T) home {
	t.Helper()
	h := psClone(t, "echo c0ffee")
	writeFile(t, filepath.Join(h.Dir, ".env"), "STARGAITE_PORT="+closedPort(t)+"\n", 0o600)
	return readHome(context.Background(), nil, h)
}

// TestHomeUnverifiedProvider: a provider none of whose chat targets is
// checked suggests checking an alias that holds one (H1), whatever its other
// kinds of target; not once one is checked, not for a provider whose key
// does not resolve, and not when no alias holds one.
func TestHomeUnverifiedProvider(t *testing.T) {
	ctx := context.Background()
	w, _ := homeWorld(t)
	w.target("mock/e", "--kind", "embeddings")
	want := rules.Suggestion{Label: "Check alias a: no target of mock is checked yet", Task: "Make an alias",
		Presets: map[string]string{"alias": "a", "unverified": "yes"}}
	hm := readHome(ctx, w.proxy(), w.h)
	if !slices.ContainsFunc(hm.suggested, func(s rules.Suggestion) bool { return sameSuggestion(s, want) }) {
		t.Fatalf("suggested %+v", hm.suggested)
	}
	if _, err := w.checkIn(ui.New(plain.New(&bytes.Buffer{}), nil), "mock/m"); err != nil {
		t.Fatal(err)
	}
	if hm = readHome(ctx, w.proxy(), w.h); slices.ContainsFunc(labels(hm.suggested), func(l string) bool { return strings.HasPrefix(l, "Check alias") }) {
		t.Errorf("checked: suggested %q", labels(hm.suggested))
	}

	w, _ = homeWorld(t)
	w.onMock("acme", "--protocol", "openai", "--api-key-env", "ACME_API_KEY", "--billing", "none")
	w.target("acme/m", "--kind", "chat")
	w.must("models", "set", "a", "--target", "mock/m", "--target", "acme/m")
	w.onMock("free", "--protocol", "openai", "--billing", "none")
	w.target("free/m", "--kind", "chat")
	w.onMock("half", "--protocol", "openai", "--billing", "none")
	w.target("half/v", "--kind", "chat")
	w.target("half/u", "--kind", "chat")
	w.must("models", "add", "b", "--target", "half/u", "--target", "half/v")
	if _, err := w.checkIn(ui.New(plain.New(&bytes.Buffer{}), nil), "half/v"); err != nil {
		t.Fatal(err)
	}
	hm = readHome(ctx, w.proxy(), w.h)
	var rows []string
	for _, l := range labels(hm.suggested) {
		if strings.Contains(l, "acme") || strings.Contains(l, "free") || strings.Contains(l, "half") || strings.HasPrefix(l, "Check alias") {
			rows = append(rows, l)
		}
	}
	if !slices.Equal(rows, []string{"Replace the key of acme: ACME_API_KEY does not resolve", want.Label}) {
		t.Errorf("rows %q", rows)
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

func TestHomeOpensFirstRun(t *testing.T) {
	// (a) No container at all: the first run, at once, from the Machine step.
	m := machineClone(t, psNone, psNone, "trap 'exit 130' INT\n/bin/sleep 60")
	p := closedPort(t)
	m.env("STARGAITE_PORT=" + p + "\n")
	s := &scripted{answers: map[string][]string{}, leaveWork: func(q ui.Question) bool {
		return q.Prompt == "building and starting the container" && strings.Contains(m.dockerLog(), "compose up")
	}}
	var calls []string
	if err := Home(context.Background(), ui.New(homeFront{s}, nil), proxy.New(cannedRunner(t, nil, &calls)), m.h, 15*time.Second); !errors.Is(err, ui.ErrQuit) {
		t.Fatalf("Home: %v\n%s", err, s.output())
	}
	f2 := "http://127.0.0.1:" + p + "/v1: not installed yet (./setup.sh install sets it up) · clone dev\n"
	want := f2 + "building and starting version dev (the first build takes a few minutes)\nLeft First run.\n" + f2
	if s.output() != want || len(s.homes()) != 1 || len(s.questions) != 1 || len(calls) != 0 {
		t.Fatalf("output %q; want %q; asked %q, CLI calls %q", s.output(), want, s.asked, calls)
	}

	// (b) A ready store with no alias: the first run at the alias stage, not the home's pick.
	w, _, _ := keyWorld(t)
	s = &scripted{answers: map[string][]string{"alias": {leave, leave}, "home": {"suggested:0"}}}
	if err := Home(context.Background(), ui.New(homeFront{s}, nil), w.proxy(), w.h, 15*time.Second); !errors.Is(err, ui.ErrQuit) {
		t.Fatalf("Home: %v\n%s", err, s.output())
	}
	var ids []string
	for _, q := range s.questions {
		ids = append(ids, q.ID)
	}
	if !slices.Equal(ids, []string{"alias", "home", "alias", "home"}) {
		t.Fatalf("asked %q\n%s", ids, s.output())
	}
	for _, i := range []int{0, 2} {
		if q := s.questions[i]; q.Prompt != "a name for the alias your clients call" || !slices.Equal(q.Bar.Titles, eight) || q.Bar.Current != 2 {
			t.Errorf("question %d: %q at %+v", i, q.Prompt, q.Bar)
		}
	}
	if o := s.homes()[0].Options[0]; o.Value != "suggested:0" || o.Label != "Make an alias your apps call: none exists yet" {
		t.Errorf("first option %+v", o)
	}
	if strings.Count(s.output(), "Left First run.\n") != 2 {
		t.Errorf("output:\n%s", s.output())
	}
}

func TestHomeNoFirstRunLater(t *testing.T) {
	// The first run opens on the first round only.
	w, _, _ := keyWorld(t)
	s := &scripted{answers: map[string][]string{"alias": {leave}}}
	if err := Home(context.Background(), ui.New(homeFront{s}, nil), w.proxy(), w.h, 15*time.Second); !errors.Is(err, ui.ErrQuit) {
		t.Fatalf("Home: %v\n%s", err, s.output())
	}
	if len(s.questions) != 2 || s.questions[1].ID != "home" {
		t.Fatalf("asked %q", s.asked)
	}

	// (c) A provider whose credential does not resolve, with an alias and a key: the home at once.
	env, port := admintest.Plane(t)
	w = worldOn(t, env)
	writeFile(t, filepath.Join(w.h.Dir, ".env"), "STARGAITE_PORT="+port+"\n", 0o600)
	srv := httptest.NewServer(mockprovider.New())
	t.Cleanup(srv.Close)
	w.must("providers", "add", "acme", "--base-url", srv.URL, "--allow-private-network", "--api-key-env", "NOPE_KEY",
		"--protocol", "openai", "--billing", "pay_per_use")
	w.target("acme/m", "--kind", "chat")
	w.must("models", "add", "a", "--target", "acme/m")
	w.must("keys", "create", "--model", "a", "--", "k")
	s = &scripted{answers: map[string][]string{}}
	if err := Home(context.Background(), ui.New(homeFront{s}, nil), w.proxy(), w.h, 15*time.Second); !errors.Is(err, ui.ErrQuit) {
		t.Fatalf("Home: %v\n%s", err, s.output())
	}
	if len(s.questions) != 1 || s.questions[0].ID != "home" ||
		!slices.ContainsFunc(s.questions[0].Options, func(o ui.Option) bool { return o.Label == "Replace the key of acme: NOPE_KEY does not resolve" }) {
		t.Fatalf("asked %+v", s.questions)
	}
}

// setUp is an export with a provider, an alias and a key: the home opens
// no first run.
const setUp = `{"version":1,"providers":{"p":{}},"models":{"main":{"targets":["p/m"]}},"keys":{"k":{}}}`

// TestHomeReexec: a task that restarts the console, or ends the session
// with an error, ends the home with it, its result said first.
func TestHomeReexec(t *testing.T) {
	w := upgradeWorld(t, 1)
	w.export = setUp
	s := &scripted{answers: map[string][]string{"home": {"Upgrade"}, "yes": {"yes"}}}
	err := Home(context.Background(), ui.New(s, nil), w.proxy(), w.h, 15*time.Second)
	if !errors.Is(err, ErrReexec) || !strings.Contains(s.output(), "\nupgraded: ") {
		t.Fatalf("err %v\n%s", err, s.output())
	}
	w = upgradeWorld(t, 1)
	w.export = setUp
	t.Setenv("EXPORT_DO", "exit 1")
	s = &scripted{answers: map[string][]string{"home": {"Upgrade"}, "yes": {"yes"}}}
	err = Home(context.Background(), ui.New(s, nil), w.proxy(), w.h, 15*time.Second)
	if !errors.Is(err, ErrEnd) || err == ErrEnd || !strings.HasPrefix(err.Error(), "the new version runs, but the new console was not exported") {
		t.Fatalf("export fails: err %v\n%s", err, s.output())
	}
}

// TestHomeUpgradeRows: the home suggests Rebuild when the proxy runs
// another version than the clone's, and Upgrade when the clone's upstream,
// as last fetched, has new commits; it fetches nothing. A proxy whose
// answers this console cannot decode is mended by Rebuild alone.
func TestHomeUpgradeRows(t *testing.T) {
	rebuild := rules.Suggestion{Label: "Rebuild to this clone's version", Task: "Upgrade", Presets: map[string]string{"rebuild": "yes"}}
	upgrade := rules.Suggestion{Label: "Upgrade: 1 new commit(s) on origin/main", Task: "Upgrade"}
	w := upgradeWorld(t, 1)
	writeFile(t, filepath.Join(w.bin, "running"), "v-old\n", 0o644)
	w.git(w.h.Dir, "fetch", "-q")
	hm := readHome(context.Background(), w.proxy(), w.h)
	got := hm.suggested
	if len(got) < 2 || !reflect.DeepEqual(got[len(got)-2:], []rules.Suggestion{rebuild, upgrade}) {
		t.Fatalf("suggested %+v", got)
	}
	gitCalls := strings.Join(slices.DeleteFunc(w.entries(), func(l string) bool { return !strings.HasPrefix(l, "git ") }), "\n")
	if !strings.Contains(gitCalls, "git rev-parse --abbrev-ref @{upstream}") || !strings.Contains(gitCalls, "git rev-list --count HEAD..@{upstream}") ||
		strings.Contains(gitCalls, "fetch") {
		t.Fatalf("git calls:\n%s", gitCalls)
	}
	w.export = `{"providers":{}}`
	hm = readHome(context.Background(), w.proxy(), w.h)
	if !strings.Contains(hm.summary, "the proxy was not read: ") ||
		!strings.HasSuffix(hm.summary, "this console and the running proxy are different versions; Rebuild to this clone's version (./setup.sh upgrade --rebuild) mends it") ||
		!reflect.DeepEqual(hm.suggested, []rules.Suggestion{rebuild}) {
		t.Fatalf("summary %q, suggested %+v", hm.summary, hm.suggested)
	}
}
