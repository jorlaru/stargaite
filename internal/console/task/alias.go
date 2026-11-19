package task

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jorlaru/stargaite/internal/console/families"
	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/rules"
	"github.com/jorlaru/stargaite/internal/console/ui"
)

// modelListLimit caps a model list read to pick a model: OpenRouter's,
// without a key, is about 0.9 MiB (480 models in November 2026), near the
// free key check's 1 MiB.
const modelListLimit = 8 << 20

// A check's bound is n × (total + callSlack) + checkMargin (see bound).
const (
	callSlack   = 5 * time.Second
	checkMargin = 10 * time.Second
)

// run is one run of a task that changes aliases, providers or keys: its doors,
// what it read, and what it changed.
type run struct {
	u         *ui.UI
	px        *proxy.Proxy
	h         *host.Host
	providers []proxy.Provider // providers list, read by listProviders
	targets   []proxy.Target   // targets list, read by listTargets
	models    []proxy.Model    // models list, read by listModels
	keys      []proxy.Key      // keys list, read by listKeys
	made      []string         // what this run changed, said if it stops early
	defaultMS int64            // settings show's timeouts.total_ms, read once by bound

	slack, margin time.Duration // bound's callSlack and checkMargin when 0; tests shorten them
}

// find is the first item of s that is wanted.
func find[T any](s []T, wanted func(T) bool) (T, bool) {
	var zero T
	if i := slices.IndexFunc(s, wanted); i >= 0 {
		return s[i], true
	}
	return zero, false
}

// listModels reads the aliases.
func (r *run) listModels(ctx context.Context) error {
	var reply proxy.ModelsReply
	if err := r.px.Call(ctx, &reply, "models list", nil); err != nil {
		return err
	}
	r.models = reply.Models
	return nil
}

// model is the listed alias named n.
func (r *run) model(n string) (proxy.Model, bool) {
	return find(r.models, func(m proxy.Model) bool { return m.Name == n })
}

// listProviders reads the providers.
func (r *run) listProviders(ctx context.Context) error {
	var reply proxy.ProvidersReply
	if err := r.px.Call(ctx, &reply, "providers list", nil); err != nil {
		return err
	}
	r.providers = reply.Providers
	return nil
}

// listTargets reads the targets.
func (r *run) listTargets(ctx context.Context) error {
	var reply proxy.TargetsReply
	if err := r.px.Call(ctx, &reply, "targets list", nil); err != nil {
		return err
	}
	r.targets = reply.Targets
	return nil
}

// provider is the listed provider named n.
func (r *run) provider(n string) (proxy.Provider, bool) {
	return find(r.providers, func(p proxy.Provider) bool { return p.Name == n })
}

// pickProvider picks one of the providers listed, after q's own options;
// an option of q with an empty value gives the zero Provider. With canAdd,
// a last option runs Add providers nested, and a name from the flags that is
// not listed runs it with the same answers; either way the providers are
// then listed again.
func (r *run) pickProvider(ctx context.Context, q ui.Question, canAdd bool) (proxy.Provider, error) {
	if canAdd && r.u.Given(q) {
		n, err := r.u.Text(ctx, q)
		if err != nil {
			return proxy.Provider{}, err
		}
		if p, ok := r.provider(n); ok {
			say(ctx, r.u, "provider "+n+" exists; kept")
			return p, nil
		}
		return r.addProvider(ctx, n)
	}
	own := q.Options
	for {
		q.Options = slices.Clone(own)
		for _, p := range r.providers {
			label := p.Name
			if p.Credential != "resolved" && p.Credential != "none" {
				label += ", credential " + p.Credential
			}
			q.Options = append(q.Options, ui.Option{Value: p.Name, Label: label})
		}
		if canAdd {
			q.Options = append(q.Options, ui.Option{Value: "+add", Label: "add a provider"})
		}
		n, err := r.u.Pick(ctx, q)
		switch {
		case err != nil:
			return proxy.Provider{}, err
		case n != "+add":
			p, _ := r.provider(n)
			return p, nil
		}
		if _, err := r.addProvider(ctx, ""); err != nil {
			return proxy.Provider{}, err
		}
	}
}

// addProvider runs Add providers nested, says its result and lists the
// providers again; it returns provider n, which must then be listed, or
// the zero Provider when n is "".
func (r *run) addProvider(ctx context.Context, n string) (proxy.Provider, error) {
	res, err := r.u.Run(ctx, addSteps(), func(ctx context.Context) (string, error) { return AddProviders(ctx, r.u, r.px, r.h) })
	if err != nil {
		return proxy.Provider{}, err
	}
	say(ctx, r.u, res)
	if err := r.listProviders(ctx); err != nil || n == "" {
		return proxy.Provider{}, err
	}
	p, ok := r.provider(n)
	if !ok {
		return proxy.Provider{}, fmt.Errorf("provider %s is not listed after Add providers", n)
	}
	return p, nil
}

// pickModel picks one of p's model ids, grouped by vendor and family, from
// its model list; q carries the ID, flag and step. The id is typed instead
// when the flags give it, without a terminal, or when the list cannot be
// read, which is said in one line.
func (r *run) pickModel(ctx context.Context, q ui.Question, p proxy.Provider) (string, error) {
	typed := func() (string, error) {
		return r.u.Text(ctx, ui.Question{ID: q.ID, Flag: q.Flag, Step: q.Step, Prompt: "the model id at " + p.Name})
	}
	if r.u.Given(q) || !r.u.Asks() {
		return typed()
	}
	ids, err := r.readModels(ctx, q.Step, p)
	if err != nil {
		return "", err
	}
	if ids == nil {
		return typed()
	}
	all := false
	for {
		models, hidden := families.Sort(ids, all)
		opts := make([]ui.Option, 0, len(models)+2)
		for _, m := range models {
			group := m.Vendor + " · " + m.Family
			if m.Vendor == "Other" {
				group = "Other"
			}
			opts = append(opts, ui.Option{Value: m.ID, Label: m.ID, Group: group})
		}
		if hidden > 0 {
			opts = append(opts, ui.Option{Value: "+all", Label: fmt.Sprintf("show every model (%d not chat)", hidden)})
		}
		opts = append(opts, ui.Option{Value: "+type", Label: "type a model id"})
		id, err := r.u.Pick(ctx, ui.Question{ID: q.ID, Flag: q.Flag, Step: q.Step, Prompt: "Which model of " + p.Name + "?", Options: opts})
		switch {
		case err != nil:
			return "", err
		case id == "+all":
			all = true
		case id == "+type":
			return typed()
		default:
			return id, nil
		}
	}
}

// readModels reads p's model list under a work piece, which Esc stops, with
// p's key from .env when it has one there. A list that cannot be read is
// said in one line and gives no ids and no error.
func (r *run) readModels(ctx context.Context, step string, p proxy.Provider) ([]string, error) {
	list := host.Provider{Protocol: p.Protocol, BaseURL: p.BaseURL, AllowPrivate: p.AllowPrivateNetwork, Limit: modelListLimit}
	if p.APIKeyEnv != "" {
		key, inEnv, err := r.h.EnvGet(p.APIKeyEnv)
		if err != nil {
			return nil, err
		}
		if !inEnv {
			say(ctx, r.u, p.APIKeyEnv+" is not in .env: the list is read without a key")
		}
		list.Key = key
	}
	ids, status, listErr, err := listUnder(ctx, r.u, r.h, ui.Question{ID: "models", Step: step, Prompt: "reading the models of " + p.Name}, list)
	switch {
	case err != nil:
		return nil, err
	case listErr != nil:
		say(ctx, r.u, listErr.Error()+"; type the model id")
		return nil, nil
	case status/100 != 2:
		say(ctx, r.u, fmt.Sprintf("the model list at %s answered %d; type the model id", list.URL(), status))
		return nil, nil
	}
	return ids, nil
}

// pickFallbacks is the fallback targets, in order: from the flag fallback,
// each of a provider listed, or asked at a terminal, two at most, each of a
// provider already added (a nested Add providers would read the
// subcommand's own --provider).
func (r *run) pickFallbacks(ctx context.Context, step string) ([]string, error) {
	q := ui.Question{ID: "fallback", Flag: "fallback", Step: step, Default: "", HasDefault: true}
	if r.u.Given(q) {
		given, err := r.u.Text(ctx, q)
		if err != nil {
			return nil, err
		}
		items := strings.Split(given, ",")
		for _, it := range items {
			n, model, _ := strings.Cut(it, "/")
			if _, ok := r.provider(n); !ok || model == "" {
				return nil, &ui.UsageError{Msg: "--fallback: " + it + " is not <provider>/<model> of a provider added here; ./setup.sh provider adds one"}
			}
		}
		return items, nil
	}
	if !r.u.Asks() {
		return nil, nil
	}
	var out []string
	for i := 1; i <= 2; i++ {
		p, err := r.pickProvider(ctx, ui.Question{ID: "fallback-provider", Step: step, Prompt: fmt.Sprintf("Fallback %d of 2: from which provider?", i),
			Options: []ui.Option{{Value: "", Label: "no more fallbacks"}}}, false)
		if err != nil {
			return nil, err
		}
		if p.Name == "" {
			break
		}
		model, err := r.pickModel(ctx, ui.Question{ID: "fallback-model", Step: step}, p)
		if err != nil {
			return nil, err
		}
		out = append(out, p.Name+"/"+model)
	}
	return out, nil
}

// inAll is the paid question of a task's own check: the calls it states.
func inAll(_, total int) string { return fmt.Sprintf("%d paid call(s) in all", total) }

// calls is how many paid calls t's check makes: one per declared level, one
// more at none when it can disable reasoning, or one when it declares
// neither (design section 3, Paid count).
func calls(t proxy.Target) int {
	n := 0
	if t.Reasoning != nil {
		n = len(t.Reasoning.Levels)
		if t.Reasoning.CanDisable != nil && *t.Reasoning.CanDisable {
			n++
		}
	}
	return max(n, 1)
}

// bound is how long the check of target, n calls, may take: n × (total +
// 5 s) + 10 s, total the provider's timeouts.total_ms, else settings show's,
// so the server's own bound, n × (total + 5 s), ends first.
func (r *run) bound(ctx context.Context, target string, n int) (time.Duration, error) {
	name, _, _ := strings.Cut(target, "/")
	p, _ := r.provider(name)
	total := p.Timeouts.TotalMS
	if total == 0 && r.defaultMS == 0 {
		var s proxy.SettingsReply
		if err := r.px.Call(ctx, &s, "settings show", nil); err != nil {
			return 0, err
		}
		r.defaultMS = s.Timeouts.TotalMS
	}
	if total == 0 {
		total = r.defaultMS
	}
	return time.Duration(n)*(time.Duration(total)*time.Millisecond+cmp.Or(r.slack, callSlack)) + cmp.Or(r.margin, checkMargin), nil
}

// check checks ids, in order, through one paid step, skipping those
// verified unless --reverify; false is "Not now". prompt builds the paid
// question from the targets left to check and their calls, the count that
// opens the budget. The first failure ends the check; Esc or Ctrl-C stops
// it at the server.
func (r *run) check(ctx context.Context, step string, prompt func(k, total int) string, ids []string) (bool, error) {
	left, err := r.toCheck(ctx, ids)
	if err != nil || len(left) == 0 {
		return err == nil, err
	}
	total := 0
	for _, t := range left {
		name, _, _ := strings.Cut(t.ID, "/")
		say(ctx, r.u, fmt.Sprintf("check %s: %d paid call(s) to %s, each capped at 256 output tokens, never retried", t.ID, calls(t), name))
		total += calls(t)
	}
	say(ctx, r.u, "Esc or Ctrl-C stops the check at the server: no further call starts, and a call already sent may still be billed")
	b, err := r.u.Paid(ctx, ui.Question{ID: "check", Step: step, Prompt: prompt(len(left), total)}, total)
	if err != nil || b == nil {
		return false, err
	}
	bounds := make([]time.Duration, len(left))
	names := make([]string, len(left))
	for i, t := range left {
		if bounds[i], err = r.bound(ctx, t.ID, calls(t)); err != nil {
			return false, err
		}
		names[i] = t.ID
	}
	var failed error
	err = r.u.Work(ctx, ui.Question{ID: "checking", Step: step, Prompt: "checking " + strings.Join(names, ", ")},
		func(ctx context.Context) error {
			for i, t := range left {
				var v *proxy.VerifyReply
				if v, failed = r.px.Verify(ctx, b, t.ID, calls(t), bounds[i]); failed == nil && !v.Stored {
					failed = fmt.Errorf("target %s answered, but it or its provider changed during the check, so the result was not stored; run this again", t.ID)
				}
				if failed != nil {
					return failed
				}
				say(ctx, r.u, "target "+t.ID+" is checked")
				r.verifiedAt(t.ID, time.Now().UnixMilli())
			}
			return nil
		})
	if (errors.Is(err, ui.ErrLeave) || errors.Is(err, ui.ErrQuit)) && failed != nil {
		say(ctx, r.u, failed.Error())
	}
	return err == nil, err
}

// toCheck is the targets of ids to check, as listed: each verified one is
// said and left out unless --reverify, which only flags mode names. A target left whose provider's
// credential does not resolve stops the check before its paid step.
func (r *run) toCheck(ctx context.Context, ids []string) ([]proxy.Target, error) {
	reverify, hint := r.u.Given(ui.Question{ID: "reverify"}), ""
	if !r.u.Asks() { // flags mode: the flag is named; at a terminal, never
		hint = " (--reverify asks for it)"
	}
	var left []proxy.Target
	for _, id := range ids {
		i := slices.IndexFunc(r.targets, func(t proxy.Target) bool { return t.ID == id })
		switch {
		case i < 0:
			return nil, fmt.Errorf("target %s is not listed, so its paid calls cannot be counted", id)
		case r.targets[i].VerifiedAt > 0 && !reverify:
			say(ctx, r.u, "target "+id+" is verified; not checked again"+hint)
		default:
			left = append(left, r.targets[i])
		}
	}
	for _, t := range left {
		name, _, _ := strings.Cut(t.ID, "/")
		if p, ok := r.provider(name); ok && p.Credential == "unresolved" {
			return nil, fmt.Errorf("the credential of %s does not resolve: %s is not in the container's environment", name, p.APIKeyEnv)
		}
	}
	return left, nil
}

// verifiedAt notes in the targets read when id's check last passed: now,
// or 0 once a change cleared it.
func (r *run) verifiedAt(id string, at int64) {
	for i := range r.targets {
		if r.targets[i].ID == id {
			r.targets[i].VerifiedAt = at
		}
	}
}

// kept says, when err is not nil, what the run changed, which stays; it
// returns err.
func (r *run) kept(ctx context.Context, err error) error {
	if err != nil {
		r.sayKept(ctx)
	}
	return err
}

// sayKept says what the run changed, if anything: it stays.
func (r *run) sayKept(ctx context.Context) {
	if len(r.made) > 0 {
		say(ctx, r.u, "already changed and kept: "+strings.Join(r.made, "; "))
	}
}

// aliasPlan is an alias as a run would make or change it: its whole
// declaration, its targets in order, primary first, the targets the run
// adds, and the output cap and effort the run sets ("" for none).
type aliasPlan struct {
	name        string
	decl        proxy.AliasDecl
	targets     []string
	added       []string
	cap, effort string
}

// newPlan is the plan of alias n toward targets, from decl, adding those of
// targets that are not listed.
func (r *run) newPlan(n string, decl proxy.AliasDecl, targets []string) aliasPlan {
	a := aliasPlan{name: n, decl: decl.With("targets", targets), targets: targets}
	for _, t := range targets {
		if _, listed := find(r.targets, func(l proxy.Target) bool { return l.ID == t }); !listed {
			a.added = append(a.added, t)
		}
	}
	return a
}

// setCap sets the alias's output cap, c a positive integer, as the number it
// is: the CLI refuses "0100" or "+100", which the dry run judges as 100.
func (a *aliasPlan) setCap(c string) {
	n, _ := strconv.Atoi(c)
	a.cap, a.decl = strconv.Itoa(n), a.decl.With("max_output_tokens", n)
}

// setEffort sets the alias's default effort, e not empty.
func (a *aliasPlan) setEffort(e string) {
	a.effort, a.decl = e, a.decl.With("reasoning", map[string]string{"effort": e, "mode": "default"})
}

// flags is the alias's targets, and the cap and effort the run sets, as the
// flags of models add or models set.
func (a *aliasPlan) flags() []string {
	var f []string
	for _, t := range a.targets {
		f = append(f, "--target", t)
	}
	if a.cap != "" {
		f = append(f, "--max-output-tokens", a.cap)
	}
	if a.effort != "" {
		f = append(f, "--effort", a.effort, "--effort-mode", "default")
	}
	return f
}

// resources is the alias and its targets, as findings name them.
func (a *aliasPlan) resources() []string {
	out := []string{"models/" + a.name}
	for _, t := range a.targets {
		out = append(out, "targets/"+t)
	}
	return out
}

// settle asks what the proxy says the plan needs, the prices of a target a
// budget reaches, an output cap, an effort, through apply --dry-run, then
// adds the targets the plan adds; all before any paid call. A finding the
// console does not ask about stops it before anything is changed.
func (r *run) settle(ctx context.Context, step string, a *aliasPlan) error {
	findings, err := r.px.DryRun(ctx, newTargets(a.added, nil), a.name, a.decl)
	if err != nil {
		return err
	}
	if err := refusal("this alias", findings, a.asked); err != nil {
		return err
	}
	today := time.Now().Format(time.DateOnly)
	prices, asked, err := r.needs(ctx, step, a, findings, today)
	if err != nil {
		return err
	}
	if asked {
		if findings, err = r.px.DryRun(ctx, newTargets(a.added, prices), a.name, a.decl); err != nil {
			return err
		}
		// A listed target's prices cannot ride in the document without
		// replacing its declaration: the dry run after targets set judges it.
		repriced := func(f proxy.Finding) bool {
			t := strings.TrimPrefix(f.Resource, "targets/")
			return f.Code == "unpriced_under_budget" && prices[t] != nil && !slices.Contains(a.added, t)
		}
		if err := refusal("this alias", findings, repriced); err != nil {
			return err
		}
	}
	repriced, err := r.addTargets(ctx, a, prices)
	if err != nil || !repriced {
		return err
	}
	if findings, err = r.px.DryRun(ctx, nil, a.name, a.decl); err != nil {
		return err
	}
	return refusal("this alias", findings, nil)
}

// asked reports whether f is a finding settle asks about: a price, the
// output cap or an effort of the alias.
func (a *aliasPlan) asked(f proxy.Finding) bool {
	switch f.Code {
	case "output_cap_required", "effort_required":
		return true
	case "unpriced_under_budget":
		return slices.Contains(a.targets, strings.TrimPrefix(f.Resource, "targets/"))
	}
	return false
}

// newTargets is ids as new chat targets, each with its prices, if any.
func newTargets(ids []string, prices map[string]*proxy.Pricing) []proxy.NewTarget {
	out := make([]proxy.NewTarget, len(ids))
	for i, id := range ids {
		out[i] = proxy.NewTarget{ID: id, Pricing: prices[id]}
	}
	return out
}

// refusal is the proxy's refusal of what, one line per error finding that
// pass does not let through, or nil when there is none.
func refusal(what string, findings []proxy.Finding, pass func(proxy.Finding) bool) error {
	var lines []string
	for _, f := range findings {
		if f.Severity == "error" && (pass == nil || !pass(f)) {
			lines = append(lines, ui.OneLine(f.Resource)+": "+ui.OneLine(f.Message))
		}
	}
	if len(lines) == 0 {
		return nil
	}
	return errors.New("the proxy would refuse " + what + ":\n" + strings.Join(lines, "\n"))
}

// needs asks what the findings say the plan needs: the prices of each of its
// targets a budget reaches without them, in the plan's order, then an output
// cap, then an effort. It reports whether it asked anything.
func (r *run) needs(ctx context.Context, step string, a *aliasPlan, findings []proxy.Finding, today string) (map[string]*proxy.Pricing, bool, error) {
	has := func(code, resource string) (proxy.Finding, bool) {
		return find(findings, func(f proxy.Finding) bool {
			return f.Severity == "error" && f.Code == code && (resource == "" || f.Resource == resource)
		})
	}
	prices := map[string]*proxy.Pricing{}
	for _, t := range a.targets {
		if _, ok := has("unpriced_under_budget", "targets/"+t); !ok {
			continue
		}
		p, err := r.askPrices(ctx, step, t, today)
		if err != nil {
			return nil, false, err
		}
		prices[t] = p
	}
	asked := len(prices) > 0
	if _, ok := has("output_cap_required", ""); ok && a.cap == "" {
		c, err := r.u.Text(ctx, ui.Question{ID: "max-output-tokens", Flag: "max-output-tokens", Step: step,
			Prompt: "the alias's output cap in tokens, which an anthropic target needs", Default: "8192", HasDefault: true, Check: positive})
		if err != nil {
			return nil, false, err
		}
		a.setCap(c)
		asked = true
	}
	if f, ok := has("effort_required", ""); ok && a.effort == "" {
		// The finding is the alias's; its message names the target.
		t, _, _ := strings.Cut(strings.TrimPrefix(f.Message, "target "), " ")
		e, err := r.u.Text(ctx, ui.Question{ID: "effort", Flag: "effort", Step: step,
			Prompt: t + " always reasons: a default reasoning effort, such as low, medium or high", Check: nonEmpty})
		if err != nil {
			return nil, false, err
		}
		a.setEffort(e)
		asked = true
	}
	return prices, asked, nil
}

// askPrices says that a budget reaches target t without prices, then asks
// them, as of today.
func (r *run) askPrices(ctx context.Context, step, t, today string) (*proxy.Pricing, error) {
	say(ctx, r.u, t+" has no prices and a budget reaches it: its prices are asked")
	in, err := r.price(ctx, step, t, "input")
	if err != nil {
		return nil, err
	}
	out, err := r.price(ctx, step, t, "output")
	if err != nil {
		return nil, err
	}
	return &proxy.Pricing{In: in, Out: out, AsOf: today}, nil
}

// price asks t's price per million side ("input" or "output") tokens; the
// flags answer every target that needs one.
func (r *run) price(ctx context.Context, step, t, side string) (float64, error) {
	a, err := r.u.Text(ctx, ui.Question{ID: side + "-price", Flag: side + "-price", Step: step,
		Prompt: "the price of " + t + " in USD per million " + side + " tokens", Check: isUSD})
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(a, 64)
}

// addTargets adds the plan's new targets, each with its prices, if any, and
// sets the prices asked of a listed target, which clears its verification.
// It reports whether it set a listed target's prices.
func (r *run) addTargets(ctx context.Context, a *aliasPlan, prices map[string]*proxy.Pricing) (bool, error) {
	for _, t := range a.added {
		flags, line := []string{"--kind", "chat"}, "adding target "+t
		if p := prices[t]; p != nil {
			flags, line = append(flags, pricingFlags(p)...), line+" with prices "+pricedText(p)
		}
		say(ctx, r.u, line)
		var change proxy.TargetChange
		if err := r.px.Call(ctx, &change, "targets add", flags, t); err != nil {
			return false, err
		}
		r.made = append(r.made, "target "+t+" added")
		r.targets = append(r.targets, change.Resource)
	}
	repriced := false
	for _, t := range a.targets {
		p := prices[t]
		if p == nil || slices.Contains(a.added, t) {
			continue
		}
		if err := r.setPrices(ctx, t, p); err != nil {
			return false, err
		}
		repriced = true
	}
	return repriced, nil
}

// setPrices sets listed target t's prices, which clears its verification.
func (r *run) setPrices(ctx context.Context, t string, p *proxy.Pricing) error {
	say(ctx, r.u, "setting the prices of "+t+" to "+pricedText(p)+", which clears its verification")
	var change proxy.Change
	if err := r.px.Call(ctx, &change, "targets set", pricingFlags(p), t); err != nil {
		return err
	}
	r.made = append(r.made, "prices of "+t+" set")
	r.verifiedAt(t, 0)
	return nil
}

// pricedText is prices p in words.
func pricedText(p *proxy.Pricing) string {
	return fmt.Sprintf("%s and %s USD per million input and output tokens, as of %s", usdText(p.In), usdText(p.Out), p.AsOf)
}

// pricingFlags is prices p as the flag of targets add or targets set.
func pricingFlags(p *proxy.Pricing) []string {
	return []string{"--pricing", "input_per_million=" + usdText(p.In) + ",output_per_million=" + usdText(p.Out) + ",as_of=" + p.AsOf}
}

// usdText is a price as typed: no trailing zeros.
func usdText(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// explain says what alias n sends toward each target: the effort and output
// cap, or why the target is skipped.
func (r *run) explain(ctx context.Context, n string) error {
	var reply proxy.ExplainReply
	if err := r.px.Call(ctx, &reply, "models explain", nil, n); err != nil {
		return err
	}
	for _, t := range reply.Targets {
		if t.Skipped != nil {
			say(ctx, r.u, t.Target+": skipped ("+t.Skipped.Code+")")
			continue
		}
		effort, limit := "none", "none"
		if t.Effort.Sent != nil {
			effort = *t.Effort.Sent
		}
		if t.MaxOutput.Sent != nil {
			limit = strconv.FormatInt(*t.MaxOutput.Sent, 10)
		}
		say(ctx, r.u, t.Target+": sends effort "+effort+", output cap "+limit)
	}
	return nil
}

// warn says the findings on the given resources, but those held until the
// check when checked: it has passed by now.
func (r *run) warn(ctx context.Context, findings []proxy.Finding, checked bool, resources ...string) {
	for _, f := range findings {
		if w, ok := rules.Lookup(f.Code); slices.Contains(resources, f.Resource) && (!checked || !ok || w.Held != "check") {
			say(ctx, r.u, rules.Plain(f.Code, f.Resource, f.Message))
		}
	}
}

// nonEmpty refuses an empty answer.
func nonEmpty(a string) error {
	if a == "" {
		return errors.New("an answer is needed")
	}
	return nil
}

// positive refuses an answer that is not a whole number above 0.
func positive(a string) error {
	if n, err := strconv.Atoi(a); err != nil || n <= 0 {
		return errors.New("a whole number above 0 is needed")
	}
	return nil
}

// positiveOrEmpty is positive, or empty for none.
func positiveOrEmpty(a string) error {
	if a == "" {
		return nil
	}
	return positive(a)
}

// isUSD refuses an answer that is not a number of USD, 0 or more.
func isUSD(a string) error {
	if isLimit(a) != nil {
		return errors.New("--input-price and --output-price take a number of USD")
	}
	return nil
}

// isLimit refuses an answer that is not a number of USD, 0 or more and
// finite; the proxy judges the rest.
func isLimit(a string) error {
	if v, err := strconv.ParseFloat(a, 64); err != nil || !(v >= 0) || math.IsInf(v, 1) {
		return errors.New("a number of USD is needed")
	}
	return nil
}
