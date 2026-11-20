package task

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/rules"
	"github.com/jorlaru/stargaite/internal/console/ui"
)

func checkSteps() []ui.Step {
	return []ui.Step{{ID: "read", Title: "findings"}, {ID: "fix", Title: "fix"}}
}

// CheckEverything says each finding of validate in plain words, in its
// order, with the stargaite command that fixes it when no task does, then
// offers the findings a task fixes and runs the one picked with its presets
// (design section 4.3).
func CheckEverything(ctx context.Context, u *ui.UI, px *proxy.Proxy, h *host.Host) (string, error) {
	if err := ready(ctx, u, h, "read"); err != nil {
		return "", err
	}
	var export proxy.ExportReply
	if err := px.Call(ctx, &export, "export", nil); err != nil {
		return "", err
	}
	findings, err := px.Validate(ctx)
	switch {
	case err != nil:
		return "", err
	case len(findings) == 0:
		return "Nothing to fix: the proxy reports no finding.", nil
	}
	pick := ui.Question{ID: "fix", Step: "fix", Prompt: "Which one do you want to fix?", Options: []ui.Option{{Value: "none", Label: "none of them now"}}}
	var fixes []rules.Suggestion
	commands := false
	for _, f := range findings {
		line := findingLine(f)
		say(ctx, u, line)
		if c := rules.Command(f.Code, f.Resource); c != "" {
			say(ctx, u, "  stargaite "+c)
			commands = true
		}
		alias := ""
		if id, ok := strings.CutPrefix(f.Resource, "targets/"); ok {
			alias = holder(export, id)
		}
		if s, ok := rules.Fix(f.Code, f.Resource, alias); ok {
			pick.Options = append(pick.Options, ui.Option{Value: strconv.Itoa(len(fixes)), Label: line})
			fixes = append(fixes, s)
		}
	}
	if commands {
		cli, err := cliFromClone(h)
		if err != nil {
			return "", err
		}
		say(ctx, u, "stargaite above is the proxy's command, run from this clone as "+cli+"; a target it changes must be checked again, which is paid")
	}
	if len(fixes) == 0 {
		return fmt.Sprintf("%d finding(s), none with a task here: the lines above say what fixes each.", len(findings)), nil
	}
	picked, err := u.Pick(ctx, pick)
	switch {
	case err != nil:
		return "", err
	case picked == "none":
		return fmt.Sprintf("%d finding(s); none fixed now.", len(findings)), nil
	}
	label, res, err := runPicked(ctx, u, px, h, Menu(), fixes, "suggested:"+picked)
	switch {
	case errors.Is(err, ui.ErrQuit):
		return "", err
	case ctx.Err() != nil:
		return "", ctx.Err()
	}
	return taskLine(label, res, err), nil
}

// findingLine is a finding in plain words, "error: " first for an error. A
// credential that does not resolve outside a provider is telemetry's, which
// Send traces sets.
func findingLine(f proxy.Finding) string {
	line := rules.Plain(f.Code, f.Resource, f.Message)
	if f.Code == "credential_unresolved" && !strings.HasPrefix(f.Resource, "providers/") {
		line = "telemetry: " + f.Message + ": Send traces sets its keys"
	}
	if f.Severity == "error" {
		return "error: " + line
	}
	return line
}
