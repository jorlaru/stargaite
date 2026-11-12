package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/domain"
)

// No test reaches the network: a lookup nobody answered fails.
func init() {
	resolve = func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("no network in tests") }
}

// answer makes resolve give addr for every name, for one test.
func answer(t *testing.T, addr string) {
	saved := resolve
	resolve = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr(addr)}, nil
	}
	t.Cleanup(func() { resolve = saved })
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// annexBEnv sets every credential config.example.yaml references.
var annexBEnv = map[string]string{
	"OPENROUTER_API_KEY": "x", "ANTHROPIC_API_KEY": "x", "MINIMAX_PLAN_API_KEY": "x",
	"LANGFUSE_PUBLIC_KEY": "x", "LANGFUSE_SECRET_KEY": "x",
}

const internalProvider = `version: 1
providers:
  vllm:
    protocol: openai
    base_url: https://vllm.internal.example/v1
    billing: none
`

type run struct {
	code           int
	stdout, stderr string
}

func validate(t *testing.T, env map[string]string, args ...string) run {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Main(append([]string{"validate"}, args...), func(n string) string { return env[n] }, &stdout, &stderr)
	return run{code, stdout.String(), stderr.String()}
}

func findingsOf(t *testing.T, out string) []domain.Finding {
	t.Helper()
	var body struct {
		Findings []domain.Finding `json:"findings"`
	}
	dec := json.NewDecoder(strings.NewReader(out))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || dec.More() {
		t.Fatalf("stdout is not one findings object (%v): %q", err, out)
	}
	if body.Findings == nil {
		t.Fatalf("findings is not a list: %q", out)
	}
	return body.Findings
}

func codes(fs []domain.Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Severity+" "+f.Code+" "+f.Resource+" "+f.Field)
	}
	return out
}

func TestValidateFileExitCodes(t *testing.T) {
	answer(t, "8.8.8.8")
	example := "../../config.example.yaml"
	for _, tt := range []struct {
		name string
		env  map[string]string
		args []string
		code int
	}{
		{"annex B with every credential", annexBEnv, []string{"-f", example}, 0},
		{"annex B, flag written with two dashes", annexBEnv, []string{"--f=" + example, "--json"}, 0},
		{"warnings only", nil, []string{"-f", example}, 0},
		{"an error finding", nil, []string{"-f", writeFile(t, "version: 2\n")}, 1},
		{"a file that does not exist", nil, []string{"-f", filepath.Join(t.TempDir(), "none.yaml")}, 1},
		{"a directory", nil, []string{"-f", t.TempDir()}, 1},
		{"standard input is never read", nil, []string{"-f", "/dev/stdin"}, 1},
		{"invalid YAML", nil, []string{"-f", writeFile(t, "version: [\n")}, 1},
		{"an unknown member", nil, []string{"-f", writeFile(t, "version: 1\nsetting: {}\n")}, 1},
		{"--env-file without -f", nil, []string{"--env-file", "x"}, 2}, // without -f, validate asks the server
		{"an empty -f", nil, []string{"-f", ""}, 2},
		{"-f without its value", nil, []string{"-f"}, 2},
		{"a positional", nil, []string{"-f", example, "extra"}, 2},
		{"an unknown flag", nil, []string{"-f", example, "--file", "x"}, 2},
	} {
		for _, asJSON := range []bool{false, true} {
			args := tt.args
			if asJSON {
				args = append([]string{"--json"}, args...) // first, so that a flag missing its value stays missing
			}
			t.Run(tt.name, func(t *testing.T) {
				r := validate(t, tt.env, args...)
				if r.code != tt.code {
					t.Fatalf("exit %d, want %d; stdout %q stderr %q", r.code, tt.code, r.stdout, r.stderr)
				}
				switch {
				case r.code == 2:
					if r.stdout != "" || r.stderr == "" {
						t.Errorf("wrong usage writes to stderr only: stdout %q stderr %q", r.stdout, r.stderr)
					}
				case asJSON:
					fs := findingsOf(t, r.stdout)
					if hasError := strings.Contains(r.stdout, `"severity":"error"`); hasError != (r.code == 1) {
						t.Errorf("exit %d with findings %q", r.code, codes(fs))
					}
					// Refused before any read, not for what a read gave (PRI-8).
					if tt.name == "a directory" || tt.name == "standard input is never read" {
						if len(fs) != 1 || fs[0].Code != "file_unreadable" {
							t.Errorf("findings %q, want one file_unreadable", codes(fs))
						}
					}
				case r.stdout == "":
					t.Error("nothing on stdout")
				}
				if r.code == 0 && r.stderr != "" {
					t.Errorf("stderr %q on success", r.stderr)
				}
			})
		}
	}
}

// --json prints exactly the findings object that the checks give.
func TestValidateFileJSON(t *testing.T) {
	answer(t, "8.8.8.8")
	envFile := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(envFile, []byte("ANTHROPIC_API_KEY=x\nnot a line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := validate(t, map[string]string{"OPENROUTER_API_KEY": "x"}, "-f", "../../config.example.yaml", "--env-file", envFile, "--json")
	if r.code != 0 {
		t.Fatalf("exit %d: %q %q", r.code, r.stdout, r.stderr)
	}
	got := findingsOf(t, r.stdout)

	data, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc domain.Document
	if err := config.DecodeYAML(data, &doc); err != nil {
		t.Fatal(err)
	}
	env := config.NewEnv(func(n string) (string, bool) { return "x", n == "OPENROUTER_API_KEY" }, envFile, resolve)
	want := append(config.Validate(&doc), config.CheckResolved(context.Background(), &doc, config.OutboundURLs(&doc), env)...)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got\n%+v\nwant\n%+v", got, want)
	}
	// The env file resolved one credential and warned about one line; the
	// process environment resolved another; three stay unresolved.
	wantCodes := []string{
		"warning env_file_malformed env_file ",
		"warning credential_unresolved providers/minimax-plan api_key_env",
		"warning credential_unresolved settings telemetry.basic_auth_env",
		"warning credential_unresolved settings telemetry.basic_auth_env",
	}
	if !reflect.DeepEqual(codes(got), wantCodes) {
		t.Errorf("findings %q, want %q", codes(got), wantCodes)
	}
}

// OPS-6: an unresolved credential is reported by validate -f.
func TestValidateFileUnresolvedCredential(t *testing.T) {
	answer(t, "8.8.8.8")
	path := writeFile(t, internalProvider+"    api_key_env: VLLM_KEY\n")
	r := validate(t, nil, "-f", path)
	if r.code != 0 || r.stdout != "warning OPS-6 providers/vllm api_key_env: VLLM_KEY does not resolve\n" {
		t.Errorf("exit %d, stdout %q", r.code, r.stdout)
	}
	if r := validate(t, map[string]string{"VLLM_KEY": "x"}, "-f", path); r.code != 0 || r.stdout != "no findings\n" {
		t.Errorf("with the variable set: exit %d, stdout %q", r.code, r.stdout)
	}
}

// SEC-4: a name that resolves to a private address is refused without
// allow_private_network and accepted with it.
func TestValidateFilePrivateAddress(t *testing.T) {
	answer(t, "10.1.2.3")
	r := validate(t, nil, "-f", writeFile(t, internalProvider), "--json")
	if got := codes(findingsOf(t, r.stdout)); r.code != 1 || !reflect.DeepEqual(got, []string{"error url_refused providers/vllm base_url"}) {
		t.Errorf("without the flag: exit %d, findings %q", r.code, got)
	}
	r = validate(t, nil, "-f", writeFile(t, internalProvider+"    allow_private_network: true\n"), "--json")
	if got := findingsOf(t, r.stdout); r.code != 0 || len(got) != 0 {
		t.Errorf("with the flag: exit %d, findings %q", r.code, codes(got))
	}
	answer(t, "169.254.169.254")
	r = validate(t, nil, "-f", writeFile(t, internalProvider+"    allow_private_network: true\n"), "--json")
	if got := codes(findingsOf(t, r.stdout)); r.code != 1 || !reflect.DeepEqual(got, []string{"error url_refused providers/vllm base_url"}) {
		t.Errorf("metadata address with the flag: exit %d, findings %q", r.code, got)
	}
}

// A name a file chose can neither add a line to the text output nor send an
// escape to the terminal: each finding stays one line.
func TestValidateFileOneLinePerFinding(t *testing.T) {
	path := writeFile(t, internalProvider+"targets:\n  \"vllm/x\\n::error::injected\\e[31m\": {kind: chat}\n")
	r := validate(t, nil, "-f", path, "--json")
	n := len(findingsOf(t, r.stdout))
	r = validate(t, nil, "-f", path)
	lines := strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")
	if r.code != 1 || n == 0 || len(lines) != n || strings.Contains(r.stdout, "\x1b") {
		t.Fatalf("exit %d, %d findings, stdout %q", r.code, n, r.stdout)
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "error ") && !strings.HasPrefix(l, "warning ") {
			t.Errorf("line %q is not a finding", l)
		}
	}

	// The path given with -f reaches the message of the finding.
	r = validate(t, nil, "-f", filepath.Join(t.TempDir(), "a\n::error::b"))
	if r.code != 1 || strings.Count(r.stdout, "\n") != 1 {
		t.Errorf("missing file with a line break in its path: exit %d, stdout %q", r.code, r.stdout)
	}
}
