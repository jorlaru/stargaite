package task

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jorlaru/stargaite/internal/admintest"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
	"github.com/jorlaru/stargaite/internal/console/ui/plain"
	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// resolving is a variable admintest's container environment holds, so a
// provider whose credential names it resolves.
const resolving = "MINIMAX_PLAN_API_KEY"

// f1 is the first run's result at a terminal when nothing is missing.
const f1 = "Nothing is missing: a provider whose key resolves, an alias and a key exist; the home's tasks change them."

// eight is the first run's bar.
var eight = []string{"machine", "provider", "alias", "model", "fallbacks", "check", "key", "first answer"}

// chatSeen is one chat request the data plane got: its Authorization and body.
type chatSeen struct{ auth, body string }

// planeFront is a reverse proxy in front of the data plane, recording each
// chat request it passes on.
type planeFront struct {
	port  string
	mu    sync.Mutex
	chats []chatSeen
}

func frontOf(t *testing.T, port string) *planeFront {
	t.Helper()
	target, _ := url.Parse("http://127.0.0.1:" + port)
	rp := httputil.NewSingleHostReverseProxy(target)
	f := &planeFront{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			f.mu.Lock()
			f.chats = append(f.chats, chatSeen{r.Header.Get("Authorization"), string(body)})
			f.mu.Unlock()
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	f.port = u.Port()
	return f
}

func (f *planeFront) seen() []chatSeen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.chats)
}

// firstWorld is the first run's fixture: the task world on admintest.Plane,
// whose .env names a recording front of the data plane; the docker fake
// answers the container as running; the CLI reaches host.docker.internal on
// the host, as the container's host-gateway does; and a mock provider. The
// store starts empty.
type firstWorld struct {
	*world
	mock  *mockprovider.Server
	url   string // the mock's base URL
	front *planeFront
}

func newFirstWorld(t *testing.T) *firstWorld {
	t.Helper()
	env, port := admintest.Plane(t)
	w := worldOn(t, env)
	front := frontOf(t, port)
	writeFile(t, filepath.Join(w.h.Dir, ".env"), "STARGAITE_PORT="+front.port+"\n", 0o600)
	next := w.runner
	w.runner = func(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		onHost := make([]string, len(args))
		for i, a := range args {
			onHost[i] = strings.ReplaceAll(a, "//host.docker.internal:", "//127.0.0.1:")
		}
		return next(ctx, onHost, stdin, stdout, stderr)
	}
	mock := mockprovider.New()
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	return &firstWorld{world: w, mock: mock, url: srv.URL, front: front}
}

// first runs the first run with u, as its menu entry does.
func (f *firstWorld) first(u *ui.UI) (string, error) {
	return u.Run(context.Background(), firstSteps(), func(ctx context.Context) (string, error) {
		return FirstRun(ctx, u, f.proxy(), f.h)
	})
}

// firstPlain runs it with the plain front, answers and acme's key in the
// console's environment; it returns the result, the output and the error.
func (f *firstWorld) firstPlain(answers map[string]string) (string, string, error) {
	var out bytes.Buffer
	res, err := f.first(ui.New(plain.New(&out), answers).WithEnv([]string{"ACME_KEY=sk-acme-test"}))
	return res, out.String(), err
}

// fullFlags answers every question of a first run from nothing: acme, a
// compatible service on the mock, its model m, alias main and key app.
func (f *firstWorld) fullFlags() map[string]string {
	return map[string]string{"provider": "acme", "base-url": f.url, "protocol": "openai", "api-key-env": resolving, "key-env": "ACME_KEY",
		"model": "m", "alias": "main", "key-name": "app", "rpm-limit": "30", "yes": "yes"}
}

// keyless adds provider name on the mock without a credential, through the CLI.
func (f *firstWorld) keylessMock(name string) {
	f.must("providers", "add", name, "--protocol", "openai", "--base-url", f.url, "--allow-private-network", "--billing", "none")
}

// chats is the mock's chat calls: a verify's or a try's.
func (f *firstWorld) chats() int {
	n := 0
	for _, c := range f.mock.Calls() {
		if c.Path == "/chat/completions" {
			n++
		}
	}
	return n
}

// writes is changing without the first run's own reads: export, validate
// and the presets.
func writes(argvs []string) []string {
	var out []string
	for _, a := range changing(argvs) {
		if !strings.HasPrefix(a, "export ") && !strings.HasPrefix(a, "validate ") && !strings.HasPrefix(a, "providers presets ") {
			out = append(out, a)
		}
	}
	return out
}

// with is answers with more set; an empty value removes the answer.
func with(answers map[string]string, more ...string) map[string]string {
	out := map[string]string{}
	for k, v := range answers {
		out[k] = v
	}
	for i := 0; i+1 < len(more); i += 2 {
		if more[i+1] == "" {
			delete(out, more[i])
		} else {
			out[more[i]] = more[i+1]
		}
	}
	return out
}

// recorder is a scripted front that records every question, work included.
type recorder struct {
	*scripted
	mu  sync.Mutex
	all []ui.Question
}

func (r *recorder) Ask(ctx context.Context, q ui.Question) (string, error) {
	r.mu.Lock()
	r.all = append(r.all, q)
	r.mu.Unlock()
	return r.scripted.Ask(ctx, q)
}

func (r *recorder) every() []ui.Question {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.all)
}

func record(answers map[string][]string) *recorder {
	return &recorder{scripted: &scripted{answers: answers}}
}

// question is the first question asked with prompt.
func question(t *testing.T, qs []ui.Question, prompt string) ui.Question {
	t.Helper()
	q, ok := find(qs, func(q ui.Question) bool { return q.Prompt == prompt })
	if !ok {
		t.Fatalf("%q was never asked", prompt)
	}
	return q
}

// atBar fails unless q's bar is the first run's at current.
func atBar(t *testing.T, q ui.Question, current int) {
	t.Helper()
	if !slices.Equal(q.Bar.Titles, eight) || q.Bar.Current != current {
		t.Errorf("%s: bar %+v; want the first run's at %d", q.Prompt, q.Bar, current)
	}
}

// resolvedAfterUp makes the CLI answer as a container recreated with every
// variable of .env would, once compose up ran: each credential resolved in
// providers show and list, and validate without credential_unresolved
// findings (admintest's container environment is fixed).
func (f *firstWorld) resolvedAfterUp() {
	next := f.runner
	f.runner = func(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		words := strings.Join(args, " ")
		reads := strings.HasPrefix(words, "providers show") || strings.HasPrefix(words, "providers list") || strings.HasPrefix(words, "validate")
		if f.ups() == 0 || !reads {
			return next(ctx, args, stdin, stdout, stderr)
		}
		var out bytes.Buffer
		code, err := next(ctx, args, stdin, &out, stderr)
		var v any
		if err != nil || json.Unmarshal(out.Bytes(), &v) != nil {
			_, _ = stdout.Write(out.Bytes())
			return code, err
		}
		b, _ := json.Marshal(resolvedAll(v))
		_, _ = stdout.Write(b)
		return code, nil
	}
}

// resolvedAll is v with every "credential" resolved and no
// credential_unresolved finding.
func resolvedAll(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if k == "credential" && e == "unresolved" {
				x[k] = "resolved"
			} else {
				x[k] = resolvedAll(e)
			}
		}
	case []any:
		var kept []any
		for _, e := range x {
			if m, ok := e.(map[string]any); !ok || m["code"] != "credential_unresolved" {
				kept = append(kept, resolvedAll(e))
			}
		}
		return kept
	}
	return v
}

func TestFirstRunStages(t *testing.T) {
	ids := make([]string, len(firstSteps()))
	for i, s := range firstSteps() {
		ids[i] = s.ID
	}
	if want := []string{"machine", "provider", "alias", "model", "fallbacks", "check", "key", "answer"}; !slices.Equal(ids, want) {
		t.Fatalf("firstSteps() IDs %q", ids)
	}
	for i, s := range firstSteps() {
		if s.Title != eight[i] {
			t.Fatalf("firstSteps() = %+v", firstSteps())
		}
	}
	for _, s := range stages() {
		last := 0
		for _, inner := range s.steps {
			at := slices.Index(ids, s.at[inner.ID])
			if at < 0 || at < last {
				t.Fatalf("stage %s: step %s is drawn at %q, after %q", s.name, inner.ID, s.at[inner.ID], ids[last])
			}
			last = at
		}
	}
	var names []string
	for _, s := range stages() {
		names = append(names, s.name)
	}
	if !slices.Equal(names, []string{"provider", "alias", "key"}) {
		t.Fatalf("stages %q", names)
	}

	// A stage's flags are its own task's, and with port and yes they are
	// the first run's, so a flag added to one list and not the others fails.
	entries := map[string][]string{}
	for _, e := range Menu() {
		entries[e.Label] = append(slices.Clone(e.Flags), e.Bools...)
	}
	own := map[string]string{"provider": "Add providers", "alias": "Make an alias", "key": "Give an app or agent its key"}
	all := []string{"port", "yes"}
	for _, s := range stages() {
		for _, flag := range s.flags {
			if !slices.Contains(entries[own[s.name]], flag) {
				t.Errorf("stage %s: --%s is not a flag of %s", s.name, flag, own[s.name])
			}
		}
		all = append(all, s.flags...)
	}
	first := entries["First run"]
	slices.Sort(all)
	slices.Sort(first)
	if !slices.Equal(all, first) {
		t.Errorf("the stages' flags with port and yes %q; the First run entry's %q", all, first)
	}
}

func TestFirstRunFlags(t *testing.T) {
	f := newFirstWorld(t)
	res, out, err := f.firstPlain(f.fullFlags())
	if err != nil || res != "Done: key app, which may call main." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	want := []string{
		"providers add --json --base-url http://host.docker.internal:" + strings.TrimPrefix(f.url, "http://127.0.0.1:") +
			" --api-key-env " + resolving + " --protocol openai --billing pay_per_use --allow-private-network -- acme",
		"targets add --json --kind chat -- acme/m", verifyOf("acme/m"), "models add --json --target acme/m -- main",
		"keys create --json --model main --rpm-limit 30 -- app"}
	if got := writes(f.cli()); !slices.Equal(got, want) {
		t.Errorf("CLI calls:\n%q\nwant:\n%q", got, want)
	}
	secrets := secretLine.FindAllString(out, -1)
	if len(secrets) != 1 {
		t.Fatalf("%d secrets alone on a line:\n%s", len(secrets), out)
	}
	seen := f.front.seen()
	if len(seen) != 1 || seen[0].auth != "Bearer "+secrets[0] || !strings.Contains(seen[0].body, `"max_tokens":256`) {
		t.Errorf("the data plane got %+v", seen)
	}
	if f.chats() != 2 || len(f.verifies()) != 1 {
		t.Errorf("%d mock chat calls, verifies %q; want the check's and the try's", f.chats(), f.verifies())
	}
	order := []string{"the container runs; kept", "Done: acme added, key not checked.",
		"check acme/m: 1 paid call(s) to acme, each capped at 256 output tokens, never retried", "Done: alias main toward acme/m.",
		"try alias main: up to 1 paid call(s), one per target if the earlier fail, each capped at 256 output tokens, never retried"}
	hasLines(t, out, order...)
	for i := 1; i < len(order); i++ {
		if !before(out, order[i-1], order[i]) {
			t.Errorf("%q is not before %q:\n%s", order[i-1], order[i], out)
		}
	}
	if where := f.leaks("sk-acme-test", out); len(where) > 0 {
		t.Errorf("acme's key leaked to %q", where)
	}
}

func TestFirstRunFlagsAgain(t *testing.T) {
	f := newFirstWorld(t)
	if res, out, err := f.firstPlain(f.fullFlags()); err != nil {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	was, chats, seen := len(f.cli()), f.chats(), len(f.front.seen())
	res, out, err := f.firstPlain(f.fullFlags())
	const t14 = "key app exists, and its secret cannot be shown again: no key made and no test call; Change a key rotates it"
	if err != nil || res != t14 {
		t.Fatalf("again: %q, %v\n%s", res, err, out)
	}
	if got := writes(f.cli()[was:]); len(got) != 0 || f.chats() != chats || len(f.front.seen()) != seen {
		t.Errorf("again: CLI calls %q, %d more mock calls, %d more plane requests", got, f.chats()-chats, len(f.front.seen())-seen)
	}
	hasLines(t, out, "the container runs; kept", "provider acme exists; kept", resolving+" is in .env; kept",
		"alias main exists, toward acme/m; kept: Change an alias changes it")
	if secretLine.MatchString(out) {
		t.Errorf("again: a secret was printed:\n%s", out)
	}

	was = len(f.cli())
	res, out, err = f.firstPlain(with(f.fullFlags(), "key-name", "second"))
	if err != nil || res != "Done: key second, which may call main." {
		t.Fatalf("second: %q, %v\n%s", res, err, out)
	}
	if got := writes(f.cli()[was:]); !slices.Equal(got, []string{"keys create --json --model main --rpm-limit 30 -- second"}) ||
		len(f.verifies()) != 1 || len(f.front.seen()) != seen+1 {
		t.Errorf("second: CLI calls %q, verifies %q, %d plane requests", got, f.verifies(), len(f.front.seen()))
	}
}

func TestFirstRunReverify(t *testing.T) {
	f := newFirstWorld(t)
	if res, out, err := f.firstPlain(f.fullFlags()); err != nil {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	res, out, err := f.firstPlain(with(f.fullFlags(), "key-name", "third", "reverify", "yes"))
	if err != nil || res != "Done: key third, which may call main." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	if len(f.verifies()) != 2 || f.chats() != 4 {
		t.Errorf("verifies %q, %d mock chat calls", f.verifies(), f.chats())
	}
	hasLines(t, out, "alias main exists, toward acme/m; kept, and checked again")
	if !before(out, "alias main exists, toward acme/m; kept, and checked again", "creating key third, which may call main") {
		t.Errorf("the alias's result is not before the key's:\n%s", out)
	}
}

func TestFirstRunUnresolvedOnly(t *testing.T) {
	f := newFirstWorld(t)
	f.must("providers", "add", "acme", "--base-url", f.url, "--allow-private-network", "--api-key-env", "ACME_API_KEY",
		"--protocol", "openai", "--billing", "pay_per_use")
	f.target("acme/m", "--kind", "chat")
	f.must("models", "add", "main", "--target", "acme/m")
	f.must("keys", "create", "--model", "main", "--", "app")
	r := record(map[string][]string{"provider": {leave}})
	res, err := f.first(ui.New(r, nil))
	if !errors.Is(err, ui.ErrLeave) || taskLine("First run", res, err) != "Left First run." {
		t.Fatalf("%q, %v\n%s", res, err, r.output())
	}
	atBar(t, question(t, r.questions, "Which providers do you want to add?"), 1)
	if r.questions[0].Prompt != "Which providers do you want to add?" {
		t.Errorf("first asked %q", r.questions[0].Prompt)
	}
	for _, q := range r.every() {
		if q.Bar.Current >= 2 {
			t.Errorf("%q asked at %d", q.Prompt, q.Bar.Current)
		}
	}
	if got := writes(f.cli()); len(got) != 0 || len(f.verifies()) != 0 {
		t.Errorf("CLI calls %q", got)
	}
}

func TestFirstRunResumeAtAlias(t *testing.T) {
	f := newFirstWorld(t)
	f.keylessMock("mock")
	env := f.file(".env")
	r := record(map[string][]string{"alias": {leave}})
	if _, err := f.first(ui.New(r, nil)); !errors.Is(err, ui.ErrLeave) {
		t.Fatalf("err = %v\n%s", err, r.output())
	}
	if len(r.questions) == 0 || r.questions[0].Prompt != "a name for the alias your clients call" {
		t.Fatalf("asked %q", r.asked)
	}
	atBar(t, r.questions[0], 2)
	for _, q := range r.every() {
		if q.Kind == ui.KindWork && q.Bar.Current != 0 && q.Bar.Current != 2 {
			t.Errorf("work %q at %d", q.Prompt, q.Bar.Current)
		}
		if q.Prompt == r.questions[0].Prompt {
			break
		}
	}
	if got := writes(f.cli()); len(got) != 0 || f.file(".env") != env {
		t.Errorf("CLI calls %q, .env %q", got, f.file(".env"))
	}
}

func TestFirstRunResumeAtKey(t *testing.T) {
	f := newFirstWorld(t)
	f.keylessMock("mock")
	f.target("mock/m", "--kind", "chat")
	f.must("models", "add", "main", "--target", "mock/m")
	r := record(map[string][]string{"alias": {"main"}, "key-name": {""}, "rpm-limit": {""}, "secret": {""}, "try": {"check"}})
	res, err := f.first(ui.New(r, nil))
	if err != nil || res != "Done: key my-app, which may call main." {
		t.Fatalf("%q, %v\n%s", res, err, r.output())
	}
	if r.questions[0].Prompt != "Which aliases may it call?" {
		t.Fatalf("first asked %q", r.questions[0].Prompt)
	}
	atBar(t, r.questions[0], 6)
	try, _ := find(r.questions, func(q ui.Question) bool { return q.ID == "try" })
	atBar(t, try, 7)
	if len(f.verifies()) != 0 || f.chats() != 1 {
		t.Errorf("verifies %q, %d mock chat calls", f.verifies(), f.chats())
	}
}

func TestFirstRunUnresolvedProvider(t *testing.T) {
	f := newFirstWorld(t)
	f.must("providers", "add", "acme", "--base-url", f.url, "--allow-private-network", "--api-key-env", "ACME_API_KEY",
		"--protocol", "openai", "--billing", "pay_per_use")
	writeFile(t, filepath.Join(f.h.Dir, ".env"), "STARGAITE_PORT="+f.front.port+"\nACME_API_KEY='sk-in-env'\n", 0o600)
	f.resolvedAfterUp()
	r := record(map[string][]string{"provider": {"acme"}, "replace": {"no"}, "yes": {"yes"}, "alias": {leave}})
	if _, err := f.first(ui.New(r, nil)); !errors.Is(err, ui.ErrLeave) {
		t.Fatalf("err = %v\n%s", err, r.output())
	}
	if r.questions[0].Prompt != "Which providers do you want to add?" {
		t.Fatalf("first asked %q", r.questions[0].Prompt)
	}
	atBar(t, r.questions[0], 1)
	atBar(t, question(t, r.questions, "a name for the alias your clients call"), 2)
	hasLines(t, r.output(), "ACME_API_KEY is in .env; kept")
	if r.asks("key-env") || strings.Count(strings.Join(r.asked, "\n"), "continue?") != 1 || f.ups() != 1 {
		t.Errorf("asked %q, %d recreates", r.asked, f.ups())
	}
	if f.file(".env") != "STARGAITE_PORT="+f.front.port+"\nACME_API_KEY='sk-in-env'\n" {
		t.Errorf(".env %q", f.file(".env"))
	}
}

func TestFirstRunResumeAfterNotNow(t *testing.T) {
	f := newFirstWorld(t)
	flags := map[string]string{"provider": "acme", "base-url": f.url, "protocol": "openai", "api-key-env": "none", "yes": "yes"}
	r := record(map[string][]string{"alias": {""}, "model": {"m"}, "fallback-provider": {""}, "effort": {""}, "check": {"not now"}})
	res, err := f.first(ui.New(r, flags))
	if err != nil || res != "Not checked: alias main was not made." {
		t.Fatalf("%q, %v\n%s", res, err, r.output())
	}
	if f.chats() != 0 || slices.ContainsFunc(r.every(), func(q ui.Question) bool { return q.Bar.Current >= 6 }) {
		t.Errorf("%d mock chat calls, asked %q", f.chats(), r.asked)
	}
	var targets proxy.TargetsReply
	f.decoded(&targets, "targets", "list")
	if len(targets.Targets) != 1 || targets.Targets[0].ID != "acme/m" || targets.Targets[0].VerifiedAt != 0 {
		t.Fatalf("targets %+v", targets.Targets)
	}

	was := len(f.cli())
	r = record(map[string][]string{"alias": {"", leave}, "provider": {"acme"}, "model": {"m"}, "fallback-provider": {""}, "effort": {""},
		"check": {"check"}})
	if _, err := f.first(ui.New(r, nil)); !errors.Is(err, ui.ErrLeave) {
		t.Fatalf("again: err = %v\n%s", err, r.output())
	}
	atBar(t, r.questions[0], 2)
	paid := question(t, r.questions, "1 paid call(s) in all")
	atBar(t, paid, 5)
	if got := f.cli()[was:]; slices.ContainsFunc(got, func(a string) bool { return strings.HasPrefix(a, "targets add") }) || f.chats() != 1 {
		t.Errorf("again: CLI calls %q, %d mock chat calls", got, f.chats())
	}
}

func TestFirstRunNothingMissing(t *testing.T) {
	f := newFirstWorld(t)
	f.keylessMock("mock")
	f.target("mock/m", "--kind", "chat")
	f.must("models", "add", "main", "--target", "mock/m")
	f.must("keys", "create", "--model", "main", "--", "app")
	r := record(map[string][]string{})
	res, err := f.first(ui.New(r, nil))
	if err != nil || res != f1 {
		t.Fatalf("%q, %v\n%s", res, err, r.output())
	}
	if got := writes(f.cli()); len(got) != 0 || len(r.questions) != 0 {
		t.Errorf("CLI calls %q, asked %q", got, r.asked)
	}
}

// TestFirstRunFlagAtTerminal: at a terminal a stage whose thing exists runs
// when a flag of it is given, and only that stage.
func TestFirstRunFlagAtTerminal(t *testing.T) {
	f := newFirstWorld(t)
	f.keylessMock("mock")
	f.target("mock/m", "--kind", "chat")
	f.must("models", "add", "main", "--target", "mock/m")
	f.must("keys", "create", "--model", "main", "--", "app")
	r := record(map[string][]string{"alias": {"main"}, "rpm-limit": {""}, "secret": {""}, "try": {"check"}})
	res, err := f.first(ui.New(r, map[string]string{"key-name": "second"}))
	if err != nil || res != "Done: key second, which may call main." {
		t.Fatalf("%q, %v\n%s", res, err, r.output())
	}
	if got := writes(f.cli()); !slices.Equal(got, []string{"keys create --json --model main --rpm-limit 60 -- second"}) {
		t.Errorf("CLI calls %q", got)
	}
	for _, q := range r.questions {
		if q.Bar.Current < 6 {
			t.Errorf("%q asked at %d", q.Prompt, q.Bar.Current)
		}
	}
}

func TestFirstRunUsage(t *testing.T) {
	var usage *ui.UsageError
	for _, flag := range []string{"provider", "base-url", "key-env", "model", "yes"} {
		f := newFirstWorld(t)
		_, out, err := f.firstPlain(with(f.fullFlags(), flag, ""))
		if !errors.As(err, &usage) || !strings.HasPrefix(usage.Msg, "missing --"+flag) {
			t.Errorf("without --%s: err = %v\n%s", flag, err, out)
		}
	}

	f := newFirstWorld(t)
	f.keylessMock("mock")
	f.target("mock/m", "--kind", "chat")
	f.must("models", "add", "main", "--target", "mock/m")
	_, out, err := f.firstPlain(nil)
	if !errors.As(err, &usage) || !strings.HasPrefix(usage.Msg, "missing --provider") {
		t.Errorf("no flag: err = %v\n%s", err, out)
	}
	if slices.ContainsFunc(f.cli(), func(a string) bool { return strings.HasPrefix(a, "keys create") }) ||
		len(f.front.seen()) != 0 || len(f.mock.Calls()) != 0 {
		t.Errorf("no flag: CLI calls %q, %d plane requests, %d mock calls", f.cli(), len(f.front.seen()), len(f.mock.Calls()))
	}

	// The alias exists and no key: without --model it exits 2, as phase 6
	// did, before a key is made or anything paid for.
	f = newFirstWorld(t)
	f.keylessMock("mock")
	f.target("mock/m", "--kind", "chat")
	f.must("models", "add", "main", "--target", "mock/m")
	_, out, err = f.firstPlain(map[string]string{"provider": "mock"})
	if !errors.As(err, &usage) || !strings.HasPrefix(usage.Msg, "missing --model") {
		t.Errorf("only --provider: err = %v\n%s", err, out)
	}
	if slices.ContainsFunc(f.cli(), func(a string) bool { return strings.HasPrefix(a, "keys create") }) ||
		len(f.verifies()) != 0 || len(f.front.seen()) != 0 || f.chats() != 0 {
		t.Errorf("only --provider: CLI calls %q, %d plane requests, %d mock chat calls", f.cli(), len(f.front.seen()), f.chats())
	}

	f = newFirstWorld(t)
	_, _, err = f.firstPlain(with(f.fullFlags(), "provider", "acme,other"))
	if !errors.As(err, &usage) || usage.Msg != "--provider: install takes one provider; ./setup.sh provider adds several" ||
		f.dockerLog() != "" {
		t.Errorf("two providers: err = %v, docker %q", err, f.dockerLog())
	}
}

func TestFirstRunFallbackFlag(t *testing.T) {
	f := newFirstWorld(t)
	f.keylessMock("mock2")
	res, out, err := f.firstPlain(with(f.fullFlags(), "fallback", "mock2/m"))
	if err != nil || res != "Done: key app, which may call main." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	if !slices.Contains(f.cli(), "models add --json --target acme/m --target mock2/m -- main") || len(f.verifies()) != 2 {
		t.Errorf("CLI calls %q", f.cli())
	}
	hasLines(t, out, "2 paid call(s) in all")
}
