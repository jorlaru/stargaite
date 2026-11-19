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
// it on its own, the value flags and the boolean flags that subcommand takes,
// and its step table. A flag's name is the ID of the question it answers.
type Entry struct {
	Group, Label, Sub string
	Flags, Bools      []string
	Steps             []ui.Step
	Run               Func
}

// Menu returns the ported tasks in design section 4.2's group order, a new
// slice on each call.
func Menu() []Entry {
	return []Entry{
		{Group: "Providers", Label: "Add providers", Sub: "provider",
			Flags: []string{"provider", "base-url", "protocol", "api-key-env", "key-env"},
			Bools: []string{"replace", "allow-private-network", "yes"},
			Steps: addSteps(), Run: AddProviders},
		{Group: "Providers", Label: "Change a provider", Steps: changeProviderSteps(), Run: ChangeProvider},
		{Group: "Models your apps call", Label: "Make an alias", Sub: "alias",
			Flags: []string{"provider", "base-url", "protocol", "api-key-env", "key-env", "model", "alias", "fallback",
				"max-output-tokens", "effort", "input-price", "output-price"},
			Bools: []string{"replace", "allow-private-network", "yes", "reverify"},
			Steps: makeSteps(), Run: MakeAlias},
		{Group: "Models your apps call", Label: "Change an alias", Steps: changeAliasSteps(), Run: ChangeAlias},
		{Group: "Apps and agents", Label: "Give an app or agent its key", Sub: "key",
			Flags: []string{"alias", "key-name", "rpm-limit"}, Steps: giveSteps(), Run: GiveKey},
		{Group: "Apps and agents", Label: "Change a key", Steps: changeKeySteps(), Run: ChangeKey},
		{Group: "This machine", Label: "See what exists", Sub: "status",
			Steps: []ui.Step{{ID: "show", Title: "what exists"}}, Run: SeeWhatExists},
	}
}
