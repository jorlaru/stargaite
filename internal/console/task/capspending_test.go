package task

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
	"github.com/jorlaru/stargaite/internal/console/ui/plain"
)

// capSpending runs Cap spending with u.
func (w *world) capSpending(u *ui.UI) (string, error) {
	return u.Run(context.Background(), capSteps(), func(ctx context.Context) (string, error) {
		return CapSpending(ctx, u, w.proxy(), w.h)
	})
}

// capPlain runs Cap spending with the plain front and answers; it returns
// the result, the output and the error.
func (w *world) capPlain(answers map[string]string) (string, string, error) {
	var out bytes.Buffer
	res, err := w.capSpending(ui.New(plain.New(&out), answers))
	return res, out.String(), err
}

// capAt runs Cap spending with the scripted answers.
func (w *world) capAt(answers map[string][]string) (string, *scripted, error) {
	s := &scripted{answers: answers}
	res, err := w.capSpending(ui.New(s, nil))
	return res, s, err
}

// budgets is the instance's, alias a's and key k's budgets, as read back.
func (w *world) budgets() (instance, alias, key *proxy.Budget) {
	w.t.Helper()
	var s proxy.SettingsReply
	w.decoded(&s, "settings", "show")
	var m proxy.Model
	w.decoded(&m, "models", "show", "a")
	k, _ := w.listedKey("k")
	return s.Budget, m.Budget, k.Budget
}

func TestParseScope(t *testing.T) {
	for a, want := range map[string]scope{"instance": {kind: "instance"}, "alias:a": {"alias", "a"}, "key:k": {"key", "k"}, "key:a:b": {"key", "a:b"}} {
		if got, err := parseScope(a); err != nil || got != want {
			t.Errorf("parseScope(%q) = %+v, %v; want %+v", a, got, err, want)
		}
	}
	for _, a := range []string{"alias", "alias:", "key:", "everything", ""} {
		if _, err := parseScope(a); err == nil || err.Error() != "instance, alias:<name> or key:<name> is needed" {
			t.Errorf("parseScope(%q) = %v", a, err)
		}
	}
	for s, want := range map[scope]string{{kind: "instance"}: "the instance", {"alias", "a"}: "alias a", {"key", "k"}: "key k"} {
		if s.String() != want {
			t.Errorf("%+v.String() = %q; want %q", s, s.String(), want)
		}
	}
}

func TestCapSpendingFlags(t *testing.T) {
	for _, row := range []struct {
		answers      map[string]string
		change, res  string
		said         string
		which        int // 0 the instance, 1 alias a, 2 key k
		limit, reset string
	}{
		{map[string]string{"on": "instance", "limit-usd": "20", "reset": "daily"}, "settings set --json --budget limit_usd=20,reset_interval=daily",
			"Done: the instance, budget 20 USD, reset daily.", "a budget of 20 USD is set on the instance, reset daily", 0, "20", "daily"},
		{map[string]string{"on": "alias:a", "limit-usd": "5"}, "models set --json --budget limit_usd=5,reset_interval=monthly -- a",
			"Done: alias a, budget 5 USD, reset monthly.", "a budget of 5 USD is set on alias a, reset monthly", 1, "5", "monthly"},
		{map[string]string{"on": "key:k", "limit-usd": "0.5", "reset": "never"}, "keys set --json --budget limit_usd=0.5,reset_interval=never -- k",
			"Done: key k, budget 0.5 USD, reset never.", "a budget of 0.5 USD is set on key k, reset never", 2, "0.5", "never"},
	} {
		w, _, _ := keyWorld(t, "mock/m")
		w.newKey("k", "--model", "a", "--model", "gone")
		res, out, err := w.capPlain(row.answers)
		if err != nil || res != row.res {
			t.Fatalf("%v: %q, %v\n%s", row.answers, res, err, out)
		}
		hasLines(t, out, row.said)
		// A finding on the capped key itself is said; one on another resource is not.
		const unknown = "key k names an alias that does not exist: its calls to that alias get an error: Change a key sets the aliases it may call\n"
		if n := strings.Count(out, unknown); (row.which == 2) != (n == 1) || n > 1 {
			t.Errorf("%v: the key's own finding said %d time(s)\n%s", row.answers, n, out)
		}
		if got := changing(w.cli()); !slices.Equal(got, []string{row.change}) {
			t.Errorf("%v: changes %q", row.answers, got)
		}
		i, a, k := w.budgets()
		got := []*proxy.Budget{i, a, k}
		for j, b := range got {
			switch {
			case j == row.which && (b == nil || usdText(b.LimitUSD) != row.limit || b.ResetInterval != row.reset):
				t.Errorf("%v: budget %d read back %+v", row.answers, j, b)
			case j != row.which && b != nil:
				t.Errorf("%v: budget %d set too: %+v", row.answers, j, b)
			}
		}
	}
}

func TestCapSpendingReplace(t *testing.T) {
	w, _, _ := keyWorld(t, "mock/m")
	w.must("models", "set", "--budget", "limit_usd=5,reset_interval=monthly", "--", "a")
	_, out, err := w.capPlain(map[string]string{"on": "alias:a", "limit-usd": "7"})
	var usage *ui.UsageError
	if !errors.As(err, &usage) || usage.Msg != "missing --yes: the budget of alias a, 5 USD, resets monthly, is replaced by 7 USD, reset monthly" ||
		len(changing(w.cli())) != 0 {
		t.Fatalf("without yes: %v, changes %q\n%s", err, changing(w.cli()), out)
	}
	w, _, _ = keyWorld(t, "mock/m")
	w.must("models", "set", "--budget", "limit_usd=5,reset_interval=monthly", "--", "a")
	res, out, err := w.capPlain(map[string]string{"on": "alias:a", "limit-usd": "7", "yes": "yes"})
	if err != nil || res != "Done: alias a, budget 7 USD, reset monthly." ||
		!slices.Equal(changing(w.cli()), []string{"models set --json --budget limit_usd=7,reset_interval=monthly -- a"}) {
		t.Fatalf("with yes: %q, %v, changes %q\n%s", res, err, changing(w.cli()), out)
	}
}

func TestCapSpendingPricesNoPaid(t *testing.T) {
	for _, reverify := range []bool{false, true} {
		w, mock := budgetWorld(t)
		w.must("models", "add", "a", "--target", "acme/m1")
		chats, before := len(mock.Calls()), len(w.cli()) // budgetWorld's own check
		answers := map[string]string{"on": "alias:a", "limit-usd": "5", "input-price": "1.5", "output-price": "6"}
		if reverify {
			answers["reverify"] = "yes"
		}
		res, out, err := w.capPlain(answers)
		if err != nil || res != "Done: alias a, budget 5 USD, reset monthly." {
			t.Fatalf("reverify %v: %q, %v\n%s", reverify, res, err, out)
		}
		today := time.Now().Format(time.DateOnly)
		set := "models set --json --budget limit_usd=5,reset_interval=monthly -- a"
		want := []string{set, "targets set --json --pricing input_per_million=1.5,output_per_million=6,as_of=" + today + " -- acme/m1", set}
		if reverify {
			want = append(want, verifyOf("acme/m1"))
		}
		if got := changing(w.cli()[before:]); !slices.Equal(got, want) {
			t.Errorf("reverify %v: changes %q\nwant %q", reverify, got, want)
		}
		hasLines(t, out, "acme/m1 has no prices and a budget reaches it: its prices are asked",
			"setting the prices of acme/m1 to 1.5 and 6 USD per million input and output tokens, as of "+today+", which clears its verification")
		switch {
		case !reverify:
			hasLines(t, out, "1 target(s) must be checked again: 1 paid call(s) (--reverify asks for it)")
			if n := len(mock.Calls()) - chats; n != 0 || len(w.verifies()) != 1 {
				t.Errorf("paid without --reverify: %d chat(s), verifies %q", n, w.verifies())
			}
		case !strings.Contains(out, "check acme/m1: 1 paid call(s) to acme") || len(w.verifies()) != 2:
			t.Errorf("with --reverify: verifies %q\n%s", w.verifies(), out)
		}
	}
}

func TestCapSpendingUsage(t *testing.T) {
	models, set, targets := "models list --json", "models set --json --budget limit_usd=5,reset_interval=monthly -- a", "targets list --json"
	for _, row := range []struct {
		priced  bool // budgetWorld, alias a toward acme/m1
		answers map[string]string
		msg     string
		cli     []string
	}{
		{false, map[string]string{}, "missing --on: what the budget is on: instance, alias:<name> or key:<name>", nil},
		{false, map[string]string{"on": "alias"}, "--on: instance, alias:<name> or key:<name> is needed", nil},
		{false, map[string]string{"on": "alias:a"}, "missing --limit-usd: the limit in USD", []string{models}},
		{false, map[string]string{"on": "alias:a", "limit-usd": "5", "reset": "weekly"}, "--reset: weekly is not one of monthly, daily, never", []string{models}},
		{false, map[string]string{"on": "alias:a", "limit-usd": "5,reset_interval=never"}, "--limit-usd: a number of USD is needed", []string{models}},
		{true, map[string]string{"on": "alias:a", "limit-usd": "5"}, "missing --input-price: the price of acme/m1 in USD per million input tokens",
			[]string{models, set, targets}},
		{true, map[string]string{"on": "alias:a", "limit-usd": "5", "input-price": "1,as_of=2020-01-01"},
			"--input-price: --input-price and --output-price take a number of USD", []string{models, set, targets}},
	} {
		w, _, _ := keyWorld(t, "mock/m")
		if row.priced {
			w, _ = budgetWorld(t)
			w.must("models", "add", "a", "--target", "acme/m1")
		}
		before := len(w.cli()) // budgetWorld's own check
		_, out, err := w.capPlain(row.answers)
		var usage *ui.UsageError
		if !errors.As(err, &usage) || usage.Msg != row.msg {
			t.Errorf("%v: %v; want %q\n%s", row.answers, err, row.msg, out)
		}
		if got := w.cli()[before:]; !slices.Equal(got, row.cli) {
			t.Errorf("%v: CLI calls %q; want %q", row.answers, got, row.cli)
		}
	}
}

func TestCapSpendingCostReportOnly(t *testing.T) {
	const line = "target rep/m1 has no prices: its cost is the provider's own report, and a call without one counts nothing"
	const orphan = "target rep/orphan has no prices" // in no alias, so no budget reaches it
	for _, row := range []struct {
		on   string
		says bool
	}{{"alias:a", true}, {"alias:b", false}, {"instance", true}} {
		w, _, _ := keyWorld(t)
		w.onMock("rep", "--protocol", "openai", "--billing", "pay_per_use", "--cost-report", "path=usage.cost,usd_per_unit=1")
		w.target("rep/m1", "--kind", "chat")
		w.target("rep/orphan", "--kind", "chat")
		w.onMock("acme", "--protocol", "openai", "--billing", "pay_per_use")
		w.target("acme/m1", "--kind", "chat", "--pricing", "input_per_million=1,output_per_million=1,as_of=2026-11-01")
		w.must("models", "add", "a", "--target", "rep/m1")
		w.must("models", "add", "b", "--target", "acme/m1")
		res, out, err := w.capPlain(map[string]string{"on": row.on, "limit-usd": "5"})
		if err != nil || !strings.HasPrefix(res, "Done: ") {
			t.Fatalf("%s: %q, %v\n%s", row.on, res, err, out)
		}
		if n := strings.Count(out, line+"\n"); (row.says && n != 1) || (!row.says && n != 0) || strings.Contains(out, orphan) {
			t.Errorf("%s: the line said %d time(s)\n%s", row.on, n, out)
		}
		if slices.Contains(changing(w.cli()), "targets set") || strings.Contains(out, "its prices are asked") {
			t.Errorf("%s: a price was asked\n%s", row.on, out)
		}
	}

	// A key's budget reaches the targets of the aliases it may call.
	w, _, _ := keyWorld(t)
	w.onMock("rep", "--protocol", "openai", "--billing", "pay_per_use", "--cost-report", "path=usage.cost,usd_per_unit=1")
	w.target("rep/m1", "--kind", "chat")
	w.must("models", "add", "a", "--target", "rep/m1")
	w.target("mock/m", "--kind", "chat")
	w.must("models", "add", "b", "--target", "mock/m")
	w.newKey("ka", "--model", "a")
	w.newKey("kb", "--model", "b")
	w.newKey("kall", "--model", "*")
	for k, says := range map[string]bool{"ka": true, "kb": false, "kall": true} {
		_, out, err := w.capPlain(map[string]string{"on": "key:" + k, "limit-usd": "5"})
		if n := strings.Count(out, line+"\n"); err != nil || (says && n != 1) || (!says && n != 0) {
			t.Errorf("key %s: %v, the line said %d time(s)\n%s", k, err, n, out)
		}
	}
}

func TestCapSpendingUnknown(t *testing.T) {
	for _, on := range []string{"alias:nope", "key:nope"} {
		w, _, _ := keyWorld(t, "mock/m")
		_, out, err := w.capPlain(map[string]string{"on": on, "limit-usd": "5"})
		kind, n, _ := strings.Cut(on, ":")
		var usage *ui.UsageError
		if err == nil || errors.As(err, &usage) || err.Error() != "there is no "+kind+" "+n || len(changing(w.cli())) != 0 {
			t.Errorf("%s: %v, changes %q\n%s", on, err, changing(w.cli()), out)
		}
	}
}

func TestCapSpendingAtTerminal(t *testing.T) {
	for _, yes := range []string{"no", "yes"} {
		w, _, _ := keyWorld(t, "mock/m")
		w.must("models", "set", "--budget", "limit_usd=5,reset_interval=monthly", "--", "a")
		w.newKey("k", "--model", "a")
		res, s, err := w.capAt(map[string][]string{"on": {"instance"}, "limit-usd": {"5"}, "reset": {"monthly"}, "yes": {yes}})
		q, _ := find(s.questions, func(q ui.Question) bool { return q.ID == "on" })
		var got []string
		for _, o := range q.Options {
			got = append(got, o.Value+"|"+o.Label+"|"+o.Group)
		}
		want := []string{"instance|everything (the instance)|", "alias:a|alias a; budget 5 USD, resets monthly|Aliases", "key:k|key k|Apps and agents"}
		if q.Prompt != "What is the cap on?" || !slices.Equal(got, want) {
			t.Errorf("pick %q: %q", q.Prompt, got)
		}
		switch {
		case yes == "no" && (err == nil || err.Error() != "stopped; nothing more was changed" || len(changing(w.cli())) != 0):
			t.Errorf("declined: %v, changes %q\n%s", err, changing(w.cli()), s.output())
		case yes == "yes" && (err != nil || res != "Done: the instance, budget 5 USD, reset monthly." ||
			!slices.Equal(changing(w.cli()), []string{"settings set --json --budget limit_usd=5,reset_interval=monthly"})):
			t.Errorf("agreed: %q, %v, changes %q\n%s", res, err, changing(w.cli()), s.output())
		}
	}
}

func TestCapSpendingRemove(t *testing.T) {
	for _, row := range []struct{ on, change, res string }{
		{"instance", "settings set --json --budget=", "Done: the instance, no budget."},
		{"alias:a", "models set --json --budget= -- a", "Done: alias a, no budget."},
	} {
		for _, yes := range []string{"yes", "no"} {
			w, _, _ := keyWorld(t, "mock/m")
			w.must("settings", "set", "--budget", "limit_usd=10,reset_interval=daily")
			w.must("models", "set", "--budget", "limit_usd=5,reset_interval=monthly", "--", "a")
			res, s, err := w.capAt(map[string][]string{"on": {row.on}, "budget": {"remove"}, "yes": {yes}})
			i, a, _ := w.budgets()
			removed := map[string]*proxy.Budget{"instance": i, "alias:a": a}[row.on]
			switch {
			case yes == "yes" && (err != nil || res != row.res || !slices.Equal(changing(w.cli()), []string{row.change}) || removed != nil):
				t.Errorf("%s: %q, %v, changes %q, read back %+v\n%s", row.on, res, err, changing(w.cli()), removed, s.output())
			case yes == "no" && (err == nil || len(changing(w.cli())) != 0 || removed == nil):
				t.Errorf("%s declined: %v, changes %q", row.on, err, changing(w.cli()))
			}
		}
	}
}

func TestCapSpendingLeave(t *testing.T) {
	w, _, _ := keyWorld(t, "mock/m")
	_, s, err := w.capAt(map[string][]string{"on": {"alias:a"}, "limit-usd": {leave}})
	if !errors.Is(err, ui.ErrLeave) || len(changing(w.cli())) != 0 || strings.Contains(s.output(), "already changed") {
		t.Errorf("%v, changes %q\n%s", err, changing(w.cli()), s.output())
	}
}

// TestCapSpendingRefusesNext: the call that crosses a budget Cap spending
// set is answered and the next refused, before any provider call, on the
// scope it was set on (design Review Focus 3).
func TestCapSpendingRefusesNext(t *testing.T) {
	w, mock, port := keyWorld(t)
	w.onMock("acme", "--protocol", "openai", "--billing", "pay_per_use")
	w.target("acme/m1", "--kind", "chat", "--pricing", "input_per_million=1,output_per_million=1,as_of=2026-11-01")
	w.must("models", "add", "a", "--target", "acme/m1")
	k, k2 := w.newKey("k", "--model", "a"), w.newKey("k2", "--model", "a")
	tiny := map[string]string{"limit-usd": "0.0000001", "reset": "never"}
	tiny["on"] = "alias:a"
	if res, out, err := w.capPlain(tiny); err != nil || res != "Done: alias a, budget 0.0000001 USD, reset never." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	refusedBy := func(secret, msg string) {
		t.Helper()
		if code, body := chatReply(t, port, secret); code != 402 || !strings.Contains(body, msg) {
			t.Fatalf("the next call: %d %s; want 402 %s", code, body, msg)
		}
	}
	if a := chat(t, port, k); a != 200 {
		t.Fatalf("alias budget: the crossing call %d", a)
	}
	refusedBy(k, `the budget of model \"a\" is exhausted`)
	if len(mock.Calls()) != 1 {
		t.Fatalf("alias budget: %d mock call(s)", len(mock.Calls()))
	}
	if res, s, err := w.capAt(map[string][]string{"on": {"alias:a"}, "budget": {"remove"}, "yes": {"yes"}}); err != nil || res != "Done: alias a, no budget." {
		t.Fatalf("remove: %q, %v\n%s", res, err, s.output())
	}
	tiny["on"] = "key:k2"
	if res, out, err := w.capPlain(tiny); err != nil || res != "Done: key k2, budget 0.0000001 USD, reset never." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	if a := chat(t, port, k2); a != 200 {
		t.Fatalf("key budget: the crossing call %d", a)
	}
	refusedBy(k2, "the budget of this key is exhausted")
	if len(mock.Calls()) != 2 {
		t.Fatalf("key budget: %d mock call(s)", len(mock.Calls()))
	}
	if c := chat(t, port, k); c != 200 {
		t.Errorf("key k, no budget: %d", c)
	}
}

func TestCapSpendingNotRunning(t *testing.T) {
	w := newWorld(t)
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "docker"), "#!/bin/sh\n[ \"$*\" = \"compose ps -q\" ] || exit 9\n", 0o755)
	t.Setenv("PATH", bin)
	_, out, err := w.capPlain(map[string]string{"on": "instance", "limit-usd": "5"})
	if err == nil || err.Error() != "no container runs here: ./setup.sh install starts it" || len(w.cli()) != 0 {
		t.Fatalf("err = %v, CLI %q\n%s", err, w.cli(), out)
	}
}

// TestCapSpendingInstanceTelemetry: capping the instance, or removing its
// cap, does not say telemetry's unresolved credential, also on settings: a
// budget neither causes nor resolves it.
func TestCapSpendingInstanceTelemetry(t *testing.T) {
	w, _, _ := keyWorld(t, "mock/m")
	w.must("telemetry", "set", "--otlp-endpoint", "http://host.docker.internal:4318", "--header-env", "X-Tok=NOPE_TOK", "--allow-private-network")
	res, out, err := w.capPlain(map[string]string{"on": "instance", "limit-usd": "5"})
	if err != nil || res != "Done: the instance, budget 5 USD, reset monthly." || strings.Contains(out, "not in the container") {
		t.Errorf("%q, %v\n%s", res, err, out)
	}
	res, s, err := w.capAt(map[string][]string{"on": {"instance"}, "budget": {"remove"}, "yes": {"yes"}})
	if err != nil || res != "Done: the instance, no budget." || strings.Contains(s.output(), "not in the container") {
		t.Errorf("removed: %q, %v\n%s", res, err, s.output())
	}
}
