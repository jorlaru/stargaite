package task

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
	"github.com/jorlaru/stargaite/internal/console/ui/plain"
)

// changeProvider runs Change a provider with u.
func (w *world) changeProvider(u *ui.UI) (string, error) {
	return u.Run(context.Background(), changeProviderSteps(), func(ctx context.Context) (string, error) {
		return ChangeProvider(ctx, u, w.proxy(), w.h)
	})
}

// changeProviderPlain runs Change a provider with the plain front, answers
// and environ.
func (w *world) changeProviderPlain(answers map[string]string, environ ...string) (string, string, error) {
	var out bytes.Buffer
	res, err := w.changeProvider(ui.New(plain.New(&out), answers).WithEnv(environ))
	return res, out.String(), err
}

// withKey adds acme on the world's model list with ANTHROPIC_API_KEY, and
// writes key to .env as that variable, after the lines already there.
func (w *world) withKey(key string) {
	w.t.Helper()
	w.listed("acme", "ANTHROPIC_API_KEY", w.models.port())
	writeFile(w.t, filepath.Join(w.h.Dir, ".env"), w.file(".env")+"ANTHROPIC_API_KEY='"+key+"'\nOTHER=1\n", 0o600)
}

func TestChangeProviderReplaceKey(t *testing.T) {
	w := newWorld(t)
	w.withKey("old")
	res, out, err := w.changeProviderPlain(map[string]string{"provider": "acme", "change": "key", "key-env": "SRC", "yes": "yes"}, "SRC=good")
	if err != nil || res != "Done: acme, key replaced, key checked." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	if got := w.models.requests(); !slices.Equal(got, []string{"Bearer old", "Bearer good"}) {
		t.Errorf("requests %q", got)
	}
	// The re-check of the current key and the check of the new key read apart.
	if old, nw := strings.Index(out, "acme refused the current key (401)\n"), strings.Index(out, "acme: key checked\n"); old < 0 || nw < old {
		t.Errorf("the two key checks are not told apart:\n%s", out)
	}
	if env := w.file(".env"); !strings.Contains(env, "\nANTHROPIC_API_KEY='good'\n") || strings.Contains(env, "old") || !strings.Contains(env, "\nOTHER=1\n") {
		t.Errorf(".env = %q", env)
	}
	if w.ups() != 1 {
		t.Errorf("%d compose ups, want 1", w.ups())
	}
	for _, key := range []string{"'old'", "good"} {
		if where := w.leaks(key, out); len(where) > 0 {
			t.Errorf("%s leaked to %q", key, where)
		}
	}
}

func TestChangeProviderReplaceRefused(t *testing.T) {
	w := newWorld(t)
	w.withKey("old")
	env := w.file(".env")
	_, out, err := w.changeProviderPlain(map[string]string{"provider": "acme", "change": "key", "key-env": "SRC", "yes": "yes"}, "SRC=bad")
	if err == nil || err.Error() != "acme refused the key 1 time(s); nothing was written for it" {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if w.file(".env") != env || w.ups() != 0 {
		t.Errorf(".env = %q, %d compose ups", w.file(".env"), w.ups())
	}
}

func TestChangeProviderTimeoutRecheck(t *testing.T) {
	answers := map[string]string{"provider": "mock", "change": "timeout", "total-seconds": "9"}
	checked := func(t *testing.T) (*world, int) {
		w, _ := checkWorld(t)
		w.target("mock/m", "--kind", "chat")
		if _, err := w.checkIn(ui.New(plain.New(&bytes.Buffer{}), nil), "mock/m"); err != nil {
			t.Fatal(err)
		}
		return w, len(w.cli())
	}
	w, before := checked(t)
	var out bytes.Buffer
	u := ui.New(plain.New(&out), answers)
	r := &run{u: u, px: w.proxy(), h: w.h}
	res, err := u.Run(context.Background(), changeProviderSteps(), r.changeProvider)
	if err != nil || res != "Done: mock, timeout 9 s." {
		t.Fatalf("%q, %v\n%s", res, err, out.String())
	}
	if got := changing(w.cli()[before:]); !slices.Equal(got, []string{"providers set --json --total-ms 9000 -- mock", verifyOf("mock/m")}) {
		t.Errorf("changing calls %q", got)
	}
	if !strings.Contains(out.String(), "1 target(s) must be checked again: 1 paid call(s)\n") {
		t.Errorf("output:\n%s", out.String())
	}
	if d, err := r.bound(context.Background(), "mock/m", 1); err != nil || d != 24*time.Second {
		t.Errorf("bound %v, %v; want 24s", d, err)
	}

	// Run again with the same value: no diff, so mock/m stays verified.
	before = len(w.cli())
	out.Reset()
	if res, err := w.changeProvider(ui.New(plain.New(&out), answers)); err != nil || res != "Done: mock, timeout 9 s." {
		t.Fatalf("again: %q, %v\n%s", res, err, out.String())
	}
	if got := changing(w.cli()[before:]); !slices.Equal(got, []string{"providers set --json --total-ms 9000 -- mock"}) ||
		strings.Contains(out.String(), "paid call") || strings.Contains(out.String(), "checked again") {
		t.Errorf("again: changing calls %q\n%s", got, out.String())
	}

	// Not now.
	w, before = checked(t)
	s := &scripted{answers: map[string][]string{"check": {"not now"}}}
	if res, err := w.changeProvider(ui.New(s, answers)); err != nil || res != "Done: mock, timeout 9 s." {
		t.Fatalf("not now: %q, %v\n%s", res, err, s.output())
	}
	if got := changing(w.cli()[before:]); indexOf(got, "targets verify ") >= 0 || !strings.Contains(s.output(), "1 target(s) left unchecked: mock/m\n") {
		t.Errorf("not now: changing calls %q\n%s", got, s.output())
	}

	// Esc at the paid step: the timeout set stays, and both are said.
	w, before = checked(t)
	s = &scripted{answers: map[string][]string{"check": {leave}}}
	if _, err := w.changeProvider(ui.New(s, answers)); !errors.Is(err, ui.ErrLeave) {
		t.Fatalf("esc: %v\n%s", err, s.output())
	}
	if got := changing(w.cli()[before:]); indexOf(got, "targets verify ") >= 0 ||
		!strings.HasSuffix(s.output(), "1 target(s) left unchecked: mock/m\nalready changed and kept: mock: timeout 9 s\n") {
		t.Errorf("esc: changing calls %q\n%s", got, s.output())
	}

	// Two targets, the second's re-check fails: only it is left unchecked.
	w, _ = checkWorld(t)
	w.target("mock/a", "--kind", "chat")
	w.target("mock/b", "--kind", "chat")
	if _, err := w.checkIn(ui.New(plain.New(&bytes.Buffer{}), nil), "mock/a", "mock/b"); err != nil {
		t.Fatal(err)
	}
	w.override(map[string][]reply{verifyOf("mock/b"): {{`{"target":"mock/b","ok":false,"stored":false,"calls":[{"effort":"","status":404}]}`, 1}}})
	out.Reset()
	if _, err := w.changeProvider(ui.New(plain.New(&out), answers)); err == nil ||
		!strings.HasSuffix(out.String(), "1 target(s) left unchecked: mock/b\nalready changed and kept: mock: timeout 9 s\n") {
		t.Errorf("two targets: %v\n%s", err, out.String())
	}
}

func TestChangeProviderRemoveRefusedSeveral(t *testing.T) {
	w := removeWorld(t)
	w.must("models", "add", "b", "--target", "acme/m1")
	w.must("models", "add", "c", "--target", "acme/m2")
	_, out, err := w.changeProviderPlain(map[string]string{"provider": "acme", "change": "remove", "yes": "yes"})
	want := "acme cannot be removed: aliases b, c have no other target; give each a target of another provider, or remove them, first"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if got := changing(w.cli()); len(got) != 0 {
		t.Errorf("changing calls %q", got)
	}
}

func TestChangeProviderTimeoutDefault(t *testing.T) {
	w, _ := checkWorld(t)
	w.must("providers", "set", "--total-ms", "9000", "--", "mock")
	res, out, err := w.changeProviderPlain(map[string]string{"provider": "mock", "change": "timeout", "total-seconds": ""})
	if err != nil || res != "Done: mock, timeout the default." || !strings.Contains(out, "setting the timeout of mock to the default\n") {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	var p proxy.Provider
	w.decoded(&p, "providers", "show", "mock")
	if !slices.Contains(w.cli(), "providers set --json --total-ms= -- mock") || p.Timeouts.TotalMS != 0 {
		t.Errorf("CLI calls %q, mock %+v", w.cli(), p)
	}
}

func TestChangeProviderPlan(t *testing.T) {
	w, _ := checkWorld(t)
	var p struct {
		Billing string            `json:"billing"`
		Quota   []json.RawMessage `json:"quota"`
	}
	res, out, err := w.changeProviderPlain(map[string]string{"provider": "mock", "change": "plan", "billing": "subscription",
		"quota": "5h:requests=1000 7d:tokens=5000000"})
	if err != nil || res != "Done: mock, billing subscription." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	w.decoded(&p, "providers", "show", "mock")
	if !slices.Contains(w.cli(), "providers set --json --billing subscription --quota 5h:requests=1000 --quota 7d:tokens=5000000 -- mock") ||
		p.Billing != "subscription" || len(p.Quota) != 2 {
		t.Fatalf("CLI calls %q, mock %+v", w.cli(), p)
	}
	res, out, err = w.changeProviderPlain(map[string]string{"provider": "mock", "change": "plan", "billing": "pay_per_use"})
	if err != nil || res != "Done: mock, billing pay_per_use." {
		t.Fatalf("pay per use: %q, %v\n%s", res, err, out)
	}
	p.Quota = nil
	w.decoded(&p, "providers", "show", "mock")
	if !slices.Contains(w.cli(), "providers set --json --billing pay_per_use --quota= -- mock") || p.Billing != "pay_per_use" || len(p.Quota) != 0 {
		t.Errorf("pay per use: CLI calls %q, mock %+v", w.cli(), p)
	}
}

// removeWorld is checkWorld with acme, its key in .env, its targets acme/m1
// and acme/m2, and mock/m.
func removeWorld(t *testing.T) *world {
	t.Helper()
	w, _ := checkWorld(t)
	w.withKey("k")
	for _, id := range []string{"acme/m1", "acme/m2", "mock/m"} {
		w.target(id, "--kind", "chat")
	}
	return w
}

func TestChangeProviderRemove(t *testing.T) {
	w := removeWorld(t)
	w.must("models", "add", "a", "--target", "acme/m1", "--target", "mock/m")
	env := w.file(".env")
	res, out, err := w.changeProviderPlain(map[string]string{"provider": "acme", "change": "remove", "yes": "yes"})
	if err != nil || res != "Done: acme removed." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	want := []string{"models set --json --target mock/m -- a", "targets remove --json --yes -- acme/m1", "targets remove --json --yes -- acme/m2",
		"providers remove --json --yes -- acme"}
	if got := changing(w.cli()); !slices.Equal(got, want) {
		t.Errorf("changing calls:\n%q\nwant:\n%q", got, want)
	}
	if !strings.Contains(out, "removing acme: takes its targets out of a; removes the targets acme/m1, acme/m2 and the provider; "+
		"removes ANTHROPIC_API_KEY from .env and recreates the container\n") {
		t.Errorf("output:\n%s", out)
	}
	if got := w.file(".env"); got != strings.Replace(env, "ANTHROPIC_API_KEY='k'\n", "", 1) || w.ups() != 1 {
		t.Errorf(".env = %q, %d compose ups", got, w.ups())
	}
}

func TestChangeProviderRemoveFails(t *testing.T) {
	w := newWorld(t)
	listed := `{"providers":[` + acmeListed("unresolved") + `,{"name":"mock","credential":"none"}]}`
	w.canned(map[string]reply{
		"providers list --json":                  {listed, 0},
		"targets list --json":                    {`{"targets":[{"id":"acme/m1"},{"id":"acme/m2"},{"id":"mock/m"}]}`, 0},
		"models list --json":                     {`{"models":[{"name":"a","targets":["acme/m1","mock/m"]}]}`, 0},
		"models set --json --target mock/m -- a": {`{"findings":[]}`, 0},
		"targets remove --json --yes -- acme/m1": {`{"findings":[]}`, 0},
		"targets remove --json --yes -- acme/m2": {`{"findings":[]}`, 0},
		"providers remove --json --yes -- acme":  {`{"error":{"message":"refused"}}`, 1},
	})
	files := w.files()
	_, out, err := w.changeProviderPlain(map[string]string{"provider": "acme", "change": "remove", "yes": "yes"})
	if err == nil || err.Error() != "stargaite providers remove: refused" {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if !strings.HasSuffix(out, "already changed and kept: alias a now toward mock/m; target acme/m1 removed; target acme/m2 removed\n") {
		t.Errorf("output:\n%s", out)
	}
	if !maps.Equal(files, w.files()) || w.ups() != 0 {
		t.Errorf("%d compose ups, files %q", w.ups(), w.files())
	}
}

func TestChangeProviderRemoveRefused(t *testing.T) {
	w := removeWorld(t)
	w.must("models", "add", "b", "--target", "acme/m1")
	files := w.files()
	_, out, err := w.changeProviderPlain(map[string]string{"provider": "acme", "change": "remove", "yes": "yes"})
	want := "acme cannot be removed: alias b has no other target; give it a target of another provider, or remove the alias, first"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if got := changing(w.cli()); len(got) != 0 || !maps.Equal(files, w.files()) || w.ups() != 0 {
		t.Errorf("changing calls %q, %d compose ups", got, w.ups())
	}
}

func TestChangeProviderRemoveSharedVariable(t *testing.T) {
	w := newWorld(t)
	w.withKey("k")
	w.listed("acme2", "ANTHROPIC_API_KEY", w.models.port())
	env := w.file(".env")
	res, out, err := w.changeProviderPlain(map[string]string{"provider": "acme", "change": "remove", "yes": "yes"})
	if err != nil || res != "Done: acme removed." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	if !strings.Contains(out, "removing acme: removes the provider\n") || strings.Contains(out, ".env") || w.file(".env") != env || w.ups() != 0 {
		t.Errorf(".env = %q, %d compose ups, output:\n%s", w.file(".env"), w.ups(), out)
	}
}

func TestChangeProviderRemoveTelemetryVariable(t *testing.T) {
	w := newWorld(t)
	w.withKey("k")
	w.must("telemetry", "set", "--otlp-endpoint", "http://127.0.0.1:9", "--allow-private-network", "--header-env", "X-Key=ANTHROPIC_API_KEY")
	env := w.file(".env")
	res, out, err := w.changeProviderPlain(map[string]string{"provider": "acme", "change": "remove", "yes": "yes"})
	if err != nil || res != "Done: acme removed." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	if strings.Contains(out, ".env") || w.file(".env") != env || w.ups() != 0 {
		t.Errorf(".env = %q, %d compose ups, output:\n%s", w.file(".env"), w.ups(), out)
	}
}

func TestChangeProviderEsc(t *testing.T) {
	w := newWorld(t)
	w.withKey("k")
	w.models.stall = func() {}
	files := w.files()
	s := &scripted{answers: map[string][]string{"provider": {"acme"}},
		leaveWork: func(q ui.Question) bool { return strings.HasPrefix(q.Prompt, "checking the key of acme") }}
	done := make(chan error, 1)
	go func() { _, err := w.changeProvider(ui.New(s, nil)); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, ui.ErrLeave) {
			t.Fatalf("err = %v\n%s", err, s.output())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Esc did not stop the free re-check within 2 s")
	}
	if got := changing(w.cli()); len(got) != 0 || !maps.Equal(files, w.files()) || w.ups() != 0 {
		t.Errorf("changing calls %q, %d compose ups", got, w.ups())
	}
}
