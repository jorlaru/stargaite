// Package task holds the console's tasks: each one asks through a ui.UI,
// reads and changes the proxy only through a proxy.Proxy and touches the
// machine only through a host.Host, so it runs the same in the plain and in
// the guided front.
package task

import (
	"context"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
)

// Func runs a task. A non-empty result is said when it ends.
type Func func(ctx context.Context, u *ui.UI, px *proxy.Proxy, h *host.Host) (string, error)

// Entry is a task in the menu: its group, its label, the subcommand that runs
// it without a terminal, and its step table.
type Entry struct {
	Group, Label, Sub string
	Steps             []ui.Step
	Run               Func
}

// Menu returns the ported tasks, a new slice on each call.
func Menu() []Entry {
	return []Entry{
		{Group: "This machine", Label: "See what exists", Sub: "status",
			Steps: []ui.Step{{ID: "show", Title: "what exists"}}, Run: SeeWhatExists},
	}
}
