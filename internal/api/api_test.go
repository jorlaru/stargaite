package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthLive(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/health/live", http.StatusOK},
		{http.MethodPost, "/health/live", http.StatusMethodNotAllowed},
		{http.MethodGet, "/health/other", http.StatusNotFound},
	} {
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.status {
			t.Errorf("%s %s: status %d, want %d", tc.method, tc.path, rec.Code, tc.status)
		}
		if tc.status != http.StatusOK {
			continue
		}
		var body struct{ Status string }
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Status != "ok" {
			t.Errorf("body %q: status %q, error %v", rec.Body, body.Status, err)
		}
		if got := rec.Header().Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type %q", got)
		}
	}
}
