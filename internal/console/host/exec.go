package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const (
	// killDelay is how long a cancelled command's process group has after
	// SIGINT before it is killed.
	killDelay = 10 * time.Second
	// captureLimit caps the standard output a command may write.
	captureLimit = 8 << 20
	// lineLimit cuts each line Display shows.
	lineLimit = 64 << 10
)

// Capture runs name in the clone with standard input closed and standard
// error discarded, and returns its standard output and exit code. A non-zero
// exit is not an error; a start failure, more than 8 MiB of output, or the
// context ending is, whatever the exit code.
func (h *Host) Capture(ctx context.Context, name string, args ...string) (stdout []byte, code int, err error) {
	return h.capture(ctx, nil, name, args...)
}

func (h *Host) capture(ctx context.Context, env []string, name string, args ...string) ([]byte, int, error) {
	var out capped
	code, err := h.run(ctx, env, nil, &out, nil, name, args...)
	if err == nil && out.over {
		err = fmt.Errorf("%s wrote more than 8 MiB", name)
	}
	return out.buf.Bytes(), code, err
}

// CLI runs the proxy's CLI in its container, as setup.sh does: docker compose
// exec -T, with the admin variables the TLS override needs. A nil stdin is
// /dev/null; an *os.File is handed to docker itself. It is a proxy.Runner.
func (h *Host) CLI(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	admin, err := h.admin()
	if err != nil {
		return 0, err
	}
	argv := append([]string{"compose", "exec", "-T"}, admin...)
	argv = append(append(argv, "stargaite", "/stargaite"), args...)
	return h.run(ctx, nil, stdin, stdout, stderr, "docker", argv...)
}

// Display runs name in the clone and hands say each line of its standard
// output and standard error, as they come, from the goroutine os/exec copies
// them on while the caller waits. A line naming .env is cut after its line
// number, since Compose quotes the rest of a .env line it cannot parse, which
// may be a credential. The exit code and the error are Capture's.
func (h *Host) Display(ctx context.Context, say func(string), name string, args ...string) (int, error) {
	out := &lines{say: say}
	code, err := h.run(ctx, nil, nil, out, out, name, args...)
	if len(out.buf) > 0 {
		out.flush()
	}
	return code, err
}

// Up runs docker compose up -d with args, shown through say and bounded by
// upBound, then waits for readiness.
func (h *Host) Up(ctx context.Context, say func(string), args ...string) error {
	bounded, cancel := context.WithTimeout(ctx, h.upBound)
	defer cancel()
	code, err := h.Display(bounded, say, "docker", append([]string{"compose", "up", "-d"}, args...)...)
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("docker compose up: %w", ctx.Err())
	case bounded.Err() != nil:
		return errors.New("docker compose up was not done within 20 minutes; its output is above")
	case err != nil || code != 0:
		return errors.New("docker compose up failed or was interrupted; its output is above")
	}
	return h.WaitReady(ctx, say)
}

// WaitReady checks readiness until it answers, as setup.sh's wait_ready
// does; otherwise it shows the container's last 30 log lines. A data plane
// that .env or the override cannot name is an error at once: no try would
// mend it.
func (h *Host) WaitReady(ctx context.Context, say func(string)) error {
	if _, err := h.Plane(); err != nil {
		return err
	}
	bounded, cancel := context.WithTimeout(ctx, h.ready.bound)
	defer cancel()
	for i := 0; i < h.ready.tries && bounded.Err() == nil; i++ {
		if i > 0 {
			select {
			case <-bounded.Done():
				continue
			case <-time.After(h.ready.pause):
			}
		}
		if h.readyCheck(bounded) == nil {
			return nil
		}
	}
	if ctx.Err() != nil {
		return fmt.Errorf("waiting for the proxy: %w", ctx.Err())
	}
	say("not ready within 120 s; the last 30 log lines:")
	logs, cancelLogs := context.WithTimeout(ctx, h.bound)
	defer cancelLogs()
	_, _ = h.Display(logs, say, "docker", "compose", "logs", "--tail", "30", "stargaite")
	if o, err := h.ReadOverride(); err == nil && o.Get("tls_name") != "" {
		say("./setup.sh remote again repairs a remote that did not finish")
	}
	return errors.New("the proxy did not become ready")
}

// ContainerRuns reports whether the clone's container runs. Compose's
// standard error is never shown, since it may quote a line of .env.
func (h *Host) ContainerRuns(ctx context.Context) (bool, error) {
	bounded, cancel := context.WithTimeout(ctx, h.bound)
	defer cancel()
	out, code, err := h.Capture(bounded, "docker", "compose", "ps", "-q")
	switch {
	case err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded):
		return false, fmt.Errorf("docker compose ps did not answer within %s", h.bound)
	case err != nil:
		return false, err
	case code != 0:
		return false, fmt.Errorf("docker compose ps failed with exit %d", code)
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// CloneVersion is the clone's git describe, as setup.sh's clone_version
// takes it, or dev when git is missing or fails.
func (h *Host) CloneVersion(ctx context.Context) string {
	bounded, cancel := context.WithTimeout(ctx, h.bound)
	defer cancel()
	env := []string{"GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -o BatchMode=yes"}
	out, code, err := h.capture(bounded, env, "git", "describe", "--tags", "--always", "--dirty")
	v := strings.TrimSpace(string(out))
	if err != nil || code != 0 || v == "" {
		return "dev"
	}
	return v
}

// admin is the variables each CLI call gets under TLS, as setup.sh's
// tls_state sets them: the CLI reaches the admin API by its TLS name,
// trusting the private CA when there is one. The override is read on each
// call.
func (h *Host) admin() ([]string, error) {
	name, ca, err := h.tlsNames()
	if err != nil || name == "" {
		return nil, err
	}
	args := []string{"-e", "STARGAITE_ADMIN_URL=https://" + name + ":4001"}
	if ca == "" {
		return args, nil
	}
	return append(args, "-e", "SSL_CERT_FILE=/tls/"+ca), nil
}

// run runs name in its own session, so it has no controlling terminal and
// its process group can be signalled as one. A cancel sends the group SIGINT,
// then the child is killed killDelay later; once it is reaped, whatever is
// left of the group is killed.
func (h *Host) run(ctx context.Context, env []string, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) (int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = h.Dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGINT) }
	cmd.WaitDelay = killDelay
	err := cmd.Run()
	if cmd.Process != nil {
		// ESRCH, the usual answer, means nothing of the group is left.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if ctx.Err() != nil {
		return 0, fmt.Errorf("%s: %w", name, ctx.Err())
	}
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		return exit.ExitCode(), nil
	case err != nil:
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return 0, nil
}

// capped keeps standard output up to captureLimit and notes an overrun.
type capped struct {
	buf  bytes.Buffer
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	if c.buf.Len()+len(p) > captureLimit {
		c.over = true
		return 0, errors.New("more than 8 MiB")
	}
	return c.buf.Write(p)
}

// lines hands say each line written to it: cut at lineLimit, the rest of a
// longer line dropped, and cut after the line number of a line naming .env.
type lines struct {
	say  func(string)
	buf  []byte
	drop bool // the rest of a line over lineLimit
}

func (l *lines) Write(p []byte) (int, error) {
	for _, c := range p {
		switch {
		case c == '\n' && l.drop:
			l.drop = false
		case c == '\n':
			l.flush()
		case !l.drop:
			l.buf = append(l.buf, c)
			if len(l.buf) == lineLimit {
				l.flush()
				l.drop = true
			}
		}
	}
	return len(p), nil
}

func (l *lines) flush() {
	l.say(cutEnvLine(string(l.buf)))
	l.buf = l.buf[:0]
}

// cutEnvLine cuts a line naming .env after its last line number, as
// setup.sh's check_docker does.
func cutEnvLine(line string) string {
	if !strings.Contains(line, ".env") {
		return line
	}
	if m := regexp.MustCompile(`^(.*line [0-9]+)`).FindString(line); m != "" {
		return m
	}
	return line
}
