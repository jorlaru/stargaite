package task

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
)

// SeeWhatExists says what the proxy holds, one line per thing: both
// versions, the providers and their credentials, the aliases with their
// targets in order, the targets in no alias, the keys, the budgets and
// telemetry. It only reads, and stops at the first read that fails, with
// the lines already said kept.
func SeeWhatExists(ctx context.Context, u *ui.UI, px *proxy.Proxy, h *host.Host) (string, error) {
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
					lines = append(lines, "target "+t.ID+": in no alias")
				}
			}
			return lines
		}},
		{&keys, "keys list", func() []string {
			var lines []string
			for _, k := range keys.Keys {
				calls := strings.Join(k.Models, ", ")
				switch {
				case slices.Equal(k.Models, []string{"*"}):
					calls = "every alias"
				case len(k.Models) == 0:
					calls = "no alias"
				}
				lines = append(lines, "key "+k.Name+": may call "+calls+budget(k.Budget))
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
	return "", nil
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
