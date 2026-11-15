package admin

import (
	"encoding/json/jsontext"
	"net/http"

	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/telemetry"
)

// telemetryPresets are the merge patches telemetry set --preset applies to
// the stored block before the explicit members (TEL-11): the conventional
// variable names of the credentials and the static headers, nothing more.
var telemetryPresets = map[string]string{
	"langfuse": `{"basic_auth_env":["LANGFUSE_PUBLIC_KEY","LANGFUSE_SECRET_KEY"],"headers":{"x-langfuse-ingestion-version":"4"}}`,
}

// telemetryView is what telemetry status answers: the effective block, the
// references of the published exporter that do not resolve, by name, and the
// exporter's counters.
type telemetryView struct {
	Telemetry  *domain.Telemetry `json:"telemetry"`
	Unresolved []string          `json:"unresolved,omitzero"`
	Exporter   telemetry.Status  `json:"exporter"`
}

func (s *server) showTelemetry(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		s.fail(w, invalid("this route takes no query parameter"))
		return
	}
	snap := s.cfg.Snapshot()
	v := telemetryView{Telemetry: snap.Settings.Telemetry, Exporter: s.spans.Status()}
	if snap.Exporter != nil {
		v.Unresolved = snap.Exporter.Unresolved
	}
	write(w, http.StatusOK, v)
}

// setTelemetry is telemetry set: the body is a merge patch of
// settings.telemetry plus an optional preset, applied first. Only the result
// is stored; it is validated (TEL-2, SEC-4, SEC-5) and audited like any
// change of settings.
func (s *server) setTelemetry(w http.ResponseWriter, r *http.Request) {
	data, err := body(w, r, maxBody)
	if err != nil {
		s.fail(w, err)
		return
	}
	var members map[string]jsontext.Value
	if err := config.Decode(data, &members); err != nil {
		s.fail(w, invalid("%v", err))
		return
	}
	var preset string
	if raw, ok := members["preset"]; ok {
		if config.Decode(raw, &preset) != nil {
			s.fail(w, invalid("preset must be a string"))
			return
		}
		delete(members, "preset")
	}
	from, known := telemetryPresets[preset]
	if preset != "" && !known {
		s.fail(w, invalid("preset %.64q does not exist; the only one is langfuse", preset))
		return
	}
	patch, err := config.Encode(members)
	if err != nil {
		s.fail(w, err)
		return
	}
	var declared domain.Telemetry
	res, err := s.cfg.Commit(r.Context(), config.Change{Summary: "telemetry set", Kind: config.KindSetting, Name: "telemetry", Mutate: func(st *config.State) (map[string]string, error) {
		var t domain.Telemetry
		if st.Doc.Settings.Telemetry != nil {
			t = *st.Doc.Settings.Telemetry
		}
		if preset != "" {
			if err := merge(&t, jsontext.Value(from)); err != nil {
				return nil, err
			}
		}
		if err := merge(&t, patch); err != nil {
			return nil, err
		}
		st.Doc.Settings.Telemetry, declared = &t, t
		return nil, nil
	}})
	if err != nil {
		s.fail(w, err)
		return
	}
	write(w, http.StatusOK, written{res, config.WithDefaults(domain.Settings{Telemetry: &declared}).Telemetry})
}

// unsetTelemetry is telemetry unset: the block goes, and trust_traceparent
// and content_max_bytes return to their defaults. Spans still queued are
// dropped and counted by the exporter.
func (s *server) unsetTelemetry(w http.ResponseWriter, r *http.Request) {
	res, err := s.cfg.Commit(r.Context(), config.Change{Summary: "telemetry unset", Kind: config.KindSetting, Name: "telemetry", Mutate: func(st *config.State) (map[string]string, error) {
		st.Doc.Settings.Telemetry = nil
		return nil, nil
	}})
	if err != nil {
		s.fail(w, err)
		return
	}
	write(w, http.StatusOK, res)
}

// testTelemetry is telemetry test (TEL-2): one span through the published
// exporter. Not audited: it changes nothing.
func (s *server) testTelemetry(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		s.fail(w, invalid("this route takes no query parameter"))
		return
	}
	if s.cfg.Snapshot().Exporter == nil {
		s.fail(w, invalid("no exporter is configured: telemetry set --otlp-endpoint <url> sets one"))
		return
	}
	write(w, http.StatusOK, s.spans.Test(r.Context()))
}
