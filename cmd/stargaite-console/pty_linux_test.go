//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	return onTerminalAt(t, console, env, args...)
}

// onTerminalAt is onTerminal with the console at bin.
func onTerminalAt(t *testing.T, bin string, env []string, args ...string) *terminal {
	t.Helper()
	m, s := openPTY(t)
	return startOn(t, m, s, bin, env, args...)
}

// startOn starts the console at bin with args on the pseudo-terminal m, s.
func startOn(t *testing.T, m, s *os.File, bin string, env []string, args ...string) *terminal {
	t.Helper()
	before, err := unix.IoctlGetTermios(int(s.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, args...)
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
	tm.write("See what\r")
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
	tm.write("See what\r")
	tm.waitFor("version: v1.2.3 running, dev in this clone", 1)
	tm.waitFor("provider acme: credential resolved", 1)
	tm.pressUntil("See what\r", "version: v1.2.3 running, dev in this clone", 2) // back at home: picking it runs it again
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
	tm.write("See what\r")
	tm.waitFor("See what exists: stargaite version: failed with exit 1", 1)
	tm.pressUntil("See what\r", "See what exists: stargaite version: failed with exit 1", 2) // back at home
	tm.write("\x03")
	if code, _ := tm.exit(); code != 130 {
		t.Fatalf("exit %d; want 130", code)
	}
}

func TestTeaTraceRefused(t *testing.T) {
	for _, row := range []struct {
		v    string
		args []string
	}{{"TEA_TRACE", nil}, {"TEA_DEBUG", nil}, {"TEA_TRACE", []string{"status"}}} {
		v := row.v
		env, log := fake(t, replies)
		trace := filepath.Join(t.TempDir(), "f")
		tm := onTerminal(t, append(env, v+"="+trace), row.args...)
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

// A subcommand at a terminal runs its task in the guided front.
func TestStatusAtTerminalIsGuided(t *testing.T) {
	env, _ := fake(t, replies)
	tm := onTerminal(t, env, "status")
	tm.waitFor("version: v1.2.3 running", 1)
	code, after := tm.exit()
	if code != 0 || !strings.Contains(tm.output(), "\x1b[?2004h") {
		t.Fatalf("exit %d, or not drawn by the guided front:\n%q", code, tm.output())
	}
	tm.restored(after)
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

// The secret pasted at the terminal, with the line break a copied line
// brings, never reaches the screen, an argv, a log or a file but .env.
func TestProviderSecretNeverOnTerminal(t *testing.T) {
	const secret = `s3cr3t $HOME #x y\z`
	bin, dir, env, log, flags, auth := acmeClone(t, secret)
	tm := onTerminalAt(t, bin, env, flags...)
	tm.waitFor("typed hidden", 1)
	tm.write("\x1b[200~" + secret + "\r\x1b[201~")
	tm.write("\r")
	code, after := tm.exit()
	if code != 0 || strings.Contains(tm.output(), "s3cr3t") {
		t.Fatalf("exit %d, or the secret is on the terminal:\n%q", code, tm.output())
	}
	tm.restored(after)
	dotEnv, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil || !strings.Contains(string(dotEnv), "\nACME_API_KEY='"+secret+"'\n") || strings.Contains(string(dotEnv), "\r") {
		t.Fatalf(".env %q, %v", dotEnv, err)
	}
	if got := auth(); len(got) != 1 || got[0] != "Bearer "+secret {
		t.Fatalf("the model list got %q", got)
	}
	if strings.Contains(readLog(t, log), "s3cr3t") {
		t.Fatal("the docker log holds the secret")
	}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() == ".env" {
			return err
		}
		if b, err := os.ReadFile(path); err == nil && bytes.Contains(b, []byte("s3cr3t")) {
			t.Errorf("%s holds the secret", path)
		}
		return nil
	})
}

// Ctrl-C while the container is recreated: compose up gets SIGINT, the
// console exits 130 and restores the terminal, and the terminal still shows
// how to apply the files written.
func TestProviderCtrlCDuringRecreate(t *testing.T) {
	env, log := fake(t, ollama(trapUp))
	bin, _, _ := ownClone(t, nil)
	tm := onTerminalAt(t, bin, env, "provider", "--provider", "ollama", "--yes")
	tm.waitFor("recreating the container", 1)
	waitLog(t, log, "compose up")
	sent := time.Now()
	tm.write("\x03")
	code, after := tm.exit()
	if took := time.Since(sent); code != 130 || took > 12*time.Second || !strings.Contains(readLog(t, log), "INT\n") {
		t.Fatalf("exit %d after %s, log %q", code, took, readLog(t, log))
	}
	tm.restored(after)
	if !strings.Contains(tm.output(), "the files are written: if the recreate stops") {
		t.Errorf("output:\n%q", tm.output())
	}
}

// --key-env names the variable at a terminal too: the key comes from the
// console's environment, and no hidden question is asked.
func TestProviderKeyEnvAtTerminal(t *testing.T) {
	const secret = `s3cr3t $HOME #x y\z`
	bin, dir, env, _, flags, auth := acmeClone(t, secret)
	tm := onTerminalAt(t, bin, append(env, "SRC="+secret), append(flags, "--key-env", "SRC")...)
	code, after := tm.exit()
	if code != 0 || strings.Contains(tm.output(), "typed hidden") || strings.Contains(tm.output(), "s3cr3t") {
		t.Fatalf("exit %d:\n%q", code, tm.output())
	}
	tm.restored(after)
	if dotEnv, err := os.ReadFile(filepath.Join(dir, ".env")); err != nil || !strings.Contains(string(dotEnv), "\nACME_API_KEY='"+secret+"'\n") {
		t.Fatalf(".env %q, %v", dotEnv, err)
	}
	if got := auth(); len(got) != 1 || got[0] != "Bearer "+secret {
		t.Fatalf("the model list got %q", got)
	}
}

// Esc at the hidden question leaves the subcommand: it says so, exits 1,
// and writes nothing.
func TestProviderEscLeaves(t *testing.T) {
	bin, dir, env, _, flags, _ := acmeClone(t, "k")
	before, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	tm := onTerminalAt(t, bin, env, flags...)
	tm.waitFor("typed hidden", 1)
	tm.write("\x1b")
	code, after := tm.exit()
	if code != 1 || !strings.Contains(tm.output(), "Left Add providers.") {
		t.Fatalf("exit %d:\n%q", code, tm.output())
	}
	tm.restored(after)
	if now, err := os.ReadFile(filepath.Join(dir, ".env")); err != nil || !bytes.Equal(now, before) {
		t.Fatalf(".env %q, %v; was %q", now, err, before)
	}
}

// halfTerminal runs status with only standard input, or only standard
// output, on a pseudo-terminal: the other is a pipe, or /dev/null. It
// returns the exit code and everything written.
func halfTerminal(t *testing.T, inOnTTY bool) (int, string) {
	t.Helper()
	env, _ := fake(t, replies)
	m, s := openPTY(t)
	cmd := exec.Command(console, "status")
	cmd.Env = append(env, "TERM=xterm-256color")
	var pipe bytes.Buffer
	if inOnTTY {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = s, &pipe, &pipe
	} else {
		cmd.Stdout, cmd.Stderr = s, s // standard input /dev/null
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var tty bytes.Buffer
	read := make(chan struct{})
	go func() { _, _ = io.Copy(&tty, m); close(read) }()
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		<-waited
		t.Fatal("status did not exit within 15 s")
	}
	_ = s.Close() // the master then reads what is left, then EIO
	<-read
	return cmd.ProcessState.ExitCode(), pipe.String() + tty.String()
}

// Only standard input and output both on a terminal make the guided front:
// a pipe or /dev/null on either side gives the plain lines.
func TestStatusHalfTerminalIsPlain(t *testing.T) {
	for _, inOnTTY := range []bool{true, false} {
		code, out := halfTerminal(t, inOnTTY)
		if code != 0 || !strings.Contains(out, "version: v1.2.3 running, dev in this clone") || strings.Contains(out, "\x1b[?2004h") {
			t.Fatalf("standard input on the terminal %v: exit %d\n%q", inOnTTY, code, out)
		}
	}
}

// alias at a terminal asks the alias's name first: one that exists is kept,
// with no other question and no other CLI call.
func TestAliasKeptAtTerminal(t *testing.T) {
	env, log := fake(t, `case "$*" in
"compose ps -q") echo c0ffee ;;
"compose exec -T stargaite /stargaite models list --json") echo '{"models":[{"name":"main","targets":["openai/gpt-b"]}]}' ;;
*) exit 9 ;;
esac
`)
	bin, _, _ := ownClone(t, nil)
	tm := onTerminalAt(t, bin, env, "alias")
	tm.waitFor("a name for the alias", 1)
	tm.write("\r")
	code, after := tm.exit()
	if code != 0 || !strings.Contains(tm.output(), "alias main exists, toward openai/gpt-b; kept") {
		t.Fatalf("exit %d:\n%q", code, tm.output())
	}
	tm.restored(after)
	var execs []string
	for _, l := range strings.Split(readLog(t, log), "\n") {
		if strings.HasPrefix(l, "compose exec") {
			execs = append(execs, l)
		}
	}
	if len(execs) != 1 || execs[0] != "compose exec -T stargaite /stargaite models list --json" {
		t.Fatalf("CLI execs %q", execs)
	}
}

// Esc while a target is checked closes the CLI's standard input, so the
// server starts no further call: the console says the call in flight may
// still be billed, leaves the task and makes no alias.
func TestAliasEscDuringCheckAtTerminal(t *testing.T) {
	const cli = `"compose exec -T stargaite /stargaite `
	env, log := fake(t, `case "$*" in
"compose ps -q") echo c0ffee ;;
`+cli+`models list --json") echo '{"models":[]}' ;;
`+cli+`providers list --json") echo '{"providers":[{"name":"acme","protocol":"openai","base_url":"http://localhost:1/v1","credential":"none"}]}' ;;
`+cli+`targets list --json") echo '{"targets":[]}' ;;
`+cli+`apply --json --dry-run -f -") /bin/cat >/dev/null; echo '{"revision":1,"changed":false,"diff":[],"findings":[]}' ;;
`+cli+`targets add --json --kind chat -- acme/m1") echo '{"findings":[],"resource":{"id":"acme/m1","kind":"chat"}}' ;;
`+cli+`settings show --json") echo '{"timeouts":{"total_ms":30000}}' ;;
`+cli+`targets verify --json --max-output-tokens 256 --until-stdin-closes -- acme/m1") /bin/cat >/dev/null; echo eof >>"$FAKE_LOG"; `+
		`echo '{"target":"acme/m1","ok":true,"stored":true,"calls":[{"effort":"","status":200,"output_tokens":2}]}' ;;
*) exit 9 ;;
esac
`)
	bin, _, _ := ownClone(t, nil)
	tm := onTerminalAt(t, bin, env, "alias", "--provider", "acme", "--model", "m1", "--alias", "x")
	tm.waitFor("Fallback 1 of 2", 1)
	tm.write("\r") // the cursor is on "no more fallbacks"
	tm.waitFor("a default reasoning effort", 1)
	tm.write("\r")
	tm.waitFor("1 paid call(s) in all", 1)
	time.Sleep(450 * time.Millisecond)
	tm.write("\r")
	tm.waitFor("checking acme/m1…", 1)
	waitLog(t, log, "targets verify")
	sent := time.Now()
	tm.write("\x1b")
	code, after := tm.exit()
	if took := time.Since(sent); code != 1 || took > 12*time.Second {
		t.Fatalf("exit %d after %s:\n%q", code, took, tm.output())
	}
	tm.restored(after)
	out := tm.output()
	if !strings.Contains(out, "the check of acme/m1 was stopped: the call in flight may still be billed") || !strings.Contains(out, "Left Make an alias.") {
		t.Errorf("output:\n%q", out)
	}
	if l := readLog(t, log); !strings.Contains(l, "\neof\n") || strings.Contains(l, "models add") {
		t.Errorf("docker log %q", l)
	}
}

// key at a terminal, for a name that exists: asked again, Enter makes no
// key, with no other CLI call.
func TestKeyExistsAtTerminal(t *testing.T) {
	env, log := fake(t, `case "$*" in
"compose ps -q") echo c0ffee ;;
"compose exec -T stargaite /stargaite models list --json") echo '{"models":[{"name":"main","targets":["acme/a"]}]}' ;;
"compose exec -T stargaite /stargaite keys list --json") echo '{"keys":[{"name":"my-app","models":["main"]}]}' ;;
*) exit 9 ;;
esac
`)
	bin, _, _ := ownClone(t, nil)
	tm := onTerminalAt(t, bin, env, "key")
	tm.waitFor("Which aliases may it call?", 1)
	tm.write("\r")
	tm.waitFor("a name for the key", 1)
	tm.write("\r")
	tm.waitFor("another name, or Enter to make no key", 1)
	tm.write("\r")
	code, after := tm.exit()
	if code != 0 || !strings.Contains(tm.output(), "No key made: Change a key rotates a key that exists.") {
		t.Fatalf("exit %d:\n%q", code, tm.output())
	}
	tm.restored(after)
	var execs []string
	for _, l := range strings.Split(readLog(t, log), "\n") {
		if strings.HasPrefix(l, "compose exec") {
			execs = append(execs, l)
		}
	}
	if want := []string{"compose exec -T stargaite /stargaite models list --json", "compose exec -T stargaite /stargaite keys list --json"}; !slices.Equal(execs, want) {
		t.Fatalf("CLI execs %q", execs)
	}
}

// toReveal starts key at a terminal on keyClone's world and answers it up
// to the reveal, then waits past the reveal's guard on Enter.
func toReveal(t *testing.T) *terminal {
	t.Helper()
	bin, env, _, _ := keyClone(t, http.StatusOK, chatBody, keyCLI)
	tm := onTerminalAt(t, bin, env, "key")
	for _, s := range []string{"Which aliases may it call?", "a name for the key", "the requests a minute"} {
		tm.waitFor(s, 1)
		tm.write("\r")
	}
	tm.waitFor("enter: I have stored it", 1)
	time.Sleep(450 * time.Millisecond)
	return tm
}

// The client key is written only on the alternate screen (design section
// 10): stored, then Esc at the paid step; resized while shown; and on
// SIGTERM while shown. The raw stream is checked, not an emulator.
func TestKeyOnlyOnAltScreen(t *testing.T) {
	leave := func(tm *terminal) {
		tm.write("\r")
		tm.waitFor("try it: up to 1 paid call(s)", 1)
		time.Sleep(450 * time.Millisecond)
		tm.write("\x1b")
	}
	for _, c := range []struct {
		name string
		do   func(*terminal)
		want int
	}{
		{"stored", leave, 1},
		{"resized", func(tm *terminal) {
			n := len(tm.output())
			if err := unix.IoctlSetWinsize(int(tm.slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 30, Col: 100}); err != nil {
				t.Fatal(err)
			}
			if err := tm.cmd.Process.Signal(syscall.SIGWINCH); err != nil {
				t.Fatal(err)
			}
			for deadline := time.Now().Add(5 * time.Second); len(tm.output()) == n; time.Sleep(10 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("no redraw after the resize")
				}
			}
			time.Sleep(200 * time.Millisecond)
			leave(tm)
		}, 1},
		{"SIGTERM", func(tm *terminal) {
			if err := tm.cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
		}, 143},
	} {
		t.Run(c.name, func(t *testing.T) {
			tm := toReveal(t)
			c.do(tm)
			code, after := tm.exit()
			out := tm.output()
			if code != c.want {
				t.Fatalf("exit %d, want %d:\n%q", code, c.want, out)
			}
			tm.restored(after)
			if !strings.Contains(out, "\x1b[?1049h") || !strings.Contains(out, "\x1b[?1049l") {
				t.Fatalf("the stream holds no alternate-screen enter (\\x1b[?1049h) or leave (\\x1b[?1049l):\n%q", out)
			}
			if !strings.Contains(out, keySecret) || !onAltScreenOnly(out, keySecret) {
				t.Fatalf("the secret is missing, or written off the alternate screen:\n%q", out)
			}
			// Said before the key is shown, so it stays in the history (owner decision 2).
			if i := strings.Index(out, "is made: its secret is shown next"); i < 0 || i > strings.Index(out, "\x1b[?1049h") {
				t.Fatalf("the line that the key is made is not said before the alternate screen:\n%q", out)
			}
		})
	}
}

// Ctrl-C while keys create runs, before any secret is shown: the console
// exits 130, and what the stop may leave was said before the command ran.
func TestKeyCtrlCDuringCreate(t *testing.T) {
	env, _ := fake(t, strings.Replace(keyCLI, "echo '{\"findings\":[],\"secrets\"", "trap 'exit 130' INT; /bin/sleep 60 >/dev/null 2>&1 & wait; echo '{\"findings\":[],\"secrets\"", 1))
	bin, _, _ := ownClone(t, nil)
	tm := onTerminalAt(t, bin, env, "key")
	for _, s := range []string{"Which aliases may it call?", "a name for the key", "the requests a minute"} {
		tm.waitFor(s, 1)
		tm.write("\r")
	}
	tm.waitFor("creating key my-app", 1)
	time.Sleep(200 * time.Millisecond)
	tm.write("\x03")
	code, after := tm.exit()
	if code != 130 || !strings.Contains(tm.output(), "if this stops before you store its secret, key my-app may be made without it: Change a key rotates it") {
		t.Fatalf("exit %d:\n%q", code, tm.output())
	}
	tm.restored(after)
}

// onAltScreenOnly reports whether every occurrence of s in out lies after an
// alternate-screen enter and before the next leave.
func onAltScreenOnly(out, s string) bool {
	for i := strings.Index(out, s); i >= 0; {
		on, off := strings.LastIndex(out[:i], "\x1b[?1049h"), strings.LastIndex(out[:i], "\x1b[?1049l")
		if on < 0 || off > on {
			return false
		}
		next := strings.Index(out[i+len(s):], s)
		if next < 0 {
			break
		}
		i += len(s) + next
	}
	return true
}

// execs is the CLI calls in the fake docker's log.
func execs(t *testing.T, log string) []string {
	t.Helper()
	var out []string
	for _, l := range strings.Split(readLog(t, log), "\n") {
		if strings.HasPrefix(l, "compose exec") {
			out = append(out, l)
		}
	}
	return out
}

// install at a terminal on a store with a provider and an alias: the first
// run starts at the key, under its bar, and ends with the first answer.
func TestFirstRunAtTerminalResumesAtKey(t *testing.T) {
	bin, env, log, _ := keyClone(t, http.StatusOK, chatBody, firstCLI)
	tm := onTerminalAt(t, bin, env, "install")
	tm.waitFor("Which aliases may it call?", 1)
	tm.write("\r")
	tm.waitFor("a name for the key", 1)
	tm.write("my-app\r")
	tm.waitFor("the requests a minute", 1)
	tm.write("\r")
	tm.waitFor("enter: I have stored it", 1)
	time.Sleep(450 * time.Millisecond)
	tm.write("\r")
	tm.waitFor("try it: up to 1 paid call(s)", 1)
	time.Sleep(450 * time.Millisecond)
	tm.write("\r")
	code, after := tm.exit()
	out := tm.output()
	if code != 0 || !strings.Contains(out, "answer: fake reply") || !strings.Contains(out, "Done: key my-app, which may call main.") {
		t.Fatalf("exit %d:\n%q", code, out)
	}
	tm.restored(after)
	reveal := strings.Index(out, "\x1b[?1049h")
	if k, a := strings.Index(out, "● key"), strings.LastIndex(out, "● first answer"); k < 0 || k > reveal || a < reveal {
		t.Errorf("the bar is not at key before the reveal (at %d, the reveal at %d) and at first answer after it (at %d):\n%q", k, reveal, a, out)
	}
	if !onAltScreenOnly(out, keySecret) {
		t.Errorf("the secret is written off the alternate screen:\n%q", out)
	}
	for _, e := range execs(t, log) {
		if strings.Contains(e, "providers add") || strings.Contains(e, "targets verify") || strings.Contains(e, "models add") {
			t.Errorf("CLI call %q", e)
		}
	}
}

// Ctrl-C during the first build: compose up gets SIGINT, the console exits
// 130, restores the terminal and makes no CLI call (successor of
// tests/setup's TestSetupInterrupt for install).
func TestFirstRunCtrlCDuringBuild(t *testing.T) {
	env, log := fake(t, `case "$*" in
"compose ps -q"|"compose ps -a -q") ;;
"compose up -d --build") `+trapUp+` ;;
*) exit 9 ;;
esac
`)
	bin, dir, _ := ownClone(t, nil)
	port := closedPort()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("STARGAITE_PORT="+port+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tm := onTerminalAt(t, bin, env, "install", "--port", port)
	waitLog(t, log, "compose up")
	sent := time.Now()
	tm.write("\x03")
	code, after := tm.exit()
	if took := time.Since(sent); code != 130 || took > 12*time.Second || !strings.Contains(readLog(t, log), "INT\n") {
		t.Fatalf("exit %d after %s, log %q", code, took, readLog(t, log))
	}
	tm.restored(after)
	if e := execs(t, log); len(e) != 0 {
		t.Errorf("CLI calls %q", e)
	}
}

// framesCLI answers Add providers' first reads with more presets than the
// screen has rows, so the pick fills it.
func framesCLI() string {
	presets := make([]string, 40)
	for i := range presets {
		presets[i] = fmt.Sprintf(`{"name":"p%02d","protocol":"openai","base_url":"https://p%02d.example/v1"}`, i, i)
	}
	return "case \"$*\" in\n\"compose ps -q\") echo c0ffee ;;\n" +
		"\"compose exec -T stargaite /stargaite providers presets --json\") echo '{\"presets\":[" + strings.Join(presets, ",") + "]}' ;;\n" +
		"\"compose exec -T stargaite /stargaite providers list --json\") echo '{\"providers\":[]}' ;;\n*) exit 9 ;;\nesac\n"
}

// muxSession runs script with sh as the one window of a new tmux session,
// or else GNU screen's, at 80x24, with no user configuration read, ended
// with the test. It returns how to type text and press Esc in it, and how
// to read its whole history and screen; it skips the test when neither is
// on PATH.
func muxSession(t *testing.T, script string) (typed func(string), esc func(), dump func() string) {
	t.Helper()
	name := fmt.Sprintf("stargaite-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	run := func(args ...string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("%q: %v\n%s", args, err, out)
		}
		return string(out)
	}
	if _, err := exec.LookPath("tmux"); err == nil {
		tmux := func(args ...string) string {
			return run(append([]string{"tmux", "-L", name, "-f", "/dev/null"}, args...)...)
		}
		tmux("new-session", "-d", "-x", "80", "-y", "24", "sh", "-c", script)
		t.Cleanup(func() { _ = exec.Command("tmux", "-L", name, "kill-server").Run() })
		return func(s string) { tmux("send-keys", "-l", s) }, func() { tmux("send-keys", "Escape") },
			func() string { return tmux("capture-pane", "-p", "-J", "-S", "-") }
	}
	if _, err := exec.LookPath("screen"); err == nil {
		file := filepath.Join(t.TempDir(), "hardcopy")
		run("screen", "-U", "-dmS", name, "-h", "2000", "sh", "-c", "stty rows 24 cols 80; "+script)
		t.Cleanup(func() { _ = exec.Command("screen", "-S", name, "-X", "quit").Run() })
		stuff := func(s string) { run("screen", "-S", name, "-X", "stuff", s) }
		return stuff, func() { stuff("\x1b") }, func() string {
			_ = os.Remove(file)
			run("screen", "-S", name, "-X", "hardcopy", "-h", file)
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
				if b, err := os.ReadFile(file); err == nil && len(b) > 0 {
					return string(b)
				}
			}
			t.Fatal("screen wrote no hardcopy within 10 s")
			return ""
		}
	}
	t.Skip("neither tmux nor screen is on PATH: the frames left in a multiplexer's history are not checked")
	return nil, nil, nil
}

// waitDump waits until dump holds s, polling for 10 s at most, and returns it.
func waitDump(t *testing.T, dump func() string, s string) string {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		out := dump()
		if strings.Contains(out, s) {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("%q never shown:\n%s", s, out)
		}
	}
}

// No frame of the guided front is left in a terminal multiplexer's history,
// which tmux and screen fill with the whole screen when a frame on its
// first row is erased (research-console section 3): a pick that fills the
// screen, filtered, so its height changes where it stands, then left with
// Esc.
func TestFramesNotInHistory(t *testing.T) {
	env, _ := fake(t, framesCLI())
	bin, _, _ := ownClone(t, nil)
	typed, esc, dump := muxSession(t, strings.Join(env, " ")+" "+bin+" provider; echo EXIT=$?; /bin/sleep 60")
	waitDump(t, dump, "Which providers do you want to add?")
	typed("p1")
	waitDump(t, dump, "filter: p1")
	esc()
	out := waitDump(t, dump, "EXIT=")
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimRight(l, " ")
		// The root's frame with no piece; screen's hardcopy keeps each cell's
		// low byte, so its "…" reads "&" there.
		working := l == "working…" || l == "working&"
		if strings.HasSuffix(l, "ctrl+c quit") || strings.HasPrefix(l, "filter: ") || working {
			t.Fatalf("a frame's line is left in the history, %q:\n%s", l, out)
		}
	}
}

// logsFake answers See logs: the container runs, its last lines, and a
// follow that prints a line, then waits, logging the SIGINT that ends it.
const logsFake = `case "$*" in
"compose ps -q"|"compose ps -a -q") echo c0ffee ;;
"compose logs --no-log-prefix --tail 100 stargaite") echo 'a last line' ;;
"compose logs --no-log-prefix --follow --tail 0 stargaite") trap 'echo INT >> "$FAKE_LOG"; exit 130' INT; echo 'a followed line'; /bin/sleep 60 >/dev/null 2>&1 & wait ;;
*) exit 9 ;;
esac
`

// logs at a terminal follows until Esc, which stops the follow and exits 0,
// or Ctrl-C, which exits 130; either way the follow gets SIGINT.
func TestLogsEscAtTerminal(t *testing.T) {
	for _, c := range []struct {
		name, key, said string
		want            int
	}{{"Esc", "\x1b", "Stopped following the logs.", 0}, {"Ctrl-C", "\x03", "a followed line", 130}} {
		t.Run(c.name, func(t *testing.T) {
			env, log := fake(t, logsFake)
			tm := onTerminal(t, env, "logs")
			tm.waitFor("a followed line", 1)
			tm.write(c.key)
			code, after := tm.exit()
			if out := tm.output(); code != c.want || !strings.Contains(out, "a last line") || !strings.Contains(out, c.said) {
				t.Fatalf("exit %d, want %d:\n%q", code, c.want, out)
			}
			tm.restored(after)
			if l := readLog(t, log); !strings.Contains(l, "INT\n") {
				t.Fatalf("the follow got no SIGINT: %q", l)
			}
		})
	}
}

// uninstallFake answers Uninstall over a stopped container (the home then
// reads nothing from the proxy).
const uninstallFake = `case "$*" in
"compose ps -q") ;;
"compose ps -a -q") echo c0ffee ;;
"compose config --format json") echo '{"name":"proj","services":{"stargaite":{"image":"stargaite:local"}},"volumes":{"stargaite-data":{"name":"proj_stargaite-data"}}}' ;;
"compose down") ;;
"image inspect --format {{.Id}} stargaite:local"|"image inspect --format {{.Id}} stargaite:local-previous") echo sha256:feed ;;
"rmi stargaite:local"|"rmi stargaite:local-previous") echo "Untagged: $2" ;;
*) exit 9 ;;
esac
`

// uninstall at a terminal, as a subcommand and from the home, each on a
// clone of its own (it removes the console it runs): confirmed, it removes
// the console and exits 0, the session ended, never back at the home.
func TestUninstallAtTerminal(t *testing.T) {
	if os.Getenv("STARGAITE_E2E_TREE") != "" {
		t.Skip("STARGAITE_E2E_TREE is set: uninstall would remove the tree's own console")
	}
	for _, args := range [][]string{{"uninstall"}, nil} {
		env, log := fake(t, uninstallFake)
		bin, _, _ := ownClone(t, nil)
		tm := onTerminalAt(t, bin, env, args...)
		if args == nil {
			tm.waitFor(home, 1)
			tm.write("Uninstall\r")
		}
		tm.waitFor("continue?", 1)
		tm.write("\x1b[A") // the cursor goes from No to Yes
		time.Sleep(200 * time.Millisecond)
		tm.write("\r")
		code, after := tm.exit()
		out := tm.output()
		if code != 0 || !strings.Contains(out, "Done: uninstalled; ./setup.sh installs again on the kept volume.") ||
			strings.Contains(out, "\nstargaite-console: ") || strings.Count(out, home) > 1 {
			t.Fatalf("%q: exit %d:\n%q", args, code, out)
		}
		tm.restored(after)
		if _, err := os.Stat(bin); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%q: the clone's console is still there: %v", args, err)
		}
		if _, err := os.Stat(console); err != nil {
			t.Errorf("%q: the shared console is gone: %v", args, err)
		}
		if l := readLog(t, log); !strings.Contains(l, "compose down\n") || strings.Contains(l, "compose exec") {
			t.Errorf("%q: docker log %q", args, l)
		}
	}
}

// mockCall is a call the e2e mock provider lists.
type mockCall struct {
	Path      string
	Body      []byte
	Ended     time.Time
	Cancelled bool
}

// slowCalls is the e2e mock's chat calls toward model slow (its OTLP
// traces, from telemetry set up earlier, name the model too).
func slowCalls(t *testing.T, mock string) []mockCall {
	t.Helper()
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(mock + "/mock/calls")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var all, slow []mockCall
	if err := json.NewDecoder(resp.Body).Decode(&all); err != nil {
		t.Fatal(err)
	}
	for _, c := range all {
		if c.Path == "/chat/completions" && bytes.Contains(c.Body, []byte(`"model":"slow"`)) {
			slow = append(slow, c)
		}
	}
	return slow
}

// realCLI runs the proxy's CLI in the e2e container, as e2e.sh does since
// remote set up TLS: through docker compose exec, with the admin variables.
func realCLI(tree string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", append([]string{"compose", "exec", "-T", "-e", "STARGAITE_ADMIN_URL=https://localhost:4001",
		"-e", "SSL_CERT_FILE=/tls/cert.pem", "stargaite", "/stargaite"}, args...)...)
	cmd.Dir = tree
	return cmd.Output()
}

// Esc during a hanging check against the real container (design section 10,
// layer 4), run by tests/setup/e2e.sh: the server's call to the mock ends
// with the CLI's, the stated count is never passed, and nothing is made.
// The mock only: no paid call.
func TestRealEscDuringCheck(t *testing.T) {
	tree, mock := os.Getenv("STARGAITE_E2E_TREE"), os.Getenv("STARGAITE_E2E_MOCK")
	if tree == "" || mock == "" {
		t.Skip("STARGAITE_E2E_TREE and STARGAITE_E2E_MOCK are unset: tests/setup/e2e.sh runs this against a real container")
	}
	// The provider sends its 200 status line, then holds the body: a call cut
	// after a status line leaves its target unverified too.
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Post(mock+"/mock/script", "application/json", strings.NewReader(`[{"Hang": true}]`))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("the mock was not scripted: %v %v", resp, err)
	}
	_ = resp.Body.Close()
	tm := onTerminalAt(t, console, os.Environ(), "alias", "--alias", "e2e-esc", "--provider", "mock", "--model", "slow", "--fallback", "mock2/slow")
	tm.waitFor("a default reasoning effort", 1)
	tm.write("\r")
	tm.waitFor("2 paid call(s) in all", 1)
	time.Sleep(450 * time.Millisecond)
	tm.write("\r")
	tm.waitFor("checking mock/slow, mock2/slow", 1)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if c := slowCalls(t, mock); len(c) == 1 && c[0].Ended.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no call toward slow in flight at the mock: %+v", slowCalls(t, mock))
		}
	}
	sent := time.Now()
	tm.write("\x1b")
	code, after := tm.exit()
	out := tm.output()
	if took := time.Since(sent); code != 1 || took > 12*time.Second {
		t.Fatalf("exit %d after %s:\n%q", code, took, out)
	}
	tm.restored(after)
	if !strings.Contains(out, "the check of mock/slow was stopped: the call in flight may still be billed") || !strings.Contains(out, "Left Make an alias.") {
		t.Errorf("output:\n%q", out)
	}
	for deadline := time.Now().Add(10 * time.Second); slowCalls(t, mock)[0].Ended.IsZero(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the call in flight never ended at the mock")
		}
	}
	if c := slowCalls(t, mock)[0]; !c.Cancelled || c.Ended.Sub(sent) > 5*time.Second {
		t.Errorf("the call in flight: cancelled %v, ended %s after the Esc", c.Cancelled, c.Ended.Sub(sent))
	}
	time.Sleep(30 * time.Second) // past any retry or second target the server could start
	if c := slowCalls(t, mock); len(c) != 1 {
		t.Errorf("%d calls toward slow; want 1", len(c))
	}
	if b, err := realCLI(tree, "models", "show", "e2e-esc", "--json"); err == nil {
		t.Errorf("alias e2e-esc exists: %s", b)
	}
	for _, id := range []string{"mock/slow", "mock2/slow"} {
		var target struct {
			VerifiedAt int64 `json:"verified_at"`
		}
		b, err := realCLI(tree, "targets", "show", id, "--json")
		if err != nil || json.Unmarshal(b, &target) != nil || target.VerifiedAt != 0 {
			t.Errorf("target %s: %s, %v", id, b, err)
		}
	}
}

// putBackFake answers a restore whose put-back takes 2 s.
const putBackFake = `case "$*" in
"compose ps -q"|"compose ps -a -q") echo c0ffee ;;
"compose stop") echo stopping ;;
"compose run --rm --no-deps -T stargaite restore --yes --json -") /bin/cat >/dev/null; /bin/sleep 2; echo '{"path":"-","bytes":1,"data_dir":"/data","replaced":[]}' ;;
"compose up -d --no-build") echo started ;;
*) exit 9 ;;
esac
`

// Ctrl-C or SIGTERM while the copy is put back, as a subcommand and from
// the home: the put-back and the start run to their end, and how they ended
// is printed on standard error once the terminal is restored.
func TestRestoreOutcomeAfterQuit(t *testing.T) {
	ctrlC := func(tm *terminal) { tm.write("\x03") }
	sigterm := func(tm *terminal) { _ = tm.cmd.Process.Signal(syscall.SIGTERM) }
	for _, c := range []struct {
		name     string
		viaHome  bool
		end      func(*terminal)
		wantCode int
	}{{"Ctrl-C", false, ctrlC, 130}, {"Ctrl-C from the home", true, ctrlC, 130}, {"SIGTERM", false, sigterm, 143}} {
		t.Run(c.name, func(t *testing.T) {
			env, log := fake(t, putBackFake)
			bin, dir, _ := ownClone(t, nil)
			if err := os.MkdirAll(filepath.Join(dir, "backups"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "backups", "a.db"), []byte("SQLite format 3\x00 x"), 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{"restore", "--from", "backups/a.db"}
			if c.viaHome {
				args = nil
			}
			tm := onTerminalAt(t, bin, env, args...)
			if c.viaHome {
				tm.waitFor(home, 1)
				tm.write("Back up or restore\r")
				tm.waitFor("Back up or restore?", 1)
				time.Sleep(300 * time.Millisecond)
				tm.write("\x1b[B") // from back up to restore
				time.Sleep(200 * time.Millisecond)
				tm.write("\r")
				tm.waitFor("Which copy?", 1)
				time.Sleep(300 * time.Millisecond)
				tm.write("\r")
			}
			tm.waitFor("continue?", 1)
			tm.write("\x1b[A") // the cursor goes from No to Yes
			time.Sleep(200 * time.Millisecond)
			tm.write("\r")
			waitLog(t, log, "compose run")
			c.end(tm)
			code, after := tm.exit()
			tm.restored(after)
			out := tm.output()
			tail := out[strings.LastIndex(out, "\x1b[?2004l"):]
			if code != c.wantCode || !strings.Contains(tail, "stargaite-console: quit; the restore ran to its end: Done: the store is the copy backups/a.db") {
				t.Fatalf("exit %d, want %d; after the terminal was restored:\n%q", code, c.wantCode, tail)
			}
			if l := readLog(t, log); !strings.HasSuffix(strings.TrimSpace(l), "compose up -d --no-build") {
				t.Errorf("docker log %q", l)
			}
		})
	}
}

// rebuildClone is a clone of its own made a git repository at a commit
// tagged v-new, and a fake docker that keeps the running version (v-old
// until compose build stores v-new and compose up starts it) and answers
// the home's reads and a Rebuild, whose export writes bin/stargaite-console
// as a shell script of the given mode that prints "REEXEC <pid> <argc>
// <marker> <stty -g>" to the terminal and to <state>/reexec, then exits 7.
// It returns the console, the environment (real git after the fake on
// PATH), the fake's log and the state directory.
func rebuildClone(t *testing.T, mode string) (bin string, env []string, log, state string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git here")
	}
	bin, dir, _ := ownClone(t, nil)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "compose.yaml"}, {"commit", "-q", "-m", "c"}, {"tag", "v-new"}} {
		cmd := exec.Command(real, append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	state = t.TempDir()
	newConsole := "#!/bin/sh\nline=\"REEXEC $$ $# $" + reexecMarker + " $(stty -g)\"\necho \"$line\"\necho \"$line\" >'" + state + "/reexec'\nexit 7\n"
	if err := errors.Join(os.WriteFile(filepath.Join(state, "running"), []byte("v-old\n"), 0o644),
		os.WriteFile(filepath.Join(state, "new"), []byte(newConsole), 0o644)); err != nil {
		t.Fatal(err)
	}
	cli := `"compose exec -T stargaite /stargaite `
	env, log = fake(t, "S='"+state+"'\ncase \"$*\" in\n"+
		`"compose ps -q"|"compose ps -a -q"|"compose ps -q stargaite") echo c0ffee ;;`+"\n"+
		cli+`version --json") echo "{\"version\":\"$(/bin/cat "$S/running")\"}" ;;`+"\n"+
		cli+`export --json") echo '{"version":1,"providers":{"p":{}},"models":{"main":{"targets":["p/m"]}},"keys":{"k":{}}}' ;;`+"\n"+
		cli+`validate --json") echo '{"findings":[]}' ;;`+"\n"+
		cli+`--help") printf 'usage: stargaite <command>\n  restore file\n' ;;`+"\n"+
		cli+`backup - --json") printf 'SQLite format 3\000 the store'; echo '{"bytes":26,"path":"-"}' >&2 ;;`+"\n"+
		`"inspect --format {{.Image}} {{.Config.Image}} c0ffee") echo 'sha256:0123abcd stargaite:local' ;;`+"\n"+
		`"tag sha256:0123abcd stargaite:local-previous") ;;`+"\n"+
		`"compose build") echo "$STARGAITE_VERSION" >"$S/built" ;;`+"\n"+
		`"compose up -d --no-build") /bin/cp "$S/built" "$S/running" ;;`+"\n"+
		`"compose logs --no-log-prefix --tail 100 stargaite") echo "{\"msg\":\"ready\",\"version\":\"$(/bin/cat "$S/running")\"}" ;;`+"\n"+
		`"compose logs --tail 30 stargaite") ;;`+"\n"+
		`"build --quiet --build-arg VERSION --target console --output type=local,dest=bin .") `+
		`/bin/cp "$S/new" bin/.new && /bin/chmod `+mode+` bin/.new && /bin/mv -f bin/.new bin/stargaite-console ;;`+"\n"+
		"*) exit 9 ;;\nesac\n")
	for i, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			env[i] = e + ":" + filepath.Dir(real)
		}
	}
	return bin, env, log, state
}

// rebuildAtHome picks the home's Rebuild and confirms it.
func rebuildAtHome(tm *terminal) {
	tm.waitFor("Rebuild to this clone's version", 1)
	time.Sleep(300 * time.Millisecond)
	tm.write("Rebuild\r")
	tm.waitFor("continue?", 1)
	tm.write("\x1b[A") // the cursor goes from No to Yes
	time.Sleep(200 * time.Millisecond)
	tm.write("\r")
}

// After a Rebuild from the home, the console exports the new console and
// execs it once, in the same process, with no arguments and the marker,
// the terminal restored before the new console's first byte.
func TestReexecAfterRebuild(t *testing.T) {
	bin, env, _, state := rebuildClone(t, "0755")
	tm := onTerminalAt(t, bin, env)
	pid := tm.cmd.Process.Pid
	rebuildAtHome(tm)
	code, after := tm.exit()
	tm.restored(after)
	out := tm.output()
	got, err := os.ReadFile(filepath.Join(state, "reexec"))
	f := strings.Fields(string(got))
	if code != 7 || err != nil || len(f) < 5 || f[0] != "REEXEC" || f[1] != strconv.Itoa(pid) || f[2] != "0" || f[3] != "1" {
		t.Fatalf("exit %d, the new console wrote %q (%v); want REEXEC %d 0 1\n%q", code, got, err, pid, out)
	}
	at := strings.Index(out, "REEXEC ")
	for _, seq := range []string{"\x1b[?2004l", "\x1b[?25h", "\x1b[<1u", "rebuilt: "} {
		if i := strings.LastIndex(out[:max(at, 0)], seq); i < 0 || strings.Contains(out[at:], seq) {
			t.Errorf("%q is not written before the new console's first byte (%d, at %d):\n%q", seq, i, at, out)
		}
	}
}

// A new console that cannot be executed: E2, exit 1, the terminal restored.
func TestReexecExecFails(t *testing.T) {
	bin, env, _, state := rebuildClone(t, "0644")
	tm := onTerminalAt(t, bin, env)
	rebuildAtHome(tm)
	code, after := tm.exit()
	tm.restored(after)
	out := tm.output()
	tail := out[strings.LastIndex(out, "\x1b[?2004l"):]
	if code != 1 || !strings.Contains(tail, "stargaite-console: the new console did not start: ") || !strings.Contains(tail, "; ./setup.sh starts it") {
		t.Fatalf("exit %d; after the terminal was restored:\n%q", code, tail)
	}
	if _, err := os.Stat(filepath.Join(state, "reexec")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the new console ran: %v", err)
	}
}

// A Ctrl-C typed before the console starts, on a terminal already raw so
// that it waits as a byte, is flushed: the home opens and the console runs
// on (TestFlushInput proves the ioctl).
func TestFlushAtStart(t *testing.T) {
	bin, env, _, _ := rebuildClone(t, "0755")
	m, s := openPTY(t)
	tio, err := unix.IoctlGetTermios(int(s.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	tio.Lflag &^= unix.ICANON | unix.ECHO | unix.ISIG
	if err := unix.IoctlSetTermios(int(s.Fd()), unix.TCSETS, tio); err != nil {
		t.Fatal(err)
	}
	if _, err := m.WriteString("\x03"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	tm := startOn(t, m, s, bin, env)
	tm.waitFor(home, 1)
	time.Sleep(500 * time.Millisecond)
	select {
	case <-tm.waited:
		t.Fatalf("the console read the Ctrl-C typed before it started:\n%q", tm.output())
	default:
	}
	tm.write("\x03")
	if code, after := tm.exit(); code != 130 {
		t.Errorf("exit %d; want 130", code)
	} else {
		tm.restored(after)
	}
}

// editFake replaces old with new in the fake docker that log belongs to.
func editFake(t *testing.T, log, old, new string) {
	t.Helper()
	p := filepath.Join(filepath.Dir(log), "docker")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), old) {
		t.Fatalf("the fake holds no %q", old)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(b), old, new, 1)), 0o755); err != nil {
		t.Fatal(err)
	}
}

// tailAfterRestore is what the console wrote once it restored the terminal.
func tailAfterRestore(out string) string { return out[max(strings.LastIndex(out, "\x1b[?2004l"), 0):] }

// Ctrl-C during Rebuild's backup, the image's name already the new build's,
// from the home and from the subcommand: how to return is printed once the
// terminal is restored, and nothing is started.
func TestRebuildCtrlCDuringBackup(t *testing.T) {
	for name, args := range map[string][]string{"home": nil, "subcommand": {"upgrade", "--rebuild", "--yes"}} {
		t.Run(name, func(t *testing.T) {
			bin, env, log, _ := rebuildClone(t, "0755")
			editFake(t, log, `backup - --json") printf`, `backup - --json") /bin/sleep 5; printf`)
			tm := onTerminalAt(t, bin, env, args...)
			if args == nil {
				rebuildAtHome(tm)
			}
			tm.waitFor("backing up the store to", 1)
			time.Sleep(500 * time.Millisecond)
			tm.write("\x03")
			code, after := tm.exit()
			tm.restored(after)
			tail := tailAfterRestore(tm.output())
			if code != 130 || !strings.Contains(tail, "stargaite-console: quit; the upgrade did not finish: the backup failed (stopped), so the new version "+
				"was not started: v-old still runs, the store unchanged; docker tag stargaite:local-previous stargaite:local returns to it") {
				t.Fatalf("exit %d; after the terminal was restored:\n%q", code, tail)
			}
			if strings.Contains(readLog(t, log), "compose up") {
				t.Errorf("started: %q", readLog(t, log))
			}
		})
	}
}

// E1 through main: a session restarted once already says so and exits 0,
// starting nothing.
func TestReexecMarkerSet(t *testing.T) {
	bin, env, _, state := rebuildClone(t, "0755")
	tm := onTerminalAt(t, bin, append(env, reexecMarker+"=1"))
	rebuildAtHome(tm)
	code, after := tm.exit()
	tm.restored(after)
	tail := tailAfterRestore(tm.output())
	if code != 0 || !strings.Contains(tail, "stargaite-console: the console was restarted once in this session; ./setup.sh starts the new one") {
		t.Fatalf("exit %d; after the terminal was restored:\n%q", code, tail)
	}
	if _, err := os.Stat(filepath.Join(state, "reexec")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the new console ran: %v", err)
	}
}

// V14 through main: an export that fails after the rebuild says the result,
// then exits 1 with why, starting nothing.
func TestRebuildExportFails(t *testing.T) {
	bin, env, log, state := rebuildClone(t, "0755")
	editFake(t, log, `/bin/cp "$S/new" bin/.new &&`, `exit 1; /bin/cp "$S/new" bin/.new &&`)
	tm := onTerminalAt(t, bin, env)
	rebuildAtHome(tm)
	code, after := tm.exit()
	tm.restored(after)
	out := tm.output()
	tail := tailAfterRestore(out)
	if code != 1 || !strings.Contains(tail, "stargaite-console: the new version runs, but the new console was not exported (") ||
		!strings.Contains(out[:len(out)-len(tail)], "rebuilt: ") {
		t.Fatalf("exit %d:\n%q", code, out)
	}
	if _, err := os.Stat(filepath.Join(state, "reexec")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the new console ran: %v", err)
	}
}
