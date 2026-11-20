package task

import (
	"context"
	"fmt"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
)

func uninstallSteps() []ui.Step {
	return []ui.Step{{ID: "remove", Title: "uninstall"}}
}

// Uninstall removes the container and its network, the two images and the
// console, once confirmed, and keeps the volume and every file of the
// reader's install, printing the commands that remove them (design section
// 3, Uninstall). It never passes Compose -v or --rmi, nor docker rmi -f.
// Done, it ends the session (ErrEnd): its console is gone.
func Uninstall(ctx context.Context, u *ui.UI, _ *proxy.Proxy, h *host.Host) (string, error) {
	image, volume, err := h.Config(ctx)
	if err != nil {
		return "", err
	}
	// Two lines, so that neither is cut at a long project or image name.
	say(ctx, u, "uninstall keeps the volume "+volume+", which holds the store (keys, configuration and usage), "+
		".env, compose.override.yaml, backups/ and tls/")
	if err := confirm(ctx, u, "remove", fmt.Sprintf("uninstall stops and removes the container and its network, then the images %s and "+
		"%s-previous and bin/stargaite-console", image, image)); err != nil {
		return "", err
	}
	if err := u.Work(ctx, ui.Question{ID: "down", Step: "remove", Prompt: "stopping and removing the container"},
		func(ctx context.Context) error { return h.Compose(ctx, sayer(ctx, u), "down") }); err != nil {
		return "", err
	}
	// What is said of a tag RemoveImage removed, did not find, or Docker refused to remove.
	words := map[string]string{"removed": "removed", "not there": "not there", "refused": "kept, Docker refused to remove it (its words are above)"}
	for _, tag := range []string{image, image + "-previous"} {
		outcome, err := h.RemoveImage(ctx, sayer(ctx, u), tag)
		if err != nil {
			return "", err
		}
		say(ctx, u, "image "+tag+": "+words[outcome])
	}
	if err := h.RemoveConsole(); err != nil {
		return "", err
	}
	for _, line := range []string{"bin/stargaite-console: removed", "what it kept goes, if you want, with these commands in this clone:",
		"  docker volume rm " + volume, "  rm .env compose.override.yaml", "  rm -r backups tls"} {
		say(ctx, u, line)
	}
	return "Done: uninstalled; ./setup.sh installs again on the kept volume.", ErrEnd
}
