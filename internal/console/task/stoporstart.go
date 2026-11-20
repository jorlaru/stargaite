package task

import (
	"context"
	"errors"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
)

// errNoContainer is a machine task's answer when the clone has no container.
var errNoContainer = errors.New("no container here: ./setup.sh install makes it")

func stopSteps() []ui.Step {
	return []ui.Step{{ID: "action", Title: "what"}, {ID: "machine", Title: "the container"}}
}

// StopOrStart stops, starts or restarts the container (design section 4.3).
// Stop and a restart of a running container interrupt traffic and are
// confirmed; a start is said. The subcommands stop, start and restart answer
// what to do with their preset.
func StopOrStart(ctx context.Context, u *ui.UI, _ *proxy.Proxy, h *host.Host) (string, error) {
	exists, runs, err := container(ctx, h)
	switch {
	case err != nil:
		return "", err
	case !exists:
		return "", errNoContainer
	}
	action, err := u.Pick(ctx, ui.Question{ID: "action", Step: "action", Prompt: "What do you want to do with the container?",
		Options: []ui.Option{{Value: "stop", Label: "stop it"}, {Value: "start", Label: "start it"}, {Value: "restart", Label: "restart it"}}})
	if err != nil {
		return "", err
	}
	switch action {
	case "start":
		if runs {
			return "The container runs; nothing was changed.", nil
		}
		if err := start(ctx, u, h, true, false, false); err != nil {
			return "", err
		}
		return "Done: the container is started and ready.", nil
	case "stop":
		if !runs {
			return "The container is stopped already; nothing was changed.", nil
		}
		if err := confirm(ctx, u, "machine", "the container is stopped: requests in flight are given up to 60 s to finish, "+
			"then the data plane answers nothing until it is started"); err != nil {
			return "", err
		}
		if err := u.Work(ctx, ui.Question{ID: "stop", Step: "machine", Prompt: "stopping the container"},
			func(ctx context.Context) error { return h.Compose(ctx, sayer(ctx, u), "stop") }); err != nil {
			return "", err
		}
		return "Done: the container is stopped.", nil
	}
	if runs {
		if err := confirm(ctx, u, "machine", "the container is restarted: requests in flight are given up to 60 s to finish, "+
			"and the rate-limit windows restart"); err != nil {
			return "", err
		}
	} else {
		say(ctx, u, "restarting the stopped container")
	}
	if err := u.Work(ctx, ui.Question{ID: "restart", Step: "machine", Prompt: "restarting the container and waiting until it is ready"},
		func(ctx context.Context) error {
			if err := h.Compose(ctx, sayer(ctx, u), "restart"); err != nil {
				return err
			}
			return h.WaitReady(ctx, sayer(ctx, u))
		}); err != nil {
		return "", err
	}
	return "Done: the container is restarted and ready.", nil
}
