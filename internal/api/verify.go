package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/jsontext"
	"fmt"
	"net/http"
	"slices"

	"github.com/jorlaru/stargaite/internal/admission"
	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/params"
	"github.com/jorlaru/stargaite/internal/plan"
	"github.com/jorlaru/stargaite/internal/routing"
	"github.com/jorlaru/stargaite/internal/telemetry"
)

// verifyPrompt is the body of every verify call, less its closing brace and
// its level (phase-4 design section 6): no cap member, so the only cap sent
// is the operator's, written by Build as an alias cap (RSN-9, PAR-3).
const verifyPrompt = `{"model":"` + domain.VerifyKey + `","messages":[{"role":"user","content":"What is 17 × 23? Answer with the number only."}]`

// discard is the response writer of a verify call: the answer call relays
// reaches no client, and a deadline set on it has no effect.
type discard http.Header

func (d discard) Header() http.Header       { return http.Header(d) }
func (discard) Write(b []byte) (int, error) { return len(b), nil }
func (discard) WriteHeader(int)             {}

// verifyPlan is one call of a run, built before the first call is made.
type verifyPlan struct {
	level  string
	body   *params.Body
	client *plan.Client
	out    *plan.Outgoing
}

// Verify returns the live check of RSN-9 over d's data plane: the snapshot is
// read once per run; each call is its own request through exclude, call,
// finish and done, under the key _verify with no alias, so only the global
// budget, the provider's quota, the store and the credential apply, and each
// call gets fresh breakers, so it neither opens nor obeys a circuit. A
// refusal of the request comes before any call: an unknown target, one that
// is not chat, a cap that is not positive, an anthropic target without a cap
// (PAR-7), a cap above the target's max_output_tokens (PAR-6), and a body
// Check or Build refuses. A refusal before the provider ends the run with
// that error on its call; a provider error does not. The run also ends when
// ctx does, and the levels left leave no record. The run holds Flight from
// start to end, so serve never sees it at zero between two calls (RES-6).
func Verify(d Deps) func(ctx context.Context, target string, maxOutput *int64) (domain.Verification, *domain.Error) {
	return func(ctx context.Context, id string, maxOutput *int64) (domain.Verification, *domain.Error) {
		d.Flight.Add(1)
		defer d.Flight.Done()
		snap := d.Config.Snapshot()
		t := snap.Targets[id]
		refused := func(format string, args ...any) (domain.Verification, *domain.Error) {
			return domain.Verification{}, domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf(format, args...))
		}
		switch {
		case t == nil:
			return domain.Verification{}, domain.NewError(domain.CodeNotFound, fmt.Sprintf("target %.200q does not exist", id))
		case t.Spec.Kind != domain.KindChat:
			return refused("targets verify checks chat targets only: %.200s is of kind %.64q", id, t.Spec.Kind)
		case maxOutput != nil && *maxOutput < 1:
			return refused("max_output_tokens must be a positive integer")
		case maxOutput == nil && t.Provider.Spec.Protocol == domain.ProtocolAnthropic:
			return refused("an anthropic target needs --max-output-tokens: the proxy never fills in a cap (PAR-7)")
		case maxOutput != nil && t.Spec.MaxOutputTokens > 0 && *maxOutput > t.Spec.MaxOutputTokens:
			return refused("max_output_tokens %d is above the target's max_output_tokens %d (PAR-6)", *maxOutput, t.Spec.MaxOutputTokens)
		}
		var m *domain.Model // phase-1 design section 9's nil Model, or one that carries only the operator's cap
		if maxOutput != nil {
			m = &domain.Model{MaxOutputTokens: *maxOutput}
		}
		var plans []verifyPlan
		for _, level := range domain.VerifyLevels(t.Spec.Reasoning) {
			raw := []byte(verifyPrompt)
			if level != "" {
				sent := level
				if level == domain.LevelOn {
					sent = "medium" // every level but none means on (RSN-4)
				}
				q, _ := jsontext.AppendQuote(nil, sent) // a stored level is valid UTF-8: the strict decoder read it
				raw = append(append(raw, `,"reasoning_effort":`...), q...)
			}
			p := verifyPlan{level: level}
			var pe *params.Error
			if p.body, pe = params.Scan(append(raw, '}'), params.Chat.Walk); pe != nil {
				return refused("level %.64q cannot be sent: %s", level, pe.Message)
			}
			var e *domain.Error
			if p.client, e = plan.Check(domain.EndpointChat, p.body, m, nil); e != nil { // the proxy's own body has no unknown key
				return domain.Verification{}, e
			}
			var skip *plan.Skip
			p.out, skip = plan.Build(plan.Input{Endpoint: domain.EndpointChat, Body: p.body, Model: m, Upstream: t.Upstream,
				Target: &t.Spec, Provider: &t.Provider.Spec, Client: p.client})
			if skip != nil {
				if skip.Code == domain.CodeInternalError {
					d.Log.Error("build failed", "target", id, "error", skip.Param)
				}
				return domain.Verification{}, domain.NewError(skip.Code, fmt.Sprintf("target %.200q cannot be verified at level %.64q: %s", id, level, skip.Code))
			}
			plans = append(plans, p)
		}

		v := domain.Verification{Target: id, Version: d.Version, Spec: t.Spec, Provider: t.Provider.Spec}
		trace := telemetry.NewContext() // one trace for the run
		for _, p := range plans {
			if ctx.Err() != nil {
				break
			}
			c, end := verifyCall(ctx, d, snap, t, trace, p)
			v.Calls = append(v.Calls, c)
			if end {
				break
			}
		}
		v.OK = len(v.Calls) == len(plans) && !slices.ContainsFunc(v.Calls, func(c domain.VerifyCall) bool { return c.Status/100 != 2 || c.Error != nil })
		v.At = d.Config.Now()
		return v, nil
	}
}

// verifyCall is one call of a verify run as its own request: Admit with the
// global scope only, exclude, then call with a discarding writer, finish and
// done. end says the run stops here: a refusal before the provider, or the
// call ended with ctx.
func verifyCall(ctx context.Context, d Deps, snap *config.Snapshot, t *config.Target, trace telemetry.Context, p verifyPlan) (c domain.VerifyCall, end bool) {
	h := &handler{Deps: d, breakers: routing.NewBreakers(d.Config.Now)}
	rq := &request{id: rand.Text(), traceID: hex.EncodeToString(trace.Trace[:]), trace: trace, ep: domain.EndpointChat, snap: snap,
		key: &config.Key{Name: domain.VerifyKey}, body: p.body, client: p.client, started: d.Config.Now()}
	h.Flight.Add(1)
	defer h.done(rq)
	c = domain.VerifyCall{Effort: p.level, EffortSent: p.out.Effort.Sent, Body: string(p.out.Body), RequestID: rq.id}
	defer func() { c.Error = rq.err }()
	if rq.release, rq.err = h.Admission.Admit(admission.Scope{Name: "global", Parallel: snap.Settings.MaxParallelRequests}); rq.err != nil {
		return c, true
	}
	gen, e := h.exclude(rq, t)
	if rq.err = e; e != nil {
		return c, true
	}
	// call reads only the request's context.
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "/", http.NoBody)
	if err != nil {
		rq.err = domain.NewError(domain.CodeInternalError, err.Error())
		return c, true
	}
	a := h.call(discard{}, r, rq, t, p.out, 1, gen)
	rq.err = a.end.err
	c.Status, c.ReasoningTokens, c.OutputTokens = a.status, a.rec.ReasoningTokens, a.rec.OutputTokens
	return c, ctx.Err() != nil
}
