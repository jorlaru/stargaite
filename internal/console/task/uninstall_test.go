package task

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
	"github.com/jorlaru/stargaite/internal/console/ui/plain"
)

// N1 for the image stargaite:local and the volume of a project named as a
// clone directory may be, long enough that one line of both would be cut:
// what uninstall keeps, then what it removes, which is confirmed.
const (
	keptText = "uninstall keeps the volume stargaite-clone-on-a-server_stargaite-data, which holds the store " +
		"(keys, configuration and usage), .env, compose.override.yaml, backups/ and tls/"
	uninstallText = "uninstall stops and removes the container and its network, then the images stargaite:local and " +
		"stargaite:local-previous and bin/stargaite-console"
)

// kept is what Uninstall keeps in the clone.
var kept = []string{".env", "compose.override.yaml", "backups/a.db", "tls/cert.pem"}

// uninstallClone is a clone with the console and the files Uninstall keeps,
// whose docker fake answers compose config (with a credential in its
// environment, as real Compose prints it), ps, down with down, image
// inspect and rmi, -previous's rmi with previous, logging each call.
func uninstallClone(t *testing.T, down, previous string) *machineWorld {
	t.Helper()
	bin := t.TempDir()
	m := &machineWorld{t: t, h: host.New(t.TempDir()), log: filepath.Join(bin, "log")}
	config := `{"name":"stargaite-clone-on-a-server","services":{"stargaite":{"environment":{"K":"sk-SECRET-x"},"image":"stargaite:local"}},` +
		`"volumes":{"stargaite-data":{"name":"stargaite-clone-on-a-server_stargaite-data"}}}`
	writeFile(t, filepath.Join(bin, "docker"), "#!/bin/sh\necho \"$*\" >>'"+m.log+"'\ncase \"$*\" in\n"+
		"'compose config --format json') echo '"+config+"' ;;\n'compose ps -q'|'compose ps -a -q') echo c0ffee ;;\n"+
		"'compose down') "+down+" ;;\n'image inspect --format {{.Id}} '*) echo sha256:ab ;;\n"+
		"'rmi stargaite:local') echo 'Untagged: stargaite:local' ;;\n'rmi stargaite:local-previous') "+previous+" ;;\n*) exit 9 ;;\nesac\n", 0o755)
	t.Setenv("PATH", bin)
	writeFile(t, filepath.Join(m.h.Dir, "bin", "stargaite-console"), "console", 0o755)
	for _, f := range kept {
		writeFile(t, filepath.Join(m.h.Dir, f), "kept "+f, 0o600)
	}
	return m
}

func (m *machineWorld) uninstall(u *ui.UI) (string, error) {
	return u.Run(context.Background(), uninstallSteps(), func(ctx context.Context) (string, error) {
		return Uninstall(ctx, u, nil, m.h)
	})
}

// keptWhole checks the files Uninstall keeps are as they were.
func (m *machineWorld) keptWhole() {
	m.t.Helper()
	for _, f := range kept {
		if b, err := os.ReadFile(filepath.Join(m.h.Dir, f)); err != nil || string(b) != "kept "+f {
			m.t.Errorf("%s: %q, %v", f, b, err)
		}
	}
}

func (m *machineWorld) consoleThere() bool {
	_, err := os.Stat(filepath.Join(m.h.Dir, "bin", "stargaite-console"))
	return err == nil
}

func TestUninstallFlags(t *testing.T) {
	m := uninstallClone(t, ":", "echo 'Untagged: stargaite:local-previous'")
	var out bytes.Buffer
	res, err := m.uninstall(ui.New(plain.New(&out), map[string]string{"yes": "yes"}))
	if res != "Done: uninstalled; ./setup.sh installs again on the kept volume." || !errors.Is(err, ErrEnd) {
		t.Fatalf("%q, %v\n%s", res, err, out.String())
	}
	want := "compose config --format json\ncompose down\nimage inspect --format {{.Id}} stargaite:local\nrmi stargaite:local\n" +
		"image inspect --format {{.Id}} stargaite:local-previous\nrmi stargaite:local-previous\n"
	if m.dockerLog() != want {
		t.Errorf("docker log %q", m.dockerLog())
	}
	for _, f := range strings.Fields(m.dockerLog()) {
		if f == "-v" || f == "--volumes" || f == "--rmi" || f == "-f" || f == "--force" {
			t.Errorf("an argv holds %s: %q", f, m.dockerLog())
		}
	}
	tail := "Untagged: stargaite:local\nimage stargaite:local: removed\nUntagged: stargaite:local-previous\nimage stargaite:local-previous: removed\n" +
		"bin/stargaite-console: removed\n" +
		"what it kept goes, if you want, with these commands in this clone:\n  docker volume rm stargaite-clone-on-a-server_stargaite-data\n" +
		"  rm .env compose.override.yaml\n  rm -r backups tls\n"
	if !strings.HasPrefix(out.String(), keptText+"\n"+uninstallText+"\n") || !strings.HasSuffix(out.String(), tail) || strings.Contains(out.String(), "sk-SECRET") {
		t.Errorf("output:\n%s", out.String())
	}
	if m.consoleThere() {
		t.Error("bin/stargaite-console is still there")
	}
	m.keptWhole()

	m = uninstallClone(t, ":", ":")
	var usage *ui.UsageError
	_, err = m.uninstall(ui.New(plain.New(&out), nil))
	if !errors.As(err, &usage) || err.Error() != "missing --yes: "+uninstallText || m.dockerLog() != "compose config --format json\n" || !m.consoleThere() {
		t.Errorf("without --yes: %v, docker log %q", err, m.dockerLog())
	}

	m = uninstallClone(t, ":", "echo 'conflict: unable to delete stargaite:local-previous (must be forced)' >&2; exit 1")
	out.Reset()
	if _, err = m.uninstall(ui.New(plain.New(&out), map[string]string{"yes": "yes"})); !errors.Is(err, ErrEnd) || m.consoleThere() ||
		!strings.Contains(out.String(), "conflict: unable to delete stargaite:local-previous (must be forced)\n"+
			"image stargaite:local-previous: kept, Docker refused to remove it (its words are above)\nbin/stargaite-console: removed\n") {
		t.Errorf("refused: %v\n%s", err, out.String())
	}
	m.keptWhole()

	m = uninstallClone(t, "exit 1", ":")
	if _, err = m.uninstall(ui.New(plain.New(&out), map[string]string{"yes": "yes"})); err == nil || errors.Is(err, ErrEnd) ||
		err.Error() != "docker compose down failed with exit 1" || strings.Contains(m.dockerLog(), "rmi") || !m.consoleThere() {
		t.Errorf("down fails: %v, docker log %q", err, m.dockerLog())
	}
	m.keptWhole()
}

// TestHomeEndsAfterUninstall: Uninstall from the home ends the session, its
// result said once, with no second read of what exists.
func TestHomeEndsAfterUninstall(t *testing.T) {
	m := uninstallClone(t, ":", ":")
	p := closedPort(t)
	m.env("STARGAITE_PORT=" + p + "\n")
	servePlane(t, p)
	var calls []string
	reads := homeReads() // a provider, an alias and a key: the home asks, the first run does not open
	reads["export --json"] = reply{out: `{"version":1,"providers":{"p":{}},"models":{"a":{"targets":["p/m"]}},"keys":{"k":{}}}`}
	s := &scripted{answers: map[string][]string{"home": {"Uninstall"}, "yes": {"yes"}}}
	err := Home(context.Background(), ui.New(homeFront{s}, nil), proxy.New(serial(cannedRunner(t, reads, &calls))), m.h, 15*time.Second)
	if !errors.Is(err, ErrEnd) {
		t.Fatalf("Home: %v\n%s", err, s.output())
	}
	if len(s.homes()) != 1 || len(calls) != 3 || strings.Count(s.output(), "Done: uninstalled; ./setup.sh installs again on the kept volume.\n") != 1 {
		t.Errorf("%d home picks, CLI calls %q\n%s", len(s.homes()), calls, s.output())
	}
}
