package task

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/ui"
)

// machine is the first run's Machine step (design sections 4.1 and 4.5): the
// host port, from --port or .env (4000 without either), its line in .env,
// then the container built, started, recreated or kept, and ready. What
// this machine lacks is reported at once, and every confirm comes before
// its write. It decides only the host's side: nothing in the proxy.
func machine(ctx context.Context, u *ui.UI, h *host.Host) error {
	line, hasLine, published, err := envPort(u, h)
	if err != nil {
		return err
	}
	port, flag := published, portQuestion("", nil)
	if u.Given(flag) {
		if port, err = askPort(ctx, u, flag); err != nil {
			return err
		}
	}
	exists, runs, err := container(ctx, h)
	taken := func(p int) bool { return (p != published || !runs) && host.PortTaken(p) }
	need := "" // what this machine lacks: the two cannot both be known, so one at most
	switch {
	case err != nil: // a container of this clone may hold the port: it is not probed
		need = "docker compose ps: " + err.Error()
	case !taken(port):
	case u.Asks() && !u.Given(flag):
		if port, err = anotherPort(ctx, u, port, taken); err != nil {
			return err
		}
	default:
		need = fmt.Sprintf("a free host port: 127.0.0.1:%d is taken; give another with --port <n>", port)
	}
	if need != "" {
		return errors.New("this machine needs, before anything else: " + need)
	}
	if err := writePort(ctx, u, h, line, hasLine, published, port, exists); err != nil {
		return err
	}
	return start(ctx, u, h, exists, runs, exists && port != published)
}

// envPort is .env's STARGAITE_PORT line and the port the container
// publishes by it: 4000 without the line, 0 when the line is no port number
// and --port replaces it. The override is checked too, before any Docker
// call: only a bad port line is what --port replaces.
func envPort(u *ui.UI, h *host.Host) (line string, hasLine bool, published int, err error) {
	line, hasLine, err = h.EnvGet("STARGAITE_PORT")
	if err != nil {
		return "", false, 0, err
	}
	published = 4000
	if hasLine {
		var ok bool
		if published, ok = host.PortNumber(line); !ok && !u.Given(portQuestion("", nil)) {
			return "", false, 0, errors.New("STARGAITE_PORT in .env is not a port number, 1 to 65535: write it as STARGAITE_PORT=<n>, or give --port")
		}
	}
	if _, _, err := h.TLSNames(); err != nil {
		return "", false, 0, err
	}
	return line, hasLine, published, nil
}

// container reports whether the clone's container exists and runs.
func container(ctx context.Context, h *host.Host) (exists, runs bool, err error) {
	if exists, err = h.ContainerExists(ctx); err != nil || !exists {
		return false, false, err
	}
	runs, err = h.ContainerRuns(ctx)
	return exists, runs, err
}

// portQuestion is the host port's question, which --port answers: a port
// number, and not taken when taken is given.
func portQuestion(prompt string, taken func(int) bool) ui.Question {
	return ui.Question{ID: "port", Flag: "port", Step: "machine", Prompt: prompt, Check: func(a string) error {
		n, ok := host.PortNumber(a)
		switch {
		case !ok:
			return errors.New("a port number, 1 to 65535")
		case taken != nil && taken(n):
			return fmt.Errorf("127.0.0.1:%d is taken too", n)
		}
		return nil
	}}
}

// anotherPort asks for a port in place of port, which is taken; the first
// free one above it is the default, and a taken one is asked again.
func anotherPort(ctx context.Context, u *ui.UI, port int, taken func(int) bool) (int, error) {
	q := portQuestion(fmt.Sprintf("127.0.0.1:%d is taken: another port", port), taken)
	if next := host.NextFree(port); next != 0 {
		q.Default, q.HasDefault = strconv.Itoa(next), true
	}
	return askPort(ctx, u, q)
}

// askPort asks q, a port question, and returns its port.
func askPort(ctx context.Context, u *ui.UI, q ui.Question) (int, error) {
	a, err := u.Text(ctx, q)
	if err != nil {
		return 0, err
	}
	n, _ := host.PortNumber(a)
	return n, nil
}

// writePort writes port to .env's STARGAITE_PORT when it is not there yet:
// said, or confirmed when the container exists and is recreated to publish
// another port than it does.
func writePort(ctx context.Context, u *ui.UI, h *host.Host, line string, hasLine bool, published, port int, exists bool) error {
	recreated := fmt.Sprintf(", and the container is recreated to publish 127.0.0.1:%d: "+
		"requests in flight are given time to finish, and the rate-limit windows restart", port)
	written := fmt.Sprintf("writing STARGAITE_PORT=%d to .env", port)
	replaced := fmt.Sprintf("STARGAITE_PORT is replaced in .env, from %s to %d", line, port)
	var err error
	switch {
	case !hasLine && exists && port != published:
		err = confirm(ctx, u, "machine", written+recreated)
	case !hasLine:
		say(ctx, u, written)
	case port == published:
		return nil
	case exists:
		err = confirm(ctx, u, "machine", replaced+recreated)
	default:
		err = confirm(ctx, u, "machine", replaced)
	}
	if err != nil {
		return err
	}
	return h.EnvSet("STARGAITE_PORT", strconv.Itoa(port))
}

// start builds and starts the container when none exists, recreates it
// when its port changed, starts it when stopped, or keeps it; then it is
// ready. Each wait is a work piece, which Esc stops.
func start(ctx context.Context, u *ui.UI, h *host.Host, exists, runs, changed bool) error {
	switch {
	case !exists:
		version := h.CloneVersion(ctx)
		say(ctx, u, "building and starting version "+version+" (the first build takes a few minutes)")
		return u.Work(ctx, ui.Question{ID: "build", Step: "machine", Prompt: "building and starting the container"},
			func(ctx context.Context) error { return h.Build(ctx, sayer(ctx, u), version) })
	case changed:
		return recreate(ctx, u, h, "machine", false)
	case !runs:
		say(ctx, u, "starting the stopped container")
		return u.Work(ctx, ui.Question{ID: "start", Step: "machine", Prompt: "starting the container and waiting until it is ready"},
			func(ctx context.Context) error { return h.Up(ctx, sayer(ctx, u), "--no-build") })
	}
	say(ctx, u, "the container runs; kept")
	return ready(ctx, u, h, "machine")
}
