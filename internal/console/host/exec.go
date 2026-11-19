package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	code, err := h.run(ctx, env, &out, nil, name, args...)
	if err == nil && out.over {
		err = fmt.Errorf("%s wrote more than 8 MiB", name)
	}
	return out.buf.Bytes(), code, err
}

// CLI runs the proxy's CLI in its container, as setup.sh does: docker compose
// exec -T, with the admin variables the TLS override needs. It is a
// proxy.Runner.
func (h *Host) CLI(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	admin, err := h.admin()
	if err != nil {
		return 0, err
	}
	argv := append([]string{"compose", "exec", "-T"}, admin...)
	argv = append(append(argv, "stargaite", "/stargaite"), args...)
	return h.run(ctx, nil, stdout, stderr, "docker", argv...)
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

// admin reads the override's "# setup:" lines on each call, as setup.sh's
// tls_state does: under TLS the CLI reaches the admin API by its TLS name,
// trusting the private CA when there is one.
func (h *Host) admin() ([]string, error) {
	data, err := os.ReadFile(filepath.Join(h.Dir, "compose.override.yaml"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("compose.override.yaml: %w", err)
	}
	var name, ca string
	for line := range strings.SplitSeq(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "# setup: tls_name="); ok {
			name = v
		}
		if v, ok := strings.CutPrefix(line, "# setup: tls_ca="); ok {
			ca = v
		}
	}
	if name == "" {
		return nil, nil
	}
	if !regexp.MustCompile(`^[A-Za-z0-9.-]+$`).MatchString(name) {
		return nil, errors.New("compose.override.yaml: tls_name is not a host name")
	}
	args := []string{"-e", "STARGAITE_ADMIN_URL=https://" + name + ":4001"}
	if ca == "" {
		return args, nil
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`).MatchString(ca) {
		return nil, errors.New("compose.override.yaml: tls_ca is not a file name in tls/")
	}
	return append(args, "-e", "SSL_CERT_FILE=/tls/"+ca), nil
}

// run runs name in its own session, so it has no controlling terminal and
// its process group can be signalled as one. A cancel sends the group SIGINT,
// then the child is killed killDelay later; once it is reaped, whatever is
// left of the group is killed.
func (h *Host) run(ctx context.Context, env []string, stdout, stderr io.Writer, name string, args ...string) (int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = h.Dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
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
