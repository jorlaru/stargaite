package task

import (
	"context"
	"errors"
	"fmt"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
)

// The steps tasks share: plain functions over the doors they need.

// say prints a line of the run. A line that cannot be printed stops
// nothing: at a terminal its one cause is the context's end, which stops the
// next step.
func say(ctx context.Context, u *ui.UI, line string) { _ = u.Say(ctx, line) }

// sayer is say for the host's lines.
func sayer(ctx context.Context, u *ui.UI) func(string) {
	return func(line string) { say(ctx, u, line) }
}

// confirm says text, then asks to continue, as setup.sh's confirm does:
// without a terminal --yes answers, and its absence is a usage error that
// names the change.
func confirm(ctx context.Context, u *ui.UI, step, text string) error {
	say(ctx, u, text)
	yes, err := u.Confirm(ctx, ui.Question{ID: "yes", Flag: "yes", Step: step, Prompt: "continue?", Default: "no"})
	var usage *ui.UsageError
	switch {
	case errors.As(err, &usage):
		return &ui.UsageError{Msg: "missing --yes: " + text}
	case err != nil:
		return err
	case !yes:
		return errors.New("stopped; nothing more was changed")
	}
	return nil
}

// ready checks the container runs, then waits until the proxy is ready
// under a work piece, which Esc stops.
func ready(ctx context.Context, u *ui.UI, h *host.Host, step string) error {
	runs, err := h.ContainerRuns(ctx)
	switch {
	case err != nil:
		return err
	case !runs:
		return errors.New("no container runs here: ./setup.sh install starts it")
	}
	return u.Work(ctx, ui.Question{ID: "ready", Step: step, Prompt: "waiting until the proxy is ready"},
		func(ctx context.Context) error { return h.WaitReady(ctx, sayer(ctx, u)) })
}

// recreate recreates the container and waits until it is ready, under a
// work piece. When wrote, it first says how to finish the recreate by hand.
func recreate(ctx context.Context, u *ui.UI, h *host.Host, step string, wrote bool) error {
	say(ctx, u, "recreating the container")
	if wrote {
		// The next run finds the files kept and does not recreate. Said
		// before the recreate, so a stop at any point, Ctrl-C included,
		// leaves it shown, and it claims nothing about how far it got.
		say(ctx, u, "the files are written: if the recreate stops before the container is recreated, "+
			"docker compose up -d --no-build in this clone does it")
	}
	return u.Work(ctx, ui.Question{ID: "recreate", Step: step, Prompt: "recreating the container and waiting until it is ready"},
		func(ctx context.Context) error { return h.Up(ctx, sayer(ctx, u), "--no-build") })
}

// resolved reads provider name back: its credential must resolve, or it
// has none.
func resolved(ctx context.Context, px *proxy.Proxy, name, variable string) error {
	var shown proxy.ProviderReply
	if err := px.Call(ctx, &shown, "providers show", nil, name); err != nil {
		return err
	}
	if shown.Credential != "resolved" && shown.Credential != "none" {
		return fmt.Errorf("the credential of %s does not resolve: %s is not in the container's environment", name, variable)
	}
	return nil
}

// checkKey asks p's key and checks it with one free call, asking again while
// the provider refuses it: three times at most, once from the flags. A
// check that cannot tell keeps the key; only the status decides.
func checkKey(ctx context.Context, u *ui.UI, h *host.Host, step string, p proxy.Provider) (key, outcome string, err error) {
	q := ui.Question{ID: "key-env", Flag: "key-env", Item: p.Name, Prompt: p.APIKeyEnv, Step: step}
	for tries := 1; ; tries++ {
		key, err := u.Secret(ctx, q)
		if err != nil {
			return "", "", err
		}
		outcome, refused, err := tryKey(ctx, u, h, step, p, key, "key")
		switch {
		case err != nil:
			return "", "", err
		case !refused:
			return key, outcome, nil
		case u.Given(q) || tries == 3:
			return "", "", fmt.Errorf("%s refused the key %d time(s); nothing was written for it", p.Name, tries)
		}
	}
}

// tryKey checks key for p with one free call under a work piece, which Esc
// stops, and says what came of it: refused is a 401 or 403; any other
// answer, or none, is "key checked" or "key not checked". what names the key
// in the said lines: "key", or "current key" for a re-check of the one in .env.
func tryKey(ctx context.Context, u *ui.UI, h *host.Host, step string, p proxy.Provider, key, what string) (outcome string, refused bool, err error) {
	check := freeCheck(p, key)
	_, status, listErr, err := listUnder(ctx, u, h, ui.Question{ID: "check-key", Step: step, Prompt: "checking the key of " + p.Name + " with one free call"}, check)
	switch {
	case err != nil:
		return "", false, err
	case status/100 == 2:
		say(ctx, u, p.Name+": "+what+" checked")
		return "key checked", false, nil
	case status == 401 || status == 403:
		say(ctx, u, fmt.Sprintf("%s refused the %s (%d)", p.Name, what, status))
		return "", true, nil
	case status != 0:
		say(ctx, u, fmt.Sprintf("%s: %s not checked: %s answered %d", p.Name, what, check.URL(), status))
	default:
		say(ctx, u, p.Name+": "+what+" not checked: "+listErr.Error())
	}
	return "key not checked", false, nil
}

// listUnder reads the model list of p under the work piece q, which Esc
// stops; err is the task's error, listErr a list that could not be read.
func listUnder(ctx context.Context, u *ui.UI, h *host.Host, q ui.Question, p host.Provider) (ids []string, status int, listErr, err error) {
	err = u.Work(ctx, q, func(ctx context.Context) error {
		ids, status, listErr = h.ListModels(ctx, p)
		if listErr != nil && ctx.Err() != nil {
			return listErr
		}
		return nil
	})
	return ids, status, listErr, err
}

// freeCheck is the free check of key for p, where p is stored.
func freeCheck(p proxy.Provider, key string) host.Provider {
	return host.Provider{Protocol: p.Protocol, BaseURL: p.BaseURL, Key: key, Path: keyCheckPath(p.BaseURL),
		AllowPrivate: p.AllowPrivateNetwork}
}
