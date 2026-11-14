package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// TestH2ClientGone cancels requests over HTTP/2 while the provider call is
// running. The deadline hook of infer must not touch the response writer
// after the handler returned: on HTTP/2 that is a data race and a nil
// pointer panic that ends the process. -race reports it on the first runs.
func TestH2ClientGone(t *testing.T) {
	s, _, yaml := newResilience(t)
	s.apply(t, longTimers.Replace(yaml))
	data := httptest.NewUnstartedServer(s.data)
	data.EnableHTTP2 = true
	data.StartTLS()
	t.Cleanup(data.Close)
	client := data.Client()
	for i := range 50 {
		s.mock.Script(mockprovider.Step{Delay: time.Minute})
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, data.URL+"/v1/chat/completions", strings.NewReader(chatBody("bad-one", "")))
		req.Header.Set("Authorization", "Bearer "+s.token)
		errc := make(chan error, 1)
		go func() {
			resp, err := client.Do(req)
			if err == nil {
				if resp.ProtoMajor != 2 {
					t.Errorf("proto %s", resp.Proto)
				}
				_ = resp.Body.Close()
			}
			errc <- err
		}()
		reached(t, s.mock, i+1)
		cancel()
		if err := <-errc; err == nil {
			t.Fatalf("request %d answered; want it cancelled", i)
		}
		if c := ended(t, s.mock, i); !c.Cancelled {
			t.Fatalf("request %d: the provider call was not cancelled", i)
		}
		waitFlight(t, s.fixture)
	}
}
