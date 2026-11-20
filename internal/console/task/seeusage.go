package task

import (
	"context"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
)

// usageSteps is See usage and cost's step table.
func usageSteps() []ui.Step {
	return []ui.Step{{ID: "period", Title: "period"}, {ID: "by", Title: "grouped by"}, {ID: "table", Title: "table"}}
}

// SeeUsage prints the usage and cost of a period, grouped by key, alias or
// target, as one table. It only reads.
func SeeUsage(ctx context.Context, u *ui.UI, px *proxy.Proxy, h *host.Host) (string, error) {
	if err := ready(ctx, u, h, "period"); err != nil {
		return "", err
	}
	periods := []ui.Option{{Value: "24h", Label: "the last 24 hours"}, {Value: "168h", Label: "the last 7 days"}, {Value: "720h", Label: "the last 30 days"}}
	period, err := u.Pick(ctx, ui.Question{ID: "period", Step: "period", Prompt: "Which period?", Options: periods})
	if err != nil {
		return "", err
	}
	by, err := u.Pick(ctx, ui.Question{ID: "by", Step: "by", Prompt: "Grouped by?", Options: []ui.Option{
		{Value: "key", Label: "key (an app or agent)"}, {Value: "model", Label: "alias"}, {Value: "target", Label: "target"}}})
	if err != nil {
		return "", err
	}
	var rep proxy.UsageReply
	if err := px.Call(ctx, &rep, "usage show", []string{"--since", period, "--by", by}); err != nil {
		return "", err
	}
	if err := u.Show(ctx, usageTable(rep, by)); err != nil {
		return "", err
	}
	p, _ := find(periods, func(o ui.Option) bool { return o.Value == period })
	return "Done: usage of " + p.Label + " by " + column(by) + ".", nil
}

// column is a grouping of usage show in words: --by model groups by alias.
func column(by string) string {
	if by == "model" {
		return "alias"
	}
	return by
}

// usageTable is rep as its window, then a table grouped by by, its columns
// padded with spaces; each cell is cleaned to one line first, and a cell
// that is absent is "-".
func usageTable(rep proxy.UsageReply, by string) string {
	from := "the records"
	if rep.Source == "daily" {
		from = "the daily sums, whole UTC days"
	}
	window := "window " + stamp(rep.Since) + " to " + stamp(rep.Until) + ", from " + from
	if len(rep.Groups) == 0 {
		return window + "\nno usage in the window"
	}
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	row := func(cells ...string) {
		for i, c := range cells {
			if cells[i] = ui.OneLine(c); cells[i] == "" {
				cells[i] = "-"
			}
		}
		_, _ = tw.Write([]byte(strings.Join(cells, "\t") + "\n"))
	}
	row(column(by), "billing", "requests", "errors", "input", "output", "cost USD")
	for _, g := range rep.Groups {
		cost := ""
		if g.CostUSD != nil {
			cost = strconv.FormatFloat(*g.CostUSD, 'f', -1, 64)
		}
		grouped := map[string]string{"key": g.Key, "model": g.Model, "target": g.Target}[by]
		row(grouped, g.Billing, strconv.FormatInt(g.Requests, 10), strconv.FormatInt(g.Errors, 10),
			strconv.FormatInt(g.Input, 10), strconv.FormatInt(g.Output, 10), cost)
	}
	_ = tw.Flush()
	return window + "\n" + strings.TrimSuffix(b.String(), "\n")
}

// stamp is a time of the usage reply to the minute, in UTC; one it cannot
// read is shown as it is, cleaned to one line.
func stamp(s string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return ui.OneLine(s)
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}
