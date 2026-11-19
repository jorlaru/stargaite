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
