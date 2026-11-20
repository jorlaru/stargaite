package task

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
)

// capSteps is Cap spending's step table.
func capSteps() []ui.Step {
	return []ui.Step{{ID: "on", Title: "on what"}, {ID: "limit", Title: "limit and reset"}, {ID: "check", Title: "check"}}
}

// scope is what a budget is on: kind "instance", or "alias" or "key" with
// its name.
type scope struct{ kind, name string }

// parseScope reads a scope as --on gives it: instance, alias:<name> or
// key:<name>. The name is the CLI's to judge.
func parseScope(a string) (scope, error) {
	if a == "instance" {
		return scope{kind: "instance"}, nil
	}
	if kind, name, _ := strings.Cut(a, ":"); (kind == "alias" || kind == "key") && name != "" {
		return scope{kind, name}, nil
	}
	return scope{}, errors.New("instance, alias:<name> or key:<name> is needed")
}

// String is s in words: the instance, alias <name> or key <name>.
func (s scope) String() string {
	if s.kind == "instance" {
		return "the instance"
	}
	return s.kind + " " + s.name
}

// command is the CLI's words and positionals that set s's budget.
func (s scope) command() (string, []string) {
	switch s.kind {
	case "alias":
		return "models set", []string{s.name}
	case "key":
		return "keys set", []string{s.name}
	}
	return "settings set", nil
}

// resource is s as the proxy's findings name it.
func (s scope) resource() string {
	switch s.kind {
	case "alias":
		return "models/" + s.name
	case "key":
		return "keys/" + s.name
	}
	return "settings"
}

// CapSpending sets, replaces or removes the budget on the instance, an alias
// or a key. Every question is asked before anything is written; the prices
// the proxy's refusal names are asked and set first, and the targets they
// cleared are checked again.
func CapSpending(ctx context.Context, u *ui.UI, px *proxy.Proxy, h *host.Host) (string, error) {
	r := &run{u: u, px: px, h: h}
	res, err := r.capSpending(ctx)
	return res, r.kept(ctx, err)
}

func (r *run) capSpending(ctx context.Context) (string, error) {
	if err := ready(ctx, r.u, r.h, "on"); err != nil {
		return "", err
	}
	s, current, err := r.pickScope(ctx)
	if err != nil {
		return "", err
	}
	return r.setBudget(ctx, "limit", s, current)
}

// pickScope is what the budget is on, and its budget now: typed without a
// terminal or when --on gives it, else picked from all that exists.
func (r *run) pickScope(ctx context.Context) (scope, *proxy.Budget, error) {
	q := ui.Question{ID: "on", Flag: "on", Step: "on", Prompt: "what the budget is on: instance, alias:<name> or key:<name>",
		Check: func(a string) error { _, err := parseScope(a); return err }}
	if !r.u.Asks() || r.u.Given(q) {
		a, err := r.u.Text(ctx, q)
		if err != nil {
			return scope{}, nil, err
		}
		s, _ := parseScope(a)
		b, err := r.budgetOf(ctx, s)
		return s, b, err
	}
	if err := r.listModels(ctx); err != nil {
		return scope{}, nil, err
	}
	if err := r.listKeys(ctx); err != nil {
		return scope{}, nil, err
	}
	var set proxy.SettingsReply
	if err := r.px.Call(ctx, &set, "settings show", nil); err != nil {
		return scope{}, nil, err
	}
	budgets := map[string]*proxy.Budget{"instance": set.Budget}
	opts := []ui.Option{{Value: "instance", Label: "everything (the instance)" + budget(set.Budget)}}
	for _, m := range r.models {
		budgets["alias:"+m.Name] = m.Budget
		opts = append(opts, ui.Option{Value: "alias:" + m.Name, Label: "alias " + m.Name + budget(m.Budget), Group: "Aliases"})
	}
	for _, k := range r.keys {
		budgets["key:"+k.Name] = k.Budget
		opts = append(opts, ui.Option{Value: "key:" + k.Name, Label: "key " + k.Name + budget(k.Budget), Group: "Apps and agents"})
	}
	a, err := r.u.Pick(ctx, ui.Question{ID: "on", Step: "on", Prompt: "What is the cap on?", Options: opts})
	if err != nil {
		return scope{}, nil, err
	}
	s, _ := parseScope(a)
	return s, budgets[a], nil
}

// budgetOf reads s's budget now; an alias or a key that does not exist
// stops the task.
func (r *run) budgetOf(ctx context.Context, s scope) (*proxy.Budget, error) {
	switch s.kind {
	case "alias":
		if err := r.listModels(ctx); err != nil {
			return nil, err
		}
		m, ok := r.model(s.name)
		if !ok {
			return nil, fmt.Errorf("there is no alias %s", s.name)
		}
		return m.Budget, nil
	case "key":
		if err := r.listKeys(ctx); err != nil {
			return nil, err
		}
		k, ok := r.key(s.name)
		if !ok {
			return nil, fmt.Errorf("there is no key %s", s.name)
		}
		return k.Budget, nil
	}
	var set proxy.SettingsReply
	err := r.px.Call(ctx, &set, "settings show", nil)
	return set.Budget, err
}

// setBudget sets, replaces or removes the budget on s, current the one it
// has now (nil for none), asking every question before anything is written.
// A replacement or a removal is always confirmed; a first budget is
// confirmed at a terminal and only said without one, as phase 6's budget
// did (owner, C63-Q1). The prices the proxy's refusal names are asked first
// (priced), and no_fallback_prices is said for each target the budget
// reaches.
func (r *run) setBudget(ctx context.Context, step string, s scope, current *proxy.Budget) (string, error) {
	words, positionals := s.command()
	todo := "set"
	if current != nil {
		var err error
		if todo, err = r.u.Pick(ctx, ui.Question{ID: "budget", Step: step, Prompt: "What do you want to do with its budget?",
			Options: []ui.Option{{Value: "set", Label: "set another"}, {Value: "remove", Label: "remove it"}}, Default: "set", HasDefault: true}); err != nil {
			return "", err
		}
	}
	if todo == "remove" {
		if err := confirm(ctx, r.u, step, "the budget of "+s.String()+", "+usd(current)+", is removed"); err != nil {
			return "", err
		}
		var change proxy.Change
		if err := r.px.Call(ctx, &change, words, []string{"--budget="}, positionals...); err != nil {
			return "", err
		}
		r.warn(ctx, notBudgets(change.Findings), false, s.resource())
		return "Done: " + s.String() + ", no budget.", nil
	}
	l, err := r.u.Text(ctx, ui.Question{ID: "limit-usd", Flag: "limit-usd", Step: step, Prompt: "the limit in USD", Check: isLimit})
	if err != nil {
		return "", err
	}
	reset, err := r.u.Pick(ctx, ui.Question{ID: "reset", Flag: "reset", Step: step, Prompt: "When does it reset?",
		Options: []ui.Option{{Value: "monthly", Label: "monthly"}, {Value: "daily", Label: "daily"}, {Value: "never", Label: "never"}},
		Default: "monthly", HasDefault: true})
	if err != nil {
		return "", err
	}
	v, _ := strconv.ParseFloat(l, 64)
	l = usdText(v) // as a JSON number: the CLI refuses "05"
	what := l + " USD, reset " + reset
	text := "a budget of " + l + " USD is set on " + s.String() + ", reset " + reset // it may refuse calls at once
	if current != nil {
		text = "the budget of " + s.String() + ", " + usd(current) + ", is replaced by " + what
	}
	if current == nil && !r.u.Asks() {
		say(ctx, r.u, text)
	} else if err := confirm(ctx, r.u, step, text); err != nil {
		return "", err
	}
	findings, err := r.priced(ctx, step, s.String()+": budget "+what, words, []string{"--budget", "limit_usd=" + l + ",reset_interval=" + reset}, positionals...)
	if err != nil {
		return "", err
	}
	if err := r.warnBudget(ctx, s, findings); err != nil {
		return "", err
	}
	return "Done: " + s.String() + ", budget " + what + ".", nil
}

// warnBudget says the findings on s, and no_fallback_prices on each target
// the budget on s reaches: a target priced only by its provider's cost
// report, whose calls without a report count nothing toward the budget.
func (r *run) warnBudget(ctx context.Context, s scope, findings []proxy.Finding) error {
	resources := []string{s.resource()}
	if slices.ContainsFunc(findings, func(f proxy.Finding) bool { return f.Code == "no_fallback_prices" }) {
		ids, err := r.reached(ctx, s)
		if err != nil {
			return err
		}
		for _, f := range findings {
			if t, ok := strings.CutPrefix(f.Resource, "targets/"); ok && f.Code == "no_fallback_prices" && slices.Contains(ids, t) {
				resources = append(resources, f.Resource)
			}
		}
	}
	shown := slices.DeleteFunc(notBudgets(findings), func(f proxy.Finding) bool {
		return f.Resource != s.resource() && f.Code != "no_fallback_prices"
	})
	r.warn(ctx, shown, false, resources...)
	return nil
}

// notBudgets is the findings without credential_unresolved, on settings for
// telemetry: setting or removing a budget neither causes nor resolves it.
func notBudgets(findings []proxy.Finding) []proxy.Finding {
	return slices.DeleteFunc(slices.Clone(findings), func(f proxy.Finding) bool { return f.Code == "credential_unresolved" })
}

// reached is the targets a budget on s reaches: those of the aliases it
// covers, listed again here. The instance and a key that may call "*" cover
// every alias, an alias itself, a key the aliases it may call: the reach of
// budgetScopes in internal/config/validate.go, which this keeps in step.
func (r *run) reached(ctx context.Context, s scope) ([]string, error) {
	if err := r.listModels(ctx); err != nil {
		return nil, err
	}
	covers := []string{s.name}
	if s.kind == "key" {
		k, _ := r.key(s.name)
		covers = k.Models
	}
	var ids []string
	for _, m := range r.models {
		if s.kind == "instance" || slices.Contains(covers, "*") || slices.Contains(covers, m.Name) {
			ids = append(ids, m.Targets...)
		}
	}
	return ids, nil
}
