package task

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
	"github.com/jorlaru/stargaite/internal/console/ui/plain"
)

// closingLine is C2's last line, env the admin variables each followed by a
// space.
func closingLine(env string) string {
	return "stargaite above is the proxy's command, run from this clone as docker compose exec -T " + env +
		"stargaite /stargaite; a target it changes must be checked again, which is paid"
}

// pricingCommand is R2's pricing command for target id, as said.
func pricingCommand(id string) string {
	return "  stargaite targets set " + id + " --pricing input_per_million=<usd>,output_per_million=<usd>,as_of=<YYYY-MM-DD>"
}

// unverifiedLine is the words of target_unverified on target id.
func unverifiedLine(id string) string {
	return "target " + id + " is not checked yet: Make an alias or Change an alias checks it, after stating its paid calls"
}

func (w *world) checkEverything(u *ui.UI) (string, error) {
	return u.Run(context.Background(), checkSteps(), func(ctx context.Context) (string, error) {
		return CheckEverything(ctx, u, w.proxy(), w.h)
	})
}

// findings is how many findings validate gives now.
func (w *world) findings() int {
	w.t.Helper()
	var c proxy.Change
	w.decoded(&c, "validate")
	return len(c.Findings)
}

// saidLines is the lines out holds.
func saidLines(out string) []string {
	return strings.Split(strings.TrimSuffix(out, "\n"), "\n")
}

// TestCheckEverythingSays: every finding in plain words, in validate's
// order, each command after its finding and the closing line once; only a
// finding with a task is offered, and the re-check it opens waits for Check
// it.
func TestCheckEverythingSays(t *testing.T) {
	w, mock := checkWorld(t)
	w.target("mock/m", "--kind", "chat")
	w.must("models", "add", "a", "--target", "mock/m")
	w.onMock("payg", "--protocol", "openai", "--billing", "pay_per_use")
	w.target("payg/u", "--kind", "chat")
	w.onMock("reported", "--protocol", "openai", "--billing", "pay_per_use", "--cost-report", "path=usage.cost,usd_per_unit=1")
	w.target("reported/r", "--kind", "chat")
	w.target("mock/r", "--kind", "chat", "--dialect", "reasoning_effort", "--level", "low", "--level", "high")
	n := w.findings()
	before := len(w.cli())

	s := &scripted{answers: map[string][]string{"fix": {"0"}, "check": {"not now"}}}
	res, err := w.checkEverything(ui.New(s, nil))
	if err != nil || res != "alias a exists, toward mock/m; kept: Change an alias changes it" {
		t.Fatalf("%q, %v\n%s", res, err, s.output())
	}
	want := []string{
		"target mock/r: its reasoning levels are not declared, so an effort is sent as given and never in the off form",
		"  stargaite targets set mock/r --level <level> --can-disable=<true|false>, one --level per level it takes",
		"target payg/u has no prices, so no budget counts its spend; a budget that reaches it asks them",
		pricingCommand("payg/u"),
		"target reported/r has no prices: its cost is the provider's own report, and a call without one counts nothing",
		pricingCommand("reported/r"),
		unverifiedLine("mock/m"),
		unverifiedLine("mock/r"),
		unverifiedLine("payg/u"),
		unverifiedLine("reported/r"),
		closingLine(""),
	}
	if got := saidLines(s.output()); len(got) < len(want) || !slices.Equal(got[:len(want)], want) || n != 7 {
		t.Errorf("%d findings; said:\n%s\nwant:\n%s", n, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	q := s.questions[0]
	if q.ID != "fix" || q.Prompt != "Which one do you want to fix?" ||
		!slices.Equal(valueLabels(q), []string{"none|none of them now", "0|" + unverifiedLine("mock/m")}) {
		t.Errorf("pick %s %q: %q", q.ID, q.Prompt, valueLabels(q))
	}
	if !slices.Contains(s.asked, "1 paid call(s) in all") || len(w.verifies()) != 0 || len(mock.Calls()) != 0 {
		t.Errorf("asked %q, verifies %q, %d mock calls", s.asked, w.verifies(), len(mock.Calls()))
	}
	if !slices.Contains(w.cli()[before:], "models list --json") {
		t.Errorf("Make an alias was not opened: %q", w.cli()[before:])
	}

	s = &scripted{answers: map[string][]string{"fix": {"none"}}}
	if res, err := w.checkEverything(ui.New(s, nil)); err != nil || res != fmt.Sprintf("%d finding(s); none fixed now.", n) {
		t.Errorf("none: %q, %v", res, err)
	}
	s = &scripted{answers: map[string][]string{"fix": {leave}}}
	if _, err := w.checkEverything(ui.New(s, nil)); !errors.Is(err, ui.ErrLeave) {
		t.Errorf("Esc: %v", err)
	}

	// Ctrl-C, or the context's end, in the task opened ends Check everything
	// too, with no result.
	f := &endAt{scripted: &scripted{answers: map[string][]string{"fix": {"0"}}}, id: "check", end: func() {}, err: ui.ErrQuit}
	if res, err := w.checkEverything(ui.New(f, nil)); res != "" || !errors.Is(err, ui.ErrQuit) {
		t.Errorf("Ctrl-C: %q, %v", res, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f = &endAt{scripted: &scripted{answers: map[string][]string{"fix": {"0"}}}, id: "check", end: cancel, err: context.Canceled}
	u := ui.New(f, nil)
	res, err = u.Run(ctx, checkSteps(), func(ctx context.Context) (string, error) { return CheckEverything(ctx, u, w.proxy(), w.h) })
	if res != "" || !errors.Is(err, context.Canceled) {
		t.Errorf("the context's end: %q, %v", res, err)
	}
}

// endAt is scripted, but at question id it calls end, then answers err.
type endAt struct {
	*scripted
	id  string
	end func()
	err error
}

func (f *endAt) Ask(ctx context.Context, q ui.Question) (string, error) {
	if q.ID == f.id {
		f.end()
		return "", f.err
	}
	return f.scripted.Ask(ctx, q)
}

// TestCheckEverythingSkipsVerified: the re-check a finding opens checks only
// the alias's targets not yet verified.
func TestCheckEverythingSkipsVerified(t *testing.T) {
	w, mock := checkWorld(t)
	w.target("mock/v", "--kind", "chat")
	w.target("mock/u", "--kind", "chat")
	if _, err := w.checkIn(ui.New(plain.New(&bytes.Buffer{}), nil), "mock/v"); err != nil {
		t.Fatal(err)
	}
	w.must("models", "add", "b", "--target", "mock/v", "--target", "mock/u")
	verifies, calls := len(w.verifies()), len(mock.Calls())
	s := &scripted{answers: map[string][]string{"fix": {"0"}, "check": {"not now"}}}
	res, err := w.checkEverything(ui.New(s, nil))
	if err != nil || res != "alias b exists, toward mock/v, mock/u; kept: Change an alias changes it" {
		t.Fatalf("%q, %v\n%s", res, err, s.output())
	}
	if !slices.Equal(valueLabels(s.questions[0]), []string{"none|none of them now", "0|" + unverifiedLine("mock/u")}) ||
		!strings.Contains(s.output(), "target mock/v is verified; not checked again\n") ||
		!strings.Contains(s.output(), "check mock/u: 1 paid call(s) to mock") || strings.Contains(s.output(), "check mock/v:") ||
		!slices.Contains(s.asked, "1 paid call(s) in all") || len(w.verifies()) != verifies || len(mock.Calls()) != calls {
		t.Errorf("options %q, asked %q, verifies %q, %d mock calls\n%s", valueLabels(s.questions[0]), s.asked, w.verifies(), len(mock.Calls())-calls, s.output())
	}
}

// checkCanned is a world whose CLI answers export minimal and validate with
// findings.
func checkCanned(t *testing.T, findings string, code int) *world {
	t.Helper()
	w := newWorld(t)
	w.canned(map[string]reply{"export --json": {out: `{"version":1}`}, "validate --json": {out: `{"findings":[` + findings + `]}`, code: code}})
	return w
}

func TestCheckEverythingTelemetry(t *testing.T) {
	w := checkCanned(t, `{"severity":"warning","code":"credential_unresolved","resource":"settings","message":"LANGFUSE_SECRET is not set"}`, 0)
	s := &scripted{answers: map[string][]string{"fix": {"none"}}}
	if res, err := w.checkEverything(ui.New(s, nil)); err != nil || res != "1 finding(s); none fixed now." {
		t.Fatalf("%q, %v\n%s", res, err, s.output())
	}
	line := "telemetry: LANGFUSE_SECRET is not set: Send traces sets its keys"
	if !slices.Equal(saidLines(s.output()), []string{line}) || !slices.Equal(valueLabels(s.questions[0]), []string{"none|none of them now", "0|" + line}) {
		t.Errorf("options %q\n%s", valueLabels(s.questions[0]), s.output())
	}
}

func TestCheckEverythingTLS(t *testing.T) {
	w := checkCanned(t, `{"severity":"warning","code":"unpriced","resource":"targets/p/m","message":"x"}`, 0)
	plane, _ := url.Parse(w.models.srv.URL)
	tlsPort, cert := localhostTLS(t, httputil.NewSingleHostReverseProxy(plane))
	writeFile(t, filepath.Join(w.h.Dir, ".env"), "STARGAITE_PORT="+tlsPort+"\n", 0o600)
	writeFile(t, filepath.Join(w.h.Dir, "tls", "ca.pem"), string(cert), 0o600)
	writeFile(t, filepath.Join(w.h.Dir, "compose.override.yaml"), "# setup: tls_name=localhost\n# setup: tls_ca=ca.pem\nservices: {}\n", 0o600)
	var out bytes.Buffer
	res, err := w.checkEverything(ui.New(plain.New(&out), nil))
	if err != nil || res != "1 finding(s), none with a task here: the lines above say what fixes each." {
		t.Fatalf("%q, %v\n%s", res, err, out.String())
	}
	if !strings.HasSuffix(out.String(), pricingCommand("p/m")+"\n"+
		closingLine("-e STARGAITE_ADMIN_URL=https://localhost:4001 -e SSL_CERT_FILE=/tls/ca.pem ")+"\n") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestCheckEverythingNone(t *testing.T) {
	w := checkCanned(t, "", 0)
	s := &scripted{}
	if res, err := w.checkEverything(ui.New(s, nil)); err != nil || res != "Nothing to fix: the proxy reports no finding." ||
		s.output() != "" || len(s.questions) != 0 {
		t.Errorf("%q, %v, asked %q\n%s", res, err, s.asked, s.output())
	}
}

func TestCheckEverythingNoFix(t *testing.T) {
	w, _ := checkWorld(t)
	w.onMock("payg", "--protocol", "openai", "--billing", "pay_per_use")
	w.target("payg/u", "--kind", "chat")
	s := &scripted{}
	res, err := w.checkEverything(ui.New(s, nil))
	if err != nil || res != "2 finding(s), none with a task here: the lines above say what fixes each." || len(s.questions) != 0 {
		t.Fatalf("%q, %v, asked %q\n%s", res, err, s.asked, s.output())
	}
	hasLines(t, s.output(), "target payg/u has no prices, so no budget counts its spend; a budget that reaches it asks them", unverifiedLine("payg/u"))
}

func TestCheckEverythingError(t *testing.T) {
	w := checkCanned(t, `{"severity":"error","code":"x","resource":"targets/x","message":"refused"}`, 1)
	var out bytes.Buffer
	res, err := w.checkEverything(ui.New(plain.New(&out), nil))
	if err != nil || res != "1 finding(s), none with a task here: the lines above say what fixes each." || out.String() != "error: target x: refused\n" {
		t.Errorf("%q, %v\n%s", res, err, out.String())
	}
}

// valueLabels is the values and labels of q's options, as "value|label".
func valueLabels(q ui.Question) []string {
	var out []string
	for _, o := range q.Options {
		out = append(out, o.Value+"|"+o.Label)
	}
	return out
}
