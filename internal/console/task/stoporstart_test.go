package task

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/rules"
	"github.com/jorlaru/stargaite/internal/console/ui"
	"github.com/jorlaru/stargaite/internal/console/ui/plain"
)

// Stop or start's texts (plan S2, S4).
const (
	stopText    = "the container is stopped: requests in flight are given up to 60 s to finish, then the data plane answers nothing until it is started"
	restartText = "the container is restarted: requests in flight are given up to 60 s to finish, and the rate-limit windows restart"
)

// stopClone is machineClone whose docker fake also answers compose stop and
// compose restart, logging each call.
func stopClone(t *testing.T, ps, psa string) *machineWorld {
	t.Helper()
	bin := t.TempDir()
	m := &machineWorld{t: t, h: host.New(t.TempDir()), log: filepath.Join(bin, "log")}
	writeFile(t, filepath.Join(bin, "docker"), "#!/bin/sh\necho \"$*\" >>'"+m.log+"'\ncase \"$*\" in\n"+
		"'compose ps -q') "+ps+" ;;\n'compose ps -a -q') "+psa+" ;;\n"+
		"'compose up -d --no-build') "+upPause+" ;;\n'compose stop'|'compose restart') ;;\n"+
		"'compose logs --tail 30 stargaite') ;;\n*) exit 9 ;;\nesac\n", 0o755)
	t.Setenv("PATH", bin)
	return m
}

// stopOrStart runs Stop or start on m with u.
func (m *machineWorld) stopOrStart(u *ui.UI) (string, error) {
	return u.Run(context.Background(), stopSteps(), func(ctx context.Context) (string, error) {
		return StopOrStart(ctx, u, nil, m.h)
	})
}

func TestStopOrStartFlags(t *testing.T) {
	var usage *ui.UsageError
	for _, row := range []struct {
		name, ps, psa string
		serve         string // "up": readiness served once compose up is logged; "restart": 300 ms after compose restart is
		answers       map[string]string
		res, err      string
		log           string
		said          string // a line the output holds
	}{
		{"stop", psRuns, psRuns, "", map[string]string{"action": "stop", "yes": "yes"}, "Done: the container is stopped.", "",
			"compose ps -a -q\ncompose ps -q\ncompose stop\n", stopText},
		{"stop without --yes", psRuns, psRuns, "", map[string]string{"action": "stop"}, "", "missing --yes: " + stopText,
			"compose ps -a -q\ncompose ps -q\n", ""},
		{"start a stopped one", psNone, psRuns, "up", map[string]string{"action": "start"}, "Done: the container is started and ready.", "",
			"compose ps -a -q\ncompose ps -q\ncompose up -d --no-build\n", "starting the stopped container"},
		{"start a running one", psRuns, psRuns, "", map[string]string{"action": "start"}, "The container runs; nothing was changed.", "",
			"compose ps -a -q\ncompose ps -q\n", ""},
		{"stop a stopped one", psNone, psRuns, "", map[string]string{"action": "stop"}, "The container is stopped already; nothing was changed.", "",
			"compose ps -a -q\ncompose ps -q\n", ""},
		{"restart", psRuns, psRuns, "restart", map[string]string{"action": "restart", "yes": "yes"}, "Done: the container is restarted and ready.", "",
			"compose ps -a -q\ncompose ps -q\ncompose restart\n", restartText},
		{"restart a stopped one", psNone, psRuns, "restart", map[string]string{"action": "restart"}, "Done: the container is restarted and ready.", "",
			"compose ps -a -q\ncompose ps -q\ncompose restart\n", "restarting the stopped container"},
		{"no container", psNone, psNone, "", map[string]string{"action": "stop", "yes": "yes"}, "", "no container here: ./setup.sh install makes it",
			"compose ps -a -q\n", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			m := stopClone(t, row.ps, row.psa)
			p := closedPort(t)
			m.env("STARGAITE_PORT=" + p + "\n")
			switch row.serve {
			case "up":
				m.serveOnUp(p)
			case "restart":
				m.serveOn(p, "compose restart", 300*time.Millisecond)
			}
			var out bytes.Buffer
			res, err := m.stopOrStart(ui.New(plain.New(&out), row.answers))
			if res != row.res || (row.err == "") != (err == nil) || (err != nil && err.Error() != row.err) {
				t.Fatalf("res %q, err %v; want %q, %q\n%s", res, err, row.res, row.err, out.String())
			}
			if row.name == "stop without --yes" && !errors.As(err, &usage) {
				t.Errorf("err %T is not a usage error", err)
			}
			if m.dockerLog() != row.log {
				t.Errorf("docker log %q; want %q", m.dockerLog(), row.log)
			}
			if row.said != "" && !strings.Contains(out.String(), row.said+"\n") {
				t.Errorf("output does not say %q:\n%s", row.said, out.String())
			}
			if row.serve != "" && m.h.Ready(context.Background()) != nil {
				t.Errorf("%q came before readiness", res)
			}
		})
	}
}

func TestStopOrStartAtTerminal(t *testing.T) {
	m := stopClone(t, psRuns, psRuns)
	m.env("STARGAITE_PORT=" + closedPort(t) + "\n")
	s := &scripted{answers: map[string][]string{"action": {"stop"}, "yes": {"no"}}}
	_, err := m.stopOrStart(ui.New(s, nil))
	if err == nil || err.Error() != "stopped; nothing more was changed" {
		t.Fatalf("err = %v\n%s", err, s.output())
	}
	q := s.questions[0]
	var opts []string
	for _, o := range q.Options {
		opts = append(opts, o.Value+"|"+o.Label)
	}
	if q.ID != "action" || q.Prompt != "What do you want to do with the container?" ||
		!slices.Equal(opts, []string{"stop|stop it", "start|start it", "restart|restart it"}) {
		t.Errorf("pick %s %q: %q", q.ID, q.Prompt, opts)
	}
	if strings.Contains(m.dockerLog(), "compose stop") {
		t.Errorf("docker log %q", m.dockerLog())
	}
}

// TestHomeStopped: a stopped container is said stopped and Start is
// suggested, with no CLI call; picking it starts the container without
// asking what to do with it.
func TestHomeStopped(t *testing.T) {
	m := stopClone(t, psNone, psRuns)
	var calls []string
	px := proxy.New(cannedRunner(t, nil, &calls))
	hm := readHome(context.Background(), px, m.h)
	want := []rules.Suggestion{{Label: "Start the container: it is stopped", Task: "Stop or start", Presets: map[string]string{"action": "start"}}}
	if hm.summary != "http://127.0.0.1:4000/v1: stopped · clone dev" || !slices.EqualFunc(hm.suggested, want, sameSuggestion) || len(calls) != 0 {
		t.Fatalf("summary %q, suggested %+v, calls %q", hm.summary, hm.suggested, calls)
	}

	p := closedPort(t)
	m.env("STARGAITE_PORT=" + p + "\n")
	m.serveOnUp(p)
	s := &scripted{answers: map[string][]string{"home": {"suggested:0"}}}
	if err := Home(context.Background(), ui.New(homeFront{s}, nil), px, m.h, 15*time.Second); !errors.Is(err, ui.ErrQuit) {
		t.Fatalf("Home: %v\n%s", err, s.output())
	}
	var ids []string
	for _, q := range s.questions {
		ids = append(ids, q.ID)
	}
	if !slices.Equal(ids, []string{"home", "home"}) || m.ups("--no-build") != 1 || len(calls) != 0 ||
		!strings.Contains(s.output(), "starting the stopped container\nDone: the container is started and ready.\n") {
		t.Errorf("asked %q, docker %q, calls %q\n%s", ids, m.dockerLog(), calls, s.output())
	}
}

// TestHomeNotReady: a running container that is not ready is said not ready
// and Restart is suggested, with no CLI call; picking it asks to confirm the
// restart.
func TestHomeNotReady(t *testing.T) {
	m := stopClone(t, psRuns, psRuns)
	p := closedPort(t)
	m.env("STARGAITE_PORT=" + p + "\n")
	var calls []string
	px := proxy.New(cannedRunner(t, nil, &calls))
	hm := readHome(context.Background(), px, m.h)
	want := []rules.Suggestion{{Label: "Restart the container: it is not ready", Task: "Stop or start", Presets: map[string]string{"action": "restart"}}}
	if hm.summary != "http://127.0.0.1:"+p+"/v1: not ready · clone dev" || !slices.EqualFunc(hm.suggested, want, sameSuggestion) || len(calls) != 0 {
		t.Fatalf("summary %q, suggested %+v, calls %q", hm.summary, hm.suggested, calls)
	}

	s := &scripted{answers: map[string][]string{"home": {"suggested:0"}, "yes": {"no"}}}
	if err := Home(context.Background(), ui.New(homeFront{s}, nil), px, m.h, 15*time.Second); !errors.Is(err, ui.ErrQuit) {
		t.Fatalf("Home: %v\n%s", err, s.output())
	}
	var ids []string
	for _, q := range s.questions {
		ids = append(ids, q.ID)
	}
	if !slices.Equal(ids, []string{"home", "yes", "home"}) || strings.Contains(m.dockerLog(), "compose restart") || len(calls) != 0 ||
		!strings.Contains(s.output(), restartText+"\nStop or start: stopped; nothing more was changed\n") {
		t.Errorf("asked %q, docker %q, calls %q\n%s", ids, m.dockerLog(), calls, s.output())
	}
}

// sameSuggestion compares two suggestions, their presets by content.
func sameSuggestion(a, b rules.Suggestion) bool {
	return a.Label == b.Label && a.Task == b.Task && maps.Equal(a.Presets, b.Presets)
}
