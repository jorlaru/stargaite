package task

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
)

// changeKeySteps is Change a key's step table.
func changeKeySteps() []ui.Step {
	return []ui.Step{{ID: "which", Title: "which"}, {ID: "change", Title: "change"}, {ID: "key", Title: "key"},
		{ID: "check", Title: "check"}, {ID: "try", Title: "try"}}
}

// ChangeKey makes one change to a client key: its secret rotated (the new
// one shown once, then tried), the aliases it may call, its limits, its
// budget, or the key revoked. Each change is confirmed before it is made.
func ChangeKey(ctx context.Context, u *ui.UI, px *proxy.Proxy, h *host.Host) (string, error) {
	r := &run{u: u, px: px, h: h}
	res, err := r.changeKey(ctx)
	return res, r.kept(ctx, err)
}

func (r *run) changeKey(ctx context.Context) (string, error) {
	if err := ready(ctx, r.u, r.h, "which"); err != nil {
		return "", err
	}
	if err := r.listKeys(ctx); err != nil {
		return "", err
	}
	if len(r.keys) == 0 {
		return "No key yet: Give an app or agent its key makes one.", nil
	}
	if err := r.listModels(ctx); err != nil {
		return "", err
	}
	q := ui.Question{ID: "key", Step: "which", Prompt: "Which key?"}
	for _, k := range r.keys {
		q.Options = append(q.Options, ui.Option{Value: k.Name, Label: k.Name + " → " + mayCall(k)})
	}
	n, err := r.u.Pick(ctx, q)
	if err != nil {
		return "", err
	}
	k, _ := r.key(n)
	change, err := r.u.Pick(ctx, ui.Question{ID: "change", Step: "change", Prompt: "What do you want to change?", Options: []ui.Option{
		{Value: "rotate", Label: "rotate its secret"}, {Value: "aliases", Label: "the aliases it may call"},
		{Value: "limits", Label: "its limits: requests a minute and at once"}, {Value: "budget", Label: "its budget"},
		{Value: "revoke", Label: "revoke it"}}})
	if err != nil {
		return "", err
	}
	switch change {
	case "rotate":
		return r.rotate(ctx, k)
	case "aliases":
		return r.keyAliases(ctx, k)
	case "limits":
		return r.keyLimits(ctx, k)
	case "budget":
		return r.keyBudget(ctx, k)
	case "revoke":
		return r.revoke(ctx, k)
	}
	return "", fmt.Errorf("unknown change %q", change)
}

// mayCall is the aliases key k may call, as said here and in See what
// exists: "*" is every alias, and none is no alias.
func mayCall(k proxy.Key) string {
	switch {
	case len(k.Models) == 0:
		return "no alias"
	case len(k.Models) == 1 && k.Models[0] == "*":
		return "every alias"
	}
	return strings.Join(k.Models, ", ")
}

// rotate replaces key k's secret, the one in use kept for an hour or ended
// now, then hands the new one over.
func (r *run) rotate(ctx context.Context, k proxy.Key) (string, error) {
	grace, err := r.u.Pick(ctx, ui.Question{ID: "grace", Step: "change", Prompt: "What happens to the secret in use?", Options: []ui.Option{
		{Value: "1h", Label: "it stays valid for 1 hour, so its apps can move"}, {Value: "0s", Label: "it ends now"}}})
	if err != nil {
		return "", err
	}
	if _, inGrace := find(k.Secrets, func(s proxy.Secret) bool { return s.ValidUntil > 0 }); inGrace {
		say(ctx, r.u, "key "+k.Name+" has an older secret in its grace period: it ends now, as a key never has more than two") // SEC-8
	}
	text := "the secret of key " + k.Name + " is replaced; the one in use stays valid for 1 hour"
	if grace == "0s" {
		text = "the secret of key " + k.Name + " is replaced; the one in use ends now, and its apps are refused"
	}
	if err := confirm(ctx, r.u, "change", text); err != nil {
		return "", err
	}
	secret, err := r.newSecret(ctx, "keys rotate", []string{"--grace", grace}, k.Name, "rotated")
	if err != nil {
		return "", err
	}
	tried, err := r.handOver(ctx, k.Name, "rotated", secret, k.Models)
	switch {
	case err != nil:
		return "", err
	case !tried:
		return "Done: key " + k.Name + " rotated; not tried.", nil
	}
	return "Done: key " + k.Name + " rotated.", nil
}

// keyAliases sets the aliases key k may call.
func (r *run) keyAliases(ctx context.Context, k proxy.Key) (string, error) {
	if len(r.models) == 0 {
		return "No alias yet: Make an alias makes one.", nil
	}
	aliases, err := r.pickAliases(ctx, ui.Question{ID: "aliases", Step: "change", Prompt: "Which aliases may it call?"})
	if err != nil {
		return "", err
	}
	toward := strings.Join(aliases, ", ")
	if err := confirm(ctx, r.u, "change", "key "+k.Name+" will call only "+toward); err != nil {
		return "", err
	}
	var flags []string
	for _, a := range aliases {
		flags = append(flags, "--model", a)
	}
	findings, err := r.priced(ctx, "change", "key "+k.Name+" now calls "+toward, "keys set", flags, k.Name)
	if err != nil {
		return "", err
	}
	r.warn(ctx, findings, false, "keys/"+k.Name)
	return "Done: key " + k.Name + ", which may call " + toward + ".", nil
}

// keyLimits sets or clears key k's requests a minute and at once. Neither
// question has a default, so an empty answer clears the limit.
func (r *run) keyLimits(ctx context.Context, k proxy.Key) (string, error) {
	rpm, err := r.u.Text(ctx, ui.Question{ID: "rpm-limit", Step: "change",
		Prompt: "requests a minute, now " + limitNow(k.RPMLimit, "no limit") + "; empty for no limit", Check: positiveOrEmpty})
	if err != nil {
		return "", err
	}
	par, err := r.u.Text(ctx, ui.Question{ID: "max-parallel-requests", Step: "change",
		Prompt: "requests at once, now " + limitNow(k.MaxParallel, "the default") + "; empty for the proxy's default, 5", Check: positiveOrEmpty})
	if err != nil {
		return "", err
	}
	rpmFlags, rpmWords := limit("rpm-limit", rpm, " requests a minute", "no limit a minute")
	parFlags, parWords := limit("max-parallel-requests", par, " at once", "the default at once")
	what := rpmWords + ", " + parWords
	if err := confirm(ctx, r.u, "change", "the limits of key "+k.Name+" become "+what); err != nil {
		return "", err
	}
	if err := r.setKey(ctx, k.Name, append(rpmFlags, parFlags...)...); err != nil {
		return "", err
	}
	return "Done: key " + k.Name + ", " + what + ".", nil
}

// limitNow is a limit as keys list shows it, or none when it has none.
func limitNow(v *int64, none string) string {
	if v == nil {
		return none
	}
	return strconv.FormatInt(*v, 10)
}

// limit is a limit answered a, a whole number above 0 or empty to clear it,
// as keys set's flags and in words: the number and unit, or none.
func limit(flag, a, unit, none string) ([]string, string) {
	if a == "" {
		return []string{"--" + flag + "="}, none
	}
	n, _ := strconv.Atoi(a) // the CLI refuses "010"
	return []string{"--" + flag, strconv.Itoa(n)}, strconv.Itoa(n) + unit
}

// setKey runs keys set on key n with flags and says the findings on it.
func (r *run) setKey(ctx context.Context, n string, flags ...string) error {
	var change proxy.Change
	if err := r.px.Call(ctx, &change, "keys set", flags, n); err != nil {
		return err
	}
	r.warn(ctx, change.Findings, false, "keys/"+n)
	return nil
}

// revoke revokes key k, once agreed.
func (r *run) revoke(ctx context.Context, k proxy.Key) (string, error) {
	if err := confirm(ctx, r.u, "change", "key "+k.Name+" is revoked: its apps are refused within a second, and its name is never used again"); err != nil {
		return "", err
	}
	var change proxy.Change
	if err := r.px.Call(ctx, &change, "keys revoke", []string{"--yes"}, k.Name); err != nil {
		return "", err
	}
	return "Done: key " + k.Name + " revoked.", nil
}

// keyBudget sets, replaces or removes key k's budget. Every question is
// asked before anything is written.
func (r *run) keyBudget(ctx context.Context, k proxy.Key) (string, error) {
	todo := "set"
	if k.Budget != nil {
		var err error
		if todo, err = r.u.Pick(ctx, ui.Question{ID: "budget", Step: "change", Prompt: "What do you want to do with its budget?",
			Options: []ui.Option{{Value: "set", Label: "set another"}, {Value: "remove", Label: "remove it"}}}); err != nil {
			return "", err
		}
	}
	if todo == "remove" {
		if err := confirm(ctx, r.u, "change", "the budget of key "+k.Name+", "+usd(k.Budget)+", is removed"); err != nil {
			return "", err
		}
		if err := r.setKey(ctx, k.Name, "--budget="); err != nil {
			return "", err
		}
		return "Done: key " + k.Name + ", no budget.", nil
	}
	l, err := r.u.Text(ctx, ui.Question{ID: "limit-usd", Step: "change", Prompt: "the limit in USD", Check: isLimit})
	if err != nil {
		return "", err
	}
	reset, err := r.u.Pick(ctx, ui.Question{ID: "reset", Step: "change", Prompt: "When does it reset?",
		Options: []ui.Option{{Value: "monthly", Label: "monthly"}, {Value: "daily", Label: "daily"}, {Value: "never", Label: "never"}}})
	if err != nil {
		return "", err
	}
	v, _ := strconv.ParseFloat(l, 64)
	l = usdText(v) // as a JSON number: the CLI refuses "05"
	what := l + " USD, reset " + reset
	text := "a budget of " + l + " USD is set on key " + k.Name + ", reset " + reset // it may refuse the key's apps at once
	if k.Budget != nil {
		text = "the budget of key " + k.Name + ", " + usd(k.Budget) + ", is replaced by " + what
	}
	if err := confirm(ctx, r.u, "change", text); err != nil {
		return "", err
	}
	findings, err := r.priced(ctx, "change", "key "+k.Name+": budget "+what, "keys set", []string{"--budget", "limit_usd=" + l + ",reset_interval=" + reset}, k.Name)
	if err != nil {
		return "", err
	}
	r.warn(ctx, findings, false, "keys/"+k.Name)
	return "Done: key " + k.Name + ", budget " + what + ".", nil
}

// priced makes a change that a budget may refuse for targets without prices;
// a refusal publishes nothing. On such a refusal it asks the prices of
// exactly the targets it names, all before writing, sets them, makes the
// change once more, then checks again those of them that were checked
// (design section 3, Prices). done names the change in what the run
// changed, once made. It returns the change's findings.
func (r *run) priced(ctx context.Context, step, done, words string, flags []string, positionals ...string) ([]proxy.Finding, error) {
	var change proxy.Change
	refused, err := r.px.CallRefusable(ctx, &change, words, flags, positionals...)
	switch {
	case err != nil:
		return nil, err
	case len(refused) == 0:
		r.made = append(r.made, done)
		return change.Findings, nil
	}
	unpriced := func(f proxy.Finding) bool {
		return f.Code == "unpriced_under_budget" && strings.HasPrefix(f.Resource, "targets/")
	}
	if err := refusal("this change", refused, unpriced); err != nil {
		return nil, err
	}
	if err := r.listTargets(ctx); err != nil {
		return nil, err
	}
	var ids, verified []string
	for _, f := range refused {
		if t := strings.TrimPrefix(f.Resource, "targets/"); f.Severity == "error" && unpriced(f) {
			ids = append(ids, t)
			if listed, ok := find(r.targets, func(l proxy.Target) bool { return l.ID == t }); ok && listed.VerifiedAt > 0 {
				verified = append(verified, t)
			}
		}
	}
	today := time.Now().Format(time.DateOnly)
	prices := make([]*proxy.Pricing, len(ids))
	for i, t := range ids {
		if prices[i], err = r.askPrices(ctx, step, t, today); err != nil {
			return nil, err
		}
	}
	for i, t := range ids {
		if err := r.setPrices(ctx, t, prices[i]); err != nil {
			return nil, err
		}
	}
	if err := r.px.Call(ctx, &change, words, flags, positionals...); err != nil {
		return nil, err
	}
	r.made = append(r.made, done)
	return change.Findings, r.recheck(ctx, verified)
}
