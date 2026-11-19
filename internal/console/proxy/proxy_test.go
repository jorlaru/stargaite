package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/admintest"
	"github.com/jorlaru/stargaite/internal/cli"
	"github.com/jorlaru/stargaite/internal/console/paid"
	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// cliRunner runs the CLI in process against env, as the container would.
func cliRunner(env map[string]string) Runner {
	return func(_ context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		if stdin == nil {
			stdin = strings.NewReader("")
		}
		return cli.Main(args, func(k string) string { return env[k] }, stdin, stdout, stderr), nil
	}
}

// canned answers every call with out and code, and records the arguments.
func canned(out string, code int, seen *[][]string) Runner {
	return func(_ context.Context, args []string, _ io.Reader, stdout, _ io.Writer) (int, error) {
		if seen != nil {
			*seen = append(*seen, args)
		}
		_, _ = io.WriteString(stdout, out)
		return code, nil
	}
}

// exampleServer is the admin API in process with config.example.yaml applied.
func exampleServer(t *testing.T) map[string]string {
	t.Helper()
	env := admintest.Server(t)
	var out, errOut bytes.Buffer
	if code := cli.Main([]string{"apply", "-f", "../../../config.example.yaml", "--json"}, func(k string) string { return env[k] },
		strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("apply: exit %d:\n%s%s", code, out.String(), errOut.String())
	}
	return env
}

func TestCallArgv(t *testing.T) {
	var seen [][]string
	px := New(canned(`{"providers":[],"keys":[]}`, 0, &seen))
	if err := px.Call(context.Background(), &ProvidersReply{}, "providers list", nil); err != nil {
		t.Fatal(err)
	}
	if err := px.Call(context.Background(), &KeysReply{}, "keys show", nil, "app"); err != nil {
		t.Fatal(err)
	}
	if err := px.Call(context.Background(), &KeysReply{}, "keys list", []string{"--x", "y"}); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"providers", "list", "--json"}, {"keys", "show", "--json", "--", "app"}, {"keys", "list", "--json", "--x", "y"}}
	if !slices.EqualFunc(seen, want, slices.Equal) {
		t.Fatalf("argv = %q, want %q", seen, want)
	}
	// Standard error is not read: what the CLI writes there never reaches the answer.
	noisy := func(_ context.Context, _ []string, _ io.Reader, stdout, stderr io.Writer) (int, error) {
		_, _ = io.WriteString(stderr, "sk-x")
		_, _ = io.WriteString(stdout, `{"providers":[]}`)
		return 0, nil
	}
	if err := New(noisy).Call(context.Background(), &ProvidersReply{}, "providers list", nil); err != nil {
		t.Fatal(err)
	}
}

func TestCallRealShapes(t *testing.T) {
	px := New(cliRunner(exampleServer(t)))
	ctx := context.Background()
	var (
		version   VersionReply
		providers ProvidersReply
		models    ModelsReply
		targets   TargetsReply
		keys      KeysReply
		settings  SettingsReply
		telemetry TelemetryReply
	)
	for _, c := range []struct {
		words string
		reply Reply
	}{
		{"version", &version}, {"providers list", &providers}, {"models list", &models}, {"targets list", &targets},
		{"keys list", &keys}, {"settings show", &settings}, {"telemetry status", &telemetry},
	} {
		if err := px.Call(ctx, c.reply, c.words, nil); err != nil {
			t.Fatalf("%s: %v", c.words, err)
		}
	}
	if version.Version != "dev" {
		t.Errorf("version = %q", version.Version)
	}
	anthropic := Provider{Name: "anthropic", Credential: "resolved", Protocol: "anthropic", BaseURL: "https://api.anthropic.com", APIKeyEnv: "ANTHROPIC_API_KEY"}
	if !slices.Contains(providers.Providers, anthropic) {
		t.Errorf("providers = %+v", providers.Providers)
	}
	i := slices.IndexFunc(models.Models, func(m Model) bool { return m.Name == "fast" })
	if i < 0 || !slices.Equal(models.Models[i].Targets, []string{"minimax-plan/MiniMax-M3.1-Flash", "openrouter/qwen/qwen-3.6-flash"}) {
		t.Errorf("models = %+v", models.Models)
	}
	i = slices.IndexFunc(models.Models, func(m Model) bool { return m.Name == "deep" })
	if i < 0 || models.Models[i].Budget == nil || *models.Models[i].Budget != (Budget{LimitUSD: 500, ResetInterval: "monthly"}) {
		t.Errorf("deep's budget = %+v", models.Models)
	}
	if len(targets.Targets) == 0 || targets.Targets[0].ID != "anthropic/claude-sonnet-5-5" {
		t.Errorf("targets = %+v", targets.Targets)
	}
	i = slices.IndexFunc(keys.Keys, func(k Key) bool { return k.Name == "invoice-agent" })
	if i < 0 || !slices.Equal(keys.Keys[i].Models, []string{"fast", "deep"}) || keys.Keys[i].Budget == nil || keys.Keys[i].Budget.LimitUSD != 100 {
		t.Errorf("keys = %+v", keys.Keys)
	}
	if settings.Budget == nil || *settings.Budget != (Budget{LimitUSD: 2000, ResetInterval: "monthly"}) {
		t.Errorf("settings budget = %+v", settings.Budget)
	}
	if telemetry.Telemetry == nil || telemetry.Telemetry.OTLPEndpoint != "https://langfuse.example.com/api/public/otel" ||
		!slices.Equal(telemetry.Telemetry.BasicAuthEnv, []string{"LANGFUSE_PUBLIC_KEY", "LANGFUSE_SECRET_KEY"}) || len(telemetry.Unresolved) != 0 {
		t.Errorf("telemetry = %+v", telemetry)
	}

	var presets PresetsReply
	if err := px.Call(ctx, &presets, "providers presets", nil); err != nil {
		t.Fatal(err)
	}
	i = slices.IndexFunc(presets.Presets, func(p Preset) bool { return p.Name == "ollama" })
	if len(presets.Presets) != 18 || i < 0 || presets.Presets[i].BaseURL != "http://localhost:11434/v1" {
		t.Errorf("presets = %+v", presets.Presets)
	}
	var shown ProviderReply
	if err := px.Call(ctx, &shown, "providers show", nil, "anthropic"); err != nil {
		t.Fatal(err)
	}
	if shown.Name != "anthropic" || shown.Credential != "resolved" || shown.Protocol != "anthropic" {
		t.Errorf("providers show anthropic = %+v", shown)
	}
	for _, private := range []bool{false, true} {
		name, flags := "acme", []string{"--protocol", "openai", "--base-url", "https://api.acme.example/v1", "--billing", "none"}
		if private {
			name, flags = "local", []string{"--protocol", "openai", "--base-url", "http://127.0.0.1:8000/v1", "--allow-private-network", "--billing", "none"}
		}
		var change ProviderChange
		if err := px.Call(ctx, &change, "providers add", flags, name); err != nil {
			t.Fatal(err)
		}
		if change.Resource.Name != name || change.Resource.AllowPrivateNetwork != private || change.Findings == nil {
			t.Errorf("providers add %s = %+v", name, change)
		}
	}
}

func TestCallEmptyTelemetry(t *testing.T) {
	var reply TelemetryReply
	if err := New(cliRunner(admintest.Server(t))).Call(context.Background(), &reply, "telemetry status", nil); err != nil {
		t.Fatal(err)
	}
	if reply.Telemetry == nil || reply.Telemetry.OTLPEndpoint != "" {
		t.Fatalf("telemetry = %+v", reply.Telemetry)
	}
}

func TestCallMissingKey(t *testing.T) {
	err := New(canned(`{"provider":[]}`, 0, nil)).Call(context.Background(), &ProvidersReply{}, "providers list", nil)
	want := `stargaite providers list: the answer has no "providers": this console and the running proxy are different versions; rebuild the proxy from this clone`
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

func TestCallErrorMessage(t *testing.T) {
	px := New(cliRunner(map[string]string{"STARGAITE_ADMIN_URL": "http://127.0.0.1:1", "STARGAITE_ADMIN_KEY": admintest.Key}))
	err := px.Call(context.Background(), &ProvidersReply{}, "providers list", nil)
	if err == nil || !strings.HasPrefix(err.Error(), "stargaite providers list: the admin API was not reached") {
		t.Fatalf("err = %v", err)
	}
	err = New(canned(`{"error":{"message":"bad\u001b[2J\nthing"}}`, 1, nil)).Call(context.Background(), &ProvidersReply{}, "providers list", nil)
	if err == nil || err.Error() != "stargaite providers list: bad[2J thing" {
		t.Fatalf("err = %v, want the message cleaned", err)
	}
}

func TestCallNeverQuotesOutput(t *testing.T) {
	err := New(canned("failed to read .env: line 1: 'sk-hand-edited-secret\n", 1, nil)).Call(context.Background(), &ProvidersReply{}, "providers list", nil)
	if err == nil || err.Error() != "stargaite providers list: failed with exit 1" {
		t.Fatalf("err = %v", err)
	}
}

func TestCallExitZeroNotJSON(t *testing.T) {
	want := "stargaite providers list: the answer was not understood: this console and the running proxy are different versions; rebuild the proxy from this clone"
	for _, out := range []string{"sk-hand-edited-secret not json\n", `{"providers":"sk-x"}`} {
		err := New(canned(out, 0, nil)).Call(context.Background(), &ProvidersReply{}, "providers list", nil)
		if err == nil || err.Error() != want || strings.Contains(err.Error(), "sk-") {
			t.Fatalf("%q: err = %v", out, err)
		}
	}
}

func TestCallHonoursContext(t *testing.T) {
	block := func(ctx context.Context, _ []string, _ io.Reader, _, _ io.Writer) (int, error) {
		<-ctx.Done()
		return 130, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := New(block).Call(ctx, &ProvidersReply{}, "providers list", nil)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
	failing := func(context.Context, []string, io.Reader, io.Writer, io.Writer) (int, error) {
		return 0, errors.New("no docker")
	}
	if err := New(failing).Call(context.Background(), &ProvidersReply{}, "providers list", nil); err == nil || err.Error() != "stargaite providers list: no docker" {
		t.Fatalf("err = %v", err)
	}
}

func TestCallBound(t *testing.T) {
	var left time.Duration
	var ok bool
	record := func(ctx context.Context, _ []string, _ io.Reader, stdout, _ io.Writer) (int, error) {
		var deadline time.Time
		deadline, ok = ctx.Deadline()
		left = time.Until(deadline)
		_, _ = io.WriteString(stdout, `{"providers":[]}`)
		return 0, nil
	}
	if err := New(record).Call(context.Background(), &ProvidersReply{}, "providers list", nil); err != nil {
		t.Fatal(err)
	}
	if !ok || left <= 89*time.Second || left > 90*time.Second || cliBound != 90*time.Second {
		t.Fatalf("deadline set %v, %v left", ok, left)
	}
}

func TestCallCaptureLimit(t *testing.T) {
	big := func(_ context.Context, _ []string, _ io.Reader, stdout, _ io.Writer) (int, error) {
		chunk := bytes.Repeat([]byte(" "), 1<<20)
		for range 9 {
			if _, err := stdout.Write(chunk); err != nil {
				break
			}
		}
		return 0, nil
	}
	err := New(big).Call(context.Background(), &ProvidersReply{}, "providers list", nil)
	if err == nil || err.Error() != "stargaite providers list: the answer is larger than 8 MiB" || captureLimit != 8<<20 {
		t.Fatalf("err = %v", err)
	}
}

// budget opens a paid budget of n calls on a fresh session.
func budget(t *testing.T, n int) *paid.Budget {
	t.Helper()
	b, err := paid.NewSession().Open(n)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// verified is a passing verify answer of one call.
const verified = `{"target":"acme/m1","ok":true,"stored":true,"calls":[{"effort":"","status":200}]}`

// recording answers every call with out and code, and records the
// arguments and whether a standard input was given.
func recording(out string, code int, seen *[][]string, stdins *[]bool) Runner {
	return func(_ context.Context, args []string, stdin io.Reader, stdout, _ io.Writer) (int, error) {
		*seen = append(*seen, args)
		*stdins = append(*stdins, stdin != nil)
		_, _ = io.WriteString(stdout, out)
		return code, nil
	}
}

// TestVerifyTakesFirst: no budget, or a spent one, is refused before the CLI
// runs.
func TestVerifyTakesFirst(t *testing.T) {
	var seen [][]string
	var stdins []bool
	px := New(recording(verified, 0, &seen, &stdins))
	spent := budget(t, 1)
	if err := spent.Take(1); err != nil {
		t.Fatal(err)
	}
	for _, b := range []*paid.Budget{nil, spent} {
		if _, err := px.Verify(context.Background(), b, "acme/m1", 1, time.Minute); err == nil {
			t.Errorf("budget %+v: no error", b)
		}
	}
	if len(seen) != 0 {
		t.Fatalf("the CLI ran %d times without a budget", len(seen))
	}
}

// TestVerifyArgv: one Verify is one targets verify, with a 256-token cap,
// watching its standard input, which is given.
func TestVerifyArgv(t *testing.T) {
	var seen [][]string
	var stdins []bool
	reply, err := New(recording(verified, 0, &seen, &stdins)).Verify(context.Background(), budget(t, 1), "acme/m1", 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"targets", "verify", "--json", "--max-output-tokens", "256", "--until-stdin-closes", "--", "acme/m1"}}
	if !slices.EqualFunc(seen, want, slices.Equal) || !slices.Equal(stdins, []bool{true}) {
		t.Fatalf("argv = %q, stdin given %v; want %q with a stdin", seen, stdins, want)
	}
	if reply.Target != "acme/m1" || !reply.OK || !reply.Stored || len(reply.Calls) != 1 || reply.Calls[0].Status != 200 {
		t.Errorf("reply = %+v", reply)
	}
}

// TestVerifyFails: a failed check is an error naming each call's status and
// the provider's reason.
func TestVerifyFails(t *testing.T) {
	out := `{"target":"acme/m1","ok":false,"stored":false,"calls":[{"effort":"","status":404,"provider_message":"model not found: acme/m1"}]}`
	var seen [][]string
	var stdins []bool
	_, err := New(recording(out, 1, &seen, &stdins)).Verify(context.Background(), budget(t, 1), "acme/m1", 1, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "call: status 404, model not found: acme/m1") ||
		!strings.Contains(err.Error(), "target acme/m1 failed its check") {
		t.Fatalf("err = %v", err)
	}
	two := `{"target":"acme/m1","ok":false,"stored":false,"calls":[{"effort":"low","status":200},` +
		`{"effort":"high","status":500,"error":{"message":"upstream\u001b[2J broke"}},{"effort":"none","status":0}]}`
	_, err = New(recording(two, 1, &seen, &stdins)).Verify(context.Background(), budget(t, 3), "acme/m1", 3, time.Minute)
	want := "low: status 200, no answer\nhigh: status 500, upstream[2J broke\nnone: status 0, no answer\ntarget acme/m1 failed its check"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

// TestVerifyMoreCallsThanStated: an answer with more calls than the step
// stated is a loud error.
func TestVerifyMoreCallsThanStated(t *testing.T) {
	out := `{"target":"acme/m1","ok":true,"stored":true,"calls":[{"effort":"low","status":200},{"effort":"high","status":200}]}`
	var seen [][]string
	var stdins []bool
	_, err := New(recording(out, 0, &seen, &stdins)).Verify(context.Background(), budget(t, 1), "acme/m1", 1, time.Minute)
	want := "stargaite targets verify acme/m1: the answer has 2 calls, 1 were stated: stop and report this"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

// TestVerifyOtherOutcomes: an error envelope, an exit 2 and an exit 0 that
// is not an answer follow Call's rule, never quoting the output.
func TestVerifyOtherOutcomes(t *testing.T) {
	for _, tc := range []struct {
		out  string
		code int
		want string
	}{
		{`{"error":{"message":"target \"acme/m1\" does not exist"}}`, 1, `stargaite targets verify: target "acme/m1" does not exist`},
		{"usage: sk-hand-edited-secret\n", 2, "stargaite targets verify: failed with exit 2"},
		{`{"ok":true}`, 0, `stargaite targets verify: the answer has no "target": this console and the running proxy are different versions; rebuild the proxy from this clone`},
	} {
		var seen [][]string
		var stdins []bool
		_, err := New(recording(tc.out, tc.code, &seen, &stdins)).Verify(context.Background(), budget(t, 1), "acme/m1", 1, time.Minute)
		if err == nil || err.Error() != tc.want {
			t.Errorf("%q exit %d: err = %v, want %q", tc.out, tc.code, err, tc.want)
		}
	}
}

// mockTarget is the admin API in process with provider mock on the mock
// provider and target mock/m, whose check is 3 calls (low, high, none).
func mockTarget(t *testing.T) (map[string]string, *mockprovider.Server) {
	t.Helper()
	env := admintest.Server(t)
	mock := mockprovider.New()
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	for _, args := range [][]string{
		{"providers", "add", "mock", "--protocol", "openai", "--base-url", srv.URL, "--allow-private-network", "--billing", "none"},
		{"targets", "add", "mock/m", "--kind", "chat", "--level", "low", "--level", "high", "--can-disable"},
	} {
		var out, errOut bytes.Buffer
		if code := cli.Main(args, func(k string) string { return env[k] }, strings.NewReader(""), &out, &errOut); code != 0 {
			t.Fatalf("%q: exit %d: %s%s", args, code, out.String(), errOut.String())
		}
	}
	return env, mock
}

// TestVerifyCancelAtServer: cancelling Verify closes the CLI's standard
// input, and the server, through the real CLI in process, cancels the call
// in flight and starts no other.
func TestVerifyCancelAtServer(t *testing.T) {
	env, mock := mockTarget(t)
	mock.Script(mockprovider.Step{Hang: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := New(cliRunner(env)).Verify(ctx, budget(t, 3), "mock/m", 3, time.Minute)
		done <- err
	}()
	for deadline := time.Now().Add(5 * time.Second); len(mock.Calls()) != 1; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the check never reached the mock")
		}
	}
	cancel()
	select {
	case err := <-done:
		want := "the check of mock/m was stopped: the call in flight may still be billed"
		if err == nil || !strings.HasPrefix(err.Error(), want) || !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want %q wrapping context.Canceled", err, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Verify did not return within 5 s of the cancel")
	}
	for deadline := time.Now().Add(5 * time.Second); !mock.Calls()[0].Cancelled; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the call in flight was not cancelled at the mock")
		}
	}
	time.Sleep(300 * time.Millisecond)
	if n := len(mock.Calls()); n != 1 {
		t.Errorf("%d mock calls after the cancel, want 1", n)
	}
}

// TestVerifyEscBeforeEnterMakesNoCall: a check whose budget was never opened
// reaches no provider (design section 10, layer 2; the piece's half lands
// with the paid piece).
func TestVerifyEscBeforeEnterMakesNoCall(t *testing.T) {
	env, mock := mockTarget(t)
	if _, err := New(cliRunner(env)).Verify(context.Background(), nil, "mock/m", 3, time.Minute); err == nil {
		t.Fatal("a check without a budget ran")
	}
	if n := len(mock.Calls()); n != 0 {
		t.Fatalf("%d mock calls, want 0", n)
	}
}

// TestVerifyStopGrace: after a cancel, a CLI that does not stop on its
// closed standard input keeps its context for the grace, and no longer.
func TestVerifyStopGrace(t *testing.T) {
	if stopGrace != 10*time.Second || New(nil).grace != stopGrace {
		t.Fatalf("stopGrace %v, New's grace %v; want 10 s", stopGrace, New(nil).grace)
	}
	cancelled := make(chan time.Time, 1)
	deaf := func(ctx context.Context, _ []string, _ io.Reader, _, _ io.Writer) (int, error) {
		<-ctx.Done()
		cancelled <- time.Now()
		return 130, nil
	}
	px := New(deaf)
	px.grace = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := px.Verify(ctx, budget(t, 1), "acme/m1", 1, time.Minute)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Verify did not return within 2 s")
	}
	if waited := (<-cancelled).Sub(start); waited < 200*time.Millisecond || waited > 2*time.Second {
		t.Fatalf("the CLI's context was cancelled %v after the cancel, want the 200 ms grace", waited)
	}
}

// TestVerifyBound: at its bound a check is stopped as a cancel is: its
// standard input is closed first, before its context.
func TestVerifyBound(t *testing.T) {
	first := make(chan string, 2)
	listening := func(ctx context.Context, _ []string, stdin io.Reader, _, _ io.Writer) (int, error) {
		eof := make(chan struct{})
		go func() { _, _ = io.Copy(io.Discard, stdin); close(eof) }()
		select {
		case <-eof:
			first <- "eof"
		case <-ctx.Done():
			first <- "ctx"
		}
		return 1, nil
	}
	px := New(listening)
	px.grace = 200 * time.Millisecond
	start := time.Now()
	_, err := px.Verify(context.Background(), budget(t, 1), "acme/m1", 1, 100*time.Millisecond)
	if err == nil || !strings.HasPrefix(err.Error(), "the check of acme/m1 did not end within 100ms: the call in flight may still be billed") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("Verify returned after %v", time.Since(start))
	}
	if got := <-first; got != "eof" {
		t.Fatalf("the CLI saw %s first, want its standard input's end", got)
	}
}

// TestCallRefusesVerify: a paid command never goes through Call.
func TestCallRefusesVerify(t *testing.T) {
	var seen [][]string
	var stdins []bool
	var r VerifyReply
	err := New(recording(verified, 0, &seen, &stdins)).Call(context.Background(), &r, "targets verify", nil, "acme/m1")
	if err == nil || err.Error() != "a paid command goes through Verify" || len(seen) != 0 {
		t.Fatalf("err = %v, %d CLI runs", err, len(seen))
	}
	// The CLI matches a command by its leading words: anything after them
	// still runs the paid command.
	for _, words := range []string{"targets verify acme/m1", "targets verify --max-output-tokens=256 acme/m1", " targets  verify"} {
		err := New(recording(verified, 0, &seen, &stdins)).Call(context.Background(), &r, words, nil)
		if err == nil || err.Error() != "a paid command goes through Verify" || len(seen) != 0 {
			t.Fatalf("%q: err = %v, CLI runs %q", words, err, seen)
		}
	}
}

// TestVerifyEndedContext: a check whose context has already ended is not
// started, and its budget is left unspent.
func TestVerifyEndedContext(t *testing.T) {
	var seen [][]string
	var stdins []bool
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b := budget(t, 1)
	_, err := New(recording(verified, 0, &seen, &stdins)).Verify(ctx, b, "acme/m1", 1, time.Minute)
	if err == nil || err.Error() != "the check of acme/m1 was not started: context canceled" || !errors.Is(err, context.Canceled) || len(seen) != 0 {
		t.Fatalf("err = %v, CLI runs %q", err, seen)
	}
	if err := b.Take(1); err != nil {
		t.Fatalf("the budget was spent: %v", err)
	}
}
