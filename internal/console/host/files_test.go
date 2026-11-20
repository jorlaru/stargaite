package host

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const marker = "# Written by setup.sh from its \"# setup:\" lines below; it rewrites this file whole.\n"

// noTemp fails when a temporary file of prefix, other than file itself, is
// left in dir.
func noTemp(t *testing.T, dir, file, prefix string) {
	t.Helper()
	found, _ := filepath.Glob(filepath.Join(dir, prefix+"*"))
	if left := slices.DeleteFunc(found, func(p string) bool { return filepath.Base(p) == file }); len(left) > 0 {
		t.Fatalf("left behind: %q", left)
	}
}

func TestEnvSetExactLine(t *testing.T) {
	h := New(t.TempDir())
	env := filepath.Join(h.Dir, ".env")
	write(t, env, "STARGAITE_PORT=4000\n")
	value := `s3cr3t $HOME #x y\z`
	if err := h.EnvSet("ACME_API_KEY", value); err != nil {
		t.Fatal(err)
	}
	if got := read(t, env); got != "STARGAITE_PORT=4000\nACME_API_KEY='"+value+"'\n" {
		t.Fatalf(".env = %q", got)
	}
	info, err := os.Stat(env)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, %v", info.Mode().Perm(), err)
	}
	noTemp(t, h.Dir, ".env", ".env.")
}

func TestEnvSetRefuses(t *testing.T) {
	h := New(t.TempDir())
	env := filepath.Join(h.Dir, ".env")
	write(t, env, "STARGAITE_PORT=4000\n")
	for _, v := range []string{"it's", "two\nlines", `ends\`, ""} {
		err := h.EnvSet("ACME_API_KEY", v)
		if err == nil || !strings.Contains(err.Error(), "ACME_API_KEY") ||
			(v != "" && (!strings.Contains(err.Error(), ".env cannot hold it") || strings.Contains(err.Error(), v))) {
			t.Fatalf("EnvSet(%q) = %v", v, err)
		}
		if got := read(t, env); got != "STARGAITE_PORT=4000\n" {
			t.Fatalf(".env changed: %q", got)
		}
		noTemp(t, h.Dir, ".env", ".env.")
	}
}

func TestEnvSetBadName(t *testing.T) {
	h := New(t.TempDir())
	if err := h.EnvSet("A-B", "x"); err == nil {
		t.Fatal("A-B was taken as a name")
	}
	if _, err := os.Stat(filepath.Join(h.Dir, ".env")); !os.IsNotExist(err) {
		t.Fatalf(".env: %v", err)
	}
	noTemp(t, h.Dir, ".env", ".env.")
}

func TestEnvSetCreates(t *testing.T) {
	h := New(t.TempDir())
	if err := h.EnvSet("A", "1"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(h.Dir, ".env"))
	if err != nil || info.Mode().Perm() != 0o600 || read(t, filepath.Join(h.Dir, ".env")) != "A='1'\n" {
		t.Fatalf("mode %v, %v, %q", info.Mode().Perm(), err, read(t, filepath.Join(h.Dir, ".env")))
	}
}

// TestEnvSetFormats: .env as setup.sh's env_set wrote it, byte for byte and
// mode 0600, in each row its shared-format test had (the golden values are
// bash's output, recorded before env_set was removed).
func TestEnvSetFormats(t *testing.T) {
	for _, tc := range []struct {
		row               string
		seed              *string
		name, value, want string
	}{
		{"no .env", nil, "NAME", "v", "NAME='v'\n"},
		{"empty .env", ptr(""), "NAME", "v", "NAME='v'\n"},
		{"replaced", ptr("A=1\n"), "A", "2", "A='2'\n"},
		{"appended", ptr("A=1\n"), "B", "2", "A=1\nB='2'\n"},
		{"no final newline", ptr("A=1"), "B", "2", "A=1\nB='2'\n"},
		{"no final newline, replaced", ptr("A=1"), "A", "2", "A='2'\n"},
		{"CRLF", ptr("A=1\r\nB=2\r\n"), "B", "3", "A=1\r\nB='3'\n"},
		{"commented and twice", ptr("# NAME=old\nNAME=old\nNAME=old2\n"), "NAME", "new", "# NAME=old\nNAME='new'\nNAME='new'\n"},
		{"a value the shell would expand", ptr("A=1\n"), "KEY", `s3cr3t $HOME #x y\z`, "A=1\nKEY='s3cr3t $HOME #x y\\z'\n"},
	} {
		h := New(t.TempDir())
		env := filepath.Join(h.Dir, ".env")
		if tc.seed != nil {
			write(t, env, *tc.seed)
		}
		if err := h.EnvSet(tc.name, tc.value); err != nil {
			t.Fatalf("%s: %v", tc.row, err)
		}
		info, err := os.Stat(env)
		if err != nil {
			t.Fatal(err)
		}
		if got := read(t, env); got != tc.want || info.Mode().Perm() != 0o600 {
			t.Errorf("%s: .env %q (%v), want %q (0600)", tc.row, got, info.Mode().Perm(), tc.want)
		}
	}
}

func ptr(s string) *string { return &s }

func TestEnvGet(t *testing.T) {
	h := New(t.TempDir())
	if _, ok, err := h.EnvGet("A"); ok || err != nil {
		t.Fatalf("no .env: ok %v, err %v", ok, err)
	}
	write(t, filepath.Join(h.Dir, ".env"), "# A=commented\nA='one'\nB=\"two\"\nC=three\nA=again\nD='half")
	for _, tc := range []struct {
		name, want string
		ok         bool
	}{{"A", "one", true}, {"B", "two", true}, {"C", "three", true}, {"D", "'half", true}, {"E", "", false}} {
		got, ok, err := h.EnvGet(tc.name)
		if got != tc.want || ok != tc.ok || err != nil {
			t.Fatalf("EnvGet(%s) = %q, %v, %v", tc.name, got, ok, err)
		}
	}
}

// The fixture and the file of setup_test.go's TestSetupOverride control.
const (
	setupLines = "# setup: tls=self-signed\n# setup: tls_name=localhost\n# setup: tls_ca=cert.pem\n# setup: addresses=127.0.0.1\n"
	tlsBlock   = "    command: [\"serve\", \"--listen\", \"0.0.0.0:4000\", \"--tls-cert\", \"/tls/cert.pem\", \"--tls-key\", \"/tls/key.pem\"]\n" +
		"    ports: !override\n      - \"127.0.0.1:${STARGAITE_PORT:-4000}:4000\"\n    volumes:\n      - ./tls:/tls:ro\n"
	markedFixture = marker + setupLines + "services:\n  stargaite:\n    extra_hosts:\n      - \"localhost:127.0.0.1\"\n" + tlsBlock
	markedWant    = marker + setupLines + "# setup: host_gateway=1\nservices:\n  stargaite:\n    extra_hosts:\n" +
		"      - \"host.docker.internal:host-gateway\"\n      - \"localhost:127.0.0.1\"\n" + tlsBlock
)

func TestOverrideRegeneratesMarked(t *testing.T) {
	h := New(t.TempDir())
	path := filepath.Join(h.Dir, "compose.override.yaml")
	write(t, path, markedFixture)
	o, err := h.ReadOverride()
	if err != nil || !o.Exists() || !o.Marked() || o.Get("tls_name") != "localhost" {
		t.Fatalf("read: %+v, %v", o, err)
	}
	o.Set("host_gateway", "1")
	if err := h.WriteOverride(o); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); got != markedWant {
		t.Fatalf("override:\n%s\nwant:\n%s", got, markedWant)
	}
	noTemp(t, h.Dir, "compose.override.yaml", "compose.override.")
}

func TestOverrideRefusesUnmarked(t *testing.T) {
	h := New(t.TempDir())
	path := filepath.Join(h.Dir, "compose.override.yaml")
	mine := "services:\n  stargaite:\n    environment:\n      MINE: \"1\"\n"
	write(t, path, mine)
	o, err := h.ReadOverride()
	if err != nil || !o.Exists() || o.Marked() || o.Holds("host.docker.internal:host-gateway") || !o.Holds("MINE") {
		t.Fatalf("read: %+v, %v", o, err)
	}
	o.Set("host_gateway", "1")
	if err := h.WriteOverride(o); err == nil {
		t.Fatal("an override setup.sh did not write was rewritten")
	}
	if got := read(t, path); got != mine {
		t.Fatalf("override changed: %q", got)
	}
	noTemp(t, h.Dir, "compose.override.yaml", "compose.override.")
}

func TestOverrideNew(t *testing.T) {
	h := New(t.TempDir())
	o, err := h.ReadOverride()
	if err != nil || o.Exists() || o.Marked() {
		t.Fatalf("read: %+v, %v", o, err)
	}
	o.Set("host_gateway", "1")
	if err := h.WriteOverride(o); err != nil {
		t.Fatal(err)
	}
	want := marker + "# setup: host_gateway=1\nservices:\n  stargaite:\n    extra_hosts:\n      - \"host.docker.internal:host-gateway\"\n"
	if got := read(t, filepath.Join(h.Dir, "compose.override.yaml")); got != want {
		t.Fatalf("override:\n%s\nwant:\n%s", got, want)
	}
}

func TestOverrideSet(t *testing.T) {
	h := New(t.TempDir())
	write(t, filepath.Join(h.Dir, "compose.override.yaml"), marker+"# setup: a=1\n# setup: b=2\nservices: {}\n")
	o, err := h.ReadOverride()
	if err != nil {
		t.Fatal(err)
	}
	o.Set("a", "")
	o.Set("c", "3")
	o.Set("a", "4")
	if err := h.WriteOverride(o); err != nil {
		t.Fatal(err)
	}
	// An emptied key keeps its place, as setup.sh's override_set keeps it.
	want := marker + "# setup: a=4\n# setup: b=2\n# setup: c=3\nservices:\n  stargaite:\n"
	if got := read(t, filepath.Join(h.Dir, "compose.override.yaml")); got != want {
		t.Fatalf("override:\n%s\nwant:\n%s", got, want)
	}
}

func TestPlane(t *testing.T) {
	h := New(t.TempDir())
	if p, err := h.Plane(); err != nil || p.BaseURL != "http://127.0.0.1:4000/v1" || p.Port != 4000 {
		t.Fatalf("no .env: %+v, %v", p, err)
	}
	write(t, filepath.Join(h.Dir, ".env"), "STARGAITE_PORT=4123\n")
	if p, err := h.Plane(); err != nil || p.BaseURL != "http://127.0.0.1:4123/v1" || p.Port != 4123 {
		t.Fatalf("port 4123: %+v, %v", p, err)
	}
	write(t, filepath.Join(h.Dir, "compose.override.yaml"), "# setup: tls_name=localhost\n# setup: tls_ca=cert.pem\nservices: {}\n")
	want := Plane{BaseURL: "https://localhost:4123/v1", Name: "localhost", CA: "cert.pem", Port: 4123}
	if p, err := h.Plane(); err != nil || p != want {
		t.Fatalf("TLS: %+v, %v", p, err)
	}
	for _, port := range []string{"0", "65536", "04000", "x", "4000 "} {
		write(t, filepath.Join(h.Dir, ".env"), "STARGAITE_PORT="+port+"\n")
		_, err := h.Plane()
		if err == nil || err.Error() != "STARGAITE_PORT in .env is not a port number, 1 to 65535: write it as STARGAITE_PORT=<n>" {
			t.Fatalf("STARGAITE_PORT=%q: %v", port, err)
		}
	}
	write(t, filepath.Join(h.Dir, ".env"), "STARGAITE_PORT=4123\n")
	write(t, filepath.Join(h.Dir, "compose.override.yaml"), "# setup: tls_name=a b\n")
	if _, err := h.Plane(); err == nil || err.Error() != "compose.override.yaml: tls_name is not a host name" {
		t.Fatalf("bad name: %v", err)
	}
}

func TestEnvUnset(t *testing.T) {
	for _, tc := range []struct {
		name, before, after string
		removed             bool
	}{
		{"middle", "A=1\nK='x'\nB=2\n", "A=1\nB=2\n", true},
		{"last without newline", "A=1\nK='x'", "A=1\n", true},
		{"twice", "K='x'\nA=1\r\nK=y\nB=2", "A=1\r\nB=2", true},
		{"absent", "A=1\nKK=2\n# K=3\nB=2", "A=1\nKK=2\n# K=3\nB=2", false},
	} {
		h := New(t.TempDir())
		env := filepath.Join(h.Dir, ".env")
		write(t, env, tc.before)
		removed, err := h.EnvUnset("K")
		if err != nil || removed != tc.removed {
			t.Fatalf("%s: removed %v, %v; want %v", tc.name, removed, err, tc.removed)
		}
		if got := read(t, env); got != tc.after {
			t.Fatalf("%s: .env = %q, want %q", tc.name, got, tc.after)
		}
		want := os.FileMode(0o644) // write's: an .env not written keeps its mode
		if tc.removed {
			want = 0o600
		}
		if info, err := os.Stat(env); err != nil || info.Mode().Perm() != want {
			t.Fatalf("%s: mode %v, %v; want %v", tc.name, info.Mode().Perm(), err, want)
		}
	}
	h := New(t.TempDir())
	if removed, err := h.EnvUnset("K"); removed || err != nil {
		t.Fatalf("no .env: removed %v, %v", removed, err)
	}
	if _, err := os.Stat(filepath.Join(h.Dir, ".env")); !os.IsNotExist(err) {
		t.Fatalf("no .env: one was written: %v", err)
	}
	write(t, filepath.Join(h.Dir, ".env"), "A=1\n")
	if _, err := h.EnvUnset("A-B"); err == nil || err.Error() != `"A-B" is not a variable name` {
		t.Fatalf("A-B: %v", err)
	}
	if got := read(t, filepath.Join(h.Dir, ".env")); got != "A=1\n" {
		t.Fatalf("a refused name changed .env: %q", got)
	}
}

func TestEnvUnsetLeavesNoTemp(t *testing.T) {
	h := New(t.TempDir())
	write(t, filepath.Join(h.Dir, ".env"), "A=1\nK=2\n")
	if _, err := h.EnvUnset("K"); err != nil {
		t.Fatal(err)
	}
	noTemp(t, h.Dir, ".env", ".env.")
}
