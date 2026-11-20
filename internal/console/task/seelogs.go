package task

import (
	"context"
	"errors"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
)

func logSteps() []ui.Step {
	return []ui.Step{{ID: "logs", Title: "logs"}}
}

// SeeLogs says the container's last 100 log lines, then, at a terminal,
// follows its new ones until Esc or an hour (OPS-11: logs are followed only
// while someone watches, so --follow without a terminal is refused). Each
// line is cleaned and cut by the host and said, so a long run of them goes
// to the scrollback.
func SeeLogs(ctx context.Context, u *ui.UI, _ *proxy.Proxy, h *host.Host) (string, error) {
	if !u.Asks() && u.Given(ui.Question{ID: "follow"}) {
		return "", &ui.UsageError{Msg: "--follow needs a terminal: the console follows logs only while someone watches"}
	}
	exists, err := h.ContainerExists(ctx)
	switch {
	case err != nil:
		return "", err
	case !exists:
		return "", errNoContainer
	}
	if err := u.Work(ctx, ui.Question{ID: "tail", Step: "logs", Prompt: "reading the last log lines"}, func(ctx context.Context) error {
		_, err := h.SeeLogs(ctx, sayer(ctx, u), false)
		return err
	}); err != nil {
		return "", err
	}
	if !u.Asks() {
		return "Done: the last 100 log lines are above.", nil
	}
	var timedOut bool
	err = u.Work(ctx, ui.Question{ID: "follow", Step: "logs", Prompt: "following the logs"}, func(ctx context.Context) error {
		var err error
		timedOut, err = h.SeeLogs(ctx, sayer(ctx, u), true)
		return err
	})
	switch {
	case errors.Is(err, ui.ErrLeave):
		return "Stopped following the logs.", nil
	case err != nil:
		return "", err
	case timedOut:
		return "Followed the logs for 1 hour: stopped; See logs follows them again.", nil
	}
	return "The logs ended.", nil
}
