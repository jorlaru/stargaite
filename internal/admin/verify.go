package admin

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/domain"
)

// verifyTarget is targets verify (RSN-9): POST /admin/v1/verify/{id...} with
// {"max_output_tokens": n}, n optional. Each call of the run is bounded by
// total_ms (RES-3), so once the body is read the read deadline is cleared
// and the write deadline set once to now + k × (total_ms + 5 s) for the k
// calls the run makes; a deadline that cannot be set is a 500 before any
// call. The answer is the Verification plus stored. Every run is audited by
// Config.Verified, whose write outlives the request's context (SEC-7); a
// refusal before any call (404, 400) runs nothing and is not. No revision.
func (s *server) verifyTarget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.URL.RawQuery != "" {
		s.fail(w, invalid("this route takes no query parameter"))
		return
	}
	data, err := body(w, r, maxBody)
	if err != nil {
		s.fail(w, err)
		return
	}
	var in struct {
		MaxOutputTokens *int64 `json:"max_output_tokens"`
	}
	if err := config.Decode(data, &in); err != nil {
		s.fail(w, invalid("%v", err))
		return
	}
	snap := s.cfg.Snapshot()
	calls := 1
	if t, ok := snap.Doc.Targets[id]; ok {
		calls = len(domain.VerifyLevels(t.Reasoning))
	}
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Time{}); err != nil {
		s.fail(w, fmt.Errorf("verify: the read deadline was not cleared: %w", err))
		return
	}
	wait := time.Duration(calls) * (time.Duration(snap.Settings.Timeouts.TotalMS)*time.Millisecond + 5*time.Second)
	if err := rc.SetWriteDeadline(time.Now().Add(wait)); err != nil {
		s.fail(w, fmt.Errorf("verify: the write deadline was not set: %w", err))
		return
	}
	v, e := s.verify(r.Context(), id, in.MaxOutputTokens)
	if e != nil {
		s.fail(w, e)
		return
	}
	// The audit row's new_value: each call without its body.
	type called struct {
		Effort          string  `json:"effort"`
		EffortSent      *string `json:"effort_sent"`
		Status          int     `json:"status"`
		ReasoningTokens *int64  `json:"reasoning_tokens"`
		OutputTokens    int64   `json:"output_tokens"`
		RequestID       string  `json:"request_id"`
		Error           string  `json:"error,omitempty"` // its code
	}
	summary := []called{}
	for _, c := range v.Calls {
		k := called{c.Effort, c.EffortSent, c.Status, c.ReasoningTokens, c.OutputTokens, c.RequestID, ""}
		if c.Error != nil {
			k.Error = c.Error.Code
		}
		summary = append(summary, k)
	}
	text, err := config.Encode(summary)
	if err != nil {
		s.fail(w, err)
		return
	}
	stored, err := s.cfg.Verified(context.WithoutCancel(r.Context()), id, v.Spec, v.Provider, v.OK, v.At, v.Version, string(text))
	if err != nil {
		s.fail(w, fmt.Errorf("verify: the result was not recorded: %w", err))
		return
	}
	write(w, http.StatusOK, struct {
		domain.Verification
		Stored bool `json:"stored"`
	}{v, stored})
}
