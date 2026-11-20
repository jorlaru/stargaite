package task

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/rules"
	"github.com/jorlaru/stargaite/internal/console/ui"
)

// home is what the home says above its question.
type home struct {
	summary   string
	suggested []rules.Suggestion // from the proxy's read, or Start or Restart when the container is stopped or not ready
	first     bool               // the first run opens (design section 4.1)
}

// Home is the guided console's home (design section 4.2): before each pick,
// what exists is read within bound under a work piece, which Esc stops, and
// its summary said; then the picked task runs, a suggestion's with its
// presets, and how it ended is said. On its first round, when no container,
// provider, alias or key exists, the first run runs in place of the pick. It
// returns ui.ErrQuit on Ctrl-C, ErrReexec or an ErrEnd once a task ended
// the session, its result said, or the error that ended it.
func Home(ctx context.Context, u *ui.UI, px *proxy.Proxy, h *host.Host, bound time.Duration) error {
	menu := Menu()
	for round := 0; ; round++ {
		var hm home
		err := u.Work(ctx, ui.Question{ID: "home-read", Prompt: "reading what exists"}, func(ctx context.Context) error {
			read, cancel := context.WithTimeout(ctx, bound)
			defer cancel()
			hm = readHome(read, px, h)
			return nil
		})
		switch {
		case errors.Is(err, ui.ErrLeave):
			hm = home{summary: "what exists was not read: stopped"}
		case err != nil:
			return err
		}
		if err := u.Say(ctx, hm.summary); err != nil {
			return err
		}
		picked := "First run"
		if round > 0 || !hm.first {
			if picked, err = u.Pick(ctx, homeQuestion(hm, menu)); err != nil {
				return err
			}
		}
		label, res, err := runPicked(ctx, u, px, h, menu, hm.suggested, picked)
		switch {
		case errors.Is(err, ui.ErrQuit):
			return err
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, ErrEnd) || errors.Is(err, ErrReexec):
			if res != "" {
				if err := u.Say(ctx, res); err != nil {
					return err
				}
			}
			return err
		}
		if line := taskLine(label, res, err); line != "" {
			if err := u.Say(ctx, line); err != nil {
				return err
			}
		}
	}
}

// readHome reads what exists (design section 3, Home data): the data plane,
// the clone's version, whether the container runs, or exists, and is ready,
// then export, validate and version, concurrently, only when it is ready. A
// stopped container suggests Start, and one not ready Restart.
func readHome(ctx context.Context, px *proxy.Proxy, h *host.Host) home {
	p, err := h.Plane()
	if err != nil {
		return home{summary: "this clone's settings cannot be read: " + err.Error()}
	}
	clone := h.CloneVersion(ctx)
	runs, err := h.ContainerRuns(ctx)
	exists := runs
	if err == nil && !runs {
		exists, err = h.ContainerExists(ctx)
	}
	switch {
	case err != nil:
		return home{summary: p.BaseURL + ": not checked · clone " + clone + " · docker compose ps: " + err.Error()}
	case !exists:
		return home{summary: p.BaseURL + ": not installed yet (./setup.sh install sets it up) · clone " + clone, first: true}
	case !runs:
		return home{summary: p.BaseURL + ": stopped · clone " + clone, suggested: []rules.Suggestion{
			{Label: "Start the container: it is stopped", Task: "Stop or start", Presets: map[string]string{"action": "start"}}}}
	case h.Ready(ctx) != nil:
		return home{summary: p.BaseURL + ": not ready · clone " + clone, suggested: []rules.Suggestion{
			{Label: "Restart the container: it is not ready", Task: "Stop or start", Presets: map[string]string{"action": "restart"}}}}
	}
	var (
		export   proxy.ExportReply
		validate proxy.Change
		version  proxy.VersionReply
	)
	if err := readAll(ctx, px, read{"export", &export}, read{"validate", &validate}, read{"version", &version}); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = errors.New("no answer in time")
		}
		// A proxy this console cannot decode: Rebuild mends it, when the
		// version was read.
		return home{summary: p.BaseURL + ": ready · clone " + clone + " · the proxy was not read: " + err.Error(),
			suggested: rules.Upgrades(rules.Facts{Running: version.Version, Clone: clone})}
	}
	summary := fmt.Sprintf("%s: ready · proxy %s · clone %s · %d provider(s), %d alias(es), %d key(s)",
		p.BaseURL, version.Version, clone, len(export.Providers), len(export.Models), len(export.Keys))
	if export.Settings.Budget == nil {
		summary += " · spend not capped"
	}
	f := facts(export, validate)
	f.Running, f.Clone = version.Version, clone
	f.Upstream, f.Ahead, _ = h.Ahead(ctx)
	return home{summary: summary, suggested: rules.Suggest(f), first: f.Providers == 0 || len(f.Aliases) == 0 || f.Keys == 0}
}

// read is one CLI read: its words and the reply it decodes into.
type read struct {
	words string
	reply proxy.Reply
}

// readAll makes each read on its own goroutine and returns once all have
// returned: the error of the first, in order, that failed. A panic in a read
// is its error.
func readAll(ctx context.Context, px *proxy.Proxy, reads ...read) error {
	errs := make([]chan error, len(reads))
	for i, r := range reads {
		errs[i] = make(chan error, 1)
		ui.Go(func() { errs[i] <- px.Call(ctx, r.reply, r.words, nil) },
			func(v any) { errs[i] <- fmt.Errorf("stargaite %s: %v", r.words, v) })
	}
	var first error
	for _, c := range errs {
		if err := <-c; first == nil {
			first = err
		}
	}
	return first
}

// unresolved is, per provider, the variable its credential names that does
// not resolve: validate's credential_unresolved findings on
// providers/<name>, whose message starts with it (internal/config/validate.go).
func unresolved(findings []proxy.Finding) map[string]string {
	out := map[string]string{}
	for _, f := range findings {
		if p, ok := strings.CutPrefix(f.Resource, "providers/"); ok && f.Code == "credential_unresolved" {
			out[p], _, _ = strings.Cut(f.Message, " ")
		}
	}
	return out
}

// homeQuestion is the home's pick: Suggested first, valued
// "suggested:<i>", then the menu's entries in their groups; an entry without
// a group is not listed.
func homeQuestion(hm home, menu []Entry) ui.Question {
	q := ui.Question{ID: "home", Prompt: "What do you want to do?"}
	for i, s := range hm.suggested {
		q.Options = append(q.Options, ui.Option{Value: "suggested:" + strconv.Itoa(i), Label: s.Label, Group: "Suggested"})
	}
	for _, e := range menu {
		if e.Group != "" { // the first run: the home opens it
			q.Options = append(q.Options, ui.Option{Value: e.Label, Label: e.Label, Group: e.Group})
		}
	}
	return q
}

// runPicked runs what the home's pick gave: a suggestion's task with its
// presets, or a menu entry. It returns the task's label, result and error.
func runPicked(ctx context.Context, u *ui.UI, px *proxy.Proxy, h *host.Host, menu []Entry, suggested []rules.Suggestion,
	picked string) (string, string, error) {
	label, presets := picked, map[string]string(nil)
	n, isSuggestion := strings.CutPrefix(picked, "suggested:")
	if isSuggestion {
		i, _ := strconv.Atoi(n) // homeQuestion made the value, and the pick checked it
		label, presets = suggested[i].Task, suggested[i].Presets
	}
	e, ok := find(menu, func(e Entry) bool { return e.Label == label })
	if !ok {
		return label, "", fmt.Errorf("no task %s in the menu", label)
	}
	run := func(ctx context.Context) (string, error) { return e.Run(ctx, u, px, h) }
	if isSuggestion {
		res, err := u.RunWith(ctx, e.Steps, presets, run)
		return label, res, err
	}
	res, err := u.Run(ctx, e.Steps, run)
	return label, res, err
}

// taskLine is what is said when a task ends: "Left <label>." after Esc,
// "<label>: <error>", or its result, which may be empty.
func taskLine(label, res string, err error) string {
	switch {
	case errors.Is(err, ui.ErrLeave):
		return "Left " + label + "."
	case err != nil:
		return label + ": " + err.Error()
	}
	return res
}
