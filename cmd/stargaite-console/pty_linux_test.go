//go:build linux

package main

import (
	"bytes"
	"errors"
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
	bin, env, _, _ := keyClone(t, http.StatusOK, chatBody)
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
