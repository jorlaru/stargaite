package task

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/ui"
	"github.com/jorlaru/stargaite/internal/console/ui/plain"
)

// The Machine step's container states, as its docker fake's compose ps -q
// and compose ps -a -q answer them.
const (
	psRuns = "echo c0ffee"
	psNone = ":"
	psFail = "exit 1"
)

// upPause gives the test's data plane time to start once compose up is
// logged, so the first readiness check finds it.
const upPause = "/bin/sleep 0.3"

// recreatePort is the tail of the Machine step's confirm when the container
// is recreated to publish port.
func recreatePort(port string) string {
	return ", and the container is recreated to publish 127.0.0.1:" + port +
		": requests in flight are given time to finish, and the rate-limit windows restart"
}

// takenItem is M6's port item.
func takenItem(port string) string {
	return "this machine needs, before anything else: a free host port: 127.0.0.1:" + port + " is taken; give another with --port <n>"
}

// machineWorld is the Machine step's fixture: a clone whose docker fake
// answers compose ps -q with ps, compose ps -a -q with psa and compose up
// with up, logging each call and, for compose up, the STARGAITE_VERSION it
// got; no git on PATH, so the clone's version is dev.
type machineWorld struct {
	t   *testing.T
	h   *host.Host
	log string
}

func machineClone(t *testing.T, ps, psa, up string) *machineWorld {
	t.Helper()
	bin := t.TempDir()
	m := &machineWorld{t: t, h: host.New(t.TempDir()), log: filepath.Join(bin, "log")}
	writeFile(t, filepath.Join(bin, "docker"), "#!/bin/sh\necho \"$*\" >>'"+m.log+"'\ncase \"$*\" in\n"+
		"'compose ps -q') "+ps+" ;;\n'compose ps -a -q') "+psa+" ;;\n"+
		"'compose up -d --build'|'compose up -d --no-build') echo \"version $STARGAITE_VERSION\" >>'"+m.log+"'\n"+up+"\n;;\n"+
		"'compose logs --tail 30 stargaite') ;;\n*) exit 9 ;;\nesac\n", 0o755)
	t.Setenv("PATH", bin)
	return m
}

// run runs the Machine step with u under a one-step table.
func (m *machineWorld) run(u *ui.UI) error {
	_, err := u.Run(context.Background(), []ui.Step{{ID: "machine", Title: "machine"}}, func(ctx context.Context) (string, error) {
		return "", machine(ctx, u, m.h)
	})
	return err
}

// plain runs it with the plain front and answers; it returns the output.
func (m *machineWorld) plain(answers map[string]string) (string, error) {
	var out bytes.Buffer
	err := m.run(ui.New(plain.New(&out), answers))
	return out.String(), err
}

func (m *machineWorld) env(content string) {
	writeFile(m.t, filepath.Join(m.h.Dir, ".env"), content, 0o600)
}

func (m *machineWorld) envFile() string {
	b, _ := os.ReadFile(filepath.Join(m.h.Dir, ".env"))
	return string(b)
}

func (m *machineWorld) dockerLog() string { b, _ := os.ReadFile(m.log); return string(b) }

// ups is how many times compose up ran with flag, --build or --no-build.
func (m *machineWorld) ups(flag string) int {
	return strings.Count(m.dockerLog(), "compose up -d "+flag+"\n")
}

// plane answers readiness on 127.0.0.1 and records, per connection, whether
// it sent a request: one that sent none is the port probe's.
type plane struct {
	mu    sync.Mutex
	conns map[net.Conn]bool
	srv   *http.Server
}

func listenPlane(port string) (*plane, error) {
	l, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		return nil, err
	}
	p := &plane{conns: map[net.Conn]bool{}}
	p.srv = &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		ConnState: func(c net.Conn, s http.ConnState) {
			p.mu.Lock()
			defer p.mu.Unlock()
			if s == http.StateActive || s == http.StateNew && !p.conns[c] {
				p.conns[c] = s == http.StateActive
			}
		}}
	go func() { _ = p.srv.Serve(l) }()
	return p, nil
}

func servePlane(t *testing.T, port string) *plane {
	t.Helper()
	p, err := listenPlane(port)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.srv.Close() })
	return p
}

// probes is the connections that sent no request.
func (p *plane) probes() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, sent := range p.conns {
		if !sent {
			n++
		}
	}
	return n
}

// serveOnUp serves readiness on port once compose up is logged, as a
// container would: the port must be free before.
func (m *machineWorld) serveOnUp(port string) {
	var mu sync.Mutex
	var p *plane
	stop := make(chan struct{})
	m.t.Cleanup(func() {
		close(stop)
		mu.Lock()
		defer mu.Unlock()
		if p != nil {
			_ = p.srv.Close()
		}
	})
	go func() {
		for !strings.Contains(m.dockerLog(), "compose up") {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
		mu.Lock()
		defer mu.Unlock()
		select {
		case <-stop:
		default:
			p, _ = listenPlane(port)
		}
	}()
}

// heldPort is a loopback port held open until the test ends.
func heldPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

func TestMachineKept(t *testing.T) {
	p := closedPort(t)
	m := machineClone(t, psRuns, psRuns, "")
	pl := servePlane(t, p)
	m.env("STARGAITE_PORT=" + p + "\n")
	out, err := m.plain(nil)
	if err != nil || out != "the container runs; kept\n" {
		t.Fatalf("err = %v, output %q", err, out)
	}
	if m.ups("--build")+m.ups("--no-build") != 0 || m.envFile() != "STARGAITE_PORT="+p+"\n" || pl.probes() != 0 {
		t.Fatalf("docker %q, .env %q, %d port probes", m.dockerLog(), m.envFile(), pl.probes())
	}
}

func TestMachineBuild(t *testing.T) {
	p := closedPort(t)
	m := machineClone(t, psNone, psNone, upPause)
	m.serveOnUp(p)
	out, err := m.plain(map[string]string{"port": p})
	want := "writing STARGAITE_PORT=" + p + " to .env\nbuilding and starting version dev (the first build takes a few minutes)\n"
	if err != nil || out != want {
		t.Fatalf("err = %v, output %q; want %q", err, out, want)
	}
	if m.ups("--build") != 1 || m.ups("--no-build") != 0 || !strings.Contains(m.dockerLog(), "version dev\n") {
		t.Fatalf("docker %q", m.dockerLog())
	}
	info, err := os.Stat(filepath.Join(m.h.Dir, ".env"))
	if err != nil || m.envFile() != "STARGAITE_PORT='"+p+"'\n" || info.Mode().Perm() != 0o600 {
		t.Fatalf(".env %q, err %v", m.envFile(), err)
	}
}

func TestMachineStopped(t *testing.T) {
	p := closedPort(t)
	m := machineClone(t, psNone, psRuns, upPause)
	m.env("STARGAITE_PORT=" + p + "\n")
	m.serveOnUp(p)
	out, err := m.plain(nil)
	if err != nil || out != "starting the stopped container\n" || m.ups("--no-build") != 1 || m.ups("--build") != 0 {
		t.Fatalf("err = %v, output %q, docker %q", err, out, m.dockerLog())
	}
}

func TestMachinePort(t *testing.T) {
	var usage *ui.UsageError
	t.Run("a: --port taken, no container", func(t *testing.T) {
		m := machineClone(t, psNone, psNone, "")
		q := heldPort(t)
		_, err := m.plain(map[string]string{"port": q})
		if err == nil || err.Error() != takenItem(q) || errors.As(err, &usage) || m.envFile() != "" || m.ups("--build") != 0 {
			t.Fatalf("err = %v, .env %q, docker %q", err, m.envFile(), m.dockerLog())
		}
		// At a terminal a taken --port is reported too, not asked again.
		s := &scripted{answers: map[string][]string{}}
		err = m.run(ui.New(s, map[string]string{"port": q}))
		if err == nil || err.Error() != takenItem(q) || len(s.questions) != 0 || m.envFile() != "" || m.ups("--build") != 0 {
			t.Fatalf("at a terminal: err = %v, asked %q, .env %q, docker %q", err, s.asked, m.envFile(), m.dockerLog())
		}
	})
	t.Run("b: asked another port at a terminal", func(t *testing.T) {
		m := machineClone(t, psNone, psNone, upPause)
		p, q := heldPort(t), heldPort(t)
		n, _ := strconv.Atoi(p)
		next := strconv.Itoa(host.NextFree(n))
		m.env("STARGAITE_PORT=" + p + "\n")
		m.serveOnUp(next)
		s := &scripted{answers: map[string][]string{"port": {q, ""}, "yes": {"yes"}}}
		if err := m.run(ui.New(s, nil)); err != nil {
			t.Fatalf("err = %v\n%s", err, s.output())
		}
		asked := s.questions[0]
		if asked.ID != "port" || asked.Prompt != "127.0.0.1:"+p+" is taken: another port" || asked.Default != next || !asked.HasDefault {
			t.Fatalf("asked %+v", asked)
		}
		out := s.output()
		if strings.Count(out, "127.0.0.1:"+q+" is taken too\n") != 1 ||
			!strings.Contains(out, "STARGAITE_PORT is replaced in .env, from "+p+" to "+next+"\n") {
			t.Fatalf("output %q", out)
		}
		if m.envFile() != "STARGAITE_PORT='"+next+"'\n" || m.ups("--build") != 1 {
			t.Fatalf(".env %q, docker %q", m.envFile(), m.dockerLog())
		}
	})
	t.Run("b: taken three times", func(t *testing.T) {
		m := machineClone(t, psNone, psNone, "")
		p, q := heldPort(t), heldPort(t)
		m.env("STARGAITE_PORT=" + p + "\n")
		s := &scripted{answers: map[string][]string{"port": {q, q, q}}}
		err := m.run(ui.New(s, nil))
		if err == nil || err.Error() != "127.0.0.1:"+q+" is taken too" || m.envFile() != "STARGAITE_PORT="+p+"\n" || m.ups("--build") != 0 {
			t.Fatalf("err = %v, .env %q, docker %q", err, m.envFile(), m.dockerLog())
		}
	})
	t.Run("c: running, another --port", func(t *testing.T) {
		m := machineClone(t, psRuns, psRuns, upPause)
		p, q := closedPort(t), closedPort(t)
		m.env("STARGAITE_PORT=" + p + "\n")
		_, err := m.plain(map[string]string{"port": q})
		want := "missing --yes: STARGAITE_PORT is replaced in .env, from " + p + " to " + q + recreatePort(q)
		if !errors.As(err, &usage) || usage.Msg != want || m.envFile() != "STARGAITE_PORT="+p+"\n" || m.ups("--no-build") != 0 {
			t.Fatalf("err = %v; want %q; .env %q, docker %q", err, want, m.envFile(), m.dockerLog())
		}
		m.serveOnUp(q)
		if _, err := m.plain(map[string]string{"port": q, "yes": "yes"}); err != nil ||
			m.envFile() != "STARGAITE_PORT='"+q+"'\n" || m.ups("--no-build") != 1 || m.ups("--build") != 0 {
			t.Fatalf("err = %v, .env %q, docker %q", err, m.envFile(), m.dockerLog())
		}
	})
	t.Run("d: running, --port taken", func(t *testing.T) {
		m := machineClone(t, psRuns, psRuns, "")
		p, q := closedPort(t), heldPort(t)
		m.env("STARGAITE_PORT=" + p + "\n")
		_, err := m.plain(map[string]string{"port": q, "yes": "yes"})
		if err == nil || err.Error() != takenItem(q) || m.envFile() != "STARGAITE_PORT="+p+"\n" || m.ups("--no-build") != 0 {
			t.Fatalf("err = %v, .env %q, docker %q", err, m.envFile(), m.dockerLog())
		}
	})
	t.Run("e: running on 4000 without the line, another --port", func(t *testing.T) {
		m := machineClone(t, psRuns, psRuns, upPause)
		q := closedPort(t)
		m.env("OTHER='1'\n")
		_, err := m.plain(map[string]string{"port": q})
		want := "missing --yes: writing STARGAITE_PORT=" + q + " to .env" + recreatePort(q)
		if !errors.As(err, &usage) || usage.Msg != want || m.envFile() != "OTHER='1'\n" || m.ups("--no-build") != 0 {
			t.Fatalf("err = %v; want %q; .env %q, docker %q", err, want, m.envFile(), m.dockerLog())
		}
		m.serveOnUp(q)
		if _, err := m.plain(map[string]string{"port": q, "yes": "yes"}); err != nil ||
			m.envFile() != "OTHER='1'\nSTARGAITE_PORT='"+q+"'\n" || m.ups("--no-build") != 1 {
			t.Fatalf("err = %v, .env %q, docker %q", err, m.envFile(), m.dockerLog())
		}
	})
}

func TestMachineReport(t *testing.T) {
	m := machineClone(t, psFail, psFail, "")
	p := heldPort(t)
	m.env("STARGAITE_PORT=" + p + "\n")
	_, err := m.plain(nil)
	if err == nil || err.Error() != "this machine needs, before anything else: docker compose ps: docker compose ps failed with exit 1" ||
		m.envFile() != "STARGAITE_PORT="+p+"\n" {
		t.Fatalf("Compose failing: err = %v, .env %q", err, m.envFile())
	}
	m = machineClone(t, psNone, psNone, "")
	m.env("STARGAITE_PORT=" + p + "\n")
	if _, err := m.plain(nil); err == nil || err.Error() != takenItem(p) || m.envFile() != "STARGAITE_PORT="+p+"\n" || m.ups("--build") != 0 {
		t.Fatalf("port taken: err = %v, .env %q, docker %q", err, m.envFile(), m.dockerLog())
	}
}

func TestMachineBadEnvPort(t *testing.T) {
	m := machineClone(t, psNone, psNone, upPause)
	m.env("STARGAITE_PORT=abc\n")
	_, err := m.plain(nil)
	const want = "STARGAITE_PORT in .env is not a port number, 1 to 65535: write it as STARGAITE_PORT=<n>, or give --port"
	if err == nil || err.Error() != want || m.dockerLog() != "" {
		t.Fatalf("err = %v, docker %q", err, m.dockerLog())
	}
	p := closedPort(t)
	m.serveOnUp(p)
	out, err := m.plain(map[string]string{"port": p, "yes": "yes"})
	if err != nil || !strings.Contains(out, "STARGAITE_PORT is replaced in .env, from abc to "+p+"\n") ||
		m.envFile() != "STARGAITE_PORT='"+p+"'\n" || m.ups("--build") != 1 {
		t.Fatalf("--port: err = %v, output %q, .env %q, docker %q", err, out, m.envFile(), m.dockerLog())
	}
}

func TestMachineBadOverride(t *testing.T) {
	m := machineClone(t, psNone, psNone, "")
	writeFile(t, filepath.Join(m.h.Dir, "compose.override.yaml"), "# setup: tls_name=not_a_host!\nservices: {}\n", 0o600)
	for _, answers := range []map[string]string{nil, {"port": closedPort(t)}} {
		if _, err := m.plain(answers); err == nil || err.Error() != "compose.override.yaml: tls_name is not a host name" || m.dockerLog() != "" {
			t.Fatalf("%v: err = %v, docker %q", answers, err, m.dockerLog())
		}
	}
}

func TestMachineEscDuringBuild(t *testing.T) {
	m := machineClone(t, psNone, psNone, "trap 'echo INT >>"+"\"$0.int\"; exit 130' INT\n/bin/sleep 60")
	p := closedPort(t)
	s := &scripted{leaveWork: func(q ui.Question) bool {
		return q.Prompt == "building and starting the container" && strings.Contains(m.dockerLog(), "compose up")
	}}
	done := make(chan error, 1)
	go func() { done <- m.run(ui.New(s, map[string]string{"port": p})) }()
	select {
	case err := <-done:
		if !errors.Is(err, ui.ErrLeave) {
			t.Fatalf("err = %v\n%s", err, s.output())
		}
	case <-time.After(12 * time.Second):
		t.Fatal("Esc did not stop the build within 12 s")
	}
	interrupted, _ := os.ReadFile(filepath.Join(filepath.Dir(m.log), "docker.int"))
	if string(interrupted) != "INT\n" || m.envFile() != "STARGAITE_PORT='"+p+"'\n" || strings.Contains(m.dockerLog(), "exec") {
		t.Fatalf("interrupt %q, .env %q, docker %q", interrupted, m.envFile(), m.dockerLog())
	}
}
