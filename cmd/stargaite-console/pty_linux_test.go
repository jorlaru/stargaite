//go:build linux

package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// terminal is the console run on a pseudo-terminal of 80x24, its
// controlling terminal, with everything it writes kept.
type terminal struct {
	t             *testing.T
	master, slave *os.File
	cmd           *exec.Cmd
	before        *unix.Termios
	mu            sync.Mutex
	out           bytes.Buffer
	read          chan struct{} // closed when the master reads EOF or EIO
	waited        chan struct{} // closed when the console has exited
}

// openPTY is a pseudo-terminal pair on x/sys/unix alone.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	fd := int(m.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	s, err := os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := unix.IoctlSetWinsize(int(s.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: 80}); err != nil {
		t.Fatal(err)
	}
	return m, s
}

// onTerminal starts the console with args on a new pseudo-terminal.
func onTerminal(t *testing.T, env []string, args ...string) *terminal {
	t.Helper()
	m, s := openPTY(t)
	before, err := unix.IoctlGetTermios(int(s.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(console, args...)
	cmd.Env = append(env, "TERM=xterm-256color")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = s, s, s
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	tm := &terminal{t: t, master: m, slave: s, cmd: cmd, before: before, read: make(chan struct{}), waited: make(chan struct{})}
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-tm.waited })
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := m.Read(buf)
			tm.mu.Lock()
			tm.out.Write(buf[:n])
			tm.mu.Unlock()
			if err != nil {
				close(tm.read)
				return
			}
		}
	}()
	go func() { _ = cmd.Wait(); close(tm.waited) }()
	return tm
}

func (tm *terminal) output() string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.out.String()
}

// waitFor waits until s has been written n times.
func (tm *terminal) waitFor(s string, n int) {
	tm.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); strings.Count(tm.output(), s) < n; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			tm.t.Fatalf("%q not written %d times:\n%q", s, n, tm.output())
		}
	}
}

// pressUntil writes key every half second until s has been written n
// times: a key that comes while no question is asked is dropped, and the
// frame that asks again may equal the last one drawn, which is not drawn
// again, so nothing on screen says when the question is back.
func (tm *terminal) pressUntil(key, s string, n int) {
	tm.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); strings.Count(tm.output(), s) < n; {
		if time.Now().After(deadline) {
			tm.t.Fatalf("%q not written %d times:\n%q", s, n, tm.output())
		}
		tm.write(key)
		for next := time.Now().Add(500 * time.Millisecond); time.Now().Before(next) && strings.Count(tm.output(), s) < n; {
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func (tm *terminal) write(s string) {
	tm.t.Helper()
	if _, err := io.WriteString(tm.master, s); err != nil {
		tm.t.Fatal(err)
	}
}

// exit waits for the console to exit, then for the last of its output,
// and returns its exit code and the terminal's settings after it.
func (tm *terminal) exit() (int, *unix.Termios) {
	tm.t.Helper()
	select {
	case <-tm.waited:
	case <-time.After(15 * time.Second):
		tm.t.Fatalf("the console did not exit:\n%q", tm.output())
	}
	after, err := unix.IoctlGetTermios(int(tm.slave.Fd()), unix.TCGETS)
	if err != nil {
		tm.t.Fatal(err)
	}
	_ = tm.slave.Close() // the master then reads what is left, then EIO
	select {
	case <-tm.read:
	case <-time.After(5 * time.Second):
		tm.t.Fatal("the terminal's output did not end")
	}
	return tm.cmd.ProcessState.ExitCode(), after
}

// restored checks that the console left the terminal as it found it.
func (tm *terminal) restored(after *unix.Termios) {
	tm.t.Helper()
	b := tm.before
	if b.Iflag != after.Iflag || b.Oflag != after.Oflag || b.Lflag != after.Lflag || b.Cflag != after.Cflag {
		tm.t.Fatalf("termios before %+v, after %+v", *b, *after)
	}
	out := tm.output()
	on := strings.LastIndex(out, "\x1b[?2004h")
	if on < 0 || !strings.Contains(out[on:], "\x1b[?2004l") || !strings.Contains(out[on:], "\x1b[?25h") {
		tm.t.Fatalf("bracketed paste or cursor not restored:\n%q", out)
	}
	if push, pop := strings.Count(out, "\x1b[>1u"), strings.Count(out, "\x1b[<1u"); push != pop {
		tm.t.Fatalf("kitty keyboard pushed %d times, popped %d", push, pop)
	}
	if m := strings.LastIndex(out, "\x1b[>4;2m"); m >= 0 && !strings.Contains(out[m:], "\x1b[>4m") {
		tm.t.Fatalf("modifyOtherKeys not reset:\n%q", out)
	}
}

const home = "What do you want to do?"

func TestRestoreAfterCtrlC(t *testing.T) {
	env, _ := fake(t, replies)
	tm := onTerminal(t, env)
	tm.waitFor(home, 1)
	tm.write("\x03")
	code, after := tm.exit()
	if code != 130 {
		t.Fatalf("exit %d; want 130\n%q", code, tm.output())
	}
	tm.restored(after)
}

func restoreAfterSignal(t *testing.T, sig syscall.Signal, want int) {
	env, _ := fake(t, replies)
	tm := onTerminal(t, env)
	tm.waitFor(home, 1)
	if err := tm.cmd.Process.Signal(sig); err != nil {
		t.Fatal(err)
	}
	code, after := tm.exit()
	if code != want {
		t.Fatalf("exit %d; want %d\n%q", code, want, tm.output())
	}
	tm.restored(after)
}

func TestRestoreAfterSIGTERM(t *testing.T) { restoreAfterSignal(t, syscall.SIGTERM, 143) }

func TestRestoreAfterSIGHUP(t *testing.T) { restoreAfterSignal(t, syscall.SIGHUP, 129) }

// Ctrl-C while a task works: the command gets SIGINT, the console exits 130.
func TestGuidedCtrlCWhileWorking(t *testing.T) {
	env, log := fake(t, hang)
	tm := onTerminal(t, env)
	tm.waitFor(home, 1)
	tm.write("\r")
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(readLog(t, log), "compose exec"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("compose exec never ran; log %q", readLog(t, log))
		}
	}
	time.Sleep(100 * time.Millisecond) // the fake has installed its trap
	sent := time.Now()
	tm.write("\x03")
	code, after := tm.exit()
	if took := time.Since(sent); code != 130 || took > 5*time.Second || !strings.Contains(readLog(t, log), "INT\n") {
		t.Fatalf("exit %d after %s, log %q; want 130 within 5s with INT", code, took, readLog(t, log))
	}
	tm.restored(after)
}

func TestGuidedSeeWhatExists(t *testing.T) {
	env, _ := fake(t, replies)
	tm := onTerminal(t, env)
	tm.waitFor(home, 1)
	tm.write("\r")
	tm.waitFor("version: v1.2.3 running, dev in this clone", 1)
	tm.waitFor("provider acme: credential resolved", 1)
	tm.pressUntil("\r", "version: v1.2.3 running, dev in this clone", 2) // back at home: Enter runs it again
	tm.write("\x03")
	if code, after := tm.exit(); code != 130 {
		t.Fatalf("exit %d; want 130", code)
	} else {
		tm.restored(after)
	}
}

func TestGuidedTaskError(t *testing.T) {
	env, _ := fake(t, "case \"$*\" in \"compose ps -q\") echo c0ffee ;; *) exit 1 ;; esac\n")
	tm := onTerminal(t, env)
	tm.waitFor(home, 1)
	tm.write("\r")
	tm.waitFor("See what exists: stargaite version: failed with exit 1", 1)
	tm.pressUntil("\r", "See what exists: stargaite version: failed with exit 1", 2) // back at home
	tm.write("\x03")
	if code, _ := tm.exit(); code != 130 {
		t.Fatalf("exit %d; want 130", code)
	}
}

func TestTeaTraceRefused(t *testing.T) {
	for _, v := range []string{"TEA_TRACE", "TEA_DEBUG"} {
		env, log := fake(t, replies)
		trace := filepath.Join(t.TempDir(), "f")
		tm := onTerminal(t, append(env, v+"="+trace))
		code, _ := tm.exit()
		want := "stargaite-console: " + v + " is set: the console would write every byte it draws to a file, a client key included; unset it"
		if code != 1 || !strings.Contains(tm.output(), want) {
			t.Fatalf("%s: exit %d\n%q", v, code, tm.output())
		}
		if _, err := os.Stat(trace); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s: the trace file exists: %v", v, err)
		}
		if l := readLog(t, log); l != "" {
			t.Fatalf("%s: docker ran: %q", v, l)
		}
	}
}

func TestChildHasNoTerminal(t *testing.T) {
	env, log := fake(t, `case "$*" in
"compose ps -q") if (: </dev/tty) 2>/dev/null; then echo tty >> "$FAKE_LOG"; else echo no-tty >> "$FAKE_LOG"; fi ;;
*) exit 9 ;;
esac
`)
	tm := onTerminal(t, env, "status")
	tm.waitFor("no container runs", 1)
	if code, _ := tm.exit(); code != 0 {
		t.Fatalf("exit %d\n%q", code, tm.output())
	}
	if l := readLog(t, log); !strings.Contains(l, "no-tty\n") || strings.Contains(l, "\ntty\n") {
		t.Fatalf("log %q; want no-tty", l)
	}
}
