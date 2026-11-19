package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/admintest"
	"github.com/jorlaru/stargaite/internal/cli"
)

// cliRunner runs the CLI in process against env, as the container would.
func cliRunner(env map[string]string) Runner {
	return func(_ context.Context, args []string, stdout, stderr io.Writer) (int, error) {
		return cli.Main(args, func(k string) string { return env[k] }, strings.NewReader(""), stdout, stderr), nil
	}
}

// canned answers every call with out and code, and records the arguments.
func canned(out string, code int, seen *[][]string) Runner {
	return func(_ context.Context, args []string, stdout, _ io.Writer) (int, error) {
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
	noisy := func(_ context.Context, _ []string, stdout, stderr io.Writer) (int, error) {
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
	if !slices.Contains(providers.Providers, Provider{Name: "anthropic", Credential: "resolved"}) {
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
	block := func(ctx context.Context, _ []string, _, _ io.Writer) (int, error) {
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
	failing := func(context.Context, []string, io.Writer, io.Writer) (int, error) { return 0, errors.New("no docker") }
	if err := New(failing).Call(context.Background(), &ProvidersReply{}, "providers list", nil); err == nil || err.Error() != "stargaite providers list: no docker" {
		t.Fatalf("err = %v", err)
	}
}

func TestCallBound(t *testing.T) {
	var left time.Duration
	var ok bool
	record := func(ctx context.Context, _ []string, stdout, _ io.Writer) (int, error) {
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
	big := func(_ context.Context, _ []string, stdout, _ io.Writer) (int, error) {
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
