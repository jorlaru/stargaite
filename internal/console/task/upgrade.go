package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/rules"
	"github.com/jorlaru/stargaite/internal/console/ui"
)

func upgradeSteps() []ui.Step {
	return []ui.Step{{ID: "check", Title: "check"}, {ID: "changes", Title: "what changes"}, {ID: "keep", Title: "previous image"},
		{ID: "merge", Title: "fast-forward"}, {ID: "build", Title: "build"}, {ID: "backup", Title: "backup"}, {ID: "start", Title: "start and check"}}
}

// upgrading is an upgrade or a rebuild, its doors and what it learns on
// the way.
type upgrading struct {
	u                           *ui.UI
	px                          *proxy.Proxy
	h                           *host.Host
	rebuild                     bool
	upstream, old               string // the clone's upstream and the commit it was at; "" for a rebuild, which moves none
	running, clone, image, copy string // the versions, the image's name and the backup's path
	valid                       int    // validate's verdict before
}

// Upgrade fast-forwards the clone to its upstream and rebuilds, as
// setup.sh's upgrade did: the running image kept as <image>-previous, a
// backup taken just before the new version starts, which is then checked.
// Nothing rolls back by itself, since a migration may be one-way: the way
// back is said before the start. With rebuild, it builds the clone's own
// version, fetching and merging nothing. At a terminal, the new console is
// then exported and the session restarts on it (ErrReexec).
func Upgrade(ctx context.Context, u *ui.UI, px *proxy.Proxy, h *host.Host) (string, error) {
	g := &upgrading{rebuild: u.Given(ui.Question{ID: "rebuild", Flag: "rebuild"}), u: u, px: px, h: h}
	if err := ready(ctx, u, h, "check"); err != nil {
		return "", err
	}
	if !g.rebuild {
		if res, err := g.check(ctx); res != "" || err != nil {
			return res, err
		}
	}
	var v proxy.VersionReply
	if err := px.Call(ctx, &v, "version", nil); err != nil {
		return "", fmt.Errorf("the running version was not read: %w", err)
	}
	g.running, g.clone = v.Version, h.CloneVersion(ctx)
	switch {
	case g.rebuild:
		say(ctx, u, "the proxy runs "+g.running+"; this clone is at "+g.clone)
	case g.running == "dev":
		say(ctx, u, "the running version is unknown (a dev build); this clone is at "+g.clone)
	case g.running != g.clone:
		say(ctx, u, "the running version, "+g.running+", is not this clone's, "+g.clone)
	}
	legacy, err := g.legacyCopy(ctx)
	if err != nil {
		return "", err
	}
	if g.valid, err = px.ValidateExit(ctx); err != nil {
		return "", err
	}
	what := "upgrading: the clone fast-forwards to " + g.upstream + " and the image is rebuilt; then a backup of the store is taken " +
		"and the container recreated on the new version, which interrupts traffic briefly"
	if g.rebuild {
		what = "rebuilding: the image of this clone's version, " + g.clone + ", is built; then a backup of the store is taken " +
			"and the container recreated on it, which interrupts traffic briefly"
	}
	if err := confirm(ctx, u, "changes", what); err != nil {
		return "", err
	}
	if g.image, err = h.KeepImage(ctx); err != nil {
		return "", err
	}
	say(ctx, u, "the running image is kept as "+g.image+"-previous")
	if err := g.merge(ctx); err != nil {
		return "", err
	}
	if err := g.build(ctx); err != nil {
		return "", err
	}
	g.copy = legacy
	if g.copy == "" {
		if err := g.backup(ctx); err != nil {
			return "", err
		}
	}
	res, err := g.start(ctx)
	if err != nil || !u.Asks() {
		return res, err
	}
	err = u.Work(ctx, ui.Question{ID: "export", Step: "start", Prompt: "exporting the new console into bin/"},
		func(ctx context.Context) error { return h.ExportConsole(ctx, sayer(ctx, u), g.clone) })
	switch {
	case pressed(err):
		err = errors.New("stopped")
	case err == nil:
		return res, ErrReexec
	}
	say(ctx, u, res)
	return "", endWith(fmt.Errorf("the new version runs, but the new console was not exported (%s); ./setup.sh exports and starts it", err))
}

// check refuses a clone that cannot fast-forward, fetches its upstream and
// says what changes; res is the outcome when it is up to date.
func (g *upgrading) check(ctx context.Context) (res string, err error) {
	u, h := g.u, g.h
	if !h.Clean(ctx) {
		return "", errors.New("the clone has local changes to tracked files (git status lists them): commit or stash them, then run this again")
	}
	var ahead int
	if err := u.Work(ctx, ui.Question{ID: "fetch", Step: "check", Prompt: "fetching the clone's upstream; 20 s at most"},
		func(ctx context.Context) (err error) { g.upstream, ahead, err = h.Fetch(ctx); return err }); err != nil {
		return "", err
	}
	upstream := g.upstream
	if ahead == 0 {
		// An upgrade that did not finish leaves the clone at a commit that
		// does not run.
		var v proxy.VersionReply
		if err := g.px.Call(ctx, &v, "version", nil); err != nil {
			return "", fmt.Errorf("the running version was not read: %w", err)
		}
		if id := h.Commit(ctx, v.Version); id != "" && id != h.Commit(ctx, "HEAD") {
			return "", fmt.Errorf("up to date with %s, but %s runs, not this clone's commit: an upgrade did not finish; "+
				"git reset --hard %s, then run this again", upstream, v.Version, v.Version)
		}
		return "up to date with " + upstream + "; nothing was changed", nil
	}
	switch ff, err := h.FastForwards(ctx); {
	case err != nil:
		return "", err
	case !ff:
		return "", fmt.Errorf("the clone has commits that %s lacks, so it cannot fast-forward: merge or rebase it yourself, then run this again", upstream)
	}
	commits, err := h.NewCommits(ctx)
	if err != nil {
		return "", err
	}
	say(ctx, u, strconv.Itoa(ahead)+" new commit(s) on "+upstream+":")
	for _, c := range commits {
		say(ctx, u, c)
	}
	switch changed, err := h.MigrationsChanged(ctx); {
	case err != nil:
		return "", err
	case changed:
		say(ctx, u, "a migration of the store changed: migrations are one-way, so the previous version cannot open the store they migrate")
	}
	g.old = h.Commit(ctx, "HEAD")
	return "", nil
}

// legacyCopy is the copy made by hand that a running CLI without restore
// needs, said how to make; "" when the CLI takes its own backup.
func (g *upgrading) legacyCopy(ctx context.Context) (string, error) {
	help, err := g.px.Help(ctx)
	if err != nil || strings.Contains(help, "\n  restore ") {
		return "", err
	}
	cli, err := cliFromClone(g.h)
	if err != nil {
		return "", err
	}
	say(ctx, g.u, "the running stargaite predates backup -, so the console cannot take the backup: make it by hand, a copy that also stays in the volume,")
	say(ctx, g.u, "  "+cli+" backup /data/pre-upgrade.db && mkdir -p backups && docker compose cp stargaite:/data/pre-upgrade.db backups/")
	path, err := g.u.Text(ctx, ui.Question{ID: "legacy-backup", Flag: "legacy-backup", Step: "changes",
		Prompt: "the copy made by hand, on this host, such as backups/pre-upgrade.db"})
	if err != nil {
		return "", err
	}
	c, err := g.h.CopyAt(path)
	return c.Path, err
}

// merge fast-forwards the clone under a work piece; the merge runs to its
// end whatever is pressed, since one stopped halfway leaves files behind, so
// it begins only once how to return is printed and the session goes on.
func (g *upgrading) merge(ctx context.Context) error {
	if g.rebuild {
		return nil
	}
	if err := g.u.Say(ctx, "if this stops before the new version starts, git reset --hard "+g.old+" returns the clone to where it was"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil { // the plain front prints whatever the context
		return err
	}
	var merged error
	err := g.u.Work(ctx, ui.Question{ID: "merge", Step: "merge", Prompt: "fast-forwarding the clone; this runs to its end"},
		func(wctx context.Context) error {
			merged = g.h.Merge(context.WithoutCancel(wctx), sayer(wctx, g.u))
			return nil
		})
	switch {
	case errors.Is(merged, host.ErrMergeStopped):
		return fmt.Errorf("git merge --ff-only did not finish within 2 minutes and was stopped: %s still runs, the store unchanged; files it wrote "+
			"may be left untracked (git status --untracked-files=all lists them): remove them, then git reset --hard %s", g.running, g.old)
	case merged != nil:
		return fmt.Errorf("git merge --ff-only failed: %s still runs, the store unchanged", g.running)
	}
	g.clone = g.h.CloneVersion(ctx)
	if pressed(err) {
		return said(ctx, g.u, err, "the clone is at "+g.clone+" and nothing was built: "+g.running+" still runs, the store unchanged; git reset --hard "+
			g.old+" returns the clone to where it was")
	}
	return err
}

// build builds the clone's version under a work piece, which Esc stops.
func (g *upgrading) build(ctx context.Context) error {
	say(ctx, g.u, "building version "+g.clone)
	err := g.u.Work(ctx, ui.Question{ID: "build", Step: "build", Prompt: "building version " + g.clone + "; Esc stops it"},
		func(ctx context.Context) error { return g.h.ComposeBuild(ctx, sayer(ctx, g.u), g.clone) })
	if err == nil {
		return nil
	}
	failed := "the build failed or took over 20 minutes: " + g.running + " still runs, the store unchanged"
	if !g.rebuild {
		failed += "; git reset --hard " + g.old + " returns the clone to it"
	}
	return said(ctx, g.u, err, failed)
}

// backup takes a backup of the store under a work piece, which Esc stops.
func (g *upgrading) backup(ctx context.Context) error {
	c, err := backupStore(ctx, g.u, g.px, g.h, "backup", g.running)
	if err == nil {
		g.copy = c.Path
		return nil
	}
	reason := err.Error()
	if pressed(err) || ctx.Err() != nil {
		reason = "stopped"
	}
	return g.notStarted(ctx, err, "the backup failed ("+reason+")")
}

// notStarted is the failure of a step after the build and before the start:
// the image's name already points at the new build, so it says how to
// return.
func (g *upgrading) notStarted(ctx context.Context, err error, why string) error {
	back := "docker tag " + g.image + "-previous " + g.image + " returns to it"
	if !g.rebuild {
		back = "docker tag " + g.image + "-previous " + g.image + " and git reset --hard " + g.old + " return to it"
	}
	return said(ctx, g.u, err, why+", so the new version was not started: "+g.running+" still runs, the store unchanged; "+back)
}

// said is a step's failure. Once the session has ended at a terminal
// (Ctrl-C, or a signal), the front has quit: the error carries text, for
// main to print once the terminal is restored. After Esc, text is said and
// the press returned; otherwise text is the error.
func said(ctx context.Context, u *ui.UI, err error, text string) error {
	switch {
	case errors.Is(err, ui.ErrQuit) || ctx.Err() != nil && u.Asks():
		return fmt.Errorf("%w; %w: %s", ui.ErrQuit, ErrUnfinished, text)
	case pressed(err):
		say(ctx, u, text)
		return err
	}
	return errors.New(text)
}

// start says the way back and how to finish the start by hand, then starts
// the new version, which runs to its end once begun, so it begins only once
// those lines are printed and the session goes on; then it checks it: its
// version and validate's verdict, as before.
func (g *upgrading) start(ctx context.Context) (string, error) {
	u, h := g.u, g.h
	lines := append(wayBack(g.old, g.running, g.image, g.copy, g.rebuild), "starting version "+g.clone,
		"if this stops before the new version starts, docker compose up -d --no-build in this clone starts it")
	for _, line := range lines {
		if err := u.Say(ctx, line); err != nil {
			return "", g.notStarted(ctx, err, "the session ended before the start")
		}
	}
	if err := ctx.Err(); err != nil { // the plain front prints whatever the context
		return "", g.notStarted(ctx, err, "the session ended before the start")
	}
	var upErr error
	err := u.Work(ctx, ui.Question{ID: "start", Step: "start", Prompt: "starting version " + g.clone + "; this runs to its end"},
		func(wctx context.Context) error {
			upErr = h.Up(context.WithoutCancel(wctx), sayer(wctx, u), "--no-build")
			return nil
		})
	if errors.Is(err, ui.ErrQuit) { // the front has quit: ./setup.sh status reads the outcome
		return "", err
	}
	ok := upErr == nil
	if ok {
		ok = g.checked(ctx)
	} else {
		say(ctx, u, upErr.Error())
	}
	done, verb := "upgraded", "the upgrade"
	if g.rebuild {
		done, verb = "rebuilt", "the rebuild"
	}
	if !ok {
		return "", errors.New(verb + " failed. Nothing rolls back by itself, since a migration may be one-way: the way back is above")
	}
	readyLine := "running " + g.clone
	_, _ = h.SeeLogs(ctx, func(l string) {
		var r struct{ Msg, Version string }
		if json.Unmarshal([]byte(l), &r) == nil && r.Msg == "ready" && r.Version != "" {
			readyLine = "version " + r.Version + " is ready"
		}
	}, false)
	return done + ": " + readyLine, nil
}

// checked reports whether the new version runs and validate's verdict is
// the one before, saying what differs.
func (g *upgrading) checked(ctx context.Context) bool {
	var v proxy.VersionReply
	if err := g.px.Call(ctx, &v, "version", nil); err != nil {
		say(ctx, g.u, err.Error())
		return false
	}
	valid, err := g.px.ValidateExit(ctx)
	if err != nil {
		say(ctx, g.u, err.Error())
		return false
	}
	if v.Version != g.clone {
		say(ctx, g.u, "the running version is "+v.Version+", not "+g.clone)
	}
	if valid != g.valid {
		say(ctx, g.u, fmt.Sprintf("validate exits %d, and %d before: the new version refuses part of the stored configuration", valid, g.valid))
	}
	return v.Version == g.clone && valid == g.valid
}

// wayBack is the way back to the version that ran, from copy, the image
// kept as <image>-previous, said once before the start: an upgrade adds the
// reset of the clone, which the console never runs.
func wayBack(old, running, image, copy string, rebuild bool) []string {
	lines := []string{"the way back to " + running + ", from this directory, losing what changed since the backup; " +
		"with the new image, as the previous one may predate restore:",
		"  docker compose stop",
		"  docker compose run --rm --no-deps -T stargaite restore --yes - < " + rules.ShellWord(copy),
		"  docker tag " + image + "-previous " + image,
		"  docker compose up -d --no-build"}
	if !rebuild {
		lines = append(lines, "and, before any later build, git reset --hard "+old+", which the console does not run")
	}
	return lines
}
