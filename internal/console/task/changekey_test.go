package task

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
	"github.com/jorlaru/stargaite/internal/console/ui/plain"
	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// changeKey runs Change a key with the scripted answers; it returns the
// result, the error and the front.
func (w *world) changeKey(answers map[string][]string) (string, *scripted, error) {
	s := &scripted{answers: answers}
	u := ui.New(s, nil)
	res, err := u.Run(context.Background(), changeKeySteps(), func(ctx context.Context) (string, error) {
		return ChangeKey(ctx, u, w.proxy(), w.h)
	})
	return res, s, err
}

// newKey makes key name through the CLI before the run, with flags, and
// returns its secret.
func (w *world) newKey(name string, flags ...string) string {
	w.t.Helper()
	var made proxy.KeyChange
	if err := json.Unmarshal([]byte(w.out(append(append([]string{"keys", "create", "--json"}, flags...), "--", name)...)), &made); err != nil {
		w.t.Fatal(err)
	}
	return made.Secrets[name]
}

// listedKey is key n as keys list shows it.
func (w *world) listedKey(n string) (proxy.Key, bool) {
	w.t.Helper()
	var reply proxy.KeysReply
	w.decoded(&reply, "keys", "list")
	return find(reply.Keys, func(k proxy.Key) bool { return k.Name == n })
}

// chat is the status of one chat completion toward alias a with secret,
// made by the test on the data plane at port.
func chat(t *testing.T, port, secret string) int {
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
	_ = resp.Body.Close()
	return resp.StatusCode
}

// revealed is the value of the reveal the front was asked, "" for none.
func (s *scripted) revealed() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, _ := find(s.questions, func(q ui.Question) bool { return q.Kind == ui.KindReveal })
	return q.Value
}

// asks reports whether the front was asked the question id.
func (s *scripted) asks(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := find(s.questions, func(q ui.Question) bool { return q.ID == id })
	return ok
}

// rotateAnswers is a rotate of key k with grace, agreed, the try answered try.
func rotateAnswers(grace, try string) map[string][]string {
	return map[string][]string{"key": {"k"}, "change": {"rotate"}, "grace": {grace}, "yes": {"yes"}, "secret": {""}, "try": {try}}
}

func TestChangeKeyNone(t *testing.T) {
	w, _, _ := keyWorld(t, "mock/m")
	res, _, err := w.changeKey(nil)
	if err != nil || res != "No key yet: Give an app or agent its key makes one." {
		t.Fatalf("%q, %v", res, err)
	}
	if got := w.cli(); !slices.Equal(got, []string{"keys list --json"}) {
		t.Errorf("CLI calls %q", got)
	}
}

func TestChangeKeyOptions(t *testing.T) {
	w, _, _ := keyWorld(t, "mock/m")
	w.newKey("k", "--model", "a")
	w.newKey("all", "--model", "*")
	_, s, _ := w.changeKey(map[string][]string{"key": {"k"}, "change": {leave}})
	var labels []string
	for _, q := range s.questions {
		for _, o := range q.Options {
			labels = append(labels, q.ID+": "+o.Value+" "+o.Label)
		}
	}
	want := []string{"key: all all → every alias", "key: k k → a", "change: rotate rotate its secret", "change: aliases the aliases it may call",
		"change: limits its limits: requests a minute and at once", "change: budget its budget", "change: revoke revoke it"}
	if !slices.Equal(labels, want) {
		t.Errorf("options %q", labels)
	}
}

func TestChangeKeyRotate(t *testing.T) {
	w, mock, port := keyWorld(t, "mock/m")
	old := w.newKey("k", "--model", "a")
	res, s, err := w.changeKey(rotateAnswers("1h", "check"))
	if err != nil || res != "Done: key k rotated." {
		t.Fatalf("%q, %v\n%s", res, err, s.output())
	}
	if i := indexOf(w.cli(), "keys rotate "); i < 0 || w.cli()[i] != "keys rotate --json --grace 1h -- k" {
		t.Errorf("CLI calls %q", w.cli())
	}
	hasLines(t, s.output(), "the secret of key k is replaced; the one in use stays valid for 1 hour", mayLose("k", "rotated"), keySaid("k", "rotated"))
	if !before(s.output(), "the secret of key k is replaced; the one in use stays valid for 1 hour", mayLose("k", "rotated")) ||
		!before(s.output(), mayLose("k", "rotated"), keySaid("k", "rotated")) {
		t.Errorf("out of order:\n%s", s.output())
	}
	if got := s.revealed(); !secretLine.MatchString(got) || got == old {
		t.Errorf("revealed %q, old %q", got, old)
	}
	if len(mock.Calls()) != 1 {
		t.Errorf("%d mock calls from the try", len(mock.Calls()))
	}
	if status := chat(t, port, old); status != 200 {
		t.Errorf("the old secret answered %d in its grace", status)
	}
}

func TestChangeKeyRotateNow(t *testing.T) {
	w, _, port := keyWorld(t, "mock/m")
	old := w.newKey("k", "--model", "a")
	res, s, err := w.changeKey(rotateAnswers("0s", "not now"))
	if err != nil || res != "Done: key k rotated; not tried." {
		t.Fatalf("%q, %v\n%s", res, err, s.output())
	}
	if i := indexOf(w.cli(), "keys rotate "); i < 0 || w.cli()[i] != "keys rotate --json --grace 0s -- k" {
		t.Errorf("CLI calls %q", w.cli())
	}
	hasLines(t, s.output(), "the secret of key k is replaced; the one in use ends now, and its apps are refused")
	if status := chat(t, port, old); status != 401 {
		t.Errorf("the old secret answered %d once ended", status)
	}
}

func TestChangeKeyRotateInGrace(t *testing.T) {
	w, _, _ := keyWorld(t, "mock/m")
	w.newKey("k", "--model", "a")
	w.must("keys", "rotate", "--grace", "1h", "--", "k")
	_, s, err := w.changeKey(rotateAnswers("1h", "not now"))
	if err != nil || !before(s.output(), "key k has an older secret in its grace period: it ends now, as a key never has more than two",
		"the secret of key k is replaced; the one in use stays valid for 1 hour") {
		t.Errorf("%v\n%s", err, s.output())
	}
}

func TestChangeKeyRotateNoAlias(t *testing.T) {
	w, mock, _ := keyWorld(t)
	w.newKey("k", "--model", "*")
	res, s, err := w.changeKey(rotateAnswers("1h", "check"))
	if err != nil || res != "Done: key k rotated; not tried." {
		t.Fatalf("%q, %v\n%s", res, err, s.output())
	}
	hasLines(t, s.output(), "no alias key k may call exists, so it is not tried")
	if len(mock.Calls()) != 0 || s.asks("try") || strings.Contains(s.output(), "Point an OpenAI-compatible client") {
		t.Errorf("%d mock calls\n%s", len(mock.Calls()), s.output())
	}
}

func TestChangeKeyRotateDeclined(t *testing.T) {
	w, _, _ := keyWorld(t, "mock/m")
	w.newKey("k", "--model", "a")
	answers := rotateAnswers("1h", "check")
	answers["yes"] = []string{"no"}
	_, s, err := w.changeKey(answers)
	if err == nil || err.Error() != "stopped; nothing more was changed" || indexOf(w.cli(), "keys rotate ") >= 0 || s.revealed() != "" {
		t.Errorf("%v, CLI calls %q", err, w.cli())
	}
}

func TestChangeKeyAliases(t *testing.T) {
	w, _, _ := keyWorld(t, "mock/m")
	w.must("models", "add", "b", "--target", "mock/m")
	w.newKey("k", "--model", "a")
	res, s, err := w.changeKey(map[string][]string{"key": {"k"}, "change": {"aliases"}, "aliases": {"a,b"}, "yes": {"yes"}})
	if err != nil || res != "Done: key k, which may call a, b." {
		t.Fatalf("%q, %v\n%s", res, err, s.output())
	}
	hasLines(t, s.output(), "key k will call only a, b")
	if got := changing(w.cli()); !slices.Equal(got, []string{"keys set --json --model a --model b -- k"}) {
		t.Errorf("changes %q", got)
	}
	if k, _ := w.listedKey("k"); !slices.Equal(k.Models, []string{"a", "b"}) {
		t.Errorf("key k toward %q", k.Models)
	}

	w, _, _ = keyWorld(t, "mock/m")
	w.newKey("k", "--model", "a")
	if _, _, err := w.changeKey(map[string][]string{"key": {"k"}, "change": {"aliases"}, "aliases": {"a"}, "yes": {"no"}}); err == nil ||
		err.Error() != "stopped; nothing more was changed" || len(changing(w.cli())) != 0 {
		t.Errorf("declined: %v, changes %q", err, changing(w.cli()))
	}
}

func TestChangeKeyLimits(t *testing.T) {
	limits := func(rpm, par []string) map[string][]string {
		return map[string][]string{"key": {"k"}, "change": {"limits"}, "rpm-limit": rpm, "max-parallel-requests": par, "yes": {"yes"}}
	}
	for _, row := range []struct {
		rpm, par     []string
		argv, result string
	}{
		{[]string{""}, []string{"3"}, "keys set --json --rpm-limit= --max-parallel-requests 3 -- k", "Done: key k, no limit a minute, 3 at once."},
		{[]string{"0", "30"}, []string{""}, "keys set --json --rpm-limit 30 --max-parallel-requests= -- k", "Done: key k, 30 requests a minute, the default at once."},
	} {
		w, _, _ := keyWorld(t, "mock/m")
		w.newKey("k", "--model", "a")
		res, s, err := w.changeKey(limits(row.rpm, row.par))
		if err != nil || res != row.result {
			t.Fatalf("%q: %q, %v\n%s", row.rpm, res, err, s.output())
		}
		if got := changing(w.cli()); !slices.Equal(got, []string{row.argv}) {
			t.Errorf("%q: changes %q", row.rpm, got)
		}
		if asked := strings.Count(strings.Join(s.asked, "\n"), "requests a minute, now"); asked != len(row.rpm) {
			t.Errorf("%q: rpm asked %d times", row.rpm, asked)
		}
	}

	w, _, _ := keyWorld(t, "mock/m")
	w.newKey("k", "--model", "a", "--rpm-limit", "60", "--max-parallel-requests", "3")
	res, s, err := w.changeKey(limits([]string{""}, []string{""}))
	if err != nil || res != "Done: key k, no limit a minute, the default at once." {
		t.Fatalf("%q, %v\n%s", res, err, s.output())
	}
	if !slices.Contains(s.asked, "requests a minute, now 60; empty for no limit") ||
		!slices.Contains(s.asked, "requests at once, now 3; empty for the proxy's default, 5") {
		t.Errorf("asked %q", s.asked)
	}
	hasLines(t, s.output(), "the limits of key k become no limit a minute, the default at once")
	if got := changing(w.cli()); !slices.Equal(got, []string{"keys set --json --rpm-limit= --max-parallel-requests= -- k"}) {
		t.Errorf("changes %q", got)
	}
	// keys list shows the published default, 5, for a key that declares none
	// (probes.md, Task 1).
	if k, _ := w.listedKey("k"); k.RPMLimit != nil || k.MaxParallel == nil || *k.MaxParallel != 5 {
		t.Errorf("key k: rpm %v, at once %v", k.RPMLimit, k.MaxParallel)
	}

	w, _, _ = keyWorld(t, "mock/m")
	w.newKey("k", "--model", "a")
	declined := limits([]string{"30"}, []string{""})
	declined["yes"] = []string{"no"}
	if _, _, err := w.changeKey(declined); err == nil || err.Error() != "stopped; nothing more was changed" || len(changing(w.cli())) != 0 {
		t.Errorf("declined: %v, changes %q", err, changing(w.cli()))
	}
}

func TestChangeKeyRevoke(t *testing.T) {
	w, _, _ := keyWorld(t, "mock/m")
	w.newKey("k", "--model", "a")
	res, s, err := w.changeKey(map[string][]string{"key": {"k"}, "change": {"revoke"}, "yes": {"yes"}})
	if err != nil || res != "Done: key k revoked." {
		t.Fatalf("%q, %v\n%s", res, err, s.output())
	}
	hasLines(t, s.output(), "key k is revoked: its apps are refused within a second, and its name is never used again")
	if got := changing(w.cli()); !slices.Equal(got, []string{"keys revoke --json --yes -- k"}) {
		t.Errorf("changes %q", got)
	}
	if _, listed := w.listedKey("k"); listed {
		t.Error("key k is still listed")
	}

	w, _, _ = keyWorld(t, "mock/m")
	w.newKey("k", "--model", "a")
	if _, _, err := w.changeKey(map[string][]string{"key": {"k"}, "change": {"revoke"}, "yes": {"no"}}); err == nil || len(changing(w.cli())) != 0 {
		t.Errorf("declined: %v, changes %q", err, changing(w.cli()))
	}
}

func TestChangeKeyLeave(t *testing.T) {
	w, _, _ := keyWorld(t, "mock/m")
	w.newKey("k", "--model", "a")
	_, s, err := w.changeKey(map[string][]string{"key": {"k"}, "change": {leave}})
	if !errors.Is(err, ui.ErrLeave) || len(changing(w.cli())) != 0 || strings.Contains(s.output(), "already changed") {
		t.Errorf("%v, changes %q\n%s", err, changing(w.cli()), s.output())
	}
}

// budgetWorld is keyWorld with acme, a pay-per-use provider on the mock
// with no cost report, and its target acme/m1, unpriced and checked.
func budgetWorld(t *testing.T, targets ...string) (*world, *mockprovider.Server) {
	t.Helper()
	w, mock, _ := keyWorld(t, targets...)
	w.onMock("acme", "--protocol", "openai", "--billing", "pay_per_use")
	w.target("acme/m1", "--kind", "chat")
	if checked, err := w.checkIn(ui.New(plain.New(&bytes.Buffer{}), nil), "acme/m1"); err != nil || !checked {
		t.Fatalf("checking acme/m1: %v", err)
	}
	return w, mock
}

// askedAt is a scripted front that also notes, per question ID, how many
// CLI calls the task had made when it was last asked.
type askedAt struct {
	*scripted
	w  *world
	at map[string]int
}

func (a askedAt) Ask(ctx context.Context, q ui.Question) (string, error) {
	if q.Kind != ui.KindWork {
		a.at[q.ID] = len(a.w.cli())
	}
	return a.scripted.Ask(ctx, q)
}

// changeKeyAt is changeKey, noting when each question was asked.
func (w *world) changeKeyAt(answers map[string][]string) (string, askedAt, error) {
	a := askedAt{scripted: &scripted{answers: answers}, w: w, at: map[string]int{}}
	u := ui.New(a, nil)
	res, err := u.Run(context.Background(), changeKeySteps(), func(ctx context.Context) (string, error) {
		return ChangeKey(ctx, u, w.proxy(), w.h)
	})
	return res, a, err
}

// keyCalls is the argvs from the first keys set on that set keys, list
// targets or set targets, in order.
func keyCalls(argvs []string) []string {
	var out []string
	for _, a := range argvs[max(indexOf(argvs, "keys set "), 0):] {
		if strings.HasPrefix(a, "keys set ") || strings.HasPrefix(a, "targets list ") || strings.HasPrefix(a, "targets set ") {
			out = append(out, a)
		}
	}
	return out
}

func budgetAnswers(more map[string][]string) map[string][]string {
	answers := map[string][]string{"key": {"k"}, "change": {"budget"}, "limit-usd": {"5"}, "reset": {"monthly"}, "yes": {"yes"}}
	maps.Copy(answers, more)
	return answers
}

func TestChangeKeyBudget(t *testing.T) {
	w, _, _ := keyWorld(t, "mock/m")
	w.newKey("k", "--model", "a")
	res, s, err := w.changeKey(budgetAnswers(nil))
	if err != nil || res != "Done: key k, budget 5 USD, reset monthly." {
		t.Fatalf("%q, %v\n%s", res, err, s.output())
	}
	hasLines(t, s.output(), "a budget of 5 USD is set on key k, reset monthly")
	if got := changing(w.cli()); !slices.Equal(got, []string{"keys set --json --budget limit_usd=5,reset_interval=monthly -- k"}) {
		t.Errorf("changes %q", got)
	}
	if s.asks("input-price") || s.asks("budget") {
		t.Errorf("asked %q", s.asked)
	}

	// A first budget is confirmed too: it may refuse the key's apps at once.
	w, _, _ = keyWorld(t, "mock/m")
	w.newKey("k", "--model", "a")
	if _, _, err := w.changeKey(budgetAnswers(map[string][]string{"yes": {"no"}})); err == nil ||
		err.Error() != "stopped; nothing more was changed" || len(changing(w.cli())) != 0 {
		t.Errorf("declined: %v, changes %q", err, changing(w.cli()))
	}
}

func TestChangeKeyBudgetPrices(t *testing.T) {
	w, _ := budgetWorld(t)
	w.must("models", "add", "a", "--target", "acme/m1")
	w.newKey("k", "--model", "a")
	today := time.Now().Format(time.DateOnly)
	res, a, err := w.changeKeyAt(budgetAnswers(map[string][]string{"input-price": {"1.5"}, "output-price": {"6"}, "check": {"not now"}}))
	out := a.output()
	if err != nil || res != "Done: key k, budget 5 USD, reset monthly." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	set := "keys set --json --budget limit_usd=5,reset_interval=monthly -- k"
	want := []string{set, "targets list --json", "targets set --json --pricing input_per_million=1.5,output_per_million=6,as_of=" + today + " -- acme/m1", set}
	if got := keyCalls(w.cli()); len(got) < 4 || !slices.Equal(got[:4], want) {
		t.Fatalf("CLI calls:\n%q\nwant first:\n%q", got, want)
	}
	if a.at["output-price"] > indexOf(w.cli(), "targets set ") {
		t.Errorf("the output price was asked after targets set: %d, %q", a.at["output-price"], w.cli())
	}
	if !slices.Contains(a.asked, "1 target(s) must be checked again: 1 paid call(s)") || len(w.verifies()) != 1 {
		t.Errorf("asked %q, verify argvs %q", a.asked, w.verifies())
	}
	hasLines(t, out, "acme/m1 has no prices and a budget reaches it: its prices are asked",
		"setting the prices of acme/m1 to 1.5 and 6 USD per million input and output tokens, as of "+today+", which clears its verification",
		"1 target(s) left unchecked: acme/m1")
	if k, _ := w.listedKey("k"); k.Budget == nil || *k.Budget != (proxy.Budget{LimitUSD: 5, ResetInterval: "monthly"}) {
		t.Errorf("key k budget %+v", k.Budget)
	}
}

// TestChangeKeyBudgetPricesLeave: Esc at the re-check, after the budget is
// set, says the budget among what was changed.
func TestChangeKeyBudgetPricesLeave(t *testing.T) {
	w, _ := budgetWorld(t)
	w.must("models", "add", "a", "--target", "acme/m1")
	w.newKey("k", "--model", "a")
	_, s, err := w.changeKey(budgetAnswers(map[string][]string{"input-price": {"1.5"}, "output-price": {"6"}, "check": {leave}}))
	if !errors.Is(err, ui.ErrLeave) {
		t.Fatalf("err = %v\n%s", err, s.output())
	}
	hasLines(t, s.output(), "already changed and kept: prices of acme/m1 set; key k: budget 5 USD, reset monthly")
}

func TestChangeKeyBudgetOtherRefusal(t *testing.T) {
	w, _, _ := keyWorld(t, "mock/m")
	w.newKey("k", "--model", "a")
	w.override(map[string][]reply{"keys set --json --budget limit_usd=5,reset_interval=monthly -- k": {refused("unknown_alias", "keys/k")}})
	_, s, err := w.changeKey(budgetAnswers(nil))
	if err == nil {
		t.Fatalf("no error\n%s", s.output())
	}
	if lines := strings.Split(err.Error(), "\n"); len(lines) != 2 || lines[0] != "the proxy would refuse this change:" || lines[1] != "keys/k: unknown_alias message" {
		t.Errorf("err = %q", err)
	}
	if s.asks("input-price") || indexOf(w.cli(), "targets set ") >= 0 {
		t.Errorf("asked %q, CLI calls %q", s.asked, w.cli())
	}
}

func TestChangeKeyBudgetReplace(t *testing.T) {
	for _, yes := range []string{"yes", "no"} {
		w, _, _ := keyWorld(t, "mock/m")
		w.newKey("k", "--model", "a", "--budget", "limit_usd=5,reset_interval=monthly")
		res, s, err := w.changeKey(budgetAnswers(map[string][]string{"budget": {"set"}, "limit-usd": {"10"}, "reset": {"daily"}, "yes": {yes}}))
		hasLines(t, s.output(), "the budget of key k, 5 USD, resets monthly, is replaced by 10 USD, reset daily")
		switch {
		case yes == "yes" && (err != nil || res != "Done: key k, budget 10 USD, reset daily."):
			t.Errorf("%q, %v\n%s", res, err, s.output())
		case yes == "no" && (err == nil || len(changing(w.cli())) != 0):
			t.Errorf("declined: %v, changes %q", err, changing(w.cli()))
		}
	}
}

func TestChangeKeyBudgetRemove(t *testing.T) {
	for _, yes := range []string{"yes", "no"} {
		w, _, _ := keyWorld(t, "mock/m")
		w.newKey("k", "--model", "a", "--budget", "limit_usd=5,reset_interval=monthly")
		res, s, err := w.changeKey(map[string][]string{"key": {"k"}, "change": {"budget"}, "budget": {"remove"}, "yes": {yes}})
		hasLines(t, s.output(), "the budget of key k, 5 USD, resets monthly, is removed")
		switch {
		case yes == "yes" && (err != nil || res != "Done: key k, no budget." || !slices.Equal(changing(w.cli()), []string{"keys set --json --budget= -- k"})):
			t.Errorf("%q, %v, changes %q\n%s", res, err, changing(w.cli()), s.output())
		case yes == "no" && (err == nil || len(changing(w.cli())) != 0):
			t.Errorf("declined: %v, changes %q", err, changing(w.cli()))
		}
	}
}

func TestChangeKeyAliasesPriced(t *testing.T) {
	w, _ := budgetWorld(t, "mock/m")
	w.must("models", "add", "b", "--target", "acme/m1")
	w.newKey("k", "--model", "a", "--budget", "limit_usd=5,reset_interval=monthly")
	res, a, err := w.changeKeyAt(map[string][]string{"key": {"k"}, "change": {"aliases"}, "aliases": {"a,b"}, "yes": {"yes"},
		"input-price": {"1"}, "output-price": {"2"}, "check": {"not now"}})
	if err != nil || res != "Done: key k, which may call a, b." {
		t.Fatalf("%q, %v\n%s", res, err, a.output())
	}
	set := "keys set --json --model a --model b -- k"
	today := time.Now().Format(time.DateOnly)
	want := []string{set, "targets list --json", "targets set --json --pricing input_per_million=1,output_per_million=2,as_of=" + today + " -- acme/m1", set}
	if got := keyCalls(w.cli()); len(got) < 4 || !slices.Equal(got[:4], want) {
		t.Fatalf("CLI calls:\n%q\nwant first:\n%q", got, want)
	}
	if a.at["output-price"] > indexOf(w.cli(), "targets set ") {
		t.Errorf("the output price was asked after targets set: %d, %q", a.at["output-price"], w.cli())
	}
	if k, _ := w.listedKey("k"); !slices.Equal(k.Models, []string{"a", "b"}) {
		t.Errorf("key k toward %q", k.Models)
	}
}

func TestMayCall(t *testing.T) {
	for _, row := range []struct {
		models []string
		want   string
	}{
		{[]string{"a", "b"}, "a, b"},
		{[]string{"*"}, "every alias"},
		{nil, "no alias"},
	} {
		if got := mayCall(proxy.Key{Models: row.models}); got != row.want {
			t.Errorf("mayCall(%q) = %q; want %q", row.models, got, row.want)
		}
	}
}

func TestIsLimit(t *testing.T) {
	for _, a := range []string{"5", "0", "0.5"} {
		if err := isLimit(a); err != nil {
			t.Errorf("isLimit(%q) = %v", a, err)
		}
	}
	for _, a := range []string{"x", "-1", "Inf"} {
		if err := isLimit(a); err == nil || err.Error() != "a number of USD is needed" {
			t.Errorf("isLimit(%q) = %v", a, err)
		}
	}
}
