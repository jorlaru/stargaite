package task

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
	"github.com/jorlaru/stargaite/internal/console/ui/plain"
	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// makeAlias runs Make an alias with u.
func (w *world) makeAlias(u *ui.UI) (string, error) {
	return u.Run(context.Background(), makeSteps(), func(ctx context.Context) (string, error) {
		return MakeAlias(ctx, u, w.proxy(), w.h)
	})
}

// makePlain runs Make an alias with the plain front and answers; it returns
// the result, the output and the error.
func (w *world) makePlain(answers map[string]string) (string, string, error) {
	var out bytes.Buffer
	res, err := w.makeAlias(ui.New(plain.New(&out), answers))
	return res, out.String(), err
}

// override answers each argv in answers from its list, in order; every other
// argv goes to the world's CLI.
func (w *world) override(answers map[string][]reply) {
	next := w.runner
	var mu sync.Mutex
	w.runner = func(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		key := strings.Join(args, " ")
		mu.Lock()
		list, ok := answers[key]
		if len(list) > 0 {
			answers[key] = list[1:]
		}
		mu.Unlock()
		switch {
		case !ok:
			return next(ctx, args, stdin, stdout, stderr)
		case len(list) == 0:
			w.t.Errorf("no more answers for %q", key)
			return 2, nil
		}
		_, _ = io.WriteString(stdout, list[0].out)
		return list[0].code, nil
	}
}

// refused is an apply's refusal holding one error finding.
func refused(code, resource string) reply {
	return reply{`{"error":{"code":"invalid_config","message":"refused","findings":[{"severity":"error","code":"` + code +
		`","resource":"` + resource + `","message":"` + code + ` message"}]}}`, 1}
}

// changing is the argvs that change the configuration, or check, in order.
func changing(argvs []string) []string {
	var out []string
	for _, a := range argvs {
		if !strings.Contains(a, " list ") && !strings.HasPrefix(a, "apply ") && !strings.Contains(a, " show ") && !strings.HasPrefix(a, "settings show") &&
			!strings.Contains(a, " explain ") && !strings.HasPrefix(a, "telemetry status") {
			out = append(out, a)
		}
	}
	return out
}

func TestMakeAliasFlags(t *testing.T) {
	w, mock := checkWorld(t)
	res, out, err := w.makePlain(map[string]string{"provider": "mock", "model": "m", "alias": "local"})
	if err != nil || res != "Done: alias local toward mock/m." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	want := []string{"models list --json", "providers list --json", "targets list --json", "apply --json --dry-run -f -",
		"targets add --json --kind chat -- mock/m", "settings show --json", verifyOf("mock/m"), "models add --json --target mock/m -- local"}
	if got := w.cli(); !slices.Equal(got, want) {
		t.Errorf("CLI calls:\n%q\nwant:\n%q", got, want)
	}
	if !strings.Contains(out, "provider mock exists; kept\n") || !strings.Contains(out, "adding alias local toward mock/m\n") {
		t.Errorf("output:\n%s", out)
	}
	if n := len(mock.Calls()); n != 1 {
		t.Errorf("%d mock calls, want 1", n)
	}
}

func TestMakeAliasRunAgainKept(t *testing.T) {
	w, mock := checkWorld(t)
	answers := map[string]string{"provider": "mock", "model": "m", "alias": "local"}
	if res, out, err := w.makePlain(answers); err != nil {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	before, calls := len(w.cli()), len(mock.Calls())
	res, out, err := w.makePlain(answers)
	if err != nil || res != "alias local exists, toward mock/m; kept: Change an alias changes it" {
		t.Fatalf("again: %q, %v\n%s", res, err, out)
	}
	if got := w.cli()[before:]; !slices.Equal(got, []string{"models list --json"}) || len(mock.Calls()) != calls {
		t.Errorf("again: CLI calls %q, %d mock calls", got, len(mock.Calls())-calls)
	}

	w, mock = checkWorld(t)
	w.onMock("mock2", "--protocol", "openai", "--billing", "none")
	answers["fallback"] = "mock2/m"
	if res, out, err := w.makePlain(answers); err != nil || res != "Done: alias local toward mock/m, mock2/m." {
		t.Fatalf("fallback: %q, %v\n%s", res, err, out)
	}
	before, calls = len(w.cli()), len(mock.Calls())
	res, out, err = w.makePlain(answers)
	if err != nil || res != "alias local exists, toward mock/m, mock2/m; kept: Change an alias changes it" ||
		!strings.Contains(out, "mock2/m is already a target of local; kept\n") {
		t.Fatalf("fallback again: %q, %v\n%s", res, err, out)
	}
	if got := w.cli()[before:]; !slices.Equal(got, []string{"models list --json"}) || len(mock.Calls()) != calls {
		t.Errorf("fallback again: CLI calls %q, %d mock calls", got, len(mock.Calls())-calls)
	}
}

func TestMakeAliasMissingModel(t *testing.T) {
	w, mock := checkWorld(t)
	files := w.files()
	_, out, err := w.makePlain(map[string]string{"provider": "mock", "alias": "other"})
	var usage *ui.UsageError
	if !errors.As(err, &usage) || err.Error() != "missing --model: the model id at mock" {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if got := w.cli(); !slices.Equal(got, []string{"models list --json", "providers list --json"}) {
		t.Errorf("CLI calls %q", got)
	}
	if after := w.files(); !maps.Equal(after, files) {
		t.Errorf("files changed: %v", after)
	}
	if len(w.models.requests()) != 0 || len(mock.Calls()) != 0 {
		t.Errorf("list requests %q, %d mock calls", w.models.requests(), len(mock.Calls()))
	}
}

func TestMakeAliasWithFallback(t *testing.T) {
	w, mock := checkWorld(t)
	w.onMock("mock2", "--protocol", "openai", "--billing", "none")
	res, out, err := w.makePlain(map[string]string{"provider": "mock", "model": "m", "fallback": "mock2/m", "alias": "fb"})
	if err != nil || res != "Done: alias fb toward mock/m, mock2/m." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	if !slices.Contains(w.cli(), "models add --json --target mock/m --target mock2/m -- fb") {
		t.Errorf("CLI calls %q", w.cli())
	}
	if got := w.verifies(); !slices.Equal(got, []string{verifyOf("mock/m"), verifyOf("mock2/m")}) {
		t.Errorf("verify argvs %q", got)
	}
	if len(mock.Calls()) != 2 {
		t.Errorf("%d mock calls, want 2", len(mock.Calls()))
	}
	var fb proxy.Model
	w.decoded(&fb, "models", "show", "fb")
	if !slices.Equal(fb.Targets, []string{"mock/m", "mock2/m"}) {
		t.Errorf("fb toward %q", fb.Targets)
	}
}

// pricedWorld is checkWorld with an instance budget and acme, a pay-per-use
// provider on the mock with no cost report.
func pricedWorld(t *testing.T) (*world, *mockprovider.Server) {
	t.Helper()
	w, mock := checkWorld(t)
	w.must("settings", "set", "--budget", "limit_usd=1,reset_interval=never")
	w.onMock("acme", "--protocol", "openai", "--billing", "pay_per_use")
	return w, mock
}

// indexOf is the index of the first argv that starts with prefix, -1 if none.
func indexOf(argvs []string, prefix string) int {
	return slices.IndexFunc(argvs, func(a string) bool { return strings.HasPrefix(a, prefix) })
}

func TestMakeAliasPricesFlags(t *testing.T) {
	w, _ := pricedWorld(t)
	today := time.Now().Format(time.DateOnly)
	res, out, err := w.makePlain(map[string]string{"provider": "acme", "model": "m", "alias": "x", "input-price": "1.5", "output-price": "6"})
	if err != nil || res != "Done: alias x toward acme/m." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	var order []string
	for _, a := range w.cli() {
		if strings.HasPrefix(a, "apply ") || strings.HasPrefix(a, "targets ") {
			order = append(order, a)
		}
	}
	want := []string{"targets list --json", "apply --json --dry-run -f -", "apply --json --dry-run -f -",
		"targets add --json --kind chat --pricing input_per_million=1.5,output_per_million=6,as_of=" + today + " -- acme/m", verifyOf("acme/m")}
	docs := w.applied()
	if !slices.Equal(order, want) || len(docs) != 2 {
		t.Fatalf("CLI calls:\n%q\nwant:\n%q", order, want)
	}
	var second struct {
		Targets map[string]struct {
			Pricing *proxy.Pricing `json:"pricing"`
		} `json:"targets"`
	}
	if err := json.Unmarshal([]byte(docs[1]), &second); err != nil || second.Targets["acme/m"].Pricing == nil ||
		*second.Targets["acme/m"].Pricing != (proxy.Pricing{In: 1.5, Out: 6, AsOf: today}) {
		t.Errorf("second dry run %s", docs[1])
	}
	if !strings.Contains(out, "acme/m has no prices and a budget reaches it: its prices are asked\n") ||
		!strings.Contains(out, "adding target acme/m with prices 1.5 and 6 USD per million input and output tokens, as of "+today+"\n") {
		t.Errorf("output:\n%s", out)
	}
	if len(w.models.requests()) != 0 {
		t.Errorf("list requests %q", w.models.requests())
	}
}

func TestMakeAliasPricesMissing(t *testing.T) {
	w, mock := pricedWorld(t)
	_, out, err := w.makePlain(map[string]string{"provider": "acme", "model": "m", "alias": "x"})
	var usage *ui.UsageError
	if !errors.As(err, &usage) || err.Error() != "missing --input-price: the price of acme/m in USD per million input tokens" {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if got := w.cli(); got[len(got)-1] != "apply --json --dry-run -f -" || len(w.applied()) != 1 || len(changing(got)) != 0 {
		t.Errorf("CLI calls %q", got)
	}
	if strings.Contains(out, "already changed") || len(mock.Calls()) != 0 {
		t.Errorf("%d mock calls, output:\n%s", len(mock.Calls()), out)
	}
}

func TestMakeAliasOutputCap(t *testing.T) {
	w, mock := checkWorld(t)
	w.onMock("claude", "--protocol", "anthropic", "--billing", "none")
	res, out, err := w.makePlain(map[string]string{"provider": "claude", "model": "m", "alias": "x"})
	if err != nil || res != "Done: alias x toward claude/m." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	docs := w.applied()
	var second struct {
		Models map[string]struct {
			MaxOutputTokens int `json:"max_output_tokens"`
		} `json:"models"`
	}
	if len(docs) != 2 || json.Unmarshal([]byte(docs[1]), &second) != nil || second.Models["x"].MaxOutputTokens != 8192 {
		t.Fatalf("dry runs %q", docs)
	}
	if !slices.Contains(w.cli(), "models add --json --target claude/m --max-output-tokens 8192 -- x") || len(mock.Calls()) != 1 {
		t.Errorf("CLI calls %q, %d mock calls", w.cli(), len(mock.Calls()))
	}

	// A cap typed with a leading zero is sent as the number the dry run judged.
	w, _ = checkWorld(t)
	w.onMock("claude", "--protocol", "anthropic", "--billing", "none")
	if res, out, err := w.makePlain(map[string]string{"provider": "claude", "model": "m", "alias": "x", "max-output-tokens": "0100"}); err != nil ||
		res != "Done: alias x toward claude/m." || !slices.Contains(w.cli(), "models add --json --target claude/m --max-output-tokens 100 -- x") {
		t.Fatalf("0100: %q, %v, CLI calls %q\n%s", res, err, w.cli(), out)
	}

	// A cap the proxy refuses, typed at the question the dry run raised.
	w, mock = checkWorld(t)
	w.onMock("claude", "--protocol", "anthropic", "--billing", "none")
	w.override(map[string][]reply{"apply --json --dry-run -f -": {
		refused("output_cap_required", "models/x"), refused("output_cap_exceeds_target", "models/x")}})
	s := &scripted{answers: map[string][]string{"fallback-provider": {""}, "effort": {""}, "max-output-tokens": {"999999999"}}}
	_, err = w.makeAlias(ui.New(s, map[string]string{"provider": "claude", "model": "m", "alias": "x"}))
	if err == nil || !strings.HasPrefix(err.Error(), "the proxy would refuse this alias:\nmodels/x: output_cap_exceeds_target message") {
		t.Fatalf("err = %v\n%s", err, s.output())
	}
	if !slices.Contains(s.asked, "the alias's output cap in tokens, which an anthropic target needs") {
		t.Errorf("asked %q", s.asked)
	}
	if got := changing(w.cli()); len(got) != 0 || strings.Contains(s.output(), "already changed") || len(mock.Calls()) != 0 {
		t.Errorf("changing calls %q, %d mock calls\n%s", got, len(mock.Calls()), s.output())
	}
}

func TestMakeAliasRefusedBeforeAnything(t *testing.T) {
	w, mock := checkWorld(t)
	_, out, err := w.makePlain(map[string]string{"provider": "mock", "model": "m", "alias": "x", "effort": "hgih"})
	if err == nil || !strings.HasPrefix(err.Error(), "the proxy would refuse this alias:\n") {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if got := w.cli(); got[len(got)-1] != "apply --json --dry-run -f -" || len(changing(got)) != 0 || len(mock.Calls()) != 0 {
		t.Errorf("CLI calls %q, %d mock calls", got, len(mock.Calls()))
	}
}

func TestMakeAliasCheckFails(t *testing.T) {
	w, mock := checkWorld(t)
	mock.Script(mockprovider.Step{Status: http.StatusNotFound, Body: []byte(`{"error":{"message":"no such model"}}`)})
	_, out, err := w.makePlain(map[string]string{"provider": "mock", "model": "m", "alias": "x"})
	if err == nil || !strings.Contains(err.Error(), "status 404") ||
		!strings.Contains(err.Error(), "target mock/m failed its check; no alias or key was created") {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if !strings.Contains(out, "already changed and kept: target mock/m added\n") || indexOf(w.cli(), "models add ") >= 0 {
		t.Errorf("CLI calls %q\n%s", w.cli(), out)
	}
}

func TestMakeAliasEffort(t *testing.T) {
	w, _ := checkWorld(t)
	w.target("mock/m", "--kind", "chat", "--dialect", "reasoning_effort", "--level", "low", "--level", "high")
	res, out, err := w.makePlain(map[string]string{"provider": "mock", "model": "m", "alias": "x", "effort": "low"})
	if err != nil || res != "Done: alias x toward mock/m." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	got := w.cli()
	add := slices.Index(got, "models add --json --target mock/m --effort low --effort-mode default -- x")
	if add < 0 || slices.Index(got, "models explain --json -- x") != add+1 || indexOf(got, "targets add ") >= 0 {
		t.Errorf("CLI calls %q", got)
	}
	var explained proxy.ExplainReply
	w.decoded(&explained, "models", "explain", "x")
	if len(explained.Targets) != 1 || explained.Targets[0].Effort.Sent == nil || *explained.Targets[0].Effort.Sent != "low" {
		t.Fatalf("models explain x: %+v", explained)
	}
	if !strings.HasSuffix(out, "mock/m: sends effort low, output cap none\n") {
		t.Errorf("output:\n%s", out)
	}
}

func TestMakeAliasHeldWarnings(t *testing.T) {
	w, _ := checkWorld(t)
	w.override(map[string][]reply{"models add --json --target mock/m -- x": {{`{"findings":[` +
		`{"severity":"warning","code":"target_unverified","resource":"targets/mock/m","message":"unverified message"},` +
		`{"severity":"warning","code":"url_unresolved","resource":"providers/x","message":"url message"},` +
		`{"severity":"warning","code":"reasoning_undeclared","resource":"targets/mock/m","message":"levels message"},` +
		`{"severity":"warning","code":"effort_dropped","resource":"models/x","message":"dropped message"}]}`, 0}}})
	res, out, err := w.makePlain(map[string]string{"provider": "mock", "model": "m", "alias": "x"})
	if err != nil || res != "Done: alias x toward mock/m." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	if strings.Contains(out, "not checked yet") || strings.Contains(out, "unverified message") ||
		strings.Contains(out, "did not resolve") || strings.Contains(out, "url message") || strings.Contains(out, "levels message") ||
		!strings.Contains(out, "alias x: the alias's effort is not sent toward a target whose reasoning dialect is none\n") {
		t.Errorf("output:\n%s", out)
	}
}

// TestMakeAliasRepricesListed: a listed target a budget reaches without
// prices gets the prices asked through targets set, which clears its
// verification, and a dry run after it judges the alias again.
func TestMakeAliasRepricesListed(t *testing.T) {
	w, mock := pricedWorld(t)
	w.target("acme/m", "--kind", "chat")
	today := time.Now().Format(time.DateOnly)
	res, out, err := w.makePlain(map[string]string{"provider": "acme", "model": "m", "alias": "x", "input-price": "2", "output-price": "8"})
	if err != nil || res != "Done: alias x toward acme/m." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	want := []string{"targets set --json --pricing input_per_million=2,output_per_million=8,as_of=" + today + " -- acme/m", verifyOf("acme/m"),
		"models add --json --target acme/m -- x"}
	if got := changing(w.cli()); !slices.Equal(got, want) || len(w.applied()) != 3 || len(mock.Calls()) != 1 {
		t.Errorf("changing calls %q, %d dry runs, %d mock calls", got, len(w.applied()), len(mock.Calls()))
	}
	if !strings.Contains(out, "setting the prices of acme/m to 2 and 8 USD per million input and output tokens, as of "+today+", which clears its verification\n") {
		t.Errorf("output:\n%s", out)
	}

	// The dry run after the re-pricing refuses: no paid call, no alias, and
	// the prices set are said kept.
	w, mock = pricedWorld(t)
	w.target("acme/m", "--kind", "chat")
	w.override(map[string][]reply{"apply --json --dry-run -f -": {refused("unpriced_under_budget", "targets/acme/m"),
		refused("unpriced_under_budget", "targets/acme/m"), refused("output_cap_exceeds_target", "models/x")}})
	_, out, err = w.makePlain(map[string]string{"provider": "acme", "model": "m", "alias": "x", "input-price": "2", "output-price": "8"})
	if err == nil || err.Error() != "the proxy would refuse this alias:\nmodels/x: output_cap_exceeds_target message" {
		t.Fatalf("third refuses: err = %v\n%s", err, out)
	}
	if got := changing(w.cli()); len(got) != 1 || !strings.HasPrefix(got[0], "targets set ") || len(mock.Calls()) != 0 ||
		!strings.HasSuffix(out, "already changed and kept: prices of acme/m set\n") {
		t.Errorf("third refuses: changing calls %q, %d mock calls\n%s", got, len(mock.Calls()), out)
	}
}

func TestMakeAliasNotNow(t *testing.T) {
	w, mock := checkWorld(t)
	s := &scripted{answers: map[string][]string{"fallback-provider": {""}, "effort": {""}, "check": {"not now"}}}
	res, err := w.makeAlias(ui.New(s, map[string]string{"provider": "mock", "model": "m", "alias": "x"}))
	if err != nil || res != "Not checked: alias x was not made." {
		t.Fatalf("%q, %v\n%s", res, err, s.output())
	}
	if got := changing(w.cli()); !slices.Equal(got, []string{"targets add --json --kind chat -- mock/m"}) || len(mock.Calls()) != 0 ||
		!strings.Contains(s.output(), "already changed and kept: target mock/m added\n") {
		t.Errorf("changing calls %q, %d mock calls\n%s", got, len(mock.Calls()), s.output())
	}
}

func TestMakeAliasExplainFails(t *testing.T) {
	w, _ := checkWorld(t)
	w.override(map[string][]reply{"models explain --json -- x": {{`{"error":{"message":"explain broke"}}`, 1}}})
	_, out, err := w.makePlain(map[string]string{"provider": "mock", "model": "m", "alias": "x", "effort": "low"})
	if err == nil || err.Error() != "stargaite models explain: explain broke" ||
		!strings.HasSuffix(out, "already changed and kept: target mock/m added; alias x added\n") {
		t.Fatalf("err = %v\n%s", err, out)
	}
}

// TestCheckNotStored: a check the server answered but did not store, as when
// its target or provider changed meanwhile, is no pass: the alias is not made.
func TestCheckNotStored(t *testing.T) {
	w, _ := checkWorld(t)
	w.override(map[string][]reply{verifyOf("mock/m"): {{`{"target":"mock/m","ok":true,"stored":false,"calls":[{"effort":"","status":200}]}`, 0}}})
	_, out, err := w.makePlain(map[string]string{"provider": "mock", "model": "m", "alias": "x"})
	want := "target mock/m answered, but it or its provider changed during the check, so the result was not stored; run this again"
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if indexOf(w.cli(), "models add ") >= 0 || strings.Contains(out, "target mock/m is checked") {
		t.Errorf("CLI calls %q\n%s", w.cli(), out)
	}
}

func TestMakeAliasBadPrice(t *testing.T) {
	w, _ := pricedWorld(t)
	_, out, err := w.makePlain(map[string]string{"provider": "acme", "model": "m", "alias": "x", "input-price": "abc", "output-price": "6"})
	var usage *ui.UsageError
	if !errors.As(err, &usage) || err.Error() != "--input-price: --input-price and --output-price take a number of USD" {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if got := changing(w.cli()); len(got) != 0 {
		t.Errorf("changing calls %q", got)
	}
}

func TestMakeAliasExistsBadFallback(t *testing.T) {
	w, _ := checkWorld(t)
	w.target("mock/m", "--kind", "chat")
	w.must("models", "add", "a", "--target", "mock/m")
	_, out, err := w.makePlain(map[string]string{"alias": "a", "fallback": "nope/m"})
	var usage *ui.UsageError
	if !errors.As(err, &usage) || err.Error() != "--fallback: nope/m is not <provider>/<model> of a provider added here; ./setup.sh provider adds one" {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if got := changing(w.cli()); len(got) != 0 {
		t.Errorf("changing calls %q", got)
	}
}

// TestMakeAliasEffortRequired: a target that always reasons, toward an alias
// with no effort, is the dry run's effort_required: the effort is asked again,
// naming the target, before anything is changed.
func TestMakeAliasEffortRequired(t *testing.T) {
	w, _ := checkWorld(t)
	w.target("mock/r", "--kind", "chat", "--dialect", "reasoning_effort", "--level", "low", "--level", "high", "--can-disable=false")
	s := &scripted{answers: map[string][]string{"fallback-provider": {""}, "effort": {"", "low"}, "check": {"check"}}}
	res, err := w.makeAlias(ui.New(s, map[string]string{"provider": "mock", "model": "r", "alias": "x"}))
	if err != nil || res != "Done: alias x toward mock/r." {
		t.Fatalf("%q, %v\n%s", res, err, s.output())
	}
	if !slices.Contains(s.asked, "mock/r always reasons: a default reasoning effort, such as low, medium or high") || len(w.applied()) != 2 ||
		!slices.Contains(w.cli(), "models add --json --target mock/r --effort low --effort-mode default -- x") {
		t.Errorf("asked %q, CLI calls %q", s.asked, w.cli())
	}
}

func TestMakeAliasExistsReverify(t *testing.T) {
	w, mock := checkWorld(t)
	if res, out, err := w.makePlain(map[string]string{"provider": "mock", "model": "m", "alias": "main"}); err != nil {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	res, out, err := w.makePlain(map[string]string{"alias": "main", "reverify": "yes"})
	if err != nil || res != "alias main exists, toward mock/m; kept, and checked again" {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	if got := w.verifies(); !slices.Equal(got, []string{verifyOf("mock/m"), verifyOf("mock/m")}) || len(mock.Calls()) != 2 {
		t.Errorf("verifies %q, %d mock calls", got, len(mock.Calls()))
	}
	hasLines(t, out, "check mock/m: 1 paid call(s) to mock, each capped at 256 output tokens, never retried", "target mock/m is checked")

	// Not now at a terminal: kept, and nothing said checked.
	calls := len(mock.Calls())
	s := &scripted{answers: map[string][]string{"check": {"not now"}}}
	res, err = w.makeAlias(ui.New(s, map[string]string{"alias": "main", "reverify": "yes"}))
	if err != nil || res != "alias main exists, toward mock/m; kept: Change an alias changes it" || len(mock.Calls()) != calls {
		t.Errorf("not now: %q, %v, %d more mock calls\n%s", res, err, len(mock.Calls())-calls, s.output())
	}

	// --fallback naming a target the alias has: checked again all the same.
	res, out, err = w.makePlain(map[string]string{"alias": "main", "reverify": "yes", "fallback": "mock/m"})
	if err != nil || res != "alias main exists, toward mock/m; kept, and checked again" || len(w.verifies()) != 3 {
		t.Errorf("its own fallback: %q, %v, verifies %q\n%s", res, err, w.verifies(), out)
	}

	// --fallback naming a new target wins over --reverify: it is added.
	w.onMock("mock2", "--protocol", "openai", "--billing", "none")
	res, out, err = w.makePlain(map[string]string{"alias": "main", "reverify": "yes", "fallback": "mock2/m"})
	if err != nil || res != "Done: alias main toward mock/m, mock2/m." || !slices.Equal(w.alias("main").Targets, []string{"mock/m", "mock2/m"}) {
		t.Errorf("a new fallback: %q, %v, targets %q\n%s", res, err, w.alias("main").Targets, out)
	}

	// A target whose provider's credential does not resolve: no paid call.
	w, mock = checkWorld(t)
	w.onMock("acme", "--protocol", "openai", "--api-key-env", "ACME_API_KEY", "--billing", "pay_per_use")
	w.target("acme/m", "--kind", "chat")
	w.must("models", "add", "main", "--target", "acme/m")
	_, out, err = w.makePlain(map[string]string{"alias": "main", "reverify": "yes"})
	if err == nil || err.Error() != "the credential of acme does not resolve: ACME_API_KEY is not in the container's environment" ||
		len(w.verifies()) != 0 || len(mock.Calls()) != 0 {
		t.Fatalf("unresolved: err = %v, verifies %q, %d mock calls\n%s", err, w.verifies(), len(mock.Calls()), out)
	}
}
