package task

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
)

// startIt ends every outcome of a restore whose start failed.
const startIt = "; docker compose up -d --no-build in this clone starts it"

// ErrRanOn marks the error of a restore whose put-back and start ran to
// their end after the session was ended; main prints it.
var ErrRanOn = errors.New("the restore ran to its end")

func backupSteps() []ui.Step {
	return []ui.Step{{ID: "what", Title: "back up or restore"}, {ID: "copy", Title: "which copy"}, {ID: "put", Title: "put back"}}
}

// BackUpOrRestore backs the store up into backups/ now, or puts a copy back
// (design section 4.3). The subcommands backup and restore answer which
// with their preset.
func BackUpOrRestore(ctx context.Context, u *ui.UI, px *proxy.Proxy, h *host.Host) (string, error) {
	what, err := u.Pick(ctx, ui.Question{ID: "what", Step: "what", Prompt: "Back up or restore?",
		Options: []ui.Option{{Value: "backup", Label: "back up the store now"}, {Value: "restore", Label: "restore a copy"}}})
	switch {
	case err != nil:
		return "", err
	case what == "backup":
		return backUp(ctx, u, px, h)
	}
	return restoreCopy(ctx, u, h)
}

// backUp copies the store of the running proxy into backups/, named by its
// version and the time, under a work piece, which Esc stops; a failure
// keeps nothing.
func backUp(ctx context.Context, u *ui.UI, px *proxy.Proxy, h *host.Host) (string, error) {
	if err := ready(ctx, u, h, "what"); err != nil {
		return "", err
	}
	var v proxy.VersionReply
	if err := px.Call(ctx, &v, "version", nil); err != nil {
		return "", err
	}
	var c host.Copy
	err := u.Work(ctx, ui.Question{ID: "backup", Step: "what", Prompt: "backing up the store"}, func(ctx context.Context) error {
		var err error
		c, err = h.Backup(ctx, sayer(ctx, u), v.Version, func(w io.Writer) (int64, error) { return px.Backup(ctx, w) })
		return err
	})
	switch {
	case errors.Is(err, ui.ErrLeave) || errors.Is(err, ui.ErrQuit):
		return "", err
	case err != nil:
		return "", fmt.Errorf("the backup failed (%w), so nothing was kept", err)
	}
	return fmt.Sprintf("Done: %s, %d bytes.", c.Path, c.Size), nil
}

// restoreCopy puts a copy back, once confirmed: the container stopped, the
// copy put back with the current image, the container started again. The
// put-back and the start run to their end once begun, whatever Esc or
// Ctrl-C (a store half moved is the one damage a cancel could do), and how
// to start the container is said before the stop.
func restoreCopy(ctx context.Context, u *ui.UI, h *host.Host) (string, error) {
	exists, err := h.ContainerExists(ctx)
	switch {
	case err != nil:
		return "", err
	case !exists:
		return "", errNoContainer
	}
	c, found, err := pickCopy(ctx, u, h)
	switch {
	case err != nil:
		return "", err
	case !found:
		return "No copy in backups/ yet: Back up or restore backs up the store now.", nil
	}
	taken := c.Time.UTC().Format("2006-01-02 15:04") + " UTC"
	if err := confirm(ctx, u, "copy", fmt.Sprintf("the store is replaced by the copy %s, taken %s: keys revoked since come back, "+
		"spend counted since is forgotten, and no audit record is written; the container is stopped and started again, "+
		"which interrupts traffic", c.Path, taken)); err != nil {
		return "", err
	}
	say(ctx, u, "restoring "+c.Path+", taken "+taken)
	say(ctx, u, "if this stops before the container starts again, docker compose up -d --no-build in this clone starts it")
	if err := u.Work(ctx, ui.Question{ID: "stop", Step: "put", Prompt: "stopping the container"},
		func(ctx context.Context) error { return h.Compose(ctx, sayer(ctx, u), "stop") }); err != nil {
		return "", err
	}
	var code int
	var putErr, upErr error
	err = u.Work(ctx, ui.Question{ID: "put", Step: "put", Prompt: "putting the copy back and starting the container; this runs to its end"},
		func(wctx context.Context) error {
			// Only the two commands outlive the work piece: its lines are
			// dropped once it ends, never waiting on a front that has quit.
			code, putErr = h.Restore(context.WithoutCancel(wctx), sayer(wctx, u), c)
			upErr = h.Up(context.WithoutCancel(wctx), sayer(wctx, u), "--no-build")
			return nil
		})
	res, failed := restored(c.Path, taken, code, putErr, upErr)
	// Ctrl-C, or a signal at a terminal: the front has quit, so the error
	// carries how it ended, for main to print once the terminal is restored.
	if errors.Is(err, ui.ErrQuit) || ctx.Err() != nil && u.Asks() {
		if failed == nil {
			failed = errors.New(res)
		}
		return "", fmt.Errorf("%w; %w: %w", ui.ErrQuit, ErrRanOn, failed)
	}
	return res, failed
}

// pickCopy is the copy --from names, any file, or the one picked among
// backups/; found is false at a terminal when backups/ holds none.
func pickCopy(ctx context.Context, u *ui.UI, h *host.Host) (c host.Copy, found bool, err error) {
	q := ui.Question{ID: "from", Flag: "from", Step: "copy", Prompt: "the copy to put back"}
	if u.Given(q) {
		path, err := u.Text(ctx, q)
		if err != nil {
			return host.Copy{}, false, err
		}
		c, err := h.CopyAt(path)
		return c, err == nil, err
	}
	copies, err := h.Copies()
	switch {
	case err != nil:
		return host.Copy{}, false, err
	case len(copies) == 0 && u.Asks():
		return host.Copy{}, false, nil
	}
	q.Prompt = "Which copy?"
	for _, c := range copies {
		q.Options = append(q.Options, ui.Option{Value: c.Path,
			Label: fmt.Sprintf("%s · %d bytes · %s UTC", filepath.Base(c.Path), c.Size, c.Time.UTC().Format("2006-01-02 15:04"))})
	}
	path, err := u.Pick(ctx, q)
	if err != nil {
		return host.Copy{}, false, err
	}
	return copies[slices.IndexFunc(copies, func(c host.Copy) bool { return c.Path == path })], true, nil
}

// restored is how a restore ended, from the put-back (its exit code, or an
// error when it did not start or end) and the start after it, which always
// runs. It never says the store is as it was: restore can fail after moving
// it.
func restored(path, taken string, code int, putErr, upErr error) (string, error) {
	var put string
	switch {
	case putErr != nil:
		put = "the copy may not be put back: " + putErr.Error() + "; its words above say whether anything moved"
	case code != 0:
		put = fmt.Sprintf("the copy was not put back (restore exits %d): its words above say whether anything moved", code)
	case upErr != nil:
		return "", fmt.Errorf("the store is the copy %s, but the container did not start: %w%s", path, upErr, startIt)
	default:
		return fmt.Sprintf("Done: the store is the copy %s, taken %s.", path, taken), nil
	}
	if upErr != nil {
		return "", fmt.Errorf("%s, and the container did not start: %w%s", put, upErr, startIt)
	}
	return "", errors.New(put + "; the container is started again")
}
