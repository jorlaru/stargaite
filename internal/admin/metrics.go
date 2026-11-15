package admin

import (
	"bytes"
	"cmp"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/jorlaru/stargaite/internal/admission"
	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/telemetry"
	"github.com/jorlaru/stargaite/internal/usage"
)

// metricsType is the Prometheus text format 0.0.4, the only one served
// (OPS-8, phase-4 design section 15 point 14).
const metricsType = "text/plain; version=0.0.4; charset=utf-8"

// family is one metric family: its name, type, help text and label names.
type family struct {
	name, typ, help string
	labels          []string
}

// byKey are the labels of a family counted by key and alias.
var byKey = []string{"key", "model"}

// names maps each usage.Counter name to its family. A counter's Key, Model
// and Label fill the family's labels in that order, as many as it names; a
// counter name missing here, or one carrying a value its family has no label
// for, makes render fail, never a silent omission.
var names = map[string]family{
	"requests":           {"stargaite_requests_total", "counter", "Authenticated inference requests answered, by key, alias and HTTP status.", []string{"key", "model", "status"}},
	"tokens":             {"stargaite_tokens_total", "counter", "Tokens recorded, by key, alias and type.", []string{"key", "model", "type"}},
	"cost_usd":           {"stargaite_cost_usd_total", "counter", "Cost recorded in USD, by key and alias.", byKey},
	"unpriced":           {"stargaite_unpriced_requests_total", "counter", "Pay-per-use provider calls recorded without a cost (USE-12).", byKey},
	"length":             {"stargaite_length_finishes_total", "counter", "Provider calls that finished at an output or context limit.", byKey},
	"empty":              {"stargaite_empty_responses_total", "counter", "Provider calls that answered no text and no tool call.", byKey},
	"model_mismatch":     {"stargaite_model_mismatches_total", "counter", "Provider calls answered by another model than the one sent (USE-4).", byKey},
	"params_changed":     {"stargaite_params_changed_total", "counter", "Parameters changed before sending, by key, alias and action.", []string{"key", "model", "action"}},
	"budget_warning":     {"stargaite_budget_warnings_total", "counter", "Budgets that crossed 80 percent of their limit (LIM-3).", byKey},
	"budget_exhausted":   {"stargaite_budget_exhausted_total", "counter", "Budgets that reached their limit (LIM-3).", byKey},
	"quota_warning":      {"stargaite_quota_warnings_total", "counter", "Quota windows that crossed 80 percent of their limit (LIM-9).", byKey},
	"spans":              {"stargaite_spans_emitted_total", "counter", "Spans handed to the exporter, by key and alias.", byKey},
	"unauthenticated":    {"stargaite_unauthenticated_requests_total", "counter", "Data-plane requests refused with 401 (TEL-5).", nil},
	"store_check_failed": {"stargaite_store_check_failures_total", "counter", "Store checks that failed after the start.", nil},
}

// The families render builds from the exporter's status, the budgets and the
// quota windows.
var (
	spansSent     = family{"stargaite_spans_sent_total", "counter", "Spans the OTLP backend accepted.", nil}
	spansDropped  = family{"stargaite_spans_dropped_total", "counter", "Spans dropped: queue full, unreachable, credentials, unset or stopped.", nil}
	spansRejected = family{"stargaite_spans_rejected_total", "counter", "Spans rejected, by HTTP status or by the backend.", []string{"reason", "status"}}
	spansQueued   = family{"stargaite_spans_queued", "gauge", "Spans waiting in the exporter's queue.", nil}
	lastError     = family{"stargaite_exporter_last_error_info", "gauge", "The exporter's last error, redacted, as telemetry status shows it.", []string{"message"}}
	budgetSpent   = family{"stargaite_budget_spent_usd", "gauge", "Spend of each declared budget in its current period, in USD.", []string{"scope", "name", "interval"}}
	budgetLimit   = family{"stargaite_budget_limit_usd", "gauge", "Limit of each declared budget, in USD.", []string{"scope", "name", "interval"}}
	quotaUsed     = family{"stargaite_quota_used", "gauge", "Requests or tokens used in each quota window held.", []string{"provider", "key", "window", "unit"}}
	quotaLimit    = family{"stargaite_quota_limit", "gauge", "Limit of each quota window held.", []string{"provider", "key", "window", "unit"}}
)

var (
	helpEscape  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	labelEscape = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
)

// render writes the metrics of phase-4 design section 7 in the text format
// 0.0.4: every family, sorted by name, with one # HELP and one # TYPE line
// before its samples, which are contiguous; a family with labels has a
// series for each combination seen, one without labels always one sample.
// Budget gauges are built from snap as the data plane's budgets are; quota
// gauges read a.Windows() as held, never Sync.
func render(w io.Writer, snap *config.Snapshot, counters, spanned *usage.Counters, a *admission.State, st telemetry.Status) error {
	samples := map[string][]string{}
	families := map[string]family{}
	add := func(f family, v float64, values ...string) {
		families[f.name] = f
		var pairs []string
		for i, l := range f.labels {
			pairs = append(pairs, l+`="`+escapeLabel(values[i])+`"`)
		}
		line := f.name
		if pairs != nil {
			line += "{" + strings.Join(pairs, ",") + "}"
		}
		samples[f.name] = append(samples[f.name], line+" "+strconv.FormatFloat(v, 'g', -1, 64))
	}
	var err error
	each := func(c usage.Counter, v float64) {
		f, ok := names[c.Name]
		values := []string{c.Key, c.Model, c.Label}
		if !ok || slices.ContainsFunc(values[len(f.labels):], func(s string) bool { return s != "" }) {
			err = cmp.Or(err, fmt.Errorf("metrics: counter %+v has no family that holds it", c))
			return
		}
		add(f, v, values[:len(f.labels)]...)
	}
	counters.Each(each)
	spanned.Each(each)
	if err != nil {
		return err
	}
	for _, f := range names {
		families[f.name] = f
		if f.labels == nil && samples[f.name] == nil {
			add(f, 0)
		}
	}

	add(spansSent, float64(st.Sent))
	add(spansDropped, float64(st.Dropped))
	add(spansQueued, float64(st.Queued))
	add(spansRejected, float64(st.RejectedByBackend), "backend", "")
	for _, code := range slices.Sorted(maps.Keys(st.RejectedByStatus)) {
		add(spansRejected, float64(st.RejectedByStatus[code]), "status", strconv.Itoa(code))
	}
	families[lastError.name] = lastError
	if st.LastError != "" {
		add(lastError, 1, st.LastError)
	}

	families[budgetSpent.name], families[budgetLimit.name] = budgetSpent, budgetLimit
	// The scopes of the data plane's budgets: global, then every key and
	// every alias with a budget, each with the revision its never counter
	// started at.
	budget := func(kind, name string, spec *domain.Budget) {
		if spec == nil {
			return
		}
		scope := "global"
		if kind != scope {
			scope = kind + ":" + name
		}
		limit := 0.0 // a missing limit_usd blocks, as admission reads it
		if spec.LimitUSD != nil {
			limit = *spec.LimitUSD
		}
		add(budgetSpent, a.Spent(admission.Budget{Scope: scope, Spec: *spec, Since: snap.NeverSince[scope]}), kind, name, spec.ResetInterval)
		add(budgetLimit, limit, kind, name, spec.ResetInterval)
	}
	budget("global", "", snap.Settings.Budget)
	for _, name := range slices.Sorted(maps.Keys(snap.Keys)) {
		budget("key", name, snap.Keys[name].Spec.Budget)
	}
	for _, name := range slices.Sorted(maps.Keys(snap.Models)) {
		budget("model", name, snap.Models[name].Spec.Budget)
	}

	families[quotaUsed.name], families[quotaLimit.name] = quotaUsed, quotaLimit
	for _, v := range a.Windows() {
		add(quotaUsed, float64(v.Used), v.Provider, v.Key, v.Window, v.Unit)
		add(quotaLimit, float64(v.Limit), v.Provider, v.Key, v.Window, v.Unit)
	}

	var b bytes.Buffer
	for _, name := range slices.Sorted(maps.Keys(families)) {
		f := families[name]
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, helpEscape.Replace(f.help), name, f.typ)
		for _, s := range samples[name] {
			b.WriteString(s + "\n")
		}
	}
	_, err = w.Write(b.Bytes())
	return err
}

// metrics answers GET /admin/v1/metrics (OPS-8, TEL-3, phase-4 design
// section 7): always the text format 0.0.4, whatever Accept asks. A read:
// not audited.
func (s *server) metrics(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		s.fail(w, invalid("this route takes no query parameter"))
		return
	}
	var b bytes.Buffer
	if err := render(&b, s.cfg.Snapshot(), s.counters, s.spanned, s.limits, s.spans.Status()); err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", metricsType)
	_, _ = w.Write(b.Bytes())
}

// escapeLabel is v as a label value of the text format: invalid UTF-8
// replaced, then backslash, double quote and line feed escaped.
func escapeLabel(v string) string {
	return labelEscape.Replace(strings.ToValidUTF8(v, "\uFFFD"))
}
