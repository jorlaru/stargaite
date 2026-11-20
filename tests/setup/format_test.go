package setup

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jorlaru/stargaite/internal/console/host"
)

// The file bash and Go both write, compose.override.yaml, is written byte for
// byte alike while setup.sh and the console share a clone. .env's writer is
// the console's alone (host's TestEnvSetFormats holds bash's bytes).

// bashFunc is the function name of ../../setup.sh, from "name() {" to the
// first line that is exactly "}".
func bashFunc(t *testing.T, name string) string {
	t.Helper()
	src, err := os.ReadFile("../../setup.sh")
	if err != nil {
		t.Fatal(err)
	}
	_, body, ok := strings.Cut(string(src), "\n"+name+"() {\n")
	end := strings.Index(body, "\n}\n")
	if !ok || end < 0 {
		t.Fatalf("setup.sh has no function %s", name)
	}
	return name + "() {\n" + body[:end] + "\n}\n"
}

// runBash runs script in dir with args, with the real PATH.
func runBash(t *testing.T, dir, script string, args ...string) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{"-c", script, "_"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bash: %v\n%s", err, out)
	}
}

// seeded returns two directories, each with file holding seed; a nil seed
// leaves no file.
func seeded(t *testing.T, file string, seed *string) (string, string) {
	t.Helper()
	a, b := t.TempDir(), t.TempDir()
	for _, dir := range []string{a, b} {
		if seed != nil {
			if err := os.WriteFile(filepath.Join(dir, file), []byte(*seed), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return a, b
}

// sameFile fails unless file is byte-equal, with the same mode, in both.
func sameFile(t *testing.T, row, file, bashDir, goDir string) {
	t.Helper()
	var got [2]string
	var mode [2]os.FileMode
	for i, dir := range []string{bashDir, goDir} {
		b, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			t.Fatalf("%s: %v", row, err)
		}
		info, err := os.Stat(filepath.Join(dir, file))
		if err != nil {
			t.Fatal(err)
		}
		got[i], mode[i] = string(b), info.Mode().Perm()
	}
	if got[0] != got[1] || mode[0] != mode[1] {
		t.Errorf("%s: bash wrote %q (%v), Go %q (%v)", row, got[0], mode[0], got[1], mode[1])
	}
}

func TestSharedOverrideFormat(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("setup.sh supports Linux only")
	}
	script := "override_marker='" + host.OverrideMarker + "'; die() { exit 1; }; agree() { :; }; agreed=0; tmp_file=\n" +
		bashFunc(t, "override_set") + `override_set "$@"`
	marked := host.OverrideMarker + "\n# setup: tls=self-signed\n# setup: tls_name=localhost\n# setup: tls_ca=cert.pem\n" +
		"# setup: addresses=127.0.0.1\nservices:\n  stargaite:\n    extra_hosts:\n      - \"localhost:127.0.0.1\"\n" +
		"    command: [\"serve\", \"--listen\", \"0.0.0.0:4000\", \"--tls-cert\", \"/tls/cert.pem\", \"--tls-key\", \"/tls/key.pem\"]\n" +
		"    ports: !override\n      - \"127.0.0.1:${STARGAITE_PORT:-4000}:4000\"\n    volumes:\n      - ./tls:/tls:ro\n"
	for _, tc := range []struct {
		row  string
		seed *string
	}{{"no override", nil}, {"marked, with TLS", &marked}} {
		bashDir, goDir := seeded(t, "compose.override.yaml", tc.seed)
		runBash(t, bashDir, script, "host_gateway=1")
		h := host.New(goDir)
		o, err := h.ReadOverride()
		if err != nil {
			t.Fatal(err)
		}
		o.Set("host_gateway", "1")
		if err := h.WriteOverride(o); err != nil {
			t.Fatalf("%s: %v", tc.row, err)
		}
		sameFile(t, tc.row, "compose.override.yaml", bashDir, goDir)
	}
}
