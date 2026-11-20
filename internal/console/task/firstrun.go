package task

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/rules"
	"github.com/jorlaru/stargaite/internal/console/ui"
)

// firstSteps is the first run's step table, its bar (design section 4.1).
func firstSteps() []ui.Step {
	return []ui.Step{{ID: "machine", Title: "machine"}, {ID: "provider", Title: "provider"}, {ID: "alias", Title: "alias"},
		{ID: "model", Title: "model"}, {ID: "fallbacks", Title: "fallbacks"}, {ID: "check", Title: "check"},
		{ID: "key", Title: "key"}, {ID: "answer", Title: "first answer"}}
}

// stage is one stage of the first run after the Machine step: a task with
// its own step table, the first run's step each of its steps is drawn at,
// and the flags that ask for it at a terminal.
type stage struct {
	name  string
	at    map[string]string
	steps []ui.Step
	flags []string
	run   Func
}

// stages is the first run's shape after the Machine step, in order.
func stages() []stage {
	return []stage{
		{name: "provider", steps: addSteps(), run: AddProviders,
			at:    map[string]string{"which": "provider", "add": "provider", "key": "provider", "write": "provider", "recreate": "provider"},
			flags: []string{"provider", "base-url", "protocol", "api-key-env", "key-env", "replace", "allow-private-network"}},
		{name: "alias", steps: makeSteps(), run: MakeAlias,
			at: map[string]string{"name": "alias", "model": "model", "fallbacks": "fallbacks", "settings": "check", "check": "check",
				"make": "check"},
			flags: []string{"alias", "model", "fallback", "max-output-tokens", "effort", "input-price", "output-price", "reverify"}},
		{name: "key", steps: giveSteps(), run: GiveKey,
			at:    map[string]string{"aliases": "key", "name": "key", "limits": "key", "key": "key", "try": "answer"},
			flags: []string{"key-name", "rpm-limit"}},
	}
}

// FirstRun is the first run (design sections 4.1 and 4.5): the Machine
// step, then Add providers, Make an alias and Give an app or agent its key,
// each keeping what exists. At a terminal a stage runs when its own thing is
// missing or a flag of it is given; without one every stage runs. A stage
// that leaves its thing missing ends the run with its result.
func FirstRun(ctx context.Context, u *ui.UI, px *proxy.Proxy, h *host.Host) (string, error) {
	if err := oneProvider(ctx, u); err != nil {
		return "", err
	}
	if err := machine(ctx, u, h); err != nil {
		return "", err
	}
	f, err := readFacts(ctx, px)
	if err != nil {
		return "", err
	}
	run := toRun(u, rules.Missing(f))
	if len(run) == 0 {
		return "Nothing is missing: a provider whose key resolves, an alias and a key exist; the home's tasks change them.", nil
	}
	var res string
	for i, s := range run {
		if i > 0 {
			say(ctx, u, res)
		}
		if s.name == "alias" && !u.Asks() && !u.Given(ui.Question{ID: "model"}) {
			// as phase 6, even when the alias exists: no key is made on a missing answer
			return "", &ui.UsageError{Msg: "missing --model: the model id at the provider"}
		}
		if res, err = u.Stages(ctx, s.at, s.steps, func(ctx context.Context) (string, error) { return s.run(ctx, u, px, h) }); err != nil {
			return "", err
		}
		if i == len(run)-1 { // nothing follows: what exists decides nothing more
			break
		}
		if f, err = readFacts(ctx, px); err != nil {
			return "", err
		}
		if slices.Contains(rules.Missing(f), s.name) {
			break
		}
	}
	return res, nil
}

// oneProvider refuses --provider naming several providers: Make an alias
// reads it as one name.
func oneProvider(ctx context.Context, u *ui.UI) error {
	q := ui.Question{ID: "provider", Flag: "provider", Step: "machine", Check: func(a string) error {
		if strings.Contains(a, ",") {
			return errors.New("install takes one provider; ./setup.sh provider adds several")
		}
		return nil
	}}
	if !u.Given(q) {
		return nil
	}
	_, err := u.Text(ctx, q)
	return err
}

// toRun is the stages that run: at a terminal those in missing or with a
// flag given, without one all.
func toRun(u *ui.UI, missing []string) []stage {
	var out []stage
	for _, s := range stages() {
		given := slices.ContainsFunc(s.flags, func(f string) bool { return u.Given(ui.Question{ID: f}) })
		if !u.Asks() || given || slices.Contains(missing, s.name) {
			out = append(out, s)
		}
	}
	return out
}

// facts is what the home and the first run judge, from export and validate.
func facts(export proxy.ExportReply, validate proxy.Change) rules.Facts {
	aliases := map[string]int{}
	for n, m := range export.Models {
		aliases[n] = len(m.Targets)
	}
	f := rules.Facts{Providers: len(export.Providers), Keys: len(export.Keys), Aliases: aliases,
		Unresolved: unresolved(validate.Findings), Uncapped: export.Settings.Budget == nil}
	f.Unverified = unverified(export, validate.Findings, f.Unresolved)
	return f
}

// unverified is, per provider whose credential resolves and none of whose
// chat targets is checked (each one validate's target_unverified), the
// first alias by name that holds the first of them an alias holds, "" none.
func unverified(export proxy.ExportReply, findings []proxy.Finding, unresolved map[string]string) map[string]string {
	never := map[string]bool{}
	for _, f := range findings {
		if id, ok := strings.CutPrefix(f.Resource, "targets/"); ok && f.Code == "target_unverified" {
			never[id] = true
		}
	}
	out, checked := map[string]string{}, map[string]bool{}
	for _, id := range slices.Sorted(maps.Keys(export.Targets)) {
		p, _, _ := strings.Cut(id, "/")
		_, bad := unresolved[p]
		switch {
		case export.Targets[id].Kind != "chat" || bad:
		case !never[id]:
			checked[p] = true
		case out[p] == "":
			out[p] = holder(export, id)
		}
	}
	for p := range checked {
		delete(out, p)
	}
	return out
}

// holder is the first alias by name whose targets hold target, "" none.
func holder(export proxy.ExportReply, target string) string {
	for _, a := range slices.Sorted(maps.Keys(export.Models)) {
		if slices.Contains(export.Models[a].Targets, target) {
			return a
		}
	}
	return ""
}

// readFacts reads export and validate, concurrently, and judges them.
func readFacts(ctx context.Context, px *proxy.Proxy) (rules.Facts, error) {
	var (
		export   proxy.ExportReply
		validate proxy.Change
	)
	if err := readAll(ctx, px, read{"export", &export}, read{"validate", &validate}); err != nil {
		return rules.Facts{}, err
	}
	return facts(export, validate), nil
}
