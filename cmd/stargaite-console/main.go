// Command stargaite-console is the guided console of a StargAIte clone
// (docs/design/phase-7.md): at a terminal it asks what to do and walks
// through it; without one, a subcommand does the same work from flags. It
// runs on the host, beside compose.yaml, and reaches the proxy only through
// its CLI in the container.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/term"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/task"
	"github.com/jorlaru/stargaite/internal/console/ui"
	"github.com/jorlaru/stargaite/internal/console/ui/guided"
	"github.com/jorlaru/stargaite/internal/console/ui/plain"
)

// version is set at link time: -X main.version=….
var version = "dev"

// Exit codes. 129, 130 and 143 are what a shell reports for SIGHUP, SIGINT
// and SIGTERM, as phase 6's setup.sh gave them.
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
	exitHUP   = 129
	exitINT   = 130
	exitTERM  = 143
)

// sessionBound is how long main waits for the session once the program has
// ended or a signal came: a command's process group gets 10 s to stop.
const sessionBound = 12 * time.Second

const usage = `usage: stargaite-console [status | help] [--version]
At a terminal with no subcommand: the guided console.
  status   what exists: providers, aliases, targets without an alias, keys, budgets, telemetry and both versions
`

func main() {
	os.Exit(run(os.Args[1:], os.Environ(), os.Stdin, os.Stdout, os.Stderr))
}

// run is the console: the arguments, the clone, the signals, then the
// session on its own goroutine, in plain mode or under the guided program.
func run(args, environ []string, stdin, stdout, stderr *os.File) int {
	if len(args) == 1 && args[0] == "--version" {
		_, _ = fmt.Fprintln(stdout, version)
		return exitOK
	}
	fail := func(code int, format string, a ...any) int {
		_, _ = fmt.Fprintf(stderr, "stargaite-console: "+format+"\n", a...)
		return code
	}
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	// A subcommand is one the menu declares; no task takes arguments yet.
	known := sub != "" && slices.ContainsFunc(task.Menu(), func(e task.Entry) bool { return e.Sub == sub })
	switch {
	case sub == "help" || sub == "-h" || sub == "--help":
		_, _ = io.WriteString(stdout, usage)
		return exitOK
	case known && len(args) > 1 && strings.HasPrefix(args[1], "--"):
		return fail(exitUsage, "unknown flag %s", ui.OneLine(args[1]))
	case known && len(args) > 1:
		return fail(exitUsage, "unexpected argument: %s", ui.OneLine(args[1]))
	case sub == "" && (!term.IsTerminal(stdin.Fd()) || !term.IsTerminal(stdout.Fd())):
		return fail(exitUsage, "no subcommand; stargaite-console help lists them")
	case sub != "" && !known:
		return fail(exitUsage, "unknown subcommand %q; stargaite-console help lists them", ui.OneLine(sub))
	}
	exe, err := os.Executable()
	if err != nil {
		return fail(exitFail, "the console's own path: %s", ui.OneLine(err.Error()))
	}
	h, err := host.Find(exe)
	if err != nil {
		return fail(exitFail, "%s", ui.OneLine(err.Error()))
	}
	if sub == "" {
		for _, v := range []string{"TEA_TRACE", "TEA_DEBUG"} {
			if set(environ, v) {
				return fail(exitFail, "%s is set: the console would write every byte it draws to a file, a client key included; unset it", v)
			}
		}
	}

	// One owner of the signals: it records the first and cancels the root.
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	var got atomic.Value
	ui.Go(func() {
		select {
		case s := <-sigs:
			got.Store(s)
			cancel()
		case <-root.Done():
		}
	}, func(any) { cancel() })

	var e ending
	px := proxy.New(h.CLI)
	if sub != "" {
		e = plainSession(root, sub, px, h, stdout, stderr)
	} else {
		e = guidedSession(root, cancel, px, h, environ, stdin, stdout, stderr)
	}
	if s, ok := got.Load().(os.Signal); ok {
		e.signal = s
	}
	return e.code()
}

// ending is what decides the exit code.
type ending struct {
	signal      os.Signal
	panicked    bool // the session or the model panicked
	interrupted bool // Ctrl-C in guided mode
	ended       bool // the session returned within sessionBound
	session     int  // its code
}

// code is the first that applies: a signal's, a panic's, Ctrl-C's, the
// bound's, then the session's own.
func (e ending) code() int {
	switch {
	case e.signal == syscall.SIGHUP:
		return exitHUP
	case e.signal == syscall.SIGINT:
		return exitINT
	case e.signal == syscall.SIGTERM:
		return exitTERM
	case e.panicked:
		return exitFail
	case e.interrupted:
		return exitINT
	case !e.ended:
		return exitFail
	}
	return e.session
}

// plainSession runs the menu entry whose subcommand is sub, with answers
// from flags or defaults, and waits for it, bounded once a signal came.
func plainSession(root context.Context, sub string, px *proxy.Proxy, h *host.Host, stdout, stderr io.Writer) ending {
	var (
		e        ending
		panicked atomic.Bool
	)
	u := ui.New(plain.New(stdout), nil)
	session := func() int {
		for _, entry := range task.Menu() {
			if entry.Sub != sub {
				continue
			}
			res, err := u.Run(root, entry.Steps, func(ctx context.Context) (string, error) { return entry.Run(ctx, u, px, h) })
			if err == nil && res != "" {
				err = u.Say(root, res)
			}
			var usageErr *ui.UsageError
			switch {
			case errors.As(err, &usageErr):
				_, _ = fmt.Fprintf(stderr, "stargaite-console: %s\n", ui.OneLine(err.Error()))
				return exitUsage
			case err != nil:
				_, _ = fmt.Fprintf(stderr, "stargaite-console: %s\n", ui.OneLine(err.Error()))
				return exitFail
			}
			return exitOK
		}
		return exitUsage // run checked sub against the menu's subcommands
	}
	done := supervise(session, func() { panicked.Store(true) }, stderr)
	select {
	case e.session = <-done:
		e.ended = true
	case <-root.Done():
		e.session, e.ended = await(done, sessionBound, stderr)
	}
	e.panicked = panicked.Load()
	return e
}

// guidedSession runs the program and, on the session goroutine, the home
// menu: pick a task, run it, say how it ended, pick again, until Ctrl-C.
func guidedSession(root context.Context, cancel context.CancelFunc, px *proxy.Proxy, h *host.Host,
	environ []string, stdin, stdout, stderr *os.File) ending {
	var (
		e        ending
		panicked atomic.Bool
		failure  error // an error that ended the session, printed after the program
	)
	opts := append(guided.Options(root, environ), tea.WithInput(stdin), tea.WithOutput(stdout))
	p := tea.NewProgram(guided.NewModel(version), opts...)
	front := guided.NewFront(p)
	u := ui.New(front, nil)
	session := func() int {
		defer front.Done()
		menu := task.Menu()
		home := ui.Question{ID: "home", Prompt: "What do you want to do?"}
		for _, entry := range menu {
			home.Options = append(home.Options, ui.Option{Value: entry.Label, Label: entry.Label})
		}
		for {
			label, err := u.Pick(root, home)
			switch {
			case errors.Is(err, ui.ErrQuit):
				return exitINT
			case err != nil:
				if root.Err() == nil {
					failure = err
				}
				return exitFail
			}
			for _, entry := range menu {
				if entry.Label != label {
					continue
				}
				res, err := u.Run(root, entry.Steps, func(ctx context.Context) (string, error) { return entry.Run(ctx, u, px, h) })
				switch {
				case errors.Is(err, ui.ErrQuit):
					return exitINT
				case root.Err() != nil:
					return exitFail
				case errors.Is(err, ui.ErrLeave):
					err = u.Say(root, "Left "+label+".")
				case err != nil:
					err = u.Say(root, label+": "+err.Error())
				case res != "":
					err = u.Say(root, res)
				}
				if err != nil {
					return exitFail
				}
			}
		}
	}
	done := supervise(session, func() { panicked.Store(true); p.Kill() }, stderr)
	final, err := p.Run()
	stopped := root.Err() != nil // by a signal: Run's error then says only that
	cancel()
	e.session, e.ended = await(done, sessionBound, stderr)
	if m, ok := final.(guided.Model); ok {
		e.interrupted = m.Interrupted()
	}
	e.panicked = panicked.Load() || errors.Is(err, tea.ErrProgramPanic)
	switch {
	case err != nil && !stopped && !e.panicked: // the terminal could not be set up or read
		_, _ = fmt.Fprintf(stderr, "stargaite-console: %s\n", ui.OneLine(err.Error()))
	case e.ended && failure != nil:
		_, _ = fmt.Fprintf(stderr, "stargaite-console: %s\n", ui.OneLine(failure.Error()))
	}
	return e
}

// supervise runs session on its own goroutine and sends its code. A panic
// is kill, then one line on stderr, then exit 1: kill returns once the
// terminal is restored, so the line is not drawn into the frame.
func supervise(session func() int, kill func(), stderr io.Writer) <-chan int {
	done := make(chan int, 1)
	ui.Go(func() { done <- session() }, func(v any) {
		kill()
		_, _ = fmt.Fprintf(stderr, "stargaite-console: internal error: %s\n", ui.OneLine(fmt.Sprint(v)))
		done <- exitFail
	})
	return done
}

// await waits for the session's code for at most bound.
func await(done <-chan int, bound time.Duration, stderr io.Writer) (code int, ok bool) {
	t := time.NewTimer(bound)
	defer t.Stop()
	select {
	case code = <-done:
		return code, true
	case <-t.C:
		_, _ = fmt.Fprintf(stderr, "stargaite-console: a step did not stop within %s\n", bound)
		return exitFail, false
	}
}

// set reports whether name is in environ, even empty.
func set(environ []string, name string) bool {
	for _, kv := range environ {
		if k, _, _ := strings.Cut(kv, "="); k == name {
			return true
		}
	}
	return false
}
