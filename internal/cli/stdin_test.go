package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/jorlaru/stargaite/internal/config"
)

// unread is a standard input no command may read: every test but TestStdin
// hands it to Main, so a command that reads standard input without a file
// argument "-" fails them (AC-41).
type unread struct{}

func (unread) Read([]byte) (int, error) { panic("standard input was read") }

// terminal is standard input on a terminal: a character device, as os.Stdin
// reports one, which no command may read.
type terminal struct{ unread }

func (terminal) Stat() (os.FileInfo, error) { return os.Stat(os.DevNull) }

// broken is standard input that fails when read.
type broken struct{}

func (broken) Read([]byte) (int, error) { return 0, errors.New("the pipe broke") }

// endless is standard input that never ends; n counts the bytes read.
type endless struct{ n *int }

func (e endless) Read(p []byte) (int, error) {
	*e.n += len(p)
	return len(p), nil
}

func withStdin(vars map[string]string, stdin io.Reader, args ...string) run {
	var stdout, stderr bytes.Buffer
	code := Main(args, getenv(vars), stdin, &stdout, &stderr)
	return run{code, stdout.String(), stderr.String()}
}

// TestStdin: the literal "-" of apply -f, validate -f and models explain
// --body reads standard input whole, as the file it stands for would be
// read (OPS-3); apply publishes it only when it ends with the YAML end
// marker (TestStdinEndMarker). A terminal is refused without being read, and
// an empty input is refused naming docker compose exec -T: both exit 2,
// before any call. A failed read is a failure, and standard input is read no
// further than a file would be. A regular file and the path /dev/stdin
// behave as before: only a file is read, and standard input is not (the
// controls).
func TestStdin(t *testing.T) {
	got, vars := recorder(t, http.StatusOK, "{}\n")
	yaml := "version: 1\nsettings: { max_parallel_requests: 3 }\n"
	body := "{\"model\": \"fast\",\n \"messages\": [{\"role\": \"user\", \"content\": \"hi\"}]}\n"
	explained, err := config.Encode(map[string]any{"body": body})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, input         string
		args                []string
		target, contentType string
		want                string // the body the server receives
	}{
		{"apply -f -", yaml + "...\n", []string{"apply", "-f", "-"}, "/admin/v1/apply?dry_run=false&prune=false", "application/yaml", yaml + "...\n"},
		{"apply -f - --dry-run", yaml, []string{"apply", "-f", "-", "--dry-run"}, "/admin/v1/apply?dry_run=true&prune=false", "application/yaml", yaml},
		{"models explain --body -", body, []string{"models", "explain", "fast", "--body", "-"}, "/admin/v1/models/fast/explain", "application/json", string(explained)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			*got = recorded{}
			if r := withStdin(vars, strings.NewReader(tc.input), tc.args...); r.code != 0 {
				t.Fatalf("exit %d; stderr %q", r.code, r.stderr)
			}
			if got.method != http.MethodPost || got.target != tc.target || got.contentType != tc.contentType || got.body != tc.want {
				t.Errorf("sent %s %s (%s)\n%s\nwant %s (%s)\n%s", got.method, got.target, got.contentType, got.body, tc.target, tc.contentType, tc.want)
			}
		})
	}

	t.Run("validate -f - checks what validate -f <file> checks", func(t *testing.T) {
		for _, content := range []string{yaml, "version: 2\n", "version: [\n"} {
			file := validate(t, nil, "-f", writeFile(t, content), "--json")
			piped := withStdin(nil, strings.NewReader(content), "validate", "-f", "-", "--json")
			if piped != file || file.stdout == "" {
				t.Errorf("%q: piped %+v, from a file %+v", content, piped, file)
			}
		}
	})

	for _, tc := range []struct {
		name  string
		stdin io.Reader
		code  int
		says  []string
	}{
		{"a terminal", terminal{}, 2, []string{"terminal", "docker compose exec -T", "docker exec -i"}},
		{"an empty input", strings.NewReader(""), 2, []string{"empty", "docker compose exec", "-T"}},
		{"a failed read", broken{}, 1, []string{"the pipe broke"}},
	} {
		for _, args := range [][]string{{"apply", "-f", "-"}, {"models", "explain", "fast", "--body", "-"}, {"validate", "-f", "-"}} {
			t.Run(tc.name+": "+strings.Join(args, " "), func(t *testing.T) {
				*got = recorded{}
				r := withStdin(vars, tc.stdin, args...)
				if r.code != tc.code || got.method != "" {
					t.Fatalf("exit %d, want %d; request %+v; stderr %q", r.code, tc.code, got, r.stderr)
				}
				out := r.stderr + r.stdout // validate reports a failed read as its finding, on stdout
				for _, s := range tc.says {
					if !strings.Contains(out, s) {
						t.Errorf("the output does not say %q: %q", s, out)
					}
				}
			})
		}
	}

	t.Run("standard input is read no further than a file", func(t *testing.T) {
		n := 0
		if r := withStdin(nil, endless{&n}, "validate", "-f", "-"); r.code != 1 || n != config.MaxYAMLBytes+1 {
			t.Errorf("exit %d after %d bytes, want 1 after %d; stdout %q", r.code, n, config.MaxYAMLBytes+1, r.stdout)
		}
	})

	// Controls: a regular file is read as before, and /dev/stdin is a path
	// like any other, refused when it is not a regular file.
	t.Run("a regular file", func(t *testing.T) {
		*got = recorded{}
		if r := withStdin(vars, unread{}, "apply", "-f", writeFile(t, yaml)); r.code != 0 || got.body != yaml {
			t.Errorf("exit %d, sent %q; stderr %q", r.code, got.body, r.stderr)
		}
	})
	t.Run("--body /dev/stdin", func(t *testing.T) {
		*got = recorded{}
		if r := withStdin(vars, unread{}, "models", "explain", "fast", "--body", "/dev/stdin"); r.code != 1 || got.method != "" {
			t.Errorf("exit %d, request %+v; stderr %q", r.code, got, r.stderr)
		}
	})
}

// TestStdinPrune: apply -f - --prune exits 2 unless it is a --dry-run, before
// standard input is read and whether --yes is given: a pipe cut short is
// still valid YAML, and would remove what its missing part declares. A dry
// run reads it and sends prune, and a file may still be pruned (control).
func TestStdinPrune(t *testing.T) {
	got, vars := recorder(t, http.StatusOK, "{}\n")
	yaml := "version: 1\n"
	for _, args := range [][]string{{"apply", "-f", "-", "--prune"}, {"apply", "-f", "-", "--prune", "--yes"}, {"apply", "--prune", "--yes", "-f", "-", "--json"}} {
		*got = recorded{}
		if r := withStdin(vars, unread{}, args...); r.code != 2 || got.method != "" || r.stdout != "" || !strings.Contains(r.stderr, "--dry-run") {
			t.Errorf("%q: exit %d, request %+v; stdout %q stderr %q", args, r.code, got, r.stdout, r.stderr)
		}
	}
	*got = recorded{}
	if r := withStdin(vars, strings.NewReader(yaml), "apply", "-f", "-", "--prune", "--dry-run"); r.code != 0 ||
		got.target != "/admin/v1/apply?dry_run=true&prune=true" || got.body != yaml {
		t.Errorf("a dry run: exit %d, sent %s %q; stderr %q", r.code, got.target, got.body, r.stderr)
	}
	*got = recorded{}
	if r := withStdin(vars, unread{}, "apply", "-f", writeFile(t, yaml), "--prune", "--yes"); r.code != 0 || got.target != "/admin/v1/apply?dry_run=false&prune=true" {
		t.Errorf("a file: exit %d, sent %s; stderr %q", r.code, got.target, r.stderr)
	}
}

// TestStdinEndMarker: apply -f - publishes only a whole document (OPS-3).
// config.example.yaml cut after the models of invoice-agent (byte 4280) is
// still valid YAML, which would leave that key without its budget and limits:
// it exits 2 and nothing is published. The same input ending with the end
// marker is published as it stands, and the export piped back, which ends
// with the marker, reproduces the instance (AC-39). An input above the size
// bound is refused for its size, not for a marker. Controls: a dry run of
// the cut input still shows its difference, and the JSON export, which a cut
// leaves unclosed, is applied without a marker and refused when cut short.
func TestStdinEndMarker(t *testing.T) {
	example, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	const at = "invoice-agent:\n    models: [fast, deep]\n"
	cut := string(example[:bytes.Index(example, []byte(at))+len(at)])

	vars := adminServer(t)
	empty := runMain(vars, "export").stdout
	if r := withStdin(vars, strings.NewReader(cut), "apply", "-f", "-", "--dry-run"); r.code != 0 || !strings.Contains(r.stdout, "invoice-agent") {
		t.Errorf("a dry run of the cut input: exit %d; stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	if r := withStdin(vars, strings.NewReader(cut), "apply", "-f", "-"); r.code != 2 || r.stdout != "" || !strings.Contains(r.stderr, "end marker ...") {
		t.Errorf("the cut input: exit %d; stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	n := 0
	if r := withStdin(vars, endless{&n}, "apply", "-f", "-"); r.code == 0 || !strings.Contains(r.stderr, "larger than") || strings.Contains(r.stderr, "end marker") {
		t.Errorf("an input above the size bound: exit %d; stderr %q", r.code, r.stderr)
	}
	if got := runMain(vars, "export").stdout; got != empty {
		t.Errorf("published: %s", got)
	}
	if r := withStdin(vars, strings.NewReader(cut+"...\n"), "apply", "-f", "-"); r.code != 0 {
		t.Fatalf("the cut input with the marker: exit %d; stderr %q", r.code, r.stderr)
	}
	if keys := string(exported(t, vars)["keys"]); !strings.Contains(keys, `"invoice-agent":{"models":["fast","deep"]}`) {
		t.Errorf("keys %s", keys)
	}

	source := adminServer(t)
	if r := runMain(source, "apply", "-f", "../../config.example.yaml"); r.code != 0 {
		t.Fatalf("apply: exit %d, stderr %s", r.code, r.stderr)
	}
	want := exported(t, source)
	for _, format := range [][]string{{"export"}, {"export", "--json"}} {
		out := runMain(source, format...).stdout
		if format[len(format)-1] == "export" && !strings.HasSuffix(out, "\n...\n") {
			t.Errorf("export does not end with the marker: %q", out[max(0, len(out)-40):])
		}
		piped := adminServer(t)
		if format[len(format)-1] == "--json" {
			if r := withStdin(piped, strings.NewReader(out[:len(out)/2]), "apply", "-f", "-"); r.code == 0 || runMain(piped, "export").stdout != empty {
				t.Errorf("the JSON export cut short | apply -f -: exit %d; stderr %q", r.code, r.stderr)
			}
		}
		if r := withStdin(piped, strings.NewReader(out), "apply", "-f", "-"); r.code != 0 {
			t.Fatalf("%q | apply -f -: exit %d, stderr %s", format, r.code, r.stderr)
		}
		if got := exported(t, piped); !maps.EqualFunc(got, want, func(a, b json.RawMessage) bool { return bytes.Equal(a, b) }) || len(got["keys"]) < 100 {
			t.Errorf("%q | apply -f -:\n%v\nwant\n%v", format, got, want)
		}
	}
}
