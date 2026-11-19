package task

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
	"github.com/jorlaru/stargaite/internal/console/ui/plain"
	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// changeAlias runs Change an alias with u.
func (w *world) changeAlias(u *ui.UI) (string, error) {
	return u.Run(context.Background(), changeAliasSteps(), func(ctx context.Context) (string, error) {
		return ChangeAlias(ctx, u, w.proxy(), w.h)
	})
}

// changeAliasPlain runs Change an alias with the plain front and answers.
func (w *world) changeAliasPlain(answers map[string]string) (string, string, error) {
	var out bytes.Buffer
	res, err := w.changeAlias(ui.New(plain.New(&out), answers))
	return res, out.String(), err
}

// aliasWorld is checkWorld with mock2 on the same mock, and alias a toward
// targets, made through the CLI, the first checked; it returns the number
// of CLI calls made so far.
func aliasWorld(t *testing.T, targets ...string) (*world, *mockprovider.Server, int) {
	t.Helper()
	w, mock := checkWorld(t)
	w.onMock("mock2", "--protocol", "openai", "--billing", "none")
	args := []string{"models", "add", "a"}
	for _, id := range targets {
		w.target(id, "--kind", "chat")
		args = append(args, "--target", id)
	}
	if _, err := w.checkIn(ui.New(plain.New(&bytes.Buffer{}), nil), targets[0]); err != nil {
		t.Fatal(err)
	}
	w.must(args...)
	return w, mock, len(w.cli())
}

// alias is alias n as the CLI shows it.
func (w *world) alias(n string) proxy.Model {
	w.t.Helper()
	var m proxy.Model
	w.decoded(&m, "models", "show", n)
	return m
}

func TestChangeAliasAddFallback(t *testing.T) {
	w, mock, before := aliasWorld(t, "mock/m")
	calls := len(mock.Calls())
	answers := map[string]string{"alias": "a", "change": "add", "fallback-provider": "mock2", "fallback-model": "m"}
	res, out, err := w.changeAliasPlain(answers)
	if err != nil || res != "Done: alias a toward mock/m, mock2/m." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	want := []string{"models list --json", "providers list --json", "models show --json -- a", "targets list --json", "apply --json --dry-run -f -",
		"targets add --json --kind chat -- mock2/m", "settings show --json", verifyOf("mock2/m"), "models set --json --target mock/m --target mock2/m -- a"}
	if got := w.cli()[before:]; !slices.Equal(got, want) {
		t.Errorf("CLI calls:\n%q\nwant:\n%q", got, want)
	}
	if !strings.Contains(out, "target mock/m is verified; not checked again (--reverify asks for it)\n") ||
		!strings.Contains(out, "alias a now toward mock/m, mock2/m\n") || len(mock.Calls()) != calls+1 {
		t.Errorf("%d mock calls, output:\n%s", len(mock.Calls())-calls, out)
	}

	// Not now at the paid step: the target added is kept, the alias unchanged.
	w, _, before = aliasWorld(t, "mock/m")
	s := &scripted{answers: map[string][]string{"check": {"not now"}}}
	res, err = w.changeAlias(ui.New(s, answers))
	if err != nil || res != "Not checked: alias a was not changed." {
		t.Fatalf("not now: %q, %v\n%s", res, err, s.output())
	}
	if got := w.cli()[before:]; indexOf(got, "targets verify ") >= 0 || indexOf(got, "models set ") >= 0 ||
		!strings.Contains(s.output(), "already changed and kept: target mock2/m added\n") {
		t.Errorf("not now: CLI calls %q\n%s", got, s.output())
	}
	if a := w.alias("a"); !slices.Equal(a.Targets, []string{"mock/m"}) {
		t.Errorf("not now: a toward %q", a.Targets)
	}

	// A target the alias already has.
	w, _, before = aliasWorld(t, "mock/m")
	res, out, err = w.changeAliasPlain(map[string]string{"alias": "a", "change": "add", "fallback-provider": "mock", "fallback-model": "m"})
	if err != nil || res != "alias a already toward mock/m; kept" || !strings.Contains(out, "mock/m is already a target of a; kept\n") {
		t.Fatalf("already: %q, %v\n%s", res, err, out)
	}
	if got := w.cli()[before:]; !slices.Equal(got, []string{"models list --json", "providers list --json"}) {
		t.Errorf("already: CLI calls %q", got)
	}
}

func TestMakeAliasExtendFlags(t *testing.T) {
	w, mock := checkWorld(t)
	w.onMock("mock2", "--protocol", "openai", "--billing", "none")
	if _, out, err := w.makePlain(map[string]string{"provider": "mock", "model": "m", "alias": "local"}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	before, calls := len(w.cli()), len(mock.Calls())
	res, out, err := w.makePlain(map[string]string{"alias": "local", "fallback": "mock2/m"})
	if err != nil || res != "Done: alias local toward mock/m, mock2/m." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	got := w.cli()[before:]
	if v := slices.DeleteFunc(slices.Clone(got), func(a string) bool { return !strings.HasPrefix(a, "targets verify ") }); !slices.Equal(v, []string{verifyOf("mock2/m")}) ||
		!slices.Contains(got, "models set --json --target mock/m --target mock2/m -- local") || len(mock.Calls()) != calls+1 {
		t.Errorf("CLI calls %q, %d mock calls", got, len(mock.Calls())-calls)
	}
	if !strings.Contains(out, "1 paid call(s) in all\n") || strings.Contains(out, "2 paid call(s) in all") {
		t.Errorf("output:\n%s", out)
	}
}

func TestChangeAliasAddKeepsSettings(t *testing.T) {
	w, _ := checkWorld(t)
	w.onMock("mock2", "--protocol", "openai", "--billing", "none")
	w.target("mock/m", "--kind", "chat")
	w.must("models", "add", "a", "--target", "mock/m", "--budget", "limit_usd=2,reset_interval=never", "--effort", "low",
		"--effort-mode", "default", "--max-output-tokens", "100")
	res, out, err := w.changeAliasPlain(map[string]string{"alias": "a", "change": "add", "fallback-provider": "mock2", "fallback-model": "m"})
	if err != nil || res != "Done: alias a toward mock/m, mock2/m." || strings.Contains(out, "prices are asked") {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	a := w.alias("a")
	if !slices.Equal(a.Targets, []string{"mock/m", "mock2/m"}) || a.Budget == nil || a.Budget.LimitUSD != 2 || a.Reasoning.Effort != "low" ||
		a.MaxOutputTokens != 100 {
		t.Errorf("a = %+v", a)
	}
}

func TestChangeAliasDrop(t *testing.T) {
	w, _, before := aliasWorld(t, "mock/m", "mock2/m")
	res, out, err := w.changeAliasPlain(map[string]string{"alias": "a", "change": "drop", "drop": "mock2/m"})
	if err != nil || res != "Done: alias a toward mock/m." || !strings.Contains(out, "alias a now toward mock/m\n") {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	if got := changing(w.cli()[before:]); !slices.Equal(got, []string{"models set --json --target mock/m -- a"}) {
		t.Errorf("changing calls %q", got)
	}
}

func TestChangeAliasReorder(t *testing.T) {
	w, _, before := aliasWorld(t, "mock/m", "mock2/m")
	res, out, err := w.changeAliasPlain(map[string]string{"alias": "a", "change": "order", "move": "mock2/m", "place": "1"})
	if err != nil || res != "Done: alias a toward mock2/m, mock/m." || !strings.Contains(out, "alias a now toward mock2/m, mock/m\n") {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	if got := changing(w.cli()[before:]); !slices.Equal(got, []string{"models set --json --target mock2/m --target mock/m -- a"}) {
		t.Errorf("changing calls %q", got)
	}
}

func TestChangeAliasEffort(t *testing.T) {
	w, _, before := aliasWorld(t, "mock/m")
	res, out, err := w.changeAliasPlain(map[string]string{"alias": "a", "change": "effort", "effort": "low"})
	if err != nil || res != "Done: alias a with effort low." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	got := w.cli()[before:]
	set := slices.Index(got, "models set --json --effort low --effort-mode default -- a")
	if set < 0 || slices.Index(got, "models explain --json -- a") != set+1 || !strings.Contains(out, "mock/m: sends effort") {
		t.Errorf("CLI calls %q\n%s", got, out)
	}
	before = len(w.cli())
	s := &scripted{answers: map[string][]string{"effort": {""}}}
	res, err = w.changeAlias(ui.New(s, map[string]string{"alias": "a", "change": "effort"}))
	if err != nil || res != "Done: alias a with no default effort." {
		t.Fatalf("clear: %q, %v\n%s", res, err, s.output())
	}
	if len(s.asked) != 1 || !strings.Contains(s.asked[0], "(now low)") ||
		!slices.Contains(w.cli()[before:], "models set --json --effort= --effort-mode= -- a") || w.alias("a").Reasoning.Effort != "" {
		t.Errorf("clear: asked %q, CLI calls %q", s.asked, w.cli()[before:])
	}

	// models explain failing after models set: the effort set is said kept.
	w, _, _ = aliasWorld(t, "mock/m")
	w.override(map[string][]reply{"models explain --json -- a": {{`{"error":{"message":"explain broke"}}`, 1}}})
	_, out, err = w.changeAliasPlain(map[string]string{"alias": "a", "change": "effort", "effort": "low"})
	if err == nil || !strings.HasSuffix(out, "already changed and kept: alias a with effort low\n") {
		t.Errorf("explain fails: %v\n%s", err, out)
	}
}

func TestChangeAliasCap(t *testing.T) {
	w, _, before := aliasWorld(t, "mock/m")
	res, out, err := w.changeAliasPlain(map[string]string{"alias": "a", "change": "cap", "max-output-tokens": "100"})
	if err != nil || res != "Done: alias a with output cap 100." || !slices.Contains(w.cli()[before:], "models set --json --max-output-tokens 100 -- a") {
		t.Fatalf("%q, %v, CLI calls %q\n%s", res, err, w.cli()[before:], out)
	}
	before = len(w.cli())
	s := &scripted{answers: map[string][]string{"max-output-tokens": {"lots", ""}}}
	res, err = w.changeAlias(ui.New(s, map[string]string{"alias": "a", "change": "cap"}))
	if err != nil || res != "Done: alias a with no output cap." {
		t.Fatalf("clear: %q, %v\n%s", res, err, s.output())
	}
	if len(s.asked) != 2 || !strings.Contains(s.asked[0], "(now 100)") || !strings.Contains(s.output(), "a whole number above 0 is needed\n") ||
		!slices.Contains(w.cli()[before:], "models set --json --max-output-tokens= -- a") || w.alias("a").MaxOutputTokens != 0 {
		t.Errorf("clear: asked %q, CLI calls %q\n%s", s.asked, w.cli()[before:], s.output())
	}

	// A leading zero is sent as the number it is.
	before = len(w.cli())
	if res, out, err := w.changeAliasPlain(map[string]string{"alias": "a", "change": "cap", "max-output-tokens": "0100"}); err != nil ||
		res != "Done: alias a with output cap 100." || !slices.Contains(w.cli()[before:], "models set --json --max-output-tokens 100 -- a") {
		t.Fatalf("0100: %q, %v, CLI calls %q\n%s", res, err, w.cli()[before:], out)
	}
}

func TestChangeAliasRemove(t *testing.T) {
	w, _, before := aliasWorld(t, "mock/m")
	_, out, err := w.changeAliasPlain(map[string]string{"alias": "a", "change": "remove"})
	var usage *ui.UsageError
	if !errors.As(err, &usage) || err.Error() != "missing --yes: removing alias a: clients that call it get an error" {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if len(changing(w.cli()[before:])) != 0 || w.alias("a").Name != "a" {
		t.Errorf("CLI calls %q", w.cli()[before:])
	}
	res, out, err := w.changeAliasPlain(map[string]string{"alias": "a", "change": "remove", "yes": "yes"})
	if err != nil || res != "Done: alias a removed." || !slices.Equal(changing(w.cli()[before:]), []string{"models remove --json --yes -- a"}) {
		t.Fatalf("%q, %v, CLI calls %q\n%s", res, err, w.cli()[before:], out)
	}

	// A key that still names the alias is said, once; a key that already
	// named another missing alias is not.
	w, _, _ = aliasWorld(t, "mock/m")
	w.must("keys", "create", "k1", "--model", "a", "--model", "zz")
	w.must("keys", "create", "k2", "--model", "zz")
	if _, out, err := w.changeAliasPlain(map[string]string{"alias": "a", "change": "remove", "yes": "yes"}); err != nil ||
		!strings.HasSuffix(out, "keys/k1 names an alias that does not exist: its calls to that alias get an error: Change a key sets the aliases it may call\n") ||
		strings.Count(out, "names an alias") != 1 {
		t.Errorf("key: %v\n%s", err, out)
	}
}

// TestExtendUnlisted: extend reads the alias from the models listed; an alias
// not listed is an error, never an alias toward the new targets alone.
func TestExtendUnlisted(t *testing.T) {
	w, _, _ := aliasWorld(t, "mock/m")
	before := len(w.cli())
	_, err := w.inAlias(ui.New(plain.New(&bytes.Buffer{}), nil), func(ctx context.Context, r *run) (string, error) {
		if err := r.listProviders(ctx); err != nil {
			return "", err
		}
		return r.extend(ctx, "a", []string{"mock2/m"})
	})
	if err == nil || err.Error() != "alias a is not listed" || len(changing(w.cli()[before:])) != 0 {
		t.Fatalf("err = %v, CLI calls %q", err, w.cli()[before:])
	}
}

func TestChangeAliasOptions(t *testing.T) {
	values := func(targets ...string) []string {
		w, _, _ := aliasWorld(t, targets...)
		s := &scripted{answers: map[string][]string{"alias": {"a"}, "change": {"explain"}}}
		if res, err := w.changeAlias(ui.New(s, nil)); err != nil || res != "" {
			t.Fatalf("%q, %v\n%s", res, err, s.output())
		}
		var out []string
		for _, q := range s.questions {
			if q.ID == "change" {
				for _, o := range q.Options {
					out = append(out, o.Value)
				}
			}
		}
		return out
	}
	if got := values("mock/m", "mock/n", "mock/o"); !slices.Equal(got, []string{"drop", "order", "effort", "cap", "explain", "remove"}) {
		t.Errorf("3 targets: %q", got)
	}
	if got := values("mock/m"); !slices.Equal(got, []string{"add", "effort", "cap", "explain", "remove"}) {
		t.Errorf("1 target: %q", got)
	}
}

func TestChangeAliasNoAlias(t *testing.T) {
	w := newWorld(t)
	res, out, err := w.changeAliasPlain(nil)
	if err != nil || res != "No alias yet: Make an alias makes one." || !slices.Equal(w.cli(), []string{"models list --json"}) {
		t.Fatalf("%q, %v, CLI calls %q\n%s", res, err, w.cli(), out)
	}
}
