package task

import (
	"bytes"
	"context"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jorlaru/stargaite/internal/admintest"
	"github.com/jorlaru/stargaite/internal/cli"
	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
	"github.com/jorlaru/stargaite/internal/console/ui/plain"
)

// cliRunner runs the CLI in process against env, as the container would.
func cliRunner(env map[string]string) proxy.Runner {
	return func(_ context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		if stdin == nil {
			stdin = strings.NewReader("")
		}
		return cli.Main(args, func(k string) string { return env[k] }, stdin, stdout, stderr), nil
	}
}

// reply is a canned answer: what the CLI prints and its exit code.
type reply struct {
	out  string
	code int
}

// cannedRunner answers by the joined arguments and records each call; a call
// it has no answer for fails the test.
func cannedRunner(t *testing.T, answers map[string]reply, calls *[]string) proxy.Runner {
	return func(_ context.Context, args []string, _ io.Reader, stdout, _ io.Writer) (int, error) {
		key := strings.Join(args, " ")
		*calls = append(*calls, key)
		r, ok := answers[key]
		if !ok {
			t.Errorf("unexpected call %q", key)
			return 2, nil
		}
		_, _ = io.WriteString(stdout, r.out)
		return r.code, nil
	}
}

// clone is a host whose docker compose ps prints ps, with no git on PATH,
// so the clone's version is dev.
func clone(t *testing.T, ps string) *host.Host {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\n[ \"$*\" = \"compose ps -q\" ] || exit 9\nprintf '%s' '" + ps + "'\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	return host.New(t.TempDir())
}

func seeWhatExists(t *testing.T, h *host.Host, runner proxy.Runner) (string, error) {
	t.Helper()
	var out bytes.Buffer
	got, err := SeeWhatExists(context.Background(), ui.New(plain.New(&out), nil), proxy.New(runner), h)
	if got != "" {
		t.Fatalf("result = %q, want the lines alone", got)
	}
	return out.String(), err
}

func hasLines(t *testing.T, out string, want ...string) {
	t.Helper()
	lines := strings.Split(out, "\n")
	for _, w := range want {
		found := false
		for _, l := range lines {
			found = found || l == w
		}
		if !found {
			t.Errorf("no line %q in:\n%s", w, out)
		}
	}
}

func TestSeeWhatExistsExample(t *testing.T) {
	env := admintest.Server(t)
	var applied, errOut bytes.Buffer
	if code := cli.Main([]string{"apply", "-f", "../../../config.example.yaml", "--json"}, func(k string) string { return env[k] },
		strings.NewReader(""), &applied, &errOut); code != 0 {
		t.Fatalf("apply: exit %d:\n%s%s", code, applied.String(), errOut.String())
	}
	out, err := seeWhatExists(t, clone(t, "c0ffee\n"), cliRunner(env))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.HasPrefix(out, "version: dev running, dev in this clone\n") {
		t.Errorf("the version line is not first:\n%s", out)
	}
	hasLines(t, out,
		"version: dev running, dev in this clone",
		"provider anthropic: credential resolved",
		"provider tei-embed: credential none",
		"alias deep: anthropic/claude-sonnet-5-5; budget 500 USD, resets monthly",
		"alias fast: minimax-plan/MiniMax-M3.1-Flash, then openrouter/qwen/qwen-3.6-flash",
		"key invoice-agent: may call fast, deep; budget 100 USD, resets monthly",
		"spend: capped at 2000 USD, resets monthly",
		"telemetry https://langfuse.example.com/api/public/otel: credentials resolved",
	)
	if strings.Contains(out, "in no alias") {
		t.Errorf("a target in no alias in:\n%s", out)
	}
}

func TestSeeWhatExistsEmpty(t *testing.T) {
	out, err := seeWhatExists(t, clone(t, "c0ffee\n"), cliRunner(admintest.Server(t)))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := "version: dev running, dev in this clone\nproviders: none\naliases: none\nkeys: none\nspend: not capped\ntelemetry: not set\n"
	if out != want {
		t.Fatalf("out:\n%s\nwant:\n%s", out, want)
	}
}

// shapes is a canned store that covers every line of See what exists.
func shapes() map[string]reply {
	return map[string]reply{
		"version --json":        {`{"version":"v1.2.3"}`, 0},
		"providers list --json": {`{"providers":[{"name":"acme","credential":"unresolved"}]}`, 0},
		"models list --json": {`{"models":[{"name":"main","targets":["acme/a"],"budget":{"limit_usd":2.5}},` +
			`{"name":"spare","targets":["acme/a","acme/b","acme/c"]}]}`, 0},
		"targets list --json": {`{"targets":[{"id":"acme/a"},{"id":"acme/b"},{"id":"acme/c"},{"id":"x/y"}]}`, 0},
		"keys list --json": {`{"keys":[{"name":"all","models":["*"]},{"name":"none","models":[]},{"name":"nil"},` +
			`{"name":"two","models":["main","spare"],"budget":{"limit_usd":0.25,"reset_interval":"daily"}}]}`, 0},
		"settings show --json":    {`{"timeouts":{},"budget":{"limit_usd":10}}`, 0},
		"telemetry status --json": {`{"telemetry":{"otlp_endpoint":"https://otel.example.com","header_env":{}},"unresolved":["A","B"]}`, 0},
	}
}

func TestSeeWhatExistsShapes(t *testing.T) {
	var calls []string
	answers := shapes()
	out, err := seeWhatExists(t, clone(t, "c0ffee"), cannedRunner(t, answers, &calls))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := `version: v1.2.3 running, dev in this clone
provider acme: credential unresolved
alias main: acme/a; budget 2.5 USD
alias spare: acme/a, then acme/b, then acme/c
target x/y: in no alias
key all: may call every alias
key none: may call no alias
key nil: may call no alias
key two: may call main, spare; budget 0.25 USD, resets daily
spend: capped at 10 USD
telemetry https://otel.example.com: credentials unresolved: A, B
`
	if out != want {
		t.Fatalf("out:\n%s\nwant:\n%s", out, want)
	}
	wantCalls := "version --json|providers list --json|models list --json|targets list --json|keys list --json|settings show --json|telemetry status --json"
	if got := strings.Join(calls, "|"); got != wantCalls {
		t.Fatalf("calls = %s", got)
	}
	for _, tc := range []struct{ telemetry, want string }{
		{`{"telemetry":{"otlp_endpoint":"https://otel.example.com"}}`, "telemetry https://otel.example.com: no credential"},
		{`{"telemetry":{"otlp_endpoint":"https://otel.example.com","basic_auth_env":[]}}`, "telemetry https://otel.example.com: credentials resolved"},
		{`{"telemetry":null}`, "telemetry: not set"},
	} {
		answers["telemetry status --json"] = reply{tc.telemetry, 0}
		out, err := seeWhatExists(t, clone(t, "c0ffee"), cannedRunner(t, answers, &calls))
		if err != nil || !strings.HasSuffix(out, "\n"+tc.want+"\n") {
			t.Fatalf("%s: %v\n%s", tc.telemetry, err, out)
		}
	}
}

func TestSeeWhatExistsNothingRuns(t *testing.T) {
	var calls []string
	out, err := seeWhatExists(t, clone(t, ""), cannedRunner(t, nil, &calls))
	if err != nil || out != "version: dev in this clone; no container runs (./setup.sh install starts it)\n" {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(calls) != 0 {
		t.Fatalf("the CLI was called: %q", calls)
	}
}

func TestSeeWhatExistsCleansProxyText(t *testing.T) {
	var calls []string
	answers := shapes()
	answers["models list --json"] = reply{`{"models":[{"name":"a\u001b[2Jb","targets":["acme/a"]}]}`, 0}
	answers["targets list --json"] = reply{`{"targets":[{"id":"acme/a"},{"id":"x/\u001b]52;c;aGk=\u0007"}]}`, 0}
	answers["telemetry status --json"] = reply{`{"telemetry":{"otlp_endpoint":"https://otel.example.com/\u202egpj"}}`, 0}
	out, err := seeWhatExists(t, clone(t, "c0ffee"), cannedRunner(t, answers, &calls))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, r := range out {
		if (r < 0x20 && r != '\n') || r == 0x202e {
			t.Fatalf("rune %U printed in:\n%q", r, out)
		}
	}
	hasLines(t, out, "alias a[2Jb: acme/a", "target x/]52;c;aGk=: in no alias", "telemetry https://otel.example.com/gpj: no credential")
}

func TestSeeWhatExistsStopsOnError(t *testing.T) {
	var calls []string
	answers := shapes()
	answers["keys list --json"] = reply{`{"error":{"message":"the admin API was not reached: refused"}}`, 1}
	out, err := seeWhatExists(t, clone(t, "c0ffee"), cannedRunner(t, answers, &calls))
	if err == nil || err.Error() != "stargaite keys list: the admin API was not reached: refused" {
		t.Fatalf("err = %v", err)
	}
	hasLines(t, out, "version: v1.2.3 running, dev in this clone", "provider acme: credential unresolved", "target x/y: in no alias")
	if strings.Contains(out, "key ") || strings.Contains(out, "spend") || len(calls) != 5 {
		t.Fatalf("the task went on after the error: %q\n%s", calls, out)
	}
}

// gitClone is a clone of a bare upstream, its branch main tracking
// origin/main, with ahead commits pushed to the upstream after it, so a
// fetch finds them. It returns the clone and the real git's directory.
func gitClone(t *testing.T, ahead int) (clone, gitDir string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git in this image")
	}
	root := t.TempDir()
	up, other := filepath.Join(root, "up.git"), filepath.Join(root, "other")
	clone = filepath.Join(root, "clone")
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(real, append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(n int) {
		f := "f" + strconv.Itoa(n)
		if err := os.WriteFile(filepath.Join(other, f), []byte(f+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(other, "add", f)
		git(other, "commit", "-q", "-m", f)
	}
	git(root, "init", "-q", "--bare", "-b", "main", up)
	git(root, "init", "-q", "-b", "main", other)
	commit(0)
	git(other, "remote", "add", "origin", up)
	git(other, "push", "-q", "-u", "origin", "main")
	git(root, "clone", "-q", up, clone)
	for i := 1; i <= ahead; i++ {
		commit(i)
	}
	if ahead > 0 {
		git(other, "push", "-q", "origin", "main")
	}
	return clone, filepath.Dir(real)
}

// TestSeeWhatExistsCheck: with check, the clone's upstream is fetched and
// the upgrade line said first, before what exists; a fetch refused stops
// the task before any CLI call.
func TestSeeWhatExistsCheck(t *testing.T) {
	for _, tc := range []struct {
		ahead    int
		detached bool
		first    string
	}{
		{1, false, "upgrade: 1 new commit(s) on origin/main; ./setup.sh upgrade applies them"},
		{0, false, "upgrade: none, up to date with origin/main"},
		{0, true, ""},
	} {
		dir, gitDir := gitClone(t, tc.ahead)
		if tc.detached {
			cmd := exec.Command(filepath.Join(gitDir, "git"), "checkout", "-q", "--detach", "HEAD")
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
		}
		bin := t.TempDir()
		if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\n[ \"$*\" = \"compose ps -q\" ] || exit 9\necho c0ffee\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin+":"+gitDir)
		var out bytes.Buffer
		var calls []string
		u := ui.New(plain.New(&out), map[string]string{"check": "yes"})
		_, err := SeeWhatExists(context.Background(), u, proxy.New(cannedRunner(t, shapes(), &calls)), host.New(dir))
		lines := strings.Split(out.String(), "\n")
		if tc.detached {
			if err == nil || err.Error() != "HEAD is detached, so no branch says what to upgrade to: git switch <branch>, then run this again" ||
				out.Len() != 0 || len(calls) != 0 {
				t.Fatalf("detached: err %v, out %q, calls %q", err, out.String(), calls)
			}
			continue
		}
		if err != nil || len(lines) < 2 || lines[0] != tc.first || !strings.HasPrefix(lines[1], "version: v1.2.3 running, ") || len(calls) != 7 {
			t.Fatalf("ahead %d: err %v, calls %q, out:\n%s", tc.ahead, err, calls, out.String())
		}
	}
}

// orphans is a canned store with target a in alias m and targets b and c in
// no alias.
func orphans() map[string]reply {
	return map[string]reply{
		"version --json":          {`{"version":"v1"}`, 0},
		"providers list --json":   {`{"providers":[]}`, 0},
		"models list --json":      {`{"models":[{"name":"m","targets":["a"]}]}`, 0},
		"targets list --json":     {`{"targets":[{"id":"a"},{"id":"b"},{"id":"c"}]}`, 0},
		"keys list --json":        {`{"keys":[]}`, 0},
		"settings show --json":    {`{"timeouts":{}}`, 0},
		"telemetry status --json": {`{"telemetry":null}`, 0},
	}
}

// TestSeeWhatExistsRemove: at a terminal, after its lines, See what exists
// offers to remove a target in no alias (X1), confirmed first.
func TestSeeWhatExistsRemove(t *testing.T) {
	const remove = "targets remove --json --yes -- c"
	run := func(answers map[string][]string, removed reply) (string, *scripted, []string, error) {
		t.Helper()
		var calls []string
		canned := orphans()
		canned[remove] = removed
		s := &scripted{answers: answers}
		u, px, h := ui.New(s, nil), proxy.New(cannedRunner(t, canned, &calls)), clone(t, "c0ffee")
		res, err := u.Run(context.Background(), Menu()[9].Steps, func(ctx context.Context) (string, error) { return SeeWhatExists(ctx, u, px, h) })
		var writes []string
		for _, c := range calls {
			if !strings.Contains(c, " list ") && !strings.HasPrefix(c, "version ") && !strings.HasPrefix(c, "settings show") &&
				!strings.HasPrefix(c, "telemetry status") {
				writes = append(writes, c)
			}
		}
		return res, s, writes, err
	}
	done := reply{`{"findings":[]}`, 0}

	res, s, writes, err := run(map[string][]string{"remove": {"none"}}, done)
	if err != nil || res != "" || len(writes) != 0 {
		t.Fatalf("none: %q, %v, writes %q\n%s", res, err, writes, s.output())
	}
	if !inOrder(s.output(), "target b: in no alias", "target c: in no alias") || len(s.questions) != 1 {
		t.Errorf("output:\n%s", s.output())
	}
	q := s.questions[0]
	if q.ID != "remove" || q.Step != "remove" || q.Kind != ui.KindPick || q.Prompt != "Remove a target that is in no alias?" ||
		!slices.Equal(valueLabels(q), []string{"none|keep them", "b|target b", "c|target c"}) {
		t.Errorf("pick %+v: %q", q, valueLabels(q))
	}

	res, s, writes, err = run(map[string][]string{"remove": {"c"}, "yes": {"no"}}, done)
	if err == nil || err.Error() != "stopped; nothing more was changed" || res != "" || len(writes) != 0 {
		t.Fatalf("no: %q, %v, writes %q", res, err, writes)
	}
	if !strings.Contains(s.output(), "target c is removed: no alias lists it, and its prices and its check go with it\n") {
		t.Errorf("output:\n%s", s.output())
	}

	res, s, writes, err = run(map[string][]string{"remove": {"c"}, "yes": {"yes"}}, done)
	if err != nil || res != "Done: target c removed." || !slices.Equal(writes, []string{remove}) {
		t.Fatalf("yes: %q, %v, writes %q\n%s", res, err, writes, s.output())
	}

	refused := reply{`{"error":{"code":"invalid_config","message":"refused","findings":[` +
		`{"severity":"error","code":"x","resource":"targets/c","message":"still wanted"}]}}`, 1}
	res, s, writes, err = run(map[string][]string{"remove": {"c"}, "yes": {"yes"}}, refused)
	if err == nil || res != "" || !slices.Equal(writes, []string{remove}) || !strings.HasSuffix(s.output(), "target c: still wanted\n") {
		t.Fatalf("refused: %q, %v, writes %q\n%s", res, err, writes, s.output())
	}
}

// TestSeeWhatExistsFlagsNoRemove: without a terminal (status) See what
// exists only reads: no pick and no write, targets in no alias or not.
func TestSeeWhatExistsFlagsNoRemove(t *testing.T) {
	var calls []string
	out, err := seeWhatExists(t, clone(t, "c0ffee"), cannedRunner(t, orphans(), &calls))
	if err != nil || !strings.HasSuffix(out, "target b: in no alias\ntarget c: in no alias\nkeys: none\nspend: not capped\ntelemetry: not set\n") {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(calls) != 7 {
		t.Errorf("calls %q", calls)
	}
}

func TestSeeWhatExistsDockerFails(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\necho sk-secret >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	var calls []string
	out, err := seeWhatExists(t, host.New(t.TempDir()), cannedRunner(t, nil, &calls))
	if err == nil || err.Error() != "docker compose ps failed with exit 1" || out != "" || len(calls) != 0 {
		t.Fatalf("err = %v, out %q, calls %q", err, out, calls)
	}
}

func TestMenuFresh(t *testing.T) {
	m := Menu()
	if len(m) != 23 {
		t.Fatalf("Menu() = %+v", m)
	}
	first := m[17]
	if first.Group != "" || first.Label != "First run" || first.Sub != "install" ||
		!slices.Equal(first.Flags, []string{"port", "provider", "base-url", "protocol", "api-key-env", "key-env", "model", "alias", "fallback",
			"max-output-tokens", "effort", "input-price", "output-price", "key-name", "rpm-limit"}) ||
		!slices.Equal(first.Bools, []string{"replace", "allow-private-network", "yes", "reverify"}) ||
		!slices.Equal(first.Steps, firstSteps()) || first.Run == nil || first.Preset != nil {
		t.Fatalf("Menu()[17] = %+v", first)
	}
	// This machine's tasks after See what exists, then, after the first run,
	// the subcommands that run one of them with a preset answer.
	machine := []Entry{
		{Group: "This machine", Label: "Open to the network", Sub: "remote", Flags: []string{"address", "tls", "tls-cert", "tls-key", "tls-name", "tls-ca"},
			Bools: []string{"yes"}, Steps: remoteSteps()},
		{Group: "This machine", Label: "Upgrade", Sub: "upgrade", Flags: []string{"legacy-backup"}, Bools: []string{"yes", "rebuild"},
			Steps: upgradeSteps()},
		{Group: "This machine", Label: "Back up or restore", Steps: backupSteps()},
		{Group: "This machine", Label: "See logs", Sub: "logs", Bools: []string{"follow"}, Steps: logSteps()},
		{Group: "This machine", Label: "Stop or start", Steps: stopSteps()},
		{Group: "This machine", Label: "Check everything", Steps: checkSteps()},
		{Group: "This machine", Label: "Uninstall", Sub: "uninstall", Bools: []string{"yes"}, Steps: uninstallSteps()},
		{Label: "Back up or restore", Sub: "backup", Preset: map[string]string{"what": "backup"}, Steps: backupSteps()},
		{Label: "Back up or restore", Sub: "restore", Flags: []string{"from"}, Bools: []string{"yes"}, Preset: map[string]string{"what": "restore"},
			Steps: backupSteps()},
		{Label: "Stop or start", Sub: "stop", Bools: []string{"yes"}, Preset: map[string]string{"action": "stop"}, Steps: stopSteps()},
		{Label: "Stop or start", Sub: "start", Preset: map[string]string{"action": "start"}, Steps: stopSteps()},
		{Label: "Stop or start", Sub: "restart", Bools: []string{"yes"}, Preset: map[string]string{"action": "restart"}, Steps: stopSteps()},
	}
	for i, e := range machine {
		at := 10 + i
		if i >= 7 {
			at++ // the first run is between
		}
		got := m[at]
		if got.Group != e.Group || got.Label != e.Label || got.Sub != e.Sub || !slices.Equal(got.Flags, e.Flags) ||
			!slices.Equal(got.Bools, e.Bools) || !maps.Equal(got.Preset, e.Preset) || (got.Preset == nil) != (e.Preset == nil) ||
			!slices.Equal(got.Steps, e.Steps) || got.Run == nil {
			t.Fatalf("Menu()[%d] = %+v", at, got)
		}
	}
	for steps, want := range map[string][]ui.Step{
		"backup":    {{ID: "what", Title: "back up or restore"}, {ID: "copy", Title: "which copy"}, {ID: "put", Title: "put back"}},
		"logs":      {{ID: "logs", Title: "logs"}},
		"stop":      {{ID: "action", Title: "what"}, {ID: "machine", Title: "the container"}},
		"check":     {{ID: "read", Title: "findings"}, {ID: "fix", Title: "fix"}},
		"uninstall": {{ID: "remove", Title: "uninstall"}},
		"remote": {{ID: "where", Title: "addresses"}, {ID: "cert", Title: "certificate"}, {ID: "write", Title: "files and override"},
			{ID: "recreate", Title: "recreate and check"}},
		"upgrade": {{ID: "check", Title: "check"}, {ID: "changes", Title: "what changes"}, {ID: "keep", Title: "previous image"},
			{ID: "merge", Title: "fast-forward"}, {ID: "build", Title: "build"}, {ID: "backup", Title: "backup"}, {ID: "start", Title: "start and check"}},
	} {
		got := map[string][]ui.Step{"backup": backupSteps(), "logs": logSteps(), "stop": stopSteps(), "check": checkSteps(),
			"uninstall": uninstallSteps(), "remote": remoteSteps(),
			"upgrade": upgradeSteps()}[steps]
		if !slices.Equal(got, want) {
			t.Fatalf("%s steps = %+v", steps, got)
		}
	}
	see := m[9]
	if see.Group != "This machine" || see.Label != "See what exists" || see.Sub != "status" || !slices.Equal(see.Bools, []string{"check"}) ||
		!slices.Equal(see.Steps, []ui.Step{{ID: "show", Title: "what exists"}, {ID: "remove", Title: "remove a target"}}) || see.Run == nil {
		t.Fatalf("Menu()[9] = %+v", see)
	}
	spend := []Entry{
		{Group: "Spend and traces", Label: "Cap spending", Sub: "budget", Flags: []string{"on", "limit-usd", "reset", "input-price", "output-price"},
			Bools: []string{"yes", "reverify"}, Steps: capSteps()},
		{Group: "Spend and traces", Label: "See usage and cost", Steps: usageSteps()},
		{Group: "Spend and traces", Label: "Send traces", Sub: "telemetry",
			Flags: []string{"backend", "endpoint", "langfuse-public-env", "langfuse-secret-env", "header-name", "key-env"},
			Bools: []string{"replace", "allow-private-network", "yes"}, Steps: traceSteps()},
	}
	for i, e := range spend {
		got := m[6+i]
		if got.Group != e.Group || got.Label != e.Label || got.Sub != e.Sub || !slices.Equal(got.Flags, e.Flags) ||
			!slices.Equal(got.Bools, e.Bools) || !slices.Equal(got.Steps, e.Steps) || got.Run == nil {
			t.Fatalf("Menu()[%d] = %+v", 6+i, got)
		}
	}
	for steps, want := range map[string][]ui.Step{
		"cap":   {{ID: "on", Title: "on what"}, {ID: "limit", Title: "limit and reset"}, {ID: "check", Title: "check"}},
		"usage": {{ID: "period", Title: "period"}, {ID: "by", Title: "grouped by"}, {ID: "table", Title: "table"}},
		"trace": {{ID: "which", Title: "where"}, {ID: "add", Title: "endpoint"}, {ID: "key", Title: "keys"}, {ID: "write", Title: "write"},
			{ID: "recreate", Title: "recreate"}, {ID: "test", Title: "test"}},
	} {
		got := map[string][]ui.Step{"cap": capSteps(), "usage": usageSteps(), "trace": traceSteps()}[steps]
		if !slices.Equal(got, want) {
			t.Fatalf("%s steps = %+v", steps, got)
		}
	}
	add := m[0]
	if add.Group != "Providers" || add.Label != "Add providers" || add.Sub != "provider" ||
		!slices.Equal(add.Flags, []string{"provider", "base-url", "protocol", "api-key-env", "key-env"}) ||
		!slices.Equal(add.Bools, []string{"replace", "allow-private-network", "yes"}) ||
		!slices.Equal(add.Steps, addSteps()) || add.Run == nil {
		t.Fatalf("Menu()[0] = %+v", add)
	}
	var titles []string
	for _, s := range addSteps() {
		if s.ID != s.Title {
			t.Fatalf("addSteps() = %+v", addSteps())
		}
		titles = append(titles, s.Title)
	}
	if !slices.Equal(titles, []string{"which", "add", "key", "write", "recreate"}) {
		t.Fatalf("addSteps() = %+v", addSteps())
	}
	for i, e := range []struct {
		group, label string
		steps        []ui.Step
		titles       []string
	}{
		{"Providers", "Change a provider", changeProviderSteps(), []string{"which", "change", "key", "recreate", "check"}},
		{"Models your apps call", "Make an alias", makeSteps(), []string{"name", "model", "fallbacks", "settings", "check", "make"}},
		{"Models your apps call", "Change an alias", changeAliasSteps(), []string{"which", "change", "settings", "check"}},
		{"Apps and agents", "Give an app or agent its key", giveSteps(), []string{"aliases", "name", "limits", "key", "try"}},
		{"Apps and agents", "Change a key", changeKeySteps(), []string{"which", "change", "key", "check", "try"}},
	} {
		got := m[i+1]
		if got.Group != e.group || got.Label != e.label || !slices.Equal(got.Steps, e.steps) || got.Run == nil {
			t.Fatalf("Menu()[%d] = %+v", i+1, got)
		}
		var titles []string
		for _, s := range e.steps {
			if s.ID != s.Title {
				t.Fatalf("%s steps = %+v", e.label, e.steps)
			}
			titles = append(titles, s.Title)
		}
		if !slices.Equal(titles, e.titles) {
			t.Fatalf("%s steps = %+v", e.label, e.steps)
		}
	}
	if m[1].Sub != "" || m[3].Sub != "" || m[5].Sub != "" || m[1].Flags != nil || m[3].Flags != nil || m[5].Flags != nil {
		t.Fatalf("Change a provider, Change an alias or Change a key has a subcommand: %+v, %+v, %+v", m[1], m[3], m[5])
	}
	if m[2].Sub != "alias" || !slices.Equal(m[2].Flags, []string{"provider", "base-url", "protocol", "api-key-env", "key-env", "model", "alias",
		"fallback", "max-output-tokens", "effort", "input-price", "output-price"}) ||
		!slices.Equal(m[2].Bools, []string{"replace", "allow-private-network", "yes", "reverify"}) {
		t.Fatalf("Menu()[2] = %+v", m[2])
	}
	if m[4].Sub != "key" || !slices.Equal(m[4].Flags, []string{"alias", "key-name", "rpm-limit"}) || !slices.Equal(m[4].Bools, []string{"yes"}) {
		t.Fatalf("Menu()[4] = %+v", m[4])
	}
	m[9].Label = "changed"
	m[9].Steps[0].Title = "changed"
	if again := Menu(); again[9].Label != "See what exists" || again[9].Steps[0].Title != "what exists" {
		t.Fatal("a change to Menu()'s result reached a second call")
	}
}
