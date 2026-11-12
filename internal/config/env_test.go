package config

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// planted marks every byte of an env file that must stay out of a finding:
// values, and the text of lines that are not understood.
const planted = "PLANTED"

func writeEnvFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// messages checks what every finding of an env file has in common and
// returns "code: message" for each.
func messages(t *testing.T, env Env) []string {
	t.Helper()
	var out []string
	for _, f := range env.Findings {
		if f.Severity != "warning" || f.Rule != "OPS-6" || f.Resource != "env_file" || f.Field != "" {
			t.Errorf("finding %+v, want a warning of OPS-6 on env_file", f)
		}
		if strings.Contains(fmt.Sprintf("%+v", f), planted) {
			t.Errorf("finding %+v carries content of the file", f)
		}
		out = append(out, f.Code+": "+f.Message)
	}
	return out
}

func noEnviron(string) (string, bool) { return "", false }

func TestEnvFile(t *testing.T) {
	const malformed = "env_file_malformed: line %d of the env file is not NAME=value and was skipped"
	const quoted = "env_file_quoted: line %d of the env file has its value in quotes, and the quotes are part of the value"
	line := func(format string, n int) string { return fmt.Sprintf(format, n) }

	tests := []struct {
		name     string
		content  string
		vars     map[string]string
		findings []string
	}{
		{name: "empty file", content: "", vars: map[string]string{}},
		{name: "plain lines", content: "A=PLANTED-1\nB_2=PLANTED-2\n_c=PLANTED-3\n",
			vars: map[string]string{"A": "PLANTED-1", "B_2": "PLANTED-2", "_c": "PLANTED-3"}},
		{name: "no line end on the last line", content: "A=PLANTED-1\nB=PLANTED-2",
			vars: map[string]string{"A": "PLANTED-1", "B": "PLANTED-2"}},
		{name: "CRLF line ends", content: "A=PLANTED-1\r\n\r\n# c\r\nB=PLANTED-2\r\n",
			vars: map[string]string{"A": "PLANTED-1", "B": "PLANTED-2"}},
		{name: "byte order mark", content: "\xef\xbb\xbfA=PLANTED-1\nB=PLANTED-2\n",
			vars: map[string]string{"A": "PLANTED-1", "B": "PLANTED-2"}},
		{name: "byte order mark anywhere else", content: "A=PLANTED-1\n\xef\xbb\xbfB=PLANTED-2\n",
			vars: map[string]string{"A": "PLANTED-1"}, findings: []string{line(malformed, 2)}},
		{name: "blank lines", content: "\n\nA=PLANTED-1\n   \n\t\nB=PLANTED-2\n\n",
			vars: map[string]string{"A": "PLANTED-1", "B": "PLANTED-2"}},
		{name: "comments", content: "# PLANTED\n  # PLANTED=x\n#A=PLANTED\nA=PLANTED-1\n",
			vars: map[string]string{"A": "PLANTED-1"}},
		{name: "a number sign inside a value belongs to it", content: "A=PLANTED#1\nB=PLANTED # not a comment\n",
			vars: map[string]string{"A": "PLANTED#1", "B": "PLANTED # not a comment"}},
		{name: "spaces around the equals sign and the line", content: "  A = PLANTED-1  \n\tB\t=\tPLANTED 2\t\n",
			vars: map[string]string{"A": "PLANTED-1", "B": "PLANTED 2"}},
		{name: "empty value", content: "A=\nB=   \n",
			vars: map[string]string{"A": "", "B": ""}},
		{name: "an equals sign inside a value belongs to it", content: "A=PLANTED=1==\n",
			vars: map[string]string{"A": "PLANTED=1=="}},
		{name: "no interpolation", content: "A=PLANTED-1\nB=$A${A}$(A)`A`\n",
			vars: map[string]string{"A": "PLANTED-1", "B": "$A${A}$(A)`A`"}},
		{name: "no escapes", content: "A=PLANTED" + backslash + "n" + backslash + "\n",
			vars: map[string]string{"A": "PLANTED" + backslash + "n" + backslash}},
		{name: "no quoting, with a warning", content: "A=\"PLANTED 1\"\nB='PLANTED 2'\nC=\"PLANTED\nD=\"\nE=''\n",
			vars:     map[string]string{"A": "\"PLANTED 1\"", "B": "'PLANTED 2'", "C": "\"PLANTED", "D": "\"", "E": "''"},
			findings: []string{line(quoted, 1), line(quoted, 2), line(quoted, 5)}},
		{name: "the last line that sets a name wins", content: "A=PLANTED-1\nB=PLANTED-2\nA=PLANTED-3\n",
			vars: map[string]string{"A": "PLANTED-3", "B": "PLANTED-2"}},
		{name: "names differ by letter case", content: "a=PLANTED-1\nA=PLANTED-2\n",
			vars: map[string]string{"a": "PLANTED-1", "A": "PLANTED-2"}},
		{name: "export is not part of the format", content: "export A=PLANTED-1\nB=PLANTED-2\n",
			vars: map[string]string{"B": "PLANTED-2"}, findings: []string{line(malformed, 1)}},
		{name: "no equals sign", content: "A=PLANTED-1\nPLANTED\nA PLANTED\nA: PLANTED\n",
			vars: map[string]string{"A": "PLANTED-1"}, findings: []string{line(malformed, 2), line(malformed, 3), line(malformed, 4)}},
		{name: "names that are not variable names",
			content: "=PLANTED\n1A=PLANTED\nA-B=PLANTED\nA.B=PLANTED\nA B=PLANTED\nA\xc3\xa9=PLANTED\n\"A\"=PLANTED\nOK=PLANTED-1\n",
			vars:    map[string]string{"OK": "PLANTED-1"},
			findings: []string{line(malformed, 1), line(malformed, 2), line(malformed, 3), line(malformed, 4),
				line(malformed, 5), line(malformed, 6), line(malformed, 7)}},
		{name: "control characters in a value", content: "A=PLANTED\x00\nB=PLANTED\x1b[0m\nC=PLANTED\tx\nD=PLANTED\rx\nE=PLANTED\x7f\nOK=PLANTED-1\n",
			vars:     map[string]string{"OK": "PLANTED-1"},
			findings: []string{line(malformed, 1), line(malformed, 2), line(malformed, 3), line(malformed, 4), line(malformed, 5)}},
		{name: "invalid UTF-8 in a value", content: "A=PLANTED\xff\nOK=PLANTED-1\n",
			vars: map[string]string{"OK": "PLANTED-1"}, findings: []string{line(malformed, 1)}},
		{name: "text outside ASCII in a value", content: "A=PLANTED-\xc3\xa9\n",
			vars: map[string]string{"A": "PLANTED-\xc3\xa9"}},
		{name: "a long line is a line", content: "A=" + strings.Repeat("PLANTED-", 100_000) + "\nB=PLANTED-2\n",
			vars: map[string]string{"A": strings.Repeat("PLANTED-", 100_000), "B": "PLANTED-2"}},
		{name: "the file at its size bound", content: "A=PLANTED-1\n#" + strings.Repeat("x", maxEnvFileBytes-13),
			vars: map[string]string{"A": "PLANTED-1"}},
		{name: "the file past its size bound", content: "A=PLANTED-1\n#" + strings.Repeat("x", maxEnvFileBytes-12),
			findings: []string{"env_file_unreadable: the env file was not read: larger than 1 MiB"}},
		{name: "as many warnings as the bound", content: strings.Repeat("PLANTED\n", maxEnvFileWarnings) + "OK=PLANTED-1\n",
			vars: map[string]string{"OK": "PLANTED-1"},
			findings: []string{line(malformed, 1), line(malformed, 2), line(malformed, 3), line(malformed, 4), line(malformed, 5),
				line(malformed, 6), line(malformed, 7), line(malformed, 8), line(malformed, 9), line(malformed, 10)}},
		{name: "more malformed lines than the bound", content: strings.Repeat("PLANTED\n", maxEnvFileWarnings+3) + "OK=PLANTED-1\n",
			vars: map[string]string{"OK": "PLANTED-1"},
			findings: []string{line(malformed, 1), line(malformed, 2), line(malformed, 3), line(malformed, 4), line(malformed, 5),
				line(malformed, 6), line(malformed, 7), line(malformed, 8), line(malformed, 9), line(malformed, 10),
				"env_file_malformed: 3 more lines of the env file are not NAME=value and were skipped"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vars, findings := readEnvFile(writeEnvFile(t, tt.content))
			if !reflect.DeepEqual(vars, tt.vars) {
				t.Errorf("variables %q, want %q", vars, tt.vars)
			}
			if got := messages(t, Env{Findings: findings}); !reflect.DeepEqual(got, tt.findings) {
				t.Errorf("findings:\n got %q\nwant %q", got, tt.findings)
			}
		})
	}
}

func TestEnvFileNotRead(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(fifo, link); err != nil {
		t.Fatal(err)
	}
	locked := writeEnvFile(t, "A=PLANTED-1\n")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
		want string
	}{
		{"missing file", filepath.Join(dir, "PLANTED-missing"), "no such file or directory"},
		{"missing directory", filepath.Join(dir, "PLANTED-missing", "env"), "no such file or directory"},
		{"directory", dir, "not a regular file"},
		{"device", "/dev/null", "not a regular file"},
		{"endless device", "/dev/zero", "not a regular file"},
		{"named pipe", fifo, "not a regular file"},
		{"link to a named pipe", link, "not a regular file"},
		{"no permission", locked, "permission denied"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name == "no permission" && os.Getuid() == 0 {
				t.Skip("root reads any file")
			}
			// Opening a pipe that nobody writes to would block for ever.
			done := make(chan Env, 1)
			go func() { done <- NewEnv(noEnviron, tt.path, nil) }()
			var env Env
			select {
			case env = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("NewEnv is still waiting for the file")
			}
			want := []string{"env_file_unreadable: the env file was not read: " + tt.want}
			if got := messages(t, env); !reflect.DeepEqual(got, want) {
				t.Errorf("findings:\n got %q\nwant %q", got, want)
			}
			if v, ok := env.Lookup("A"); ok {
				t.Errorf("Lookup(A) = %q, true from a file that was not read", v)
			}
		})
	}
}

func TestNewEnv(t *testing.T) {
	environ := func(name string) (string, bool) {
		v, ok := map[string]string{"BOTH": "from-process", "PROCESS": "from-process", "EMPTY": ""}[name]
		return v, ok
	}
	lookups := func(env Env) map[string]string {
		got := map[string]string{}
		for _, name := range []string{"BOTH", "PROCESS", "FILE", "EMPTY", "NEITHER", "ADDED"} {
			if v, ok := env.Lookup(name); ok {
				got[name] = v
			}
		}
		return got
	}

	t.Run("the process environment wins over the file", func(t *testing.T) {
		path := writeEnvFile(t, "BOTH=from-file\nFILE=from-file\nEMPTY=from-file\n")
		env := NewEnv(environ, path, nil)
		want := map[string]string{"BOTH": "from-process", "PROCESS": "from-process", "FILE": "from-file", "EMPTY": ""}
		if got := lookups(env); !reflect.DeepEqual(got, want) {
			t.Errorf("lookups %q, want %q", got, want)
		}
		if len(env.Findings) != 0 {
			t.Errorf("findings %+v, want none", env.Findings)
		}
	})

	t.Run("no file is read unless one is named", func(t *testing.T) {
		env := NewEnv(environ, "", nil)
		want := map[string]string{"BOTH": "from-process", "PROCESS": "from-process", "EMPTY": ""}
		if got := lookups(env); !reflect.DeepEqual(got, want) {
			t.Errorf("lookups %q, want %q", got, want)
		}
		if len(env.Findings) != 0 {
			t.Errorf("findings %+v, want none", env.Findings)
		}
	})

	t.Run("each call reads the file again", func(t *testing.T) {
		path := writeEnvFile(t, "FILE=first\n")
		before := NewEnv(environ, path, nil)
		if err := os.WriteFile(path, []byte("FILE=second\nADDED=new\nbroken\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		after := NewEnv(environ, path, nil)
		if v, _ := before.Lookup("FILE"); v != "first" {
			t.Errorf("the earlier Env now answers %q", v)
		}
		if _, ok := before.Lookup("ADDED"); ok {
			t.Error("the earlier Env sees a later line")
		}
		want := map[string]string{"BOTH": "from-process", "PROCESS": "from-process", "FILE": "second", "EMPTY": "", "ADDED": "new"}
		if got := lookups(after); !reflect.DeepEqual(got, want) {
			t.Errorf("lookups %q, want %q", got, want)
		}
		if len(before.Findings) != 0 || len(after.Findings) != 1 {
			t.Errorf("findings before %+v and after %+v, want none and one", before.Findings, after.Findings)
		}
	})

	// Mounted secrets are often links to the file that holds them.
	t.Run("a link to a file is followed", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(writeEnvFile(t, "FILE=from-file\n"), link); err != nil {
			t.Fatal(err)
		}
		env := NewEnv(noEnviron, link, nil)
		if v, ok := env.Lookup("FILE"); v != "from-file" || !ok || len(env.Findings) != 0 {
			t.Errorf("Lookup(FILE) = %q, %v with findings %+v", v, ok, env.Findings)
		}
	})

	t.Run("the resolver is passed on", func(t *testing.T) {
		addr := netip.MustParseAddr("192.0.2.1")
		env := NewEnv(environ, "", func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{addr}, nil })
		if got, err := env.Resolve(context.Background(), "host.example"); err != nil || len(got) != 1 || got[0] != addr {
			t.Errorf("Resolve = %v, %v", got, err)
		}
	})
}
