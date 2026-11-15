package admin

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// telemetryEnv resolves the references of the telemetry tests.
var telemetryEnv = map[string]string{"OTLP_USER": "pk-lf-1", "OTLP_PASSWORD": "sk-lf-secret", "OTLP_TOKEN": "tok-secret"}

func newTelemetryFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureEnv(t, func() config.Env {
		return config.Env{Lookup: func(n string) (string, bool) { v, ok := telemetryEnv[n]; return v, ok }, Resolve: resolve}
	})
}

type telemetryShown struct {
	Telemetry  domain.Telemetry `json:"telemetry"`
	Unresolved []string         `json:"unresolved"`
	Exporter   struct {
		Sent              int64            `json:"sent"`
		RejectedByStatus  map[string]int64 `json:"rejected_by_status"`
		RejectedByBackend int64            `json:"rejected_by_backend"`
		Dropped           int64            `json:"dropped"`
		Queued            int              `json:"queued"`
		LastError         string           `json:"last_error"`
	} `json:"exporter"`
}

type testResult struct {
	Outcome string `json:"outcome"`
	Status  int    `json:"status"`
	Message string `json:"message"`
	TraceID string `json:"trace_id"`
}

func (f *fixture) telemetry(t *testing.T) telemetryShown {
	t.Helper()
	var v telemetryShown
	decode(t, f.do(t, http.MethodGet, "/admin/v1/telemetry", nil), http.StatusOK, &v)
	return v
}

func (f *fixture) test(t *testing.T) testResult {
	t.Helper()
	var r testResult
	decode(t, f.do(t, http.MethodPost, "/admin/v1/telemetry/test", nil), http.StatusOK, &r)
	return r
}

// TestTelemetryRoutes: telemetry set merges into settings.telemetry through
// the pipeline, validated and audited; telemetry test sends one span with
// the header composed from the referenced variables to a mock receiver
// (AC-24); telemetry status shows the effective block and the counters
// (AC-31); telemetry unset removes the block.
func TestTelemetryRoutes(t *testing.T) {
	f := newTelemetryFixture(t)
	receiver := mockprovider.New()
	srv := httptest.NewServer(receiver)
	t.Cleanup(srv.Close)

	// Without an exporter: a 400, and the defaults without a block.
	errorOf(t, f.do(t, http.MethodPost, "/admin/v1/telemetry/test", nil), http.StatusBadRequest, domain.CodeInvalidRequest)
	if v := f.telemetry(t); v.Telemetry.TrustTraceparent == nil || !*v.Telemetry.TrustTraceparent || v.Telemetry.ContentMaxBytes != domain.DefaultContentMaxBytes ||
		v.Telemetry.OTLPEndpoint != "" || v.Exporter.Sent != 0 || v.Exporter.RejectedByStatus == nil {
		t.Errorf("status without a block %+v", v)
	}

	// SEC-4: a loopback endpoint needs allow_private_network; refused, audited.
	set := `{"otlp_endpoint":"` + srv.URL + `","basic_auth_env":["OTLP_USER","OTLP_PASSWORD"],"header_env":{"x-token":"OTLP_TOKEN"},"headers":{"x-tag":"a,b"}}`
	e := errorOf(t, f.do(t, http.MethodPatch, "/admin/v1/telemetry", []byte(set)), http.StatusBadRequest, domain.CodeInvalidRequest)
	if len(e.Findings) != 1 || e.Findings[0].Field != "telemetry.otlp_endpoint" || f.lastAudit(t) != "telemetry set setting telemetry refused:invalid_request" {
		t.Errorf("findings %+v, audit %q", e.Findings, f.lastAudit(t))
	}

	var res struct {
		config.Result
		Resource domain.Telemetry `json:"resource"`
	}
	decode(t, f.do(t, http.MethodPatch, "/admin/v1/telemetry", []byte(strings.Replace(set, `{`, `{"allow_private_network":true,`, 1))), http.StatusOK, &res)
	if !res.Changed || len(res.Diff) != 1 || res.Diff[0].Kind != config.KindSetting || res.Diff[0].Name != "telemetry" || res.Resource.OTLPEndpoint != srv.URL ||
		res.Resource.ContentMaxBytes != domain.DefaultContentMaxBytes || f.lastAudit(t) != "telemetry set setting telemetry applied" {
		t.Errorf("set %+v, audit %q", res, f.lastAudit(t))
	}

	r := f.test(t)
	calls := receiver.Calls()
	if r.Outcome != "accepted" || r.Status != 200 || len(r.TraceID) != 32 || len(calls) != 1 || calls[0].Path != "/v1/traces" {
		t.Fatalf("test %+v, calls %+v", r, calls)
	}
	h := calls[0].Header
	if h.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("pk-lf-1:sk-lf-secret")) || h.Get("X-Token") != "tok-secret" ||
		h.Get("X-Tag") != "a,b" || h.Get("Content-Type") != "application/json" || !strings.Contains(string(calls[0].Body), r.TraceID) ||
		!json.Valid(calls[0].Body) || !strings.Contains(string(calls[0].Body), `"stringValue":"_telemetry"`) {
		t.Errorf("the receiver got %v %s", h, calls[0].Body)
	}
	if n := f.count(t, "SELECT COUNT(*) FROM audit WHERE action = 'telemetry test'"); n != 0 {
		t.Errorf("telemetry test was audited")
	}

	// Rejections count by status, with the last error, and a test answers
	// 200 with the outcome.
	receiver.Script(mockprovider.Step{Status: 401, Body: []byte("echo sk-lf-secret tok-secret")}, mockprovider.Step{Status: 404}, mockprovider.Step{Status: 500})
	for _, want := range []string{"unauthorized", "not_found", "rejected"} {
		if r := f.test(t); r.Outcome != want {
			t.Errorf("test %+v, want %s", r, want)
		}
	}
	v := f.telemetry(t)
	if v.Exporter.Sent != 1 || v.Exporter.RejectedByStatus["401"] != 1 || v.Exporter.RejectedByStatus["404"] != 1 || v.Exporter.RejectedByStatus["500"] != 1 ||
		v.Exporter.LastError != "500 Internal Server Error" || v.Telemetry.OTLPEndpoint != srv.URL || len(v.Unresolved) != 0 {
		t.Errorf("status %+v", v)
	}

	// A header_env whose variable is unset: credential_unresolved, nothing sent.
	f.do(t, http.MethodPatch, "/admin/v1/telemetry", []byte(`{"header_env":{"x-token":"OTLP_MISSING"}}`))
	before := len(receiver.Calls())
	if r := f.test(t); r.Outcome != "credential_unresolved" || len(receiver.Calls()) != before || !strings.Contains(r.Message, "OTLP_MISSING") {
		t.Errorf("test %+v", r)
	}
	if v := f.telemetry(t); !slices.Equal(v.Unresolved, []string{"OTLP_MISSING"}) || v.Exporter.Dropped != 1 ||
		v.Exporter.LastError != "credential_unresolved: OTLP_MISSING does not resolve" {
		t.Errorf("status %+v", v)
	}

	// Nothing shows a credential value: answers, the store, the log.
	everything := f.dump(t) + f.log.String() + f.do(t, http.MethodGet, "/admin/v1/telemetry", nil).Body.String() + string(f.export(t))
	for _, secret := range telemetryEnv {
		if strings.Contains(everything, secret) {
			t.Errorf("%q shows up", secret)
		}
	}

	// unset: the block goes, the defaults stay, test is a 400 again.
	if w := f.do(t, http.MethodDelete, "/admin/v1/telemetry", nil); w.Code != http.StatusOK || f.lastAudit(t) != "telemetry unset setting telemetry applied" {
		t.Errorf("unset: %d %s, audit %q", w.Code, w.Body, f.lastAudit(t))
	}
	if v := f.telemetry(t); v.Telemetry.OTLPEndpoint != "" || v.Telemetry.ContentMaxBytes != domain.DefaultContentMaxBytes || f.cfg.Snapshot().Doc.Settings.Telemetry != nil {
		t.Errorf("after unset %+v", v)
	}
	errorOf(t, f.do(t, http.MethodPost, "/admin/v1/telemetry/test", nil), http.StatusBadRequest, domain.CodeInvalidRequest)
	errorOf(t, f.do(t, http.MethodGet, "/admin/v1/telemetry?x=1", nil), http.StatusBadRequest, domain.CodeInvalidRequest)
}

// TestTelemetryPreset: --preset langfuse fills basic_auth_env and the
// ingestion-version header; explicit members win; an unknown preset and a
// reserved static header are refused (TEL-2, TEL-11).
func TestTelemetryPreset(t *testing.T) {
	f := newTelemetryFixture(t)
	var res struct {
		config.Result
		Resource domain.Telemetry `json:"resource"`
	}
	decode(t, f.do(t, http.MethodPatch, "/admin/v1/telemetry", []byte(`{"preset":"langfuse","otlp_endpoint":"https://langfuse.example.com/api/public/otel"}`)), http.StatusOK, &res)
	if !slices.Equal(res.Resource.BasicAuthEnv, []string{"LANGFUSE_PUBLIC_KEY", "LANGFUSE_SECRET_KEY"}) || res.Resource.Headers["x-langfuse-ingestion-version"] != "4" ||
		res.Resource.AllowPrivateNetwork {
		t.Errorf("preset %+v", res.Resource)
	}
	if v := f.telemetry(t); !slices.Equal(v.Unresolved, []string{"LANGFUSE_PUBLIC_KEY", "LANGFUSE_SECRET_KEY"}) {
		t.Errorf("status %+v", v)
	}
	res.Resource = domain.Telemetry{} // decoding merges into a map
	decode(t, f.do(t, http.MethodPatch, "/admin/v1/telemetry", []byte(`{"preset":"langfuse","basic_auth_env":["OTLP_USER","OTLP_PASSWORD"],"headers":{"x-langfuse-ingestion-version":null}}`)), http.StatusOK, &res)
	if !slices.Equal(res.Resource.BasicAuthEnv, []string{"OTLP_USER", "OTLP_PASSWORD"}) || len(res.Resource.Headers) != 0 ||
		res.Resource.OTLPEndpoint != "https://langfuse.example.com/api/public/otel" {
		t.Errorf("explicit members %+v", res.Resource)
	}
	errorOf(t, f.do(t, http.MethodPatch, "/admin/v1/telemetry", []byte(`{"preset":"datadog"}`)), http.StatusBadRequest, domain.CodeInvalidRequest)
	errorOf(t, f.do(t, http.MethodPatch, "/admin/v1/telemetry", []byte(`{"preset":4}`)), http.StatusBadRequest, domain.CodeInvalidRequest)
	errorOf(t, f.do(t, http.MethodPatch, "/admin/v1/telemetry", []byte(`[]`)), http.StatusBadRequest, domain.CodeInvalidRequest)
	errorOf(t, f.do(t, http.MethodPatch, "/admin/v1/telemetry", []byte(`{"unknown":1}`)), http.StatusBadRequest, domain.CodeInvalidRequest)
	e := errorOf(t, f.do(t, http.MethodPatch, "/admin/v1/telemetry", []byte(`{"headers":{"Authorization":"Bearer sk-pasted"}}`)), http.StatusBadRequest, domain.CodeInvalidRequest)
	if len(e.Findings) != 1 || e.Findings[0].Code != "reserved_header" || strings.Contains(f.dump(t), "sk-pasted") {
		t.Errorf("findings %+v", e.Findings)
	}
}

// TestTelemetryResolvesPrivate: an endpoint whose name resolved public when
// it was set and resolves to loopback at dial time is refused there (AC-43).
func TestTelemetryResolvesPrivate(t *testing.T) {
	f := newTelemetryFixture(t)
	receiver := mockprovider.New()
	srv := httptest.NewServer(receiver)
	t.Cleanup(srv.Close)
	endpoint := strings.Replace(srv.URL, "http://127.0.0.1", "https://otel.example.com", 1)
	if w := f.do(t, http.MethodPatch, "/admin/v1/telemetry", []byte(`{"otlp_endpoint":"`+endpoint+`"}`)); w.Code != http.StatusOK {
		t.Fatalf("set: %d %s", w.Code, w.Body)
	}
	if r := f.test(t); r.Outcome != "unreachable" || !strings.Contains(r.Message, "private") || len(receiver.Calls()) != 0 {
		t.Errorf("test %+v, %d calls", r, len(receiver.Calls()))
	}
}
