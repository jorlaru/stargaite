package proxy

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http/httptest"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/admintest"
	"github.com/jorlaru/stargaite/internal/cli"
	"github.com/jorlaru/stargaite/internal/console/paid"
	"github.com/jorlaru/stargaite/internal/usage"
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
	anthropic := Provider{Name: "anthropic", Credential: "resolved", Protocol: "anthropic", BaseURL: "https://api.anthropic.com",
		APIKeyEnv: "ANTHROPIC_API_KEY"}
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
	if settings.Timeouts.TotalMS <= 0 {
		t.Errorf("settings timeouts = %+v", settings.Timeouts)
	}
	changeShapes(t, px)
	exportShapes(t)
	keyShapes(t)
}

// exportShapes: export on an empty store, then validate on a store whose only
// finding is an unresolved credential, through the real CLI (C61's probes of
// shapes read from code only).
func exportShapes(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	env := admintest.Server(t)
	px := New(cliRunner(env))
	var export ExportReply
	if err := px.Call(ctx, &export, "export", nil); err != nil {
		t.Fatalf("export: %v", err)
	}
	if export.Version != 1 || export.Providers != nil || export.Models != nil || export.Keys != nil || export.Settings.Budget != nil {
		t.Errorf("export of an empty store = %+v", export)
	}
	cliDo(t, env, "providers", "add", "x", "--protocol", "openai", "--base-url", "https://api.x.example/v1", "--api-key-env", "NOPE_KEY",
		"--billing", "none")
	var validated Change
	if err := px.Call(ctx, &validated, "validate", nil); err != nil {
		t.Fatalf("validate: %v", err)
	}
	want := []Finding{{Severity: "warning", Code: "credential_unresolved", Resource: "providers/x", Message: "NOPE_KEY does not resolve"}}
	if !slices.Equal(validated.Findings, want) {
		t.Errorf("validate = %+v, want %+v", validated.Findings, want)
	}
}

// keyShapes: the key commands C61 runs, through the real CLI, on a store with
// one alias and no budget. A secret is never printed.
func keyShapes(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	env := admintest.Server(t)
	px := New(cliRunner(env))
	cliDo(t, env, "providers", "add", "mock", "--protocol", "openai", "--base-url", "http://127.0.0.1:9/v1", "--allow-private-network", "--billing", "none")
	cliDo(t, env, "targets", "add", "mock/m", "--kind", "chat")
	cliDo(t, env, "models", "add", "a", "--target", "mock/m")
	token := regexp.MustCompile(`^sk-stargaite-[a-z2-7]{12}-[a-z2-7]{52}$`)
	key := func() (Key, bool) {
		t.Helper()
		var keys KeysReply
		if err := px.Call(ctx, &keys, "keys list", nil); err != nil {
			t.Fatalf("keys list: %v", err)
		}
		i := slices.IndexFunc(keys.Keys, func(k Key) bool { return k.Name == "k" })
		if i < 0 {
			return Key{}, false
		}
		return keys.Keys[i], true
	}

	first, findings, err := px.NewSecret(ctx, "keys create", []string{"--model", "a", "--rpm-limit", "10"}, "k")
	if err != nil || !token.MatchString(first) || findings == nil {
		t.Fatalf("keys create: err %v, findings %+v, the secret has the token's shape: %v", err, findings, token.MatchString(first))
	}
	// keys list shows the limits in force: an undeclared max_parallel_requests
	// is the published default, 5 (internal/config/snapshot.go:251, LIM-7).
	k, ok := key()
	if !ok || !slices.Equal(k.Models, []string{"a"}) || k.RPMLimit == nil || *k.RPMLimit != 10 || k.MaxParallel == nil || *k.MaxParallel != 5 ||
		!slices.Equal(k.Secrets, []Secret{{ValidUntil: 0}}) {
		t.Errorf("keys list after create = %+v", k)
	}
	second, _, err := px.NewSecret(ctx, "keys rotate", []string{"--grace", "1h"}, "k")
	if err != nil || !token.MatchString(second) || second == first {
		t.Fatalf("keys rotate: err %v, the token's shape %v, a new secret %v", err, token.MatchString(second), second != first)
	}
	k, _ = key()
	now := time.Now().UnixMilli()
	if len(k.Secrets) != 2 || !slices.ContainsFunc(k.Secrets, func(s Secret) bool { return s.ValidUntil > now }) {
		t.Errorf("keys list after rotate = %+v", k.Secrets)
	}
	var set Change
	if err := px.Call(ctx, &set, "keys set", []string{"--rpm-limit=", "--max-parallel-requests", "3"}, "k"); err != nil {
		t.Fatalf("keys set: %v", err)
	}
	if k, _ = key(); k.RPMLimit != nil || k.MaxParallel == nil || *k.MaxParallel != 3 {
		t.Errorf("keys list after set = %+v", k)
	}
	var revoked Change
	if err := px.Call(ctx, &revoked, "keys revoke", []string{"--yes"}, "k"); err != nil {
		t.Fatalf("keys revoke: %v", err)
	}
	if _, ok := key(); ok {
		t.Error("keys list holds k after its revoke")
	}
	var export ExportReply
	if err := px.Call(ctx, &export, "export", nil); err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(export.Models) != 1 || len(export.Keys) != 0 || export.Settings.Budget != nil {
		t.Errorf("export = %+v", export)
	}
}

// TestCallRefusableKeyBudget: a key's budget that reaches an unpriced
// pay-per-use target is refused with unpriced_under_budget, returned as a
// finding with nothing published; once the target is priced it is accepted.
func TestCallRefusableKeyBudget(t *testing.T) {
	env := admintest.Server(t)
	cliDo(t, env, "providers", "add", "acme", "--protocol", "openai", "--base-url", "https://api.acme.example/v1", "--billing", "pay_per_use")
	cliDo(t, env, "targets", "add", "acme/m1", "--kind", "chat")
	cliDo(t, env, "models", "add", "a", "--target", "acme/m1")
	cliDo(t, env, "keys", "create", "k", "--model", "a")
	shown := func() *Budget {
		t.Helper()
		var k Key
		if err := json.Unmarshal([]byte(cliDo(t, env, "keys", "show", "k", "--json")), &k); err != nil {
			t.Fatal(err)
		}
		return k.Budget
	}
	px := New(cliRunner(env))
	ctx := context.Background()
	flags := []string{"--budget", "limit_usd=5,reset_interval=monthly"}
	findings, err := px.CallRefusable(ctx, &Change{}, "keys set", flags, "k")
	if err != nil || !hasFinding(findings, "error", "unpriced_under_budget", "targets/acme/m1") {
		t.Fatalf("unpriced: %+v, %v; want unpriced_under_budget on targets/acme/m1", findings, err)
	}
	if b := shown(); b != nil {
		t.Fatalf("a refused change was published: budget %+v", b)
	}
	cliDo(t, env, "targets", "set", "acme/m1", "--pricing", "input_per_million=1,output_per_million=2,as_of="+time.Now().Format(time.DateOnly))
	var change Change
	findings, err = px.CallRefusable(ctx, &change, "keys set", flags, "k")
	if err != nil || findings != nil || change.Findings == nil {
		t.Fatalf("priced: findings %+v, %v, the change %+v", findings, err, change)
	}
	if b := shown(); b == nil || *b != (Budget{LimitUSD: 5, ResetInterval: "monthly"}) {
		t.Fatalf("budget = %+v", b)
	}
}

// TestCallRefusableOther: an exit 1 without findings and an exit 2 follow
// Call's rule, never quoting the output; the paid command is refused.
func TestCallRefusableOther(t *testing.T) {
	for _, tc := range []struct {
		out  string
		code int
		want string
	}{
		{`{"error":{"message":"no","findings":[]}}`, 1, "stargaite keys set: no"},
		{"usage: sk-hand-edited-secret\n", 2, "stargaite keys set: failed with exit 2"},
	} {
		findings, err := New(canned(tc.out, tc.code, nil)).CallRefusable(context.Background(), &Change{}, "keys set", nil, "k")
		if findings != nil || err == nil || err.Error() != tc.want {
			t.Errorf("%q exit %d: findings %+v, err %v; want %q", tc.out, tc.code, findings, err, tc.want)
		}
	}
	var seen [][]string
	_, err := New(canned(verified, 0, &seen)).CallRefusable(context.Background(), &VerifyReply{}, "targets verify", nil, "acme/m1")
	if err == nil || err.Error() != "a paid command goes through Verify" || len(seen) != 0 {
		t.Fatalf("targets verify: err %v, %d CLI runs", err, len(seen))
	}
}

// TestNewSecretNotUnderstood: an answer without the new secret says it is
// lost and never quotes the answer; a refusal is the CLI's message; a run
// that may have made the key says so.
func TestNewSecretNotUnderstood(t *testing.T) {
	ctx := context.Background()
	lost := "stargaite keys create: the answer was not understood, so the new secret of key k is lost: Change a key rotates it (" + stale + ")"
	for _, out := range []string{"not json sk-stargaite-leak", `{"findings":[],"secrets":{}}`, `{"findings":[],"secrets":{"other":"sk-x"}}`} {
		secret, _, err := New(canned(out, 0, nil)).NewSecret(ctx, "keys create", nil, "k")
		if secret != "" || err == nil || err.Error() != lost {
			t.Errorf("%q: secret given %v, err %v; want %q", out, secret != "", err, lost)
		}
	}
	// The admin API's own refusal, as the CLI passes it on: nothing was made.
	_, _, err := New(canned(`{"error":{"message":"key \"k\" exists","code":"conflict"}}`, 1, nil)).NewSecret(ctx, "keys create", nil, "k")
	if err == nil || err.Error() != `stargaite keys create: key "k" exists` {
		t.Errorf("exit 1: err %v", err)
	}
	// No answer of the admin API was read: the key may have been made.
	for _, c := range []struct {
		out  string
		code int
		want string
	}{
		{`{"error":{"message":"the admin API was not reached: EOF","type":"api_error","code":"admin_unreachable"}}`, 1,
			"stargaite keys create: the admin API was not reached: EOF"},
		{`{"error":{"message":"key \"k\" exists"}}`, 1, `stargaite keys create: key "k" exists`},
		{"", 137, "stargaite keys create: failed with exit 137"},
	} {
		_, _, err := New(canned(c.out, c.code, nil)).NewSecret(ctx, "keys create", nil, "k")
		if want := c.want + ": key k may have been made (or rotated); if so its secret is lost: Change a key rotates it"; err == nil || err.Error() != want {
			t.Errorf("exit %d %s: err %v, want %q", c.code, c.out, err, want)
		}
	}
	failing := func(context.Context, []string, io.Reader, io.Writer, io.Writer) (int, error) {
		return 0, errors.New("no docker")
	}
	_, _, err = New(failing).NewSecret(ctx, "keys create", nil, "k")
	if want := "stargaite keys create: no docker: key k may have been made (or rotated); if so its secret is lost: Change a key rotates it"; err == nil || err.Error() != want {
		t.Errorf("runner error: err %v, want %q", err, want)
	}
}

// changeShapes: the answers of the changes C60 makes, and the members it
// reads, through the real CLI. Provider mock is on a closed port: nothing
// here calls it.
func changeShapes(t *testing.T, px *Proxy) {
	t.Helper()
	ctx := context.Background()
	call := func(reply Reply, words string, flags []string, positionals ...string) {
		t.Helper()
		if err := px.Call(ctx, reply, words, flags, positionals...); err != nil {
			t.Fatalf("%s: %v", words, err)
		}
	}
	call(&ProviderChange{}, "providers add", []string{"--protocol", "openai", "--base-url", "http://127.0.0.1:9/v1",
		"--allow-private-network", "--billing", "none"}, "mock")
	var added TargetChange
	call(&added, "targets add", []string{"--kind", "chat", "--level", "low", "--level", "high", "--can-disable"}, "mock/m")
	if r := added.Resource.Reasoning; added.Resource.ID != "mock/m" || r == nil || !slices.Equal(r.Levels, []string{"low", "high"}) ||
		r.CanDisable == nil || !*r.CanDisable || added.Findings == nil {
		t.Errorf("targets add mock/m = %+v", added)
	}
	var targets TargetsReply
	call(&targets, "targets list", nil)
	if i := slices.IndexFunc(targets.Targets, func(x Target) bool { return x.ID == "mock/m" }); i < 0 || targets.Targets[i].VerifiedAt != 0 {
		t.Errorf("targets list = %+v", targets.Targets)
	}
	var set Change
	call(&set, "providers set", []string{"--total-ms", "9000"}, "mock")
	var providers ProvidersReply
	call(&providers, "providers list", nil)
	if i := slices.IndexFunc(providers.Providers, func(p Provider) bool { return p.Name == "mock" }); i < 0 ||
		providers.Providers[i].Timeouts.TotalMS != 9000 {
		t.Errorf("providers list = %+v", providers.Providers)
	}
	call(&Change{}, "models add", []string{"--target", "mock/m", "--max-output-tokens", "100", "--effort", "low", "--effort-mode", "default"}, "x")
	var models ModelsReply
	call(&models, "models list", nil)
	if i := slices.IndexFunc(models.Models, func(m Model) bool { return m.Name == "x" }); i < 0 ||
		models.Models[i].MaxOutputTokens != 100 || models.Models[i].Reasoning.Effort != "low" {
		t.Errorf("models list = %+v", models.Models)
	}
	var explained ExplainReply
	call(&explained, "models explain", nil, "x")
	if len(explained.Targets) != 1 || explained.Targets[0].Target != "mock/m" {
		t.Errorf("models explain x = %+v", explained)
	}
	for _, c := range []struct {
		words, name string
	}{{"models remove", "x"}, {"targets remove", "mock/m"}, {"providers remove", "mock"}} {
		var removed Change
		call(&removed, c.words, []string{"--yes"}, c.name)
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
		if err == nil || err.Error() != want || !errors.Is(err, context.Canceled) {
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

// cliDo runs one CLI command in process against env, and fails on a non-zero
// exit.
func cliDo(t *testing.T, env map[string]string, args ...string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := cli.Main(args, func(k string) string { return env[k] }, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("%q: exit %d: %s%s", args, code, out.String(), errOut.String())
	}
	return out.String()
}

// hasFinding reports whether findings hold one of code on resource, of
// severity when it is not "".
func hasFinding(findings []Finding, severity, code, resource string) bool {
	return slices.ContainsFunc(findings, func(f Finding) bool {
		return f.Code == code && f.Resource == resource && (severity == "" || f.Severity == severity)
	})
}

// TestDryRunNeedsPrices: under a budget, a new pay-per-use target without
// prices is the refusal's unpriced_under_budget, returned as a finding; with
// prices it is not; nothing is written either way.
func TestDryRunNeedsPrices(t *testing.T) {
	env := admintest.Server(t)
	cliDo(t, env, "settings", "set", "--budget", "limit_usd=1,reset_interval=never")
	cliDo(t, env, "providers", "add", "acme", "--protocol", "openai", "--base-url", "https://api.acme.example/v1", "--billing", "pay_per_use")
	before := cliDo(t, env, "export", "--json")
	px := New(cliRunner(env))
	decl := AliasDecl{}.With("targets", []string{"acme/m1"})
	findings, err := px.DryRun(context.Background(), []NewTarget{{ID: "acme/m1"}}, "x", decl)
	if err != nil || !hasFinding(findings, "error", "unpriced_under_budget", "targets/acme/m1") {
		t.Fatalf("unpriced: %+v, %v; want unpriced_under_budget on targets/acme/m1", findings, err)
	}
	today := time.Now().Format(time.DateOnly)
	findings, err = px.DryRun(context.Background(), []NewTarget{{ID: "acme/m1", Pricing: &Pricing{In: 1.5, Out: 6, AsOf: today}}}, "x", decl)
	if err != nil || hasFinding(findings, "", "unpriced_under_budget", "targets/acme/m1") {
		t.Fatalf("priced: %+v, %v; want no unpriced_under_budget", findings, err)
	}
	if after := cliDo(t, env, "export", "--json"); after != before {
		t.Fatalf("a dry run changed the configuration:\n%s\nwas\n%s", after, before)
	}
}

// teeRunner is run that also keeps the last answer.
func teeRunner(run Runner, last *[]byte) Runner {
	return func(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		var out bytes.Buffer
		code, err := run(ctx, args, stdin, io.MultiWriter(stdout, &out), stderr)
		*last = out.Bytes()
		return code, err
	}
}

// TestDryRunWholeAlias: an alias's whole declaration, as models show gives
// it, applied in a dry run changes nothing, so its budget, effort and cap
// survive the round trip and judge the scopes as before.
func TestDryRunWholeAlias(t *testing.T) {
	env := admintest.Server(t)
	cliDo(t, env, "providers", "add", "mock", "--protocol", "openai", "--base-url", "http://127.0.0.1:9/v1",
		"--allow-private-network", "--billing", "pay_per_use")
	cliDo(t, env, "targets", "add", "mock/m", "--kind", "chat", "--level", "low", "--level", "high",
		"--pricing", "input_per_million=1,output_per_million=2,as_of=2026-11-01")
	cliDo(t, env, "models", "add", "a", "--target", "mock/m", "--max-output-tokens", "100", "--effort", "low",
		"--effort-mode", "default", "--budget", "limit_usd=2,reset_interval=never")
	var last []byte
	px := New(teeRunner(cliRunner(env), &last))
	var decl AliasDecl
	if err := px.Call(context.Background(), &decl, "models show", nil, "a"); err != nil {
		t.Fatal(err)
	}
	findings, err := px.DryRun(context.Background(), nil, "a", decl.With("targets", []string{"mock/m"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		if f.Severity == "error" || (strings.HasPrefix(f.Code, "unpriced") && f.Resource == "targets/mock/m") {
			t.Errorf("finding %+v", f)
		}
	}
	var answer struct {
		Diff []any `json:"diff"`
	}
	if err := json.Unmarshal(last, &answer); err != nil || len(answer.Diff) != 0 {
		t.Fatalf("diff %v, %v; want empty:\n%s", answer.Diff, err, last)
	}
}

// TestDryRunOutputCap: a new target of an anthropic provider, in an alias
// without a cap, is the refusal's output_cap_required.
func TestDryRunOutputCap(t *testing.T) {
	env := admintest.Server(t)
	cliDo(t, env, "providers", "add", "acme", "--protocol", "anthropic", "--base-url", "https://api.acme.example", "--billing", "none")
	findings, err := New(cliRunner(env)).DryRun(context.Background(), []NewTarget{{ID: "acme/m"}}, "x",
		AliasDecl{}.With("targets", []string{"acme/m"}))
	if err != nil || !slices.ContainsFunc(findings, func(f Finding) bool { return f.Code == "output_cap_required" && f.Severity == "error" }) {
		t.Fatalf("%+v, %v; want output_cap_required", findings, err)
	}
}

// TestDryRunOtherFailure: an exit that is no refusal follows Call's rule,
// naming apply and never quoting the output.
func TestDryRunOtherFailure(t *testing.T) {
	for _, tc := range []struct {
		out  string
		code int
	}{{"usage: sk-hand-edited-secret\n", 2}, {"sk-hand-edited-secret\n", 1}, {`{"error":{"message":"no","findings":[]}}`, 1}} {
		_, err := New(canned(tc.out, tc.code, nil)).DryRun(context.Background(), nil, "x", AliasDecl{}.With("targets", []string{"a/m"}))
		if err == nil || !strings.HasPrefix(err.Error(), "stargaite apply: ") || strings.Contains(err.Error(), "sk-") {
			t.Errorf("%q exit %d: err = %v", tc.out, tc.code, err)
		}
	}
}

// TestDryRunStdin: the dry run's argv, and the document it sends on standard
// input: version 1, the new targets with their prices, the alias without its
// name.
func TestDryRunStdin(t *testing.T) {
	var argv [][]string
	var stdins []string
	run := func(_ context.Context, args []string, stdin io.Reader, stdout, _ io.Writer) (int, error) {
		argv = append(argv, args)
		data, _ := io.ReadAll(stdin)
		stdins = append(stdins, string(data))
		_, _ = io.WriteString(stdout, `{"revision":1,"changed":false,"diff":[],"findings":[]}`)
		return 0, nil
	}
	px := New(run)
	today := time.Now().Format(time.DateOnly)
	decl := AliasDecl{}.With("name", "x").With("targets", []string{"acme/a", "acme/b"}).With("max_output_tokens", 100)
	targets := []NewTarget{{ID: "acme/a", Pricing: &Pricing{In: 1.5, Out: 6, AsOf: today}}, {ID: "acme/b"}}
	if _, err := px.DryRun(context.Background(), targets, "x", decl); err != nil {
		t.Fatal(err)
	}
	if _, err := px.DryRun(context.Background(), nil, "x", decl); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"apply", "--json", "--dry-run", "-f", "-"}, {"apply", "--json", "--dry-run", "-f", "-"}}
	if !slices.EqualFunc(argv, want, slices.Equal) {
		t.Fatalf("argv = %q, want %q", argv, want)
	}
	for i, wantDoc := range []string{
		`{"version":1,"targets":{"acme/a":{"kind":"chat","pricing":{"input_per_million":1.5,"output_per_million":6,"as_of":"` + today + `"}},` +
			`"acme/b":{"kind":"chat"}},"models":{"x":{"targets":["acme/a","acme/b"],"max_output_tokens":100}}}`,
		`{"version":1,"models":{"x":{"targets":["acme/a","acme/b"],"max_output_tokens":100}}}`,
	} {
		var got, wanted any
		if err := json.Unmarshal([]byte(stdins[i]), &got); err != nil {
			t.Fatalf("stdin %d is not JSON: %v: %s", i, err, stdins[i])
		}
		_ = json.Unmarshal([]byte(wantDoc), &wanted)
		if !reflect.DeepEqual(got, wanted) {
			t.Errorf("stdin %d = %s\nwant %s", i, stdins[i], wantDoc)
		}
	}
	if decl["name"] == nil {
		t.Error("DryRun changed the caller's declaration")
	}
}

// TestTelemetryTest: an answer of exit 0 or 1 that decodes is the reply,
// whatever its outcome, and Line says it in the CLI's words; an error
// envelope and an exit 2 follow Call's rule; a message is cleaned.
func TestTelemetryTest(t *testing.T) {
	for _, tc := range []struct {
		out  string
		code int
		line string
	}{
		{`{"outcome":"accepted","status":200,"trace_id":"4bf92f3577b34da6a3ce929d0e0e4736"}`, 0,
			"credentials: resolved · reachable: yes · authorized: yes · span: accepted (200) · trace_id=4bf92f3577b34da6a3ce929d0e0e4736"},
		{`{"outcome":"unauthorized","status":401,"message":"bad credentials"}`, 1,
			"credentials: resolved · reachable: yes · authorized: no (401) · bad credentials"},
		{`{"outcome":"credential_unresolved","message":"X does not resolve"}`, 1, "credentials: unresolved · X does not resolve"},
		{`{"outcome":"unreachable","message":"dial tcp 192.0.2.1:4318: connect: connection refused"}`, 1,
			"credentials: resolved · reachable: no · dial tcp 192.0.2.1:4318: connect: connection refused"},
		{`{"outcome":"not_found","status":404}`, 1, "credentials: resolved · reachable: yes · span: not_found (404)"},
		{`{"outcome":"rejected","status":400,"message":"bad\u001b[2J span"}`, 1,
			"credentials: resolved · reachable: yes · authorized: yes · span: rejected (400) · bad[2J span"},
	} {
		var seen [][]string
		reply, err := New(canned(tc.out, tc.code, &seen)).TelemetryTest(context.Background())
		if err != nil || reply.Line() != tc.line {
			t.Errorf("%s exit %d: %v, line %q, want %q", tc.out, tc.code, err, lineOf(reply), tc.line)
		}
		if len(seen) != 1 || !slices.Equal(seen[0], []string{"telemetry", "test", "--json"}) {
			t.Errorf("argv %q", seen)
		}
	}
	for _, tc := range []struct {
		out  string
		code int
		want string
	}{
		{`{"error":{"message":"no exporter is configured: telemetry set --otlp-endpoint <url> sets one"}}`, 1,
			"stargaite telemetry test: no exporter is configured: telemetry set --otlp-endpoint <url> sets one"},
		{"usage: sk-hand-edited-secret\n", 2, "stargaite telemetry test: failed with exit 2"},
		{`{"outcome":"accepted"}`, 2, "stargaite telemetry test: failed with exit 2"},
	} {
		if reply, err := New(canned(tc.out, tc.code, nil)).TelemetryTest(context.Background()); err == nil || err.Error() != tc.want {
			t.Errorf("%q exit %d: %+v, err = %v, want %q", tc.out, tc.code, reply, err, tc.want)
		}
	}
	// The real CLI, with no exporter set.
	_, err := New(cliRunner(admintest.Server(t))).TelemetryTest(context.Background())
	if err == nil || !strings.HasPrefix(err.Error(), "stargaite telemetry test: no exporter is configured") {
		t.Errorf("no exporter: err = %v", err)
	}
}

// lineOf is reply's Line, "" for no reply.
func lineOf(reply *TelemetryTestReply) string {
	if reply == nil {
		return ""
	}
	return reply.Line()
}

// TestUsageReplyDecodes: the answer of usage show decodes, a group without
// a cost with a nil CostUSD; an answer without groups fails naming it; the
// real CLI's answer decodes.
func TestUsageReplyDecodes(t *testing.T) {
	cost := 0.0000062
	report := usage.Report{Source: "records", Since: time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC), Until: time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC),
		Groups: []usage.Group{{Billing: "pay_per_use", Key: "k", Requests: 2, Errors: 1, Input: 10, Output: 20, CostUSD: &cost},
			{Billing: "none", Key: "j", Requests: 1, Input: 3, Output: 4}}}
	out, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var reply UsageReply
	if err := New(canned(string(out), 0, nil)).Call(context.Background(), &reply, "usage show", nil); err != nil {
		t.Fatal(err)
	}
	want := UsageReply{Source: "records", Since: "2026-11-20T00:00:00Z", Until: "2026-11-20T08:00:00Z", Groups: []UsageGroup{
		{Billing: "pay_per_use", Key: "k", Requests: 2, Errors: 1, Input: 10, Output: 20, CostUSD: &cost}, {Billing: "none", Key: "j", Requests: 1, Input: 3, Output: 4}}}
	if !reflect.DeepEqual(reply, want) {
		t.Errorf("reply = %+v, want %+v", reply, want)
	}
	err = New(canned(`{"source":"records","since":"x","until":"y"}`, 0, nil)).Call(context.Background(), &UsageReply{}, "usage show", nil)
	if err == nil || err.Error() != `stargaite usage show: the answer has no "groups": `+stale {
		t.Errorf("err = %v", err)
	}
	reply = UsageReply{}
	if err := New(cliRunner(admintest.Server(t))).Call(context.Background(), &reply, "usage show", []string{"--since", "24h", "--by", "key"}); err != nil ||
		reply.Source != "records" || len(reply.Groups) != 0 {
		t.Errorf("the real CLI: %+v, %v", reply, err)
	}
}
