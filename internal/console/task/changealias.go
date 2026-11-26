package task

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/rules"
	"github.com/jorlaru/stargaite/internal/console/ui"
)

// changeAliasSteps is Change an alias's step table.
func changeAliasSteps() []ui.Step {
	return []ui.Step{{ID: "which", Title: "which"}, {ID: "change", Title: "change"}, {ID: "settings", Title: "settings"}, {ID: "check", Title: "check"}}
}

// ChangeAlias makes one change to an alias: a fallback added (checked
// through the paid step first) or removed, its targets reordered, its effort
// or output cap set or cleared, what it sends shown, or the alias removed.
// Each change is said before it is made.
func ChangeAlias(ctx context.Context, u *ui.UI, px *proxy.Proxy, h *host.Host) (string, error) {
	r := &run{u: u, px: px, h: h}
	res, err := r.changeAlias(ctx)
	return res, r.kept(ctx, err)
}

func (r *run) changeAlias(ctx context.Context) (string, error) {
	if err := ready(ctx, r.u, r.h, "which"); err != nil {
		return "", err
	}
	if err := r.listModels(ctx); err != nil {
		return "", err
	}
	if len(r.models) == 0 {
		return "No alias yet: Make an alias makes one.", nil
	}
	q := ui.Question{ID: "alias", Step: "which", Prompt: "Which alias?"}
	for _, m := range r.models {
		q.Options = append(q.Options, ui.Option{Value: m.Name, Label: m.Name + " → " + strings.Join(m.Targets, ", ")})
	}
	n, err := r.u.Pick(ctx, q)
	if err != nil {
		return "", err
	}
	m, _ := r.model(n)
	change, err := r.u.Pick(ctx, ui.Question{ID: "change", Step: "change", Prompt: "What do you want to change?", Options: aliasChanges(m)})
	if err != nil {
		return "", err
	}
	switch change {
	case "add":
		return r.addFallback(ctx, m)
	case "drop":
		return r.dropFallback(ctx, m)
	case "order":
		return r.reorder(ctx, m)
	case "effort":
		return r.aliasEffort(ctx, m)
	case "cap":
		return r.aliasCap(ctx, m)
	case "explain":
		return "", r.explain(ctx, n)
	case "remove":
		return r.removeAlias(ctx, n)
	}
	return "", fmt.Errorf("unknown change %q", change)
}

// aliasChanges is the changes alias m can take: a fallback added while it
// has fewer than three targets (design section 4.3: at most two fallbacks),
// removed or reordered while it has two or more.
func aliasChanges(m proxy.Model) []ui.Option {
	var o []ui.Option
	if len(m.Targets) < 3 {
		o = append(o, ui.Option{Value: "add", Label: "add a fallback"})
	}
	if len(m.Targets) >= 2 {
		o = append(o, ui.Option{Value: "drop", Label: "remove a fallback"}, ui.Option{Value: "order", Label: "reorder"})
	}
	return append(o, ui.Option{Value: "effort", Label: "effort, with what will be sent"}, ui.Option{Value: "cap", Label: "output cap"},
		ui.Option{Value: "explain", Label: "see what it sends"}, ui.Option{Value: "remove", Label: "remove the alias"})
}

// addFallback adds a fallback to alias m, of a provider already added.
func (r *run) addFallback(ctx context.Context, m proxy.Model) (string, error) {
	if err := r.listProviders(ctx); err != nil {
		return "", err
	}
	p, err := r.pickProvider(ctx, ui.Question{ID: "fallback-provider", Step: "change", Prompt: "A fallback for " + m.Name + ": from which provider?"}, false)
	if err != nil {
		return "", err
	}
	model, err := r.pickModel(ctx, ui.Question{ID: "fallback-model", Step: "change"}, p)
	if err != nil {
		return "", err
	}
	return r.extend(ctx, m.Name, []string{p.Name + "/" + model})
}

// extend appends adds to alias n, settles what the proxy needs, checks the
// targets through the paid step, then sets the alias's targets and, when it
// has an effort, says what it sends. The alias's own declaration rides whole
// in the dry run, so its budget, effort and cap judge the new targets. Shared
// with MakeAlias; it reads alias n from the models listed, so listModels
// comes first.
func (r *run) extend(ctx context.Context, n string, adds []string) (string, error) {
	m, ok := r.model(n)
	if !ok {
		return "", fmt.Errorf("alias %s is not listed", n)
	}
	if adds = r.fresh(ctx, m, adds); len(adds) == 0 {
		return "alias " + n + " already toward " + strings.Join(m.Targets, ", ") + "; kept", nil
	}
	var decl proxy.AliasDecl
	if err := r.px.Call(ctx, &decl, "models show", nil, n); err != nil {
		return "", err
	}
	if err := r.listTargets(ctx); err != nil {
		return "", err
	}
	a := r.newPlan(n, decl, append(slices.Clone(m.Targets), adds...))
	if err := r.settle(ctx, "settings", &a); err != nil {
		return "", err
	}
	checked, err := r.check(ctx, "check", inAll, a.targets)
	switch {
	case err != nil:
		return "", fmt.Errorf("%w; alias %s was not changed%s", err, n, r.unadd(ctx, a, err))
	case !checked:
		r.sayKept(ctx)
		return "Not checked: alias " + n + " was not changed.", nil
	}
	res, err := r.setAlias(ctx, a, true)
	if err != nil || m.Reasoning.Effort == "" && a.effort == "" {
		return res, err
	}
	// The alias has an effort: what it now sends to each target is said, and
	// its new targets are said kept should that read fail (both callers say
	// r.made on an error).
	r.made = append(r.made, "alias "+n+" toward "+strings.Join(a.targets, ", "))
	if err := r.explain(ctx, n); err != nil {
		return "", err
	}
	return res, nil
}

// setAlias says alias a's new targets, sets them, and the cap and effort
// the run asked, and says the findings on it; checked when its targets'
// check has passed.
func (r *run) setAlias(ctx context.Context, a aliasPlan, checked bool) (string, error) {
	toward := strings.Join(a.targets, ", ")
	say(ctx, r.u, "alias "+a.name+" now toward "+toward)
	if err := r.setModel(ctx, a, checked, a.flags()...); err != nil {
		return "", err
	}
	return "Done: alias " + a.name + " toward " + toward + ".", nil
}

// setModel runs models set on alias a with flags and says the findings on
// a and its targets.
func (r *run) setModel(ctx context.Context, a aliasPlan, checked bool, flags ...string) error {
	var change proxy.Change
	if err := r.px.Call(ctx, &change, "models set", flags, a.name); err != nil {
		return err
	}
	r.warn(ctx, change.Findings, checked, a.resources()...)
	return nil
}

// dropFallback removes one of alias m's fallbacks.
func (r *run) dropFallback(ctx context.Context, m proxy.Model) (string, error) {
	q := ui.Question{ID: "drop", Step: "change", Prompt: "Which fallback?"}
	for _, t := range m.Targets[1:] {
		q.Options = append(q.Options, ui.Option{Value: t, Label: t})
	}
	t, err := r.u.Pick(ctx, q)
	if err != nil {
		return "", err
	}
	rest := slices.DeleteFunc(slices.Clone(m.Targets), func(x string) bool { return x == t })
	return r.setAlias(ctx, aliasPlan{name: m.Name, targets: rest}, false)
}

// reorder moves one of alias m's targets to another place. Alias changes
// keep verification, so nothing is checked.
func (r *run) reorder(ctx context.Context, m proxy.Model) (string, error) {
	move := ui.Question{ID: "move", Step: "change", Prompt: "Which target moves?"}
	place := ui.Question{ID: "place", Step: "change", Prompt: "To which place?"}
	for i, t := range m.Targets {
		move.Options = append(move.Options, ui.Option{Value: t, Label: t})
		place.Options = append(place.Options, ui.Option{Value: strconv.Itoa(i + 1), Label: []string{"first", "second", "third"}[min(i, 2)]})
	}
	t, err := r.u.Pick(ctx, move)
	if err != nil {
		return "", err
	}
	to, err := r.u.Pick(ctx, place)
	if err != nil {
		return "", err
	}
	i, _ := strconv.Atoi(to)
	order := slices.DeleteFunc(slices.Clone(m.Targets), func(x string) bool { return x == t })
	return r.setAlias(ctx, aliasPlan{name: m.Name, targets: slices.Insert(order, i-1, t)}, false)
}

// aliasEffort sets or clears alias m's default effort, then says what it
// sends. The question has no default, so an empty answer clears it.
func (r *run) aliasEffort(ctx context.Context, m proxy.Model) (string, error) {
	e, err := r.u.Text(ctx, ui.Question{ID: "effort", Step: "change",
		Prompt: "a default reasoning effort, such as low, medium or high; empty for none (now " + cmp.Or(m.Reasoning.Effort, "none") + ")"})
	if err != nil {
		return "", err
	}
	what, line, flags := "with effort "+e, "setting the effort of alias "+m.Name+" to "+e, []string{"--effort", e, "--effort-mode", "default"}
	if e == "" {
		what, line, flags = "with no default effort", "clearing the effort of alias "+m.Name, []string{"--effort=", "--effort-mode="}
	}
	say(ctx, r.u, line)
	if err := r.setModel(ctx, aliasPlan{name: m.Name}, false, flags...); err != nil {
		return "", err
	}
	r.made = append(r.made, "alias "+m.Name+" "+what)
	if err := r.explain(ctx, m.Name); err != nil {
		return "", err
	}
	return "Done: alias " + m.Name + " " + what + ".", nil
}

// aliasCap sets or clears alias m's output cap. The question has no
// default, so an empty answer clears it.
func (r *run) aliasCap(ctx context.Context, m proxy.Model) (string, error) {
	now := "none"
	if m.MaxOutputTokens > 0 {
		now = strconv.FormatInt(m.MaxOutputTokens, 10)
	}
	c, err := r.u.Text(ctx, ui.Question{ID: "max-output-tokens", Step: "change",
		Prompt: "the alias's output cap in tokens; empty for none (now " + now + ")", Check: positiveOrEmpty})
	if err != nil {
		return "", err
	}
	if n, err := strconv.Atoi(c); err == nil {
		c = strconv.Itoa(n) // the CLI refuses "0100" or "+100"
	}
	what, line, flags := "with output cap "+c, "setting the output cap of alias "+m.Name+" to "+c, []string{"--max-output-tokens", c}
	if c == "" {
		what, line, flags = "with no output cap", "clearing the output cap of alias "+m.Name, []string{"--max-output-tokens="}
	}
	say(ctx, r.u, line)
	if err := r.setModel(ctx, aliasPlan{name: m.Name}, false, flags...); err != nil {
		return "", err
	}
	return "Done: alias " + m.Name + " " + what + ".", nil
}

// removeAlias removes alias n, once agreed, and says each key left naming it.
// The answer holds the findings of the whole configuration, so only those
// naming n are said.
func (r *run) removeAlias(ctx context.Context, n string) (string, error) {
	if err := confirm(ctx, r.u, "change", "removing alias "+n+": clients that call it get an error"); err != nil {
		return "", err
	}
	var change proxy.Change
	if err := r.px.Call(ctx, &change, "models remove", []string{"--yes"}, n); err != nil {
		return "", err
	}
	for _, f := range change.Findings {
		if f.Code == "unknown_alias" && strings.Contains(f.Message, strconv.Quote(n)) { // `alias "n" does not exist`
			say(ctx, r.u, rules.Plain(f.Code, f.Resource, f.Message))
		}
	}
	return "Done: alias " + n + " removed.", nil
}
