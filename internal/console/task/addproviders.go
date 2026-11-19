package task

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/rules"
	"github.com/jorlaru/stargaite/internal/console/ui"
)

const (
	// another is the pick's option for a service that is no preset.
	another = "+another"
	// recreates ends the first change the reader agrees to, as setup.sh's
	// agree says it.
	recreates = ", and the container is then recreated to read it: requests in flight are given time to finish, and the rate-limit windows restart"
)

// addSteps is Add providers' step table.
func addSteps() []ui.Step {
	return []ui.Step{{ID: "which", Title: "which"}, {ID: "add", Title: "add"}, {ID: "key", Title: "key"},
		{ID: "write", Title: "write"}, {ID: "recreate", Title: "recreate"}}
}

// keyCheckPath is where a key is checked for free, after the base URL, or ""
// for the model list. OpenRouter's model list answers without a key, so a
// key for OpenRouter's host, whatever the provider is named, is checked at
// /key, which states the key's limits.
func keyCheckPath(baseURL string) string {
	if u, err := url.Parse(baseURL); err == nil && strings.EqualFold(u.Hostname(), "openrouter.ai") {
		return "/key"
	}
	return ""
}

// picked is one provider of the run: as listed or as added, its findings,
// and what became of its key, in the result's words (never the key).
type picked struct {
	name     string
	res      proxy.Provider
	added    bool
	findings []proxy.Finding
	outcome  string
}

// adding is one run of Add providers.
type adding struct {
	u       *ui.UI
	px      *proxy.Proxy
	h       *host.Host
	presets []proxy.Preset
	listed  []proxy.Provider
	agreed  bool // the reader agreed to the recreate: set by confirm only
}

// envWrite is a key for .env and the provider it was asked for.
type envWrite struct{ name, value, by string }

// AddProviders adds the providers the reader picks, and their keys. Every
// question is answered, and each key checked with one free call, before
// anything is written: so a usage error or a No leaves every file as it
// was. Then the keys go to .env, the override gets the line a service on
// the host needs, and the container is recreated once to read them.
func AddProviders(ctx context.Context, u *ui.UI, px *proxy.Proxy, h *host.Host) (string, error) {
	r := &adding{u: u, px: px, h: h}
	if err := r.start(ctx); err != nil {
		return "", err
	}
	names, err := r.pick(ctx)
	if err != nil {
		return "", err
	}
	var chosen []*picked
	var writes []envWrite // the keys for .env, written once every question is answered
	for _, n := range names {
		p, err := r.provider(ctx, n)
		if err != nil {
			return "", err
		}
		var w *envWrite
		if p.outcome, w, err = r.key(ctx, p, writes); err != nil {
			return "", err
		}
		if w != nil {
			writes = append(writes, *w)
		}
		chosen = append(chosen, p)
	}
	wrote, err := r.write(ctx, chosen, writes)
	if err != nil {
		return "", err
	}
	if err := r.recreate(ctx, chosen, wrote); err != nil {
		return "", err
	}
	if err := r.resolves(ctx, chosen); err != nil {
		return "", err
	}
	r.warn(ctx, chosen)
	parts := make([]string, len(chosen))
	for i, p := range chosen {
		verb := " kept, "
		if p.added {
			verb = " added, "
		}
		parts[i] = p.name + verb + p.outcome
	}
	return "Done: " + strings.Join(parts, "; ") + ".", nil
}

// start checks the container runs and is ready, and reads the presets and
// the providers.
func (r *adding) start(ctx context.Context) error {
	if err := ready(ctx, r.u, r.h, "which"); err != nil {
		return err
	}
	var presets proxy.PresetsReply
	var listed proxy.ProvidersReply
	if err := r.px.Call(ctx, &presets, "providers presets", nil); err != nil {
		return err
	}
	if err := r.px.Call(ctx, &listed, "providers list", nil); err != nil {
		return err
	}
	r.presets, r.listed = presets.Presets, listed.Providers
	return nil
}

// pick asks for the providers: presets, providers already added, or one new
// service. A flag that answers one provider is refused with several, and so
// is a new service among several from the flags.
func (r *adding) pick(ctx context.Context) ([]string, error) {
	q := ui.Question{ID: "provider", Flag: "provider", Step: "which", Prompt: "Which providers do you want to add?", Other: another}
	for _, p := range r.presets {
		label := p.Name
		if p.Untested {
			label += ", untested"
		}
		if p.BaseURL == "" {
			label += ", asks for its URL"
		}
		if _, ok := r.listedAs(p.Name); ok {
			label += "  ✓ added"
		}
		q.Options = append(q.Options, ui.Option{Value: p.Name, Label: label})
	}
	for _, p := range r.listed {
		if _, ok := r.presetAs(p.Name); !ok {
			q.Options = append(q.Options, ui.Option{Value: p.Name, Label: p.Name + "  ✓ added"})
		}
	}
	q.Options = append(q.Options, ui.Option{Value: another, Label: "another OpenAI- or Anthropic-compatible service"})
	names, err := r.u.PickMany(ctx, q)
	if err != nil {
		return nil, err
	}
	names = distinct(names)
	if len(names) > 1 {
		for _, f := range []string{"base-url", "protocol", "api-key-env", "allow-private-network"} {
			if r.u.Given(ui.Question{ID: f}) {
				return nil, &ui.UsageError{Msg: fmt.Sprintf("--%s answers one provider: add %s one at a time", f, strings.Join(names, ", "))}
			}
		}
		for _, n := range names {
			_, listed := r.listedAs(n)
			if _, preset := r.presetAs(n); r.u.Given(q) && !listed && !preset {
				return nil, &ui.UsageError{Msg: "--provider: several providers at once are presets or providers already added; add " + n + " on its own"}
			}
		}
	}
	if i := slices.Index(names, another); i >= 0 {
		if names[i], err = r.u.Text(ctx, ui.Question{ID: "provider-name", Flag: "provider", Step: "add", Prompt: "a name for the service"}); err != nil {
			return nil, err
		}
	}
	return distinct(names), nil
}

// distinct is names without repeats, in the order first given: a provider
// named twice is added once.
func distinct(names []string) []string {
	var out []string
	for _, n := range names {
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// provider is n as listed, or adds it, from its preset or as another
// compatible service, with the questions each asks.
func (r *adding) provider(ctx context.Context, n string) (*picked, error) {
	if p, ok := r.listedAs(n); ok {
		say(ctx, r.u, "provider "+n+" exists; kept")
		return &picked{name: n, res: p}, nil
	}
	url, flags, err := r.describe(ctx, n)
	if err != nil {
		return nil, err
	}
	url, private, err := r.reach(ctx, n, url)
	if err != nil {
		return nil, err
	}
	if private {
		flags = append(flags, "--allow-private-network")
	}
	say(ctx, r.u, "adding provider "+n)
	var change proxy.ProviderChange
	if err := r.px.Call(ctx, &change, "providers add", append([]string{"--base-url", url}, flags...), n); err != nil {
		return nil, err
	}
	p := &picked{name: n, res: change.Resource, added: true}
	for _, f := range change.Findings {
		if f.Resource == "providers/"+n {
			p.findings = append(p.findings, f)
		}
	}
	return p, nil
}

// describe is n's base URL and the flags that declare it: a preset's own URL
// unless it has none or the flags give one; for another service, its
// protocol, URL and credential variable, asked.
func (r *adding) describe(ctx context.Context, n string) (string, []string, error) {
	urlQ := ui.Question{ID: "base-url", Flag: "base-url", Step: "add", Prompt: "its base URL"}
	if p, ok := r.presetAs(n); ok {
		if p.BaseURL != "" && !r.u.Given(urlQ) {
			return p.BaseURL, nil, nil
		}
		urlQ.Prompt = "the base URL of " + n
		url, err := r.u.Text(ctx, urlQ)
		return url, nil, err
	}
	say(ctx, r.u, n+" is not a preset: another OpenAI- or Anthropic-compatible service")
	protocol, err := r.u.Pick(ctx, ui.Question{ID: "protocol", Flag: "protocol", Step: "add", Prompt: "its protocol",
		Options: []ui.Option{{Value: "openai", Label: "openai"}, {Value: "anthropic", Label: "anthropic"}}, Default: "openai", HasDefault: true})
	if err != nil {
		return "", nil, err
	}
	url, err := r.u.Text(ctx, urlQ)
	if err != nil {
		return "", nil, err
	}
	variable, err := r.u.Text(ctx, ui.Question{ID: "api-key-env", Flag: "api-key-env", Step: "add",
		Prompt: "the variable for its credential, or none", Default: variableOf(n), HasDefault: true})
	if err != nil {
		return "", nil, err
	}
	if variable == "none" {
		return url, []string{"--protocol", protocol, "--billing", "none"}, nil
	}
	return url, []string{"--api-key-env", variable, "--protocol", protocol, "--billing", "pay_per_use"}, nil
}

// variableOf is the variable a new service's credential is offered in, as
// setup.sh names it: the name upper-cased, every other rune "_", then
// _API_KEY.
func variableOf(n string) string {
	return strings.Map(func(c rune) rune {
		if (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			return c
		}
		return '_'
	}, strings.ToUpper(n)) + "_API_KEY"
}

// reach is url as the container reaches it, and whether the provider must
// allow private networks: a URL on the host's loopback moves to the host's
// bridge address, which is private; for plain HTTP the reader is asked.
func (r *adding) reach(ctx context.Context, n, url string) (string, bool, error) {
	if moved, ok := host.ToContainer(url); ok {
		say(ctx, r.u, "localhost inside the container is the container: "+n+" is reached at "+moved+
			", the host's bridge address, so it must listen beyond the host's loopback (Ollama binds 127.0.0.1 unless OLLAMA_HOST=0.0.0.0)")
		return moved, true, nil
	}
	q := ui.Question{ID: "allow-private-network", Flag: "allow-private-network", Step: "add",
		Prompt: url + " is plain HTTP or a private address: allow it?", Default: "no", HasDefault: true}
	if !strings.HasPrefix(url, "http://") && !r.u.Given(q) {
		return url, false, nil
	}
	allow, err := r.u.Confirm(ctx, q)
	return url, allow, err
}

// key is what becomes of p's key, in the result's words, and the write it
// needs, if any: none without a variable; kept when an earlier write of the
// run has it, or when it is in .env and not to be replaced; otherwise asked
// and checked, and returned for the write.
func (r *adding) key(ctx context.Context, p *picked, writes []envWrite) (string, *envWrite, error) {
	v := p.res.APIKeyEnv
	if v == "" {
		return "no key", nil, nil
	}
	if i := slices.IndexFunc(writes, func(w envWrite) bool { return w.name == v }); i >= 0 {
		say(ctx, r.u, v+" is asked once, for "+writes[i].by+": "+p.name+" uses the same key")
		return "key kept in .env", nil, nil
	}
	_, inEnv, err := r.h.EnvGet(v)
	if err != nil {
		return "", nil, err
	}
	if inEnv {
		replace, err := r.u.Confirm(ctx, ui.Question{ID: "replace", Flag: "replace", Step: "key",
			Prompt: v + " is in .env: replace it?", Default: "no", HasDefault: true})
		if err != nil {
			return "", nil, err
		}
		if !replace {
			say(ctx, r.u, v+" is in .env; kept")
			return "key kept in .env", nil, nil
		}
	}
	key, checked, err := checkKey(ctx, r.u, r.h, "key", p.res)
	if err != nil {
		return "", nil, err
	}
	return checked, &envWrite{name: v, value: key, by: p.name}, nil
}

// write agrees every change, then makes them: the keys to .env, then the
// host-gateway line to the override when a provider is on the host's bridge
// address. It reports whether it wrote anything.
func (r *adding) write(ctx context.Context, run []*picked, writes []envWrite) (bool, error) {
	o, gateway, err := r.gateway(run)
	if err != nil {
		return false, err
	}
	for _, w := range writes {
		if err := r.agree(ctx, "writing "+w.name+" to .env"); err != nil {
			return false, err
		}
	}
	switch {
	case gateway && o.Exists():
		err = r.consent(ctx, "write", `compose.override.yaml is rewritten with host_gateway=1, keeping its other "# setup:" lines`+recreates)
	case gateway:
		err = r.agree(ctx, "writing compose.override.yaml with host_gateway=1")
	}
	if err != nil {
		return false, err
	}
	for _, w := range writes {
		if err := r.h.EnvSet(w.name, w.value); err != nil {
			return false, err
		}
	}
	if gateway {
		o.Set("host_gateway", "1")
		if err := r.h.WriteOverride(o); err != nil {
			return false, err
		}
	}
	return len(writes) > 0 || gateway, nil
}

// gateway reads the override when a provider is on host.docker.internal,
// and says whether it needs the host-gateway line. An override the reader
// wrote is never rewritten: adding the line is then theirs to do.
func (r *adding) gateway(run []*picked) (*host.Override, bool, error) {
	if !slices.ContainsFunc(run, func(p *picked) bool { return strings.Contains(p.res.BaseURL, "//host.docker.internal") }) {
		return nil, false, nil
	}
	o, err := r.h.ReadOverride()
	switch {
	case err != nil:
		return nil, false, err
	case o.Exists() && !o.Marked() && o.Holds("host.docker.internal:host-gateway"):
		return o, false, nil
	case o.Exists() && !o.Marked():
		return nil, false, errors.New(`compose.override.yaml was not written by setup.sh, so it is left as it is: ` +
			`add "host.docker.internal:host-gateway" to the extra_hosts of its stargaite service yourself, or move it away and run this again`)
	}
	return o, o.Get("host_gateway") != "1", nil
}

// recreate recreates the container once, when a file was written or a
// provider's key is in .env but not yet in the container.
func (r *adding) recreate(ctx context.Context, run []*picked, wrote bool) error {
	var unread []string
	for _, p := range run {
		v := p.res.APIKeyEnv
		if v == "" || p.res.Credential != "unresolved" || slices.Contains(unread, v) {
			continue
		}
		_, inEnv, err := r.h.EnvGet(v)
		if err != nil {
			return err
		}
		if inEnv {
			unread = append(unread, v)
		}
	}
	if !wrote && len(unread) == 0 {
		return nil
	}
	if !r.agreed {
		if err := r.consent(ctx, "recreate", "the container is recreated to read "+strings.Join(unread, ", ")+
			": requests in flight are given time to finish, and the rate-limit windows restart"); err != nil {
			return err
		}
	}
	return recreate(ctx, r.u, r.h, "recreate", wrote)
}

// resolves reads each provider back: its credential must now resolve.
func (r *adding) resolves(ctx context.Context, run []*picked) error {
	for _, p := range run {
		if err := resolved(ctx, r.px, p.name, p.res.APIKeyEnv); err != nil {
			return err
		}
	}
	return nil
}

// warn says the findings of the providers added, but those held until the
// recreate: every credential resolves by now.
func (r *adding) warn(ctx context.Context, run []*picked) {
	for _, p := range run {
		for _, f := range p.findings {
			if w, ok := rules.Lookup(f.Code); !ok || w.Held != "recreate" {
				say(ctx, r.u, rules.Plain(f.Code, f.Resource, f.Message))
			}
		}
	}
}

// agree says a change. The first change of the run also says the recreate
// it leads to, and asks to continue.
func (r *adding) agree(ctx context.Context, what string) error {
	if r.agreed {
		say(ctx, r.u, what)
		return nil
	}
	return r.consent(ctx, "write", what+recreates)
}

// consent is confirm, and notes that the reader agreed to the recreate.
func (r *adding) consent(ctx context.Context, step, text string) error {
	if err := confirm(ctx, r.u, step, text); err != nil {
		return err
	}
	r.agreed = true
	return nil
}

func (r *adding) listedAs(n string) (proxy.Provider, bool) {
	return find(r.listed, func(p proxy.Provider) bool { return p.Name == n })
}

func (r *adding) presetAs(n string) (proxy.Preset, bool) {
	return find(r.presets, func(p proxy.Preset) bool { return p.Name == n })
}
