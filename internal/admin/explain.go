package admin

import (
	"encoding/json/jsontext"
	"fmt"
	"net/http"
	"strconv"

	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/params"
	"github.com/jorlaru/stargaite/internal/plan"
)

// explainRequest is the body of models explain; every member is optional
// (PAR-10). Body is the exact bytes of an inbound request, as a JSON string.
type explainRequest struct {
	Endpoint        *string `json:"endpoint"`
	Effort          *string `json:"effort"`
	MaxOutputTokens *int64  `json:"max_output_tokens"`
	Body            *string `json:"body"`
}

// block is one target of the answer: skipped with its code, or what Build
// gives for it.
type block struct {
	Target  string   `json:"target"`
	Skipped *skipped `json:"skipped,omitzero"`
	*built
}

type skipped struct {
	Code  string `json:"code"`
	Param string `json:"param,omitzero"`
}

// built is what would be sent: every value comes from plan.Outgoing, so the
// layers are those read back from Body (PAR-8), and Header carries no
// credential, which only the data plane adds.
type built struct {
	Effort    effortLayers `json:"effort"`
	MaxOutput capLayers    `json:"max_output"`
	Changes   string       `json:"changes"` // the x-stargaite-params-changed value (PAR-3)
	URL       string       `json:"url"`
	Header    http.Header  `json:"header"`
	Body      string       `json:"body"` // the exact bytes of Outgoing.Body
}

type effortLayers struct {
	Requested *string `json:"requested"`
	Alias     *string `json:"alias"`
	Sent      *string `json:"sent"`
	Source    string  `json:"source"`
	Adjusted  bool    `json:"adjusted"`
}

type capLayers struct {
	Requested *int64 `json:"requested"`
	Alias     *int64 `json:"alias"`
	Sent      *int64 `json:"sent"`
}

// explainModel is models explain (PAR-10, phase-2 design section 6): the
// inbound body runs through the data plane's scan, its check of the body's
// model, plan.Check and plan.Build per target of the published snapshot, and
// the answer shows what each target would get. Nothing is sent, so no
// credential and no dynamic exclusion is involved. It is a read: not audited.
func (s *server) explainModel(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	snap := s.cfg.Snapshot()
	m := snap.Models[name]
	if m == nil {
		s.fail(w, noModel(name))
		return
	}
	data, err := body(w, r, maxBody)
	if err != nil {
		s.fail(w, err)
		return
	}
	var req explainRequest
	if len(data) > 0 {
		if err := config.Decode(data, &req); err != nil {
			s.fail(w, invalid("%v", err))
			return
		}
	}
	ep, walk, capName, effortPath := domain.EndpointChat, params.Chat.Walk, "max_completion_tokens", []string{"reasoning_effort"}
	switch e := req.Endpoint; {
	case e == nil || *e == string(domain.EndpointChat):
	case *e == string(domain.EndpointMessages):
		ep, walk, capName, effortPath = domain.EndpointMessages, params.Messages.Walk, "max_tokens", []string{"output_config", "effort"}
	default:
		s.fail(w, invalid("endpoint must be chat or messages"))
		return
	}

	raw := fmt.Appendf(nil, `{"model":%s,"messages":[{"role":"user","content":"hi"}]}`, quote(name))
	if req.Body != nil {
		raw = []byte(*req.Body)
	}
	var ops []params.Op
	if req.Effort != nil {
		ops = append(ops, params.Op{Path: effortPath, Value: quote(*req.Effort)})
	}
	if req.MaxOutputTokens != nil {
		ops = append(ops, params.Op{Path: []string{capName}, Value: strconv.AppendInt(nil, *req.MaxOutputTokens, 10)})
	}
	b, e := scan(raw, walk, snap.Settings.MaxBodyBytes)
	if e == nil && len(ops) > 0 {
		if raw, err = b.Edit(ops...); err != nil {
			e = invalid("the effort or the cap cannot be set on the body: %v", err)
		} else {
			b, e = scan(raw, walk, snap.Settings.MaxBodyBytes)
		}
	}
	if e == nil {
		// The data plane looks the alias up by the body's model.
		v, _ := b.Get("model")
		if alias, err := jsontext.AppendUnquote(nil, v); err != nil || string(alias) != name {
			e = invalid("the body's model must be the string %.64q, the alias explained", name)
			e.Param = "model"
		}
	}
	var client *plan.Client
	if e == nil {
		client, e = plan.Check(ep, b, &m.Spec)
	}
	if e != nil {
		s.fail(w, e)
		return
	}

	blocks := []block{}
	for _, t := range m.Targets {
		out, skip := plan.Build(plan.Input{Endpoint: ep, Body: b, Model: &m.Spec,
			Upstream: t.Upstream, Target: &t.Spec, Provider: &t.Provider.Spec, Client: client})
		if skip != nil {
			if skip.Code == domain.CodeInternalError { // the proxy's own fault: its reason goes to the log only
				s.log.Error("build failed", "request_id", w.Header().Get(RequestIDHeader), "target", t.ID, "error", skip.Param)
				skip.Param = ""
			}
			blocks = append(blocks, block{Target: t.ID, Skipped: &skipped{skip.Code, skip.Param}})
			continue
		}
		blocks = append(blocks, block{Target: t.ID, built: &built{
			Effort:    effortLayers{out.Effort.Requested, out.Effort.Alias, out.Effort.Sent, out.EffortSource, out.EffortAdjusted},
			MaxOutput: capLayers{out.MaxOutput.Requested, out.MaxOutput.Alias, out.MaxOutput.Sent},
			Changes:   plan.Header(out.Changes), URL: out.URL, Header: out.Header, Body: string(out.Body),
		}})
	}
	write(w, http.StatusOK, struct {
		Targets []block `json:"targets"`
	}{blocks})
}

// scan is step 2 of the data plane on raw: the body cap (LIM-6) and the scan
// with the endpoint's inbound walk (PAR-1), refused as the data plane does.
func scan(raw []byte, walk [][]string, limit int64) (*params.Body, *domain.Error) {
	if int64(len(raw)) > limit {
		return nil, domain.NewError(domain.CodeRequestTooLarge, fmt.Sprintf("the body is larger than max_body_bytes, %d", limit))
	}
	b, pe := params.Scan(raw, walk)
	if pe != nil {
		e := domain.NewError(pe.Code, pe.Message)
		e.Param = pe.Param
		return nil, e
	}
	return b, nil
}

// quote is s as a JSON string. Its callers pass a published alias name or a
// string the strict decoder read, both valid UTF-8, so the error is unreachable.
func quote(s string) []byte {
	q, _ := jsontext.AppendQuote(nil, s)
	return q
}
