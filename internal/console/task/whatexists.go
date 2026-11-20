package task

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/rules"
	"github.com/jorlaru/stargaite/internal/console/ui"
)

// SeeWhatExists says what the proxy holds, one line per thing: both
// versions, the providers and their credentials, the aliases with their
// targets in order, the targets in no alias, the keys, the budgets and
// telemetry. It reads, and stops at the first read that fails, with the
// lines already said kept. With check, it first fetches the clone's
// upstream and says whether it has an upgrade. At a terminal it then offers
// to remove a target in no alias.
func SeeWhatExists(ctx context.Context, u *ui.UI, px *proxy.Proxy, h *host.Host) (string, error) {
	if u.Given(ui.Question{ID: "check", Flag: "check"}) {
		if err := sayUpgrade(ctx, u, h); err != nil {
			return "", err
		}
	}
	runs, err := h.ContainerRuns(ctx)
	if err != nil {
		return "", err
	}
	clone := h.CloneVersion(ctx)
	if !runs {
		return "", u.Say(ctx, "version: "+clone+" in this clone; no container runs (./setup.sh install starts it)")
	}
	var (
		version   proxy.VersionReply
		providers proxy.ProvidersReply
		models    proxy.ModelsReply
		targets   proxy.TargetsReply
		keys      proxy.KeysReply
		settings  proxy.SettingsReply
		telemetry proxy.TelemetryReply
	)
	var orphans []string
	reads := []struct {
		reply proxy.Reply
		words string
		lines func() []string
	}{
		{&version, "version", func() []string {
			return []string{"version: " + version.Version + " running, " + clone + " in this clone"}
		}},
		{&providers, "providers list", func() []string {
			var lines []string
			for _, p := range providers.Providers {
				lines = append(lines, "provider "+p.Name+": credential "+p.Credential)
			}
			return orNone(lines, "providers: none")
		}},
		{&models, "models list", func() []string {
			var lines []string
			for _, m := range models.Models {
				lines = append(lines, "alias "+m.Name+": "+strings.Join(m.Targets, ", then ")+budget(m.Budget))
			}
			return orNone(lines, "aliases: none")
		}},
		{&targets, "targets list", func() []string {
			var lines []string
			for _, t := range targets.Targets {
				if !slices.ContainsFunc(models.Models, func(m proxy.Model) bool { return slices.Contains(m.Targets, t.ID) }) {
					orphans = append(orphans, t.ID)
					lines = append(lines, "target "+t.ID+": in no alias")
				}
			}
			return lines
		}},
		{&keys, "keys list", func() []string {
			var lines []string
			for _, k := range keys.Keys {
				lines = append(lines, "key "+k.Name+": may call "+mayCall(k)+budget(k.Budget))
			}
			return orNone(lines, "keys: none")
		}},
		{&settings, "settings show", func() []string {
			if settings.Budget == nil {
				return []string{"spend: not capped"}
			}
			return []string{"spend: capped at " + usd(settings.Budget)}
		}},
		{&telemetry, "telemetry status", func() []string {
			t := telemetry.Telemetry
			switch {
			case t == nil || t.OTLPEndpoint == "":
				return []string{"telemetry: not set"}
			case len(telemetry.Unresolved) > 0:
				return []string{"telemetry " + t.OTLPEndpoint + ": credentials unresolved: " + strings.Join(telemetry.Unresolved, ", ")}
			case t.BasicAuthEnv != nil || t.HeaderEnv != nil:
				return []string{"telemetry " + t.OTLPEndpoint + ": credentials resolved"}
			}
			return []string{"telemetry " + t.OTLPEndpoint + ": no credential"}
		}},
	}
	for _, r := range reads {
		if err := px.Call(ctx, r.reply, r.words, nil); err != nil {
			return "", err
		}
		for _, line := range r.lines() {
			if err := u.Say(ctx, line); err != nil {
				return "", err
			}
		}
	}
	if len(orphans) == 0 || !u.Asks() {
		return "", nil
	}
	return removeTarget(ctx, u, px, orphans)
}

// removeTarget offers to remove one of the targets in no alias, confirmed
// first; none keeps them.
func removeTarget(ctx context.Context, u *ui.UI, px *proxy.Proxy, ids []string) (string, error) {
	options := []ui.Option{{Value: "none", Label: "keep them"}}
	for _, id := range ids {
		options = append(options, ui.Option{Value: id, Label: "target " + id})
	}
	id, err := u.Pick(ctx, ui.Question{ID: "remove", Step: "remove", Prompt: "Remove a target that is in no alias?", Options: options})
	if err != nil || id == "none" {
		return "", err
	}
	if err := confirm(ctx, u, "remove", "target "+id+" is removed: no alias lists it, and its prices and its check go with it"); err != nil {
		return "", err
	}
	var change proxy.Change
	refused, err := px.CallRefusable(ctx, &change, "targets remove", []string{"--yes"}, id)
	if err != nil {
		return "", err
	}
	for _, f := range append(refused, change.Findings...) {
		say(ctx, u, rules.Plain(f.Code, f.Resource, f.Message))
	}
	if len(refused) > 0 {
		return "", errors.New("the proxy refused to remove target " + id + "; nothing was changed")
	}
	return "Done: target " + id + " removed.", nil
}

// sayUpgrade fetches the clone's upstream and says whether it has commits
// the clone lacks.
func sayUpgrade(ctx context.Context, u *ui.UI, h *host.Host) error {
	upstream, ahead, err := h.Fetch(ctx)
	if err != nil {
		return err
	}
	if ahead > 0 {
		return u.Say(ctx, "upgrade: "+strconv.Itoa(ahead)+" new commit(s) on "+upstream+"; ./setup.sh upgrade applies them")
	}
	return u.Say(ctx, "upgrade: none, up to date with "+upstream)
}

// orNone is lines, or the one line none when there are no lines.
func orNone(lines []string, none string) []string {
	if len(lines) == 0 {
		return []string{none}
	}
	return lines
}

// budget is the suffix of an alias or key line with a budget.
func budget(b *proxy.Budget) string {
	if b == nil {
		return ""
	}
	return "; budget " + usd(b)
}

// usd is "<limit> USD", then ", resets <interval>" when there is one.
func usd(b *proxy.Budget) string {
	s := strconv.FormatFloat(b.LimitUSD, 'f', -1, 64) + " USD"
	if b.ResetInterval != "" {
		s += ", resets " + b.ResetInterval
	}
	return s
}
