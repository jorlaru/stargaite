package task

import (
	"bytes"
	"context"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
	"github.com/jorlaru/stargaite/internal/console/ui/plain"
)

// seeUsage runs See usage and cost with u.
func (w *world) seeUsage(u *ui.UI) (string, error) {
	return u.Run(context.Background(), usageSteps(), func(ctx context.Context) (string, error) {
		return SeeUsage(ctx, u, w.proxy(), w.h)
	})
}

func TestUsageTable(t *testing.T) {
	cost := 0.0000062
	rep := proxy.UsageReply{Source: "records", Since: "2026-11-20T00:00:00Z", Until: "2026-11-20T08:00:00Z", Groups: []proxy.UsageGroup{
		{Billing: "pay_per_use", Key: "alpha", Requests: 3, Errors: 1, Input: 120, Output: 45, CostUSD: &cost},
		{Billing: "none", Key: "bravo", Requests: 12, Input: 7, Output: 9000},
	}}
	want := "window 2026-11-20 00:00 UTC to 2026-11-20 08:00 UTC, from the records\n" +
		"key    billing      requests  errors  input  output  cost USD\n" +
		"alpha  pay_per_use  3         1       120    45      0.0000062\n" +
		"bravo  none         12        0       7      9000    -"
	if got := usageTable(rep, "key"); got != want {
		t.Errorf("usageTable:\n%s\nwant:\n%s", got, want)
	}
	if got := usageTable(rep, "model"); !strings.HasPrefix(strings.Split(got, "\n")[1], "alias  ") || !strings.Contains(got, "\n-      ") {
		t.Errorf("by alias, the key column absent:\n%s", got)
	}

	daily := proxy.UsageReply{Source: "daily", Since: "2026-11-13T00:00:00Z", Until: "2026-11-21T00:00:00Z"}
	if got := usageTable(daily, "target"); got != "window 2026-11-13 00:00 UTC to 2026-11-21 00:00 UTC, from the daily sums, whole UTC days\nno usage in the window" {
		t.Errorf("daily, empty: %q", got)
	}
	nano := proxy.UsageReply{Source: "records", Since: "2026-11-19T10:41:46.316993629Z", Until: "2026-11-20T12:41:46.3+02:00"}
	if got := usageTable(nano, "key"); !strings.HasPrefix(got, "window 2026-11-19 10:41 UTC to 2026-11-20 10:41 UTC, from the records\n") {
		t.Errorf("a time to the minute, in UTC: %q", got)
	}

	dirty := proxy.UsageReply{Source: "records", Since: "a", Until: "b", Groups: []proxy.UsageGroup{{Billing: "none", Key: "ev\x1b[2Jil\tx"}}}
	got := usageTable(dirty, "key")
	if strings.ContainsAny(got, "\t\x1b") || !strings.Contains(got, "\nev[2Jil x  none  ") {
		t.Errorf("not cleaned: %q", got)
	}
}

// recorded waits, 5 s at most, until usage show by key holds n groups: a
// record is written after its call returns. It returns the report.
func (w *world) recorded(n int) proxy.UsageReply {
	w.t.Helper()
	var rep proxy.UsageReply
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		w.decoded(&rep, "usage", "show", "--since", "24h", "--by", "key")
		if len(rep.Groups) == n {
			return rep
		}
	}
	w.t.Fatalf("usage show holds %d group(s), not %d", len(rep.Groups), n)
	return rep
}

func TestSeeUsage(t *testing.T) {
	w, _, port := keyWorld(t, "mock/m")
	for _, k := range []string{"app-1", "app-2"} {
		if code := chat(t, port, w.newKey(k, "--model", "a")); code != 200 {
			t.Fatalf("chat with %s: %d", k, code)
		}
	}
	rep := w.recorded(2)
	var out bytes.Buffer
	res, err := w.seeUsage(ui.New(plain.New(&out), map[string]string{"period": "24h", "by": "key"}))
	if err != nil || res != "Done: usage of the last 24 hours by key." {
		t.Fatalf("%q, %v\n%s", res, err, out.String())
	}
	lines := strings.Split(out.String(), "\n")
	if !slices.ContainsFunc(lines, func(l string) bool {
		return strings.HasPrefix(l, "key    billing  requests  errors  input  output  cost USD")
	}) {
		t.Errorf("no header:\n%s", out.String())
	}
	for _, g := range rep.Groups {
		want := []string{g.Key, g.Billing, strconv.FormatInt(g.Requests, 10), strconv.FormatInt(g.Errors, 10),
			strconv.FormatInt(g.Input, 10), strconv.FormatInt(g.Output, 10), "-"}
		if !slices.ContainsFunc(lines, func(l string) bool { return slices.Equal(strings.Fields(l), want) }) {
			t.Errorf("no row %q:\n%s", want, out.String())
		}
	}
	if got := w.cli(); !slices.Equal(got, []string{"usage show --json --since 24h --by key"}) {
		t.Errorf("CLI calls %q", got)
	}
}

func TestSeeUsageAtTerminal(t *testing.T) {
	w, _, _ := keyWorld(t, "mock/m")
	s := &scripted{answers: map[string][]string{"period": {"168h"}, "by": {"model"}}}
	res, err := w.seeUsage(ui.New(s, nil))
	if err != nil || res != "Done: usage of the last 7 days by alias." {
		t.Fatalf("%q, %v\n%s", res, err, s.output())
	}
	var got []string
	for _, q := range s.questions {
		for _, o := range q.Options {
			got = append(got, q.Prompt+" "+o.Value+" "+o.Label)
		}
	}
	want := []string{"Which period? 24h the last 24 hours", "Which period? 168h the last 7 days", "Which period? 720h the last 30 days",
		"Grouped by? key key (an app or agent)", "Grouped by? model alias", "Grouped by? target target"}
	if !slices.Equal(got, want) {
		t.Errorf("options %q", got)
	}
	hasLines(t, s.output(), "no usage in the window")
}

func TestSeeUsageNotRunning(t *testing.T) {
	w := newWorld(t)
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "docker"), "#!/bin/sh\n[ \"$*\" = \"compose ps -q\" ] || exit 9\n", 0o755)
	t.Setenv("PATH", bin)
	var out bytes.Buffer
	_, err := w.seeUsage(ui.New(plain.New(&out), map[string]string{"period": "24h", "by": "key"}))
	if err == nil || err.Error() != "no container runs here: ./setup.sh install starts it" || len(w.cli()) != 0 {
		t.Fatalf("err = %v, CLI %q\n%s", err, w.cli(), out.String())
	}
}
