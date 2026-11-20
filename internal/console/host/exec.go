package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	// buildTail is how many of a build's last lines are shown when it fails.
	buildTail = 30
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
	admin, err := h.AdminEnv()
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
	return h.display(ctx, nil, nil, say, name, args...)
}

// display is Display with env added to the command's environment and stdin
// as its standard input (nil: none).
func (h *Host) display(ctx context.Context, env []string, stdin io.Reader, say func(string), name string, args ...string) (int, error) {
	out := &lines{say: say}
	code, err := h.run(ctx, env, stdin, out, out, name, args...)
	if len(out.buf) > 0 {
		out.flush()
	}
	return code, err
}

// Up runs docker compose up -d with args, shown through say and bounded by
// upBound, then waits for readiness.
func (h *Host) Up(ctx context.Context, say func(string), args ...string) error {
	return h.up(ctx, say, false, nil, args...)
}

// Build is Up with --build, building the image of version: Compose passes
// STARGAITE_VERSION to the build (compose.yaml), so a console run past the
// bootstrap still builds the clone's version. The build's many lines are
// kept back: only its last buildTail are shown, and only when it fails or
// runs out of time.
func (h *Host) Build(ctx context.Context, say func(string), version string) error {
	return h.up(ctx, say, true, []string{"STARGAITE_VERSION=" + version}, "--build")
}

// up is Up with env added to Compose's environment; when quiet, Compose's
// output is said only on failure, its last buildTail lines. Display hands
// the lines over from one goroutine, which has ended when it returns.
func (h *Host) up(ctx context.Context, say func(string), quiet bool, env []string, args ...string) error {
	bounded, cancel := context.WithTimeout(ctx, h.upBound)
	defer cancel()
	var tail []string
	show := say
	if quiet {
		show = func(l string) { tail = append(tail[max(0, len(tail)+1-buildTail):], l) }
	}
	failed := func(msg string) error {
		if len(tail) > 0 {
			say("the build's last lines:")
		}
		for _, l := range tail {
			say(l)
		}
		return errors.New(msg)
	}
	code, err := h.display(bounded, env, nil, show, "docker", append([]string{"compose", "up", "-d"}, args...)...)
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("docker compose up: %w", ctx.Err())
	case bounded.Err() != nil:
		return failed("docker compose up was not done within 20 minutes; its output is above")
	case err != nil || code != 0:
		return failed("docker compose up failed or was interrupted; its output is above")
	}
	return h.WaitReady(ctx, say)
}

// Compose runs docker compose with args (stop, restart, down), its lines
// shown through say, bounded by stopBound.
func (h *Host) Compose(ctx context.Context, say func(string), args ...string) error {
	bounded, cancel := context.WithTimeout(ctx, h.stopBound)
	defer cancel()
	code, err := h.Display(bounded, say, "docker", append([]string{"compose"}, args...)...)
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("docker compose %s: %w", args[0], ctx.Err())
	case bounded.Err() != nil:
		return fmt.Errorf("docker compose %s was not done within 2 minutes", args[0])
	case err != nil:
		return err
	case code != 0:
		return fmt.Errorf("docker compose %s failed with exit %d", args[0], code)
	}
	return nil
}

// SeeLogs hands say the container's last 100 log lines, bounded by h.bound,
// or, when follow, the lines it logs from now on until ctx ends or
// followBound passes: timedOut is then true and the error nil (OPS-11).
// Each line is cut as Display cuts it.
func (h *Host) SeeLogs(ctx context.Context, say func(string), follow bool) (timedOut bool, err error) {
	bound, args := h.bound, []string{"--tail", "100"}
	if follow {
		bound, args = h.followBound, []string{"--follow", "--tail", "0"}
	}
	bounded, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	argv := append(append([]string{"compose", "logs", "--no-log-prefix"}, args...), "stargaite")
	code, err := h.Display(bounded, say, "docker", argv...)
	switch {
	case ctx.Err() != nil:
		return false, fmt.Errorf("docker compose logs: %w", ctx.Err())
	case bounded.Err() != nil && follow:
		return true, nil
	case bounded.Err() != nil:
		return false, fmt.Errorf("docker compose logs did not answer within %s", h.bound)
	case err != nil:
		return false, err
	case code != 0:
		return false, fmt.Errorf("docker compose logs failed with exit %d", code)
	}
	return false, nil
}

// Config is the image and the volume of the clone's Compose project, read
// from docker compose config within h.bound; the volume is named from the
// project, as Compose names it, when the answer names none. The answer
// holds every .env value in clear, so no error quotes it, nor the
// decoder's error. An image that would read as a flag is none.
func (h *Host) Config(ctx context.Context) (image, volume string, err error) {
	bounded, cancel := context.WithTimeout(ctx, h.bound)
	defer cancel()
	out, code, err := h.Capture(bounded, "docker", "compose", "config", "--format", "json")
	switch {
	case err != nil:
		return "", "", err
	case code != 0:
		return "", "", fmt.Errorf("docker compose config failed with exit %d", code)
	}
	var doc struct {
		Name     string `json:"name"`
		Services struct {
			Stargaite struct {
				Image string `json:"image"`
			} `json:"stargaite"`
		} `json:"services"`
		Volumes struct {
			Data struct {
				Name string `json:"name"`
			} `json:"stargaite-data"`
		} `json:"volumes"`
	}
	if json.Unmarshal(out, &doc) != nil {
		return "", "", errors.New("docker compose config gave no JSON it could read")
	}
	image, volume = doc.Services.Stargaite.Image, doc.Volumes.Data.Name
	switch {
	case image == "" || strings.HasPrefix(image, "-"):
		return "", "", errors.New("docker compose config names no image")
	case volume == "" && doc.Name == "":
		return "", "", errors.New("docker compose config names no project")
	case volume == "":
		volume = doc.Name + "_stargaite-data"
	}
	return image, volume, nil
}

// RemoveImage removes the image tag, never forced, within h.bound, Docker's
// words said: "removed", "not there" when no image has the tag, or
// "refused", as when a container uses it. One tag per call, since one
// docker rmi of two tags may remove one and refuse the other.
func (h *Host) RemoveImage(ctx context.Context, say func(string), tag string) (string, error) {
	bounded, cancel := context.WithTimeout(ctx, h.bound)
	defer cancel()
	_, code, err := h.Capture(bounded, "docker", "image", "inspect", "--format", "{{.Id}}", tag)
	switch {
	case err != nil:
		return "", err
	case code != 0:
		return "not there", nil
	}
	code, err = h.Display(bounded, say, "docker", "rmi", tag)
	switch {
	case err != nil:
		return "", err
	case code != 0:
		return "refused", nil
	}
	return "removed", nil
}

// RemoveConsole removes bin/stargaite-console; one already gone is no
// error. The running console keeps its open file.
func (h *Host) RemoveConsole() error {
	err := os.Remove(filepath.Join(h.Dir, "bin", "stargaite-console"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
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
	h.Logs(ctx, say)
	if o, err := h.ReadOverride(); err == nil && o.Get("tls_name") != "" {
		say("./setup.sh remote again repairs a remote that did not finish")
	}
	return errors.New("the proxy did not become ready")
}

// Logs hands say the container's last 30 log lines, bounded by h.bound. It is
// best effort: a failure says nothing more.
func (h *Host) Logs(ctx context.Context, say func(string)) {
	bounded, cancel := context.WithTimeout(ctx, h.bound)
	defer cancel()
	_, _ = h.Display(bounded, say, "docker", "compose", "logs", "--tail", "30", "stargaite")
}

// ContainerRuns reports whether the clone's container runs. Compose's
// standard error is never shown, since it may quote a line of .env.
func (h *Host) ContainerRuns(ctx context.Context) (bool, error) {
	return h.ps(ctx, "-q")
}

// ContainerExists reports whether the clone's container exists, running or
// stopped, as ContainerRuns does.
func (h *Host) ContainerExists(ctx context.Context) (bool, error) {
	return h.ps(ctx, "-a", "-q")
}

// ps reports whether docker compose ps with args lists a container, bounded
// by h.bound.
func (h *Host) ps(ctx context.Context, args ...string) (bool, error) {
	bounded, cancel := context.WithTimeout(ctx, h.bound)
	defer cancel()
	out, code, err := h.Capture(bounded, "docker", append([]string{"compose", "ps"}, args...)...)
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

// AdminEnv is the variables each CLI call gets under TLS, as setup.sh's
// tls_state sets them: the CLI reaches the admin API by its TLS name,
// trusting the private CA when there is one. The override is read on each
// call.
func (h *Host) AdminEnv() ([]string, error) {
	name, ca, err := h.TLSNames()
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
