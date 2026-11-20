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
	"maps"
	"os"
	"os/signal"
	"path/filepath"
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

// homeBound is how long the home reads what exists before each pick.
const homeBound = 15 * time.Second

const usage = `usage: stargaite-console [install | provider | alias | key | budget | telemetry | remote | status | logs | stop | start | restart | backup | restore | uninstall | upgrade | help] [flags] [--version]
At a terminal with no subcommand: the guided console.
  install   the first run: this machine, then a provider, an alias with its fallbacks and a key, each kept when it
            exists, ending with one test call; at a terminal it starts at the first missing step
            --port <n> (4000), and the flags of provider, alias and key
            its check and its test call are each announced first, capped at 256 output tokens, never retried
  provider  add providers and their keys; each key is checked for free before it is written
            --provider <a,b>   presets, a provider already added, or one new name
            --base-url <url>   --protocol <openai|anthropic>   --api-key-env <VAR|none>
            --key-env <VAR> or <name=VAR,…>   names the variable that holds a key
            --replace   --allow-private-network   --yes
  alias     an alias toward a provider's model, kept when it exists; with --fallback it gets fallbacks
            --alias <name> (main)   --provider <name>   --model <id>   --fallback <provider/model,…>
            --effort <level>   --max-output-tokens <n>   --input-price <usd>   --output-price <usd>   --reverify
            and, when --provider is not added yet, provider's flags
            its new targets are checked first: each paid call is announced, capped at 256 output tokens, never retried
  key       a client key for one or more aliases, shown once, then one test call through the first
            --alias <a,b> (main)   --key-name <name> (my-app)   --rpm-limit <n> (60)
            the test call is announced first: up to one paid call per target of that alias, capped at 256 output tokens, never retried
  budget    a spending cap on the instance, an alias or a key; the prices the proxy says it needs are asked first
            --on <instance|alias:<name>|key:<name>>   --limit-usd <usd>   --reset <daily|monthly|never> (monthly)
            --input-price <usd>   --output-price <usd>   --yes replaces a cap   --reverify checks again what a price cleared
  telemetry traces to Langfuse or any OTLP endpoint, replacing what was set whole, then one test span (free)
            --backend <langfuse|otlp|off>   --endpoint <url>   --langfuse-public-env <VAR>   --langfuse-secret-env <VAR>
            --header-name <name>   --key-env <VAR>   --replace   --allow-private-network   --yes
  remote    publish the data plane beyond loopback, always with TLS: your certificate or a self-signed one; run again, it renews it
            --address <a,b>   --tls <own|self-signed>   --tls-cert <file>   --tls-key <file>   --tls-name <name>   --tls-ca <file>   --yes
  logs      the container's last 100 log lines; at a terminal it then follows them until Esc, an hour at most
            --follow is refused without a terminal: the console follows logs only while someone watches
  stop, start, restart   the container; stop and restart interrupt traffic   --yes
  backup    a copy of the store into backups/, mode 0600
  restore   a copy put back: keys revoked since come back, spend since is forgotten; the container is stopped
            and started again   --from <file>   --yes
  uninstall the container, its two images and bin/stargaite-console; the volume, .env, the override, backups/
            and tls/ are kept, and the commands that remove them printed   --yes
  upgrade   fast-forward this clone to its upstream and rebuild, keeping the previous image and taking a backup just before the new version starts, then checking it; it prints the way back
            --rebuild rebuilds this clone's version, without fetching   --legacy-backup <file>   --yes
  status    what exists: providers, aliases, targets without an alias, keys, budgets, telemetry and both versions; --check first says whether the clone's upstream has an upgrade (one git fetch)
For what has no flag here (reorder an alias, see what it sends, change a provider or a key, see usage and cost, check everything):
the guided console, or the stargaite command: docker compose exec -T stargaite /stargaite help
`

func main() {
	os.Exit(run(os.Args[1:], os.Environ(), os.Stdin, os.Stdout, os.Stderr))
}

// run is the console: the arguments, the clone, the signals, then the
// session on its own goroutine, under the guided program when standard input
// and output are a terminal, and in plain mode otherwise.
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
	// A subcommand is one the menu declares.
	menu := task.Menu()
	i := slices.IndexFunc(menu, func(e task.Entry) bool { return e.Sub == sub })
	known := sub != "" && i >= 0
	guided := term.IsTerminal(stdin.Fd()) && term.IsTerminal(stdout.Fd())
	switch {
	case sub == "help" || sub == "-h" || sub == "--help":
		_, _ = io.WriteString(stdout, usage)
		return exitOK
	case sub == "" && !guided:
		return fail(exitUsage, "no subcommand; stargaite-console help lists them")
	case sub != "" && !known:
		return fail(exitUsage, "unknown subcommand %q; stargaite-console help lists them", ui.OneLine(sub))
	}
	var only *task.Entry // the subcommand's entry; nil runs the home menu
	var answers map[string]string
	if known {
		only = &menu[i]
		var err error
		if answers, err = parseFlags(*only, args[1:]); err != nil {
			return fail(exitUsage, "%s", ui.OneLine(err.Error()))
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return fail(exitFail, "the console's own path: %s", ui.OneLine(err.Error()))
	}
	h, err := host.Find(exe, os.Getuid())
	if err != nil {
		return fail(exitFail, "%s", ui.OneLine(err.Error()))
	}
	if guided {
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
	if guided {
		e = guidedSession(root, cancel, px, h, environ, answers, only, stdin, stdout, stderr)
	} else { // without a terminal there is a subcommand: checked above
		e = plainSession(root, *only, ui.New(plain.New(stdout), answers).WithEnv(environ), px, h, stderr)
	}
	if s, ok := got.Load().(os.Signal); ok {
		e.signal = s
	}
	switch exec, line := restartDecision(e, environ); {
	case line != "":
		return fail(exitOK, "%s", line)
	case exec:
		// The program has quit and restored the terminal: the new console
		// takes this process, its pid and terminal, with the marker.
		signal.Stop(sigs)
		path := filepath.Join(h.Dir, "bin", "stargaite-console")
		err := syscall.Exec(path, []string{path}, append(slices.Clone(environ), reexecMarker+"=1"))
		return fail(exitFail, "the new console did not start: %s; ./setup.sh starts it", ui.OneLine(err.Error()))
	}
	return e.code()
}

// reexecMarker is set in the new console's environment: it restarts once.
const reexecMarker = "STARGAITE_CONSOLE_REEXEC"

// restartDecision is whether to start the new console: only when the
// session asked for it and ended cleanly, within its bound, with no signal,
// Ctrl-C or panic, and only once per session (the marker); with the marker,
// line says why it does not.
func restartDecision(e ending, environ []string) (exec bool, line string) {
	switch {
	case !e.restart || !e.ended || e.signal != nil || e.panicked || e.interrupted || e.session != exitOK:
		return false, ""
	case set(environ, reexecMarker):
		return false, "the console was restarted once in this session; ./setup.sh starts the new one"
	}
	return true, ""
}

// ending is what decides the exit code.
type ending struct {
	signal      os.Signal
	panicked    bool // the session or the model panicked
	interrupted bool // Ctrl-C in guided mode
	ended       bool // the session returned within sessionBound
	session     int  // its code
	restart     bool // the session asked to start the new console (task.ErrReexec)
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

// parseFlags reads a subcommand's flags into answers keyed by flag name:
// --name value or --name=value for one of e.Flags, --name for one of
// e.Bools, whose answer is "yes". The last one given wins. e.Preset's
// answers are added after them.
func parseFlags(e task.Entry, args []string) (map[string]string, error) {
	answers := map[string]string{}
	for i := 0; i < len(args); i++ {
		name, value, hasValue := strings.Cut(strings.TrimPrefix(args[i], "--"), "=")
		switch {
		case !strings.HasPrefix(args[i], "--"):
			return nil, fmt.Errorf("unexpected argument: %s", args[i])
		case slices.Contains(e.Bools, name) && hasValue:
			return nil, fmt.Errorf("--%s takes no value", name)
		case slices.Contains(e.Bools, name):
			answers[name] = "yes"
		case !slices.Contains(e.Flags, name):
			return nil, fmt.Errorf("unknown flag --%s", name)
		case hasValue:
			answers[name] = value
		case i+1 < len(args) && !strings.HasPrefix(args[i+1], "--"):
			i++
			answers[name] = args[i]
		default:
			return nil, fmt.Errorf("--%s needs a value", name)
		}
	}
	maps.Copy(answers, e.Preset)
	return answers, nil
}

// runOne runs entry as a subcommand and says its result. It returns the
// exit code and the error to print on standard error, if any. Leaving the
// task says so and is a failure; a task that ends the session succeeds.
func runOne(root context.Context, u *ui.UI, entry task.Entry, px *proxy.Proxy, h *host.Host) (int, error) {
	res, err := u.Run(root, entry.Steps, func(ctx context.Context) (string, error) { return entry.Run(ctx, u, px, h) })
	if err == task.ErrEnd || errors.Is(err, task.ErrReexec) { // a subcommand ends with its status: no restart
		err = nil
	}
	var usageErr *ui.UsageError
	switch {
	case err == nil && res == "":
		return exitOK, nil
	case err == nil:
		if err := u.Say(root, res); err != nil {
			return exitFail, err
		}
		return exitOK, nil
	case errors.As(err, &usageErr):
		return exitUsage, err
	case task.PrintAfter(err): // how a restore or an upgrade ended after Ctrl-C or a signal
		return exitINT, err
	case errors.Is(err, ui.ErrQuit):
		return exitINT, nil
	case errors.Is(err, ui.ErrLeave):
		return exitFail, u.Say(root, "Left "+entry.Label+".")
	}
	return exitFail, err
}

// plainSession runs entry with answers from flags or defaults, and waits for
// it, bounded once a signal came.
func plainSession(root context.Context, entry task.Entry, u *ui.UI, px *proxy.Proxy, h *host.Host, stderr io.Writer) ending {
	var (
		e        ending
		panicked atomic.Bool
	)
	session := func() int {
		code, err := runOne(root, u, entry, px, h)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "stargaite-console: %s\n", ui.OneLine(err.Error()))
		}
		return code
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

// guidedSession runs the program and, on the session goroutine, the entry
// only with the flags' answers, or with none the home (task.Home): read what
// exists, pick a task, run it, say how it ended, again, until Ctrl-C or a
// task that ends the session.
func guidedSession(root context.Context, cancel context.CancelFunc, px *proxy.Proxy, h *host.Host,
	environ []string, answers map[string]string, only *task.Entry, stdin, stdout, stderr *os.File) ending {
	var (
		e        ending
		panicked atomic.Bool
		restart  atomic.Bool
		failure  error // an error that ended the session, printed after the program
	)
	_ = flushInput(int(stdin.Fd())) // keys typed before the console starts open nothing (design section 3)
	opts := append(guided.Options(root, environ), tea.WithInput(stdin), tea.WithOutput(stdout))
	p := tea.NewProgram(guided.NewModel(version), opts...)
	front := guided.NewFront(p)
	u := ui.New(front, answers).WithEnv(environ)
	session := func() int {
		defer front.Done()
		if only != nil {
			code, err := runOne(root, u, *only, px, h)
			if err != nil && (root.Err() == nil || task.PrintAfter(err)) {
				failure = err
			}
			return code
		}
		err := task.Home(root, u, px, h, homeBound)
		switch {
		case errors.Is(err, ui.ErrQuit):
			if task.PrintAfter(err) {
				failure = err
			}
			return exitINT
		case errors.Is(err, task.ErrReexec):
			restart.Store(true)
			return exitOK
		case err == task.ErrEnd:
			return exitOK
		case root.Err() != nil:
			return exitFail
		}
		failure = err
		return exitFail
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
	e.restart = restart.Load()
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
