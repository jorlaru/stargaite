package task

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
)

// changeProviderSteps is Change a provider's step table.
func changeProviderSteps() []ui.Step {
	return []ui.Step{{ID: "which", Title: "which"}, {ID: "change", Title: "change"}, {ID: "key", Title: "key"},
		{ID: "recreate", Title: "recreate"}, {ID: "check", Title: "check"}}
}

// ChangeProvider makes one change to a provider, after checking its key
// again for free: its key replaced, its own timeout or its billing set, or
// the provider removed. Its targets that the change leaves unverified are
// checked again through the paid step.
func ChangeProvider(ctx context.Context, u *ui.UI, px *proxy.Proxy, h *host.Host) (string, error) {
	r := &run{u: u, px: px, h: h}
	res, err := r.changeProvider(ctx)
	return res, r.kept(ctx, err)
}

func (r *run) changeProvider(ctx context.Context) (string, error) {
	if err := ready(ctx, r.u, r.h, "which"); err != nil {
		return "", err
	}
	if err := r.listProviders(ctx); err != nil {
		return "", err
	}
	if len(r.providers) == 0 {
		return "No provider yet: Add providers adds one.", nil
	}
	p, err := r.pickProvider(ctx, ui.Question{ID: "provider", Step: "which", Prompt: "Which provider?"}, false)
	if err != nil {
		return "", err
	}
	if err := r.recheckKey(ctx, p); err != nil {
		return "", err
	}
	if err := r.listTargets(ctx); err != nil {
		return "", err
	}
	var verified []string // checked before the change, which may clear it
	for _, t := range r.targets {
		if strings.HasPrefix(t.ID, p.Name+"/") && t.VerifiedAt > 0 {
			verified = append(verified, t.ID)
		}
	}
	var opts []ui.Option
	if p.APIKeyEnv != "" {
		opts = append(opts, ui.Option{Value: "key", Label: "replace its key"})
	}
	opts = append(opts, ui.Option{Value: "timeout", Label: "its own timeout"}, ui.Option{Value: "plan", Label: "subscription plan or pay per use"},
		ui.Option{Value: "remove", Label: "remove it"})
	change, err := r.u.Pick(ctx, ui.Question{ID: "change", Step: "change", Prompt: "What do you want to change?", Options: opts})
	if err != nil {
		return "", err
	}
	var what string
	switch change {
	case "key":
		return r.replaceKey(ctx, p)
	case "remove":
		return r.removeProvider(ctx, p)
	case "timeout":
		what, err = r.providerTimeout(ctx, p)
	case "plan":
		what, err = r.providerPlan(ctx, p)
	default:
		return "", fmt.Errorf("unknown change %q", change)
	}
	if err != nil {
		return "", err
	}
	r.made = append(r.made, p.Name+": "+what)
	if err := r.recheck(ctx, verified); err != nil {
		return "", err
	}
	return "Done: " + p.Name + ", " + what + ".", nil
}

// recheckKey checks p's key in .env again with one free call; a key not in
// .env is said.
func (r *run) recheckKey(ctx context.Context, p proxy.Provider) error {
	if p.APIKeyEnv == "" {
		return nil
	}
	key, inEnv, err := r.h.EnvGet(p.APIKeyEnv)
	switch {
	case err != nil:
		return err
	case !inEnv:
		say(ctx, r.u, p.APIKeyEnv+" is not in .env")
		return nil
	}
	_, _, err = tryKey(ctx, r.u, r.h, "which", p, key, "current key")
	return err
}

// replaceKey asks p's new key and checks it for free; a key p refuses ends
// it with nothing written. Then the key goes to .env and the container is
// recreated to read it.
func (r *run) replaceKey(ctx context.Context, p proxy.Provider) (string, error) {
	key, outcome, err := checkKey(ctx, r.u, r.h, "key", p)
	if err != nil {
		return "", err
	}
	if err := confirm(ctx, r.u, "recreate", "writing "+p.APIKeyEnv+" to .env"+recreates); err != nil {
		return "", err
	}
	if err := r.h.EnvSet(p.APIKeyEnv, key); err != nil {
		return "", err
	}
	if err := recreate(ctx, r.u, r.h, "recreate", true); err != nil {
		return "", err
	}
	if err := resolved(ctx, r.px, p.Name, p.APIKeyEnv); err != nil {
		return "", err
	}
	return "Done: " + p.Name + ", key replaced, " + outcome + ".", nil
}

// providerTimeout sets the longest one call to p may take, or clears it for
// the setting's; it returns what changed.
func (r *run) providerTimeout(ctx context.Context, p proxy.Provider) (string, error) {
	s, err := r.u.Text(ctx, ui.Question{ID: "total-seconds", Step: "change",
		Prompt: "the longest one call to " + p.Name + " may take, in seconds; empty for the default", Check: positiveOrEmpty})
	if err != nil {
		return "", err
	}
	to, flags := "the default", []string{"--total-ms="}
	if s != "" {
		n, _ := strconv.Atoi(s)
		to, flags = strconv.Itoa(n)+" s", []string{"--total-ms", strconv.Itoa(n) + "000"} // in ms, without an overflow
	}
	say(ctx, r.u, "setting the timeout of "+p.Name+" to "+to)
	return "timeout " + to, r.setProvider(ctx, p, flags)
}

// providerPlan sets how p is billed: a subscription with its windows, or
// pay per use, which clears them; it returns what changed.
func (r *run) providerPlan(ctx context.Context, p proxy.Provider) (string, error) {
	billing, err := r.u.Pick(ctx, ui.Question{ID: "billing", Step: "change", Prompt: "How is " + p.Name + " billed?",
		Options: []ui.Option{{Value: "subscription", Label: "a subscription plan, metered by its windows"}, {Value: "pay_per_use", Label: "pay per use"}}})
	if err != nil {
		return "", err
	}
	flags := []string{"--billing", billing, "--quota="}
	if billing == "subscription" {
		windows, err := r.u.Text(ctx, ui.Question{ID: "quota", Step: "change", Prompt: "its windows, such as 5h:requests=1000 7d:tokens=5000000",
			Check: func(a string) error { return nonEmpty(strings.TrimSpace(a)) }})
		if err != nil {
			return "", err
		}
		flags = flags[:2]
		for _, w := range strings.Fields(windows) {
			flags = append(flags, "--quota", w)
		}
	}
	say(ctx, r.u, "setting the billing of "+p.Name+" to "+billing)
	return "billing " + billing, r.setProvider(ctx, p, flags)
}

// setProvider runs providers set on p with flags and says the findings on p.
func (r *run) setProvider(ctx context.Context, p proxy.Provider, flags []string) error {
	var change proxy.Change
	if err := r.px.Call(ctx, &change, "providers set", flags, p.Name); err != nil {
		return err
	}
	r.warn(ctx, change.Findings, false, "providers/"+p.Name)
	return nil
}

// checkAgain is the paid question of a re-check after a provider changed.
func checkAgain(k, total int) string {
	return fmt.Sprintf("%d target(s) must be checked again: %d paid call(s)", k, total)
}

// recheck checks again those of verified, checked before a change, whose
// verification the server cleared: it clears it only when the provider
// actually changed. The providers are read again, so the check's bound has
// the new timeout. A check that does not pass, for any reason, says the
// targets it left unchecked.
func (r *run) recheck(ctx context.Context, verified []string) error {
	if len(verified) == 0 {
		return nil
	}
	if err := r.listProviders(ctx); err != nil {
		return err
	}
	if err := r.listTargets(ctx); err != nil {
		return err
	}
	var again []string
	for _, id := range verified {
		if t, ok := find(r.targets, func(t proxy.Target) bool { return t.ID == id }); ok && t.VerifiedAt == 0 {
			again = append(again, id)
		}
	}
	if len(again) == 0 {
		return nil
	}
	if checked, err := r.check(ctx, "check", checkAgain, again); !checked {
		left := slices.DeleteFunc(again, func(id string) bool {
			t, _ := find(r.targets, func(t proxy.Target) bool { return t.ID == id })
			return t.VerifiedAt > 0 // checked before the check ended
		})
		say(ctx, r.u, fmt.Sprintf("%d target(s) left unchecked: %s", len(left), strings.Join(left, ", ")))
		return err
	}
	return nil
}

// removeProvider removes p, once agreed: its targets out of each alias that
// has another, then its targets, then p; then its variable from .env when
// nothing else names it, and the container is recreated. An alias left with
// no target refuses it before anything changes.
func (r *run) removeProvider(ctx context.Context, p proxy.Provider) (string, error) {
	if err := r.listModels(ctx); err != nil {
		return "", err
	}
	ofP := func(t string) bool { return strings.HasPrefix(t, p.Name+"/") }
	var holders []aliasPlan  // each alias with a target of p, toward its other targets
	var names, lone []string // lone: the aliases with no other target
	for _, m := range r.models {
		rest := slices.DeleteFunc(slices.Clone(m.Targets), ofP)
		switch {
		case len(rest) == len(m.Targets):
		case len(rest) == 0:
			lone = append(lone, m.Name)
		default:
			holders, names = append(holders, aliasPlan{name: m.Name, targets: rest}), append(names, m.Name)
		}
	}
	switch {
	case len(lone) == 1:
		return "", fmt.Errorf("%s cannot be removed: alias %s has no other target; give it a target of another provider, or remove the alias, first", p.Name, lone[0])
	case len(lone) > 1:
		return "", fmt.Errorf("%s cannot be removed: aliases %s have no other target; give each a target of another provider, or remove them, first",
			p.Name, strings.Join(lone, ", "))
	}
	var ids []string
	for _, t := range r.targets {
		if ofP(t.ID) {
			ids = append(ids, t.ID)
		}
	}
	unset, err := r.unsettable(ctx, p)
	if err != nil {
		return "", err
	}
	text := "removing " + p.Name + ": "
	if len(holders) > 0 {
		text += "takes its targets out of " + strings.Join(names, ", ") + "; "
	}
	if len(ids) > 0 {
		text += "removes the targets " + strings.Join(ids, ", ") + " and the provider"
	} else {
		text += "removes the provider"
	}
	if unset {
		text += "; removes " + p.APIKeyEnv + " from .env and recreates the container"
	}
	if err := confirm(ctx, r.u, "change", text); err != nil {
		return "", err
	}
	for _, a := range holders {
		if _, err := r.setAlias(ctx, a, false); err != nil {
			return "", err
		}
		r.made = append(r.made, "alias "+a.name+" now toward "+strings.Join(a.targets, ", "))
	}
	var change proxy.Change
	for _, t := range ids {
		say(ctx, r.u, "removing target "+t)
		if err := r.px.Call(ctx, &change, "targets remove", []string{"--yes"}, t); err != nil {
			return "", err
		}
		r.made = append(r.made, "target "+t+" removed")
	}
	say(ctx, r.u, "removing provider "+p.Name)
	if err := r.px.Call(ctx, &change, "providers remove", []string{"--yes"}, p.Name); err != nil {
		return "", err
	}
	r.made = append(r.made, "provider "+p.Name+" removed")
	if unset {
		if _, err := r.h.EnvUnset(p.APIKeyEnv); err != nil {
			return "", err
		}
		if err := recreate(ctx, r.u, r.h, "recreate", true); err != nil {
			return "", err
		}
	}
	return "Done: " + p.Name + " removed.", nil
}

// unsettable reports whether p's variable leaves .env with p: it is in
// .env, and neither another provider nor the telemetry names it.
func (r *run) unsettable(ctx context.Context, p proxy.Provider) (bool, error) {
	v := p.APIKeyEnv
	if v == "" {
		return false, nil
	}
	if _, inEnv, err := r.h.EnvGet(v); err != nil || !inEnv {
		return false, err
	}
	if _, shared := find(r.providers, func(o proxy.Provider) bool { return o.Name != p.Name && o.APIKeyEnv == v }); shared {
		return false, nil
	}
	var tel proxy.TelemetryReply
	if err := r.px.Call(ctx, &tel, "telemetry status", nil); err != nil {
		return false, err
	}
	b := tel.Telemetry
	return b == nil || (!slices.Contains(b.BasicAuthEnv, v) && !slices.Contains(slices.Collect(maps.Values(b.HeaderEnv)), v)), nil
}
