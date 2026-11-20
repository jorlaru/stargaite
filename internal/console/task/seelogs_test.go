package task

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/ui"
	"github.com/jorlaru/stargaite/internal/console/ui/plain"
)

// The last lines a log fake prints: a .env line quoting a credential, a
// line of terminal escapes, and one of 100 KiB.
const tailLines = "echo 'a first line'\n" +
	`echo 'failed to read /x/.env: line 2: unexpected character "'"'"'" in variable name "BAD '"'"'sk-SECRET-x"'` + "\n" +
	`printf 'esc \033[2J and \033]52;c;Zm9v\007 end\n'` + "\n" +
	"/usr/bin/head -c 102400 /dev/zero | /usr/bin/tr '\\0' a; echo\n" +
	"echo 'a last line'\n"

// followTrap is a follow that prints a line, then waits, logging the SIGINT
// that stops it.
func followTrap(log string) string {
	return "trap 'echo INT >>\"" + log + "\"; exit 130' INT\necho 'a followed line'\n/bin/sleep 60\n"
}

// logsClone is a clone whose docker fake answers compose ps -a -q with psa,
// the last lines with tailLines and the follow with follow, logging each
// call.
func logsClone(t *testing.T, psa, follow string) *machineWorld {
	t.Helper()
	bin := t.TempDir()
	m := &machineWorld{t: t, h: host.New(t.TempDir()), log: filepath.Join(bin, "log")}
	writeFile(t, filepath.Join(bin, "docker"), "#!/bin/sh\necho \"$*\" >>'"+m.log+"'\ncase \"$*\" in\n"+
		"'compose ps -a -q') "+psa+" ;;\n"+
		"'compose logs --no-log-prefix --tail 100 stargaite')\n"+tailLines+";;\n"+
		"'compose logs --no-log-prefix --follow --tail 0 stargaite')\n"+follow+"\n;;\n*) exit 9 ;;\nesac\n", 0o755)
	t.Setenv("PATH", bin)
	return m
}

func (m *machineWorld) seeLogs(u *ui.UI) (string, error) {
	return u.Run(context.Background(), logSteps(), func(ctx context.Context) (string, error) {
		return SeeLogs(ctx, u, nil, m.h)
	})
}

func TestSeeLogsFlags(t *testing.T) {
	m := logsClone(t, psRuns, "exit 9")
	var out bytes.Buffer
	res, err := m.seeLogs(ui.New(plain.New(&out), nil))
	if res != "Done: the last 100 log lines are above." || err != nil {
		t.Fatalf("res %q, err %v\n%s", res, err, out.String())
	}
	o := out.String()
	if !inOrder(o, "a first line", "failed to read /x/.env: line 2", "esc [2J and ]52;c;Zm9v end", "a last line") ||
		strings.Contains(o, "sk-SECRET") || strings.Contains(o, "\x1b") || strings.Contains(o, "\a") {
		t.Errorf("output:\n%s", o)
	}
	for _, l := range strings.Split(o, "\n") {
		if len(l) > 65536 {
			t.Errorf("a line of %d bytes", len(l))
		}
	}
	if m.dockerLog() != "compose ps -a -q\ncompose logs --no-log-prefix --tail 100 stargaite\n" {
		t.Errorf("docker log %q", m.dockerLog())
	}

	m = logsClone(t, psRuns, "exit 9")
	var usage *ui.UsageError
	_, err = m.seeLogs(ui.New(plain.New(&out), map[string]string{"follow": "yes"}))
	if !errors.As(err, &usage) || err.Error() != "--follow needs a terminal: the console follows logs only while someone watches" ||
		m.dockerLog() != "" {
		t.Errorf("--follow: err %v, docker log %q", err, m.dockerLog())
	}

	m = logsClone(t, psNone, "exit 9")
	if _, err = m.seeLogs(ui.New(plain.New(&out), nil)); err == nil || err.Error() != "no container here: ./setup.sh install makes it" ||
		m.dockerLog() != "compose ps -a -q\n" {
		t.Errorf("no container: err %v, docker log %q", err, m.dockerLog())
	}
}

func TestSeeLogsAtTerminal(t *testing.T) {
	sig := filepath.Join(t.TempDir(), "sig")
	m := logsClone(t, psRuns, followTrap(sig))
	s := &scripted{}
	var pressed time.Time
	s.leaveWork = func(q ui.Question) bool {
		if q.ID == "follow" && strings.Contains(s.output(), "a followed line") {
			pressed = time.Now()
			return true
		}
		return false
	}
	res, err := m.seeLogs(ui.New(s, nil))
	if res != "Stopped following the logs." || err != nil || time.Since(pressed) > time.Second {
		t.Fatalf("res %q, err %v, %v after the Esc\n%s", res, err, time.Since(pressed), s.output())
	}
	if !inOrder(s.output(), "a last line", "a followed line") || readFile(sig) != "INT\n" {
		t.Errorf("sig %q, output:\n%s", readFile(sig), s.output())
	}

	m = logsClone(t, psRuns, followTrap(filepath.Join(t.TempDir(), "sig")))
	m.h.ShortenFollow(200 * time.Millisecond)
	if res, err := m.seeLogs(ui.New(&scripted{}, nil)); res != "Followed the logs for 1 hour: stopped; See logs follows them again." || err != nil {
		t.Errorf("bound: res %q, err %v", res, err)
	}

	m = logsClone(t, psRuns, "exit 0")
	if res, err := m.seeLogs(ui.New(&scripted{}, nil)); res != "The logs ended." || err != nil {
		t.Errorf("ended: res %q, err %v", res, err)
	}

	m = logsClone(t, psRuns, followTrap(filepath.Join(t.TempDir(), "sig")))
	s = &scripted{workErr: ui.ErrQuit}
	s.leaveWork = func(q ui.Question) bool { return q.ID == "follow" && strings.Contains(s.output(), "a followed line") }
	if _, err := m.seeLogs(ui.New(s, nil)); !errors.Is(err, ui.ErrQuit) {
		t.Errorf("quit: err %v", err)
	}
}

// inOrder reports whether out holds each line, each after the one before.
func inOrder(out string, lines ...string) bool {
	rest := strings.Split(out, "\n")
	for _, l := range lines {
		i := slices.Index(rest, l)
		if i < 0 {
			return false
		}
		rest = rest[i+1:]
	}
	return true
}

func readFile(path string) string { b, _ := os.ReadFile(path); return string(b) }
