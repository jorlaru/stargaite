package task

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
)

// makeSteps is Make an alias's step table.
func makeSteps() []ui.Step {
	return []ui.Step{{ID: "name", Title: "name"}, {ID: "model", Title: "model"}, {ID: "fallbacks", Title: "fallbacks"},
		{ID: "settings", Title: "settings"}, {ID: "check", Title: "check"}, {ID: "make", Title: "make"}}
}

// MakeAlias makes an alias toward models of the providers added: every
// question is asked, and what the proxy needs settled through a dry run,
// before a target is added; the new targets are checked through the paid
// step before the alias is made. An alias that exists is kept; with
// --fallback it gets the fallbacks it lacks.
func MakeAlias(ctx context.Context, u *ui.UI, px *proxy.Proxy, h *host.Host) (string, error) {
	r := &run{u: u, px: px, h: h}
	res, err := r.makeAlias(ctx)
	return res, r.kept(ctx, err)
}

func (r *run) makeAlias(ctx context.Context) (string, error) {
	if err := ready(ctx, r.u, r.h, "name"); err != nil {
		return "", err
	}
	n, err := r.u.Text(ctx, ui.Question{ID: "alias", Flag: "alias", Step: "name", Prompt: "a name for the alias your clients call",
		Default: "main", HasDefault: true, Check: nonEmpty})
	if err != nil {
		return "", err
	}
	if err := r.listModels(ctx); err != nil {
		return "", err
	}
	if m, ok := r.model(n); ok {
		return r.exists(ctx, m)
	}
	if err := r.listProviders(ctx); err != nil {
		return "", err
	}
	p, err := r.pickProvider(ctx, ui.Question{ID: "provider", Flag: "provider", Step: "model", Prompt: "Which provider has the model?"}, true)
	if err != nil {
		return "", err
	}
	model, err := r.pickModel(ctx, ui.Question{ID: "model", Flag: "model", Step: "model"}, p)
	if err != nil {
		return "", err
	}
	fallbacks, err := r.pickFallbacks(ctx, "fallbacks")
	if err != nil {
		return "", err
	}
	effort, err := r.u.Text(ctx, ui.Question{ID: "effort", Flag: "effort", Step: "settings",
		Prompt: "a default reasoning effort, such as low, medium or high; empty for none", HasDefault: true})
	if err != nil {
		return "", err
	}
	limit := ui.Question{ID: "max-output-tokens", Flag: "max-output-tokens", Step: "settings", Prompt: "the alias's output cap in tokens", Check: positive}
	var c string
	if r.u.Given(limit) {
		if c, err = r.u.Text(ctx, limit); err != nil {
			return "", err
		}
	}
	if err := r.listTargets(ctx); err != nil {
		return "", err
	}
	a := r.newPlan(n, proxy.AliasDecl{}, append([]string{p.Name + "/" + model}, fallbacks...))
	if c != "" {
		a.setCap(c)
	}
	if effort != "" {
		a.setEffort(effort)
	}
	if err := r.settle(ctx, "settings", &a); err != nil {
		return "", err
	}
	toward := strings.Join(a.targets, ", ")
	checked, err := r.check(ctx, "check", inAll, a.targets)
	switch {
	case err != nil:
		return "", fmt.Errorf("%w; no alias or key was created", err)
	case !checked:
		r.sayKept(ctx)
		return "Not checked: alias " + n + " was not made.", nil
	}
	say(ctx, r.u, "adding alias "+n+" toward "+toward)
	var change proxy.Change
	if err := r.px.Call(ctx, &change, "models add", a.flags(), n); err != nil {
		return "", err
	}
	r.made = append(r.made, "alias "+n+" added")
	r.warn(ctx, change.Findings, true, a.resources()...)
	if a.effort != "" {
		if err := r.explain(ctx, n); err != nil {
			return "", err
		}
	}
	return "Done: alias " + n + " toward " + toward + ".", nil
}

// exists keeps alias m, which exists; with --fallback, the fallbacks it
// lacks are added to it, so a run again with the same flags is kept.
func (r *run) exists(ctx context.Context, m proxy.Model) (string, error) {
	kept := "alias " + m.Name + " exists, toward " + strings.Join(m.Targets, ", ") + "; kept: Change an alias changes it"
	q := ui.Question{ID: "fallback", Flag: "fallback", Step: "fallbacks"}
	if !r.u.Given(q) {
		return kept, nil
	}
	given, err := r.u.Text(ctx, q)
	if err != nil {
		return "", err
	}
	adds := r.fresh(ctx, m, strings.Split(given, ","))
	if len(adds) == 0 {
		return kept, nil
	}
	if err := r.listProviders(ctx); err != nil {
		return "", err
	}
	if _, err := r.pickFallbacks(ctx, "fallbacks"); err != nil { // refuses a fallback of a provider not added
		return "", err
	}
	return r.extend(ctx, m.Name, adds)
}

// fresh is adds without the targets alias m already has, each said kept.
func (r *run) fresh(ctx context.Context, m proxy.Model, adds []string) []string {
	var out []string
	for _, t := range adds {
		if slices.Contains(m.Targets, t) {
			say(ctx, r.u, t+" is already a target of "+m.Name+"; kept")
		} else {
			out = append(out, t)
		}
	}
	return out
}
