// Package task holds the console's tasks: each one asks through a ui.UI,
// reads and changes the proxy only through a proxy.Proxy and touches the
// machine only through a host.Host, so it runs the same in the plain and in
// the guided front.
package task

import (
	"context"
	"errors"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
)

// Func runs a task. A non-empty result is said when it ends.
type Func func(ctx context.Context, u *ui.UI, px *proxy.Proxy, h *host.Host) (string, error)

// ErrEnd is returned by a task, with its result, when nothing is left for
// the session to do (Uninstall): Home says the result and returns it, and
// main exits 0.
var ErrEnd = errors.New("the session ends")

// Entry is a task in the menu: its group, its label, the subcommand that runs
// it on its own, the value flags and the boolean flags that subcommand takes,
// the answers it gives by question ID (Preset, never a flag's), and its step
// table. A flag's name is the ID of the question it answers.
type Entry struct {
	Group, Label, Sub string
	Flags, Bools      []string
	Preset            map[string]string
	Steps             []ui.Step
	Run               Func
}

// Menu returns the ported tasks in design section 4.2's group order, then
// the first run and the subcommands that run a listed task with a preset, a
// new slice on each call. The home finds a task by label, first match, so
// the listed entries come first.
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
		{Group: "Spend and traces", Label: "Cap spending", Sub: "budget",
			Flags: []string{"on", "limit-usd", "reset", "input-price", "output-price"}, Bools: []string{"yes", "reverify"},
			Steps: capSteps(), Run: CapSpending},
		{Group: "Spend and traces", Label: "See usage and cost", Steps: usageSteps(), Run: SeeUsage}, // no subcommand: design section 4.4
		{Group: "Spend and traces", Label: "Send traces", Sub: "telemetry",
			Flags: []string{"backend", "endpoint", "langfuse-public-env", "langfuse-secret-env", "header-name", "key-env"},
			Bools: []string{"replace", "allow-private-network", "yes"}, Steps: traceSteps(), Run: SendTraces},
		{Group: "This machine", Label: "See what exists", Sub: "status",
			Steps: []ui.Step{{ID: "show", Title: "what exists"}}, Run: SeeWhatExists},
		{Group: "This machine", Label: "Back up or restore", Steps: backupSteps(), Run: BackUpOrRestore},
		{Group: "This machine", Label: "See logs", Sub: "logs", Bools: []string{"follow"}, Steps: logSteps(), Run: SeeLogs},
		{Group: "This machine", Label: "Stop or start", Steps: stopSteps(), Run: StopOrStart},
		{Group: "This machine", Label: "Check everything", Steps: checkSteps(), Run: CheckEverything}, // no subcommand: design section 4.4
		{Group: "This machine", Label: "Uninstall", Sub: "uninstall", Bools: []string{"yes"}, Steps: uninstallSteps(), Run: Uninstall},
		{Label: "First run", Sub: "install", // no group: the home opens it, its pick does not list it
			Flags: []string{"port", "provider", "base-url", "protocol", "api-key-env", "key-env", "model", "alias", "fallback",
				"max-output-tokens", "effort", "input-price", "output-price", "key-name", "rpm-limit"},
			Bools: []string{"replace", "allow-private-network", "yes", "reverify"},
			Steps: firstSteps(), Run: FirstRun},
		// No group: subcommands that run a listed task with a preset answer.
		{Label: "Back up or restore", Sub: "backup", Preset: map[string]string{"what": "backup"}, Steps: backupSteps(), Run: BackUpOrRestore},
		{Label: "Back up or restore", Sub: "restore", Flags: []string{"from"}, Bools: []string{"yes"},
			Preset: map[string]string{"what": "restore"}, Steps: backupSteps(), Run: BackUpOrRestore},
		{Label: "Stop or start", Sub: "stop", Bools: []string{"yes"}, Preset: map[string]string{"action": "stop"}, Steps: stopSteps(), Run: StopOrStart},
		{Label: "Stop or start", Sub: "start", Preset: map[string]string{"action": "start"}, Steps: stopSteps(), Run: StopOrStart},
		{Label: "Stop or start", Sub: "restart", Bools: []string{"yes"}, Preset: map[string]string{"action": "restart"}, Steps: stopSteps(),
			Run: StopOrStart},
	}
}
