package host

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/console/paid"
	"github.com/jorlaru/stargaite/internal/transport"
)

// recorder is a test server that records each request and answers with
// answer.
type recorder struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (rec *recorder) got() []*http.Request {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return slices.Clone(rec.reqs)
}

func serve(t *testing.T, answer http.HandlerFunc) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.reqs = append(rec.reqs, r)
		rec.mu.Unlock()
		answer(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func models(w http.ResponseWriter, _ *http.Request) {
	_, _ = fmt.Fprint(w, `{"data":[{"id":"m1"},{"id":"m2\u001b[31m"}]}`)
}

// noKey fails when err holds key.
func noKey(t *testing.T, err error, key string) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), key) {
		t.Fatalf("the error holds the key: %v", err)
	}
}

func TestListModelsOpenAI(t *testing.T) {
	srv, rec := serve(t, models)
	key := `k"1\2`
	ids, status, err := New(t.TempDir()).ListModels(context.Background(),
		Provider{Protocol: "openai", BaseURL: srv.URL + "/v1", Key: key, AllowPrivate: true})
	noKey(t, err, key)
	if err != nil || status != 200 || !slices.Equal(ids, []string{"m1", "m2[31m"}) {
		t.Fatalf("ids %q, status %d, err %v", ids, status, err)
	}
	r := rec.got()[0]
	if r.Method != "GET" || r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer "+key || r.Header.Get("X-Api-Key") != "" {
		t.Fatalf("request %s %s, headers %v", r.Method, r.URL, r.Header)
	}
}

func TestListModelsAnthropic(t *testing.T) {
	srv, rec := serve(t, models)
	_, _, err := New(t.TempDir()).ListModels(context.Background(),
		Provider{Protocol: "anthropic", BaseURL: srv.URL + "/", Key: "sk-ant-1", AllowPrivate: true})
	noKey(t, err, "sk-ant-1")
	r := rec.got()[0]
	if err != nil || r.URL.Path != "/v1/models" || r.URL.RawQuery != "limit=1000" || r.Header.Get("X-Api-Key") != "sk-ant-1" ||
		r.Header.Get("Anthropic-Version") != "2023-06-01" || r.Header.Get("Authorization") != "" {
		t.Fatalf("err %v, request %s, headers %v", err, r.URL, r.Header)
	}
}

func TestListModelsNoKey(t *testing.T) {
	srv, rec := serve(t, models)
	h := New(t.TempDir())
	for _, protocol := range []string{"openai", "anthropic"} {
		if _, _, err := h.ListModels(context.Background(), Provider{Protocol: protocol, BaseURL: srv.URL, AllowPrivate: true}); err != nil {
			t.Fatalf("%s: %v", protocol, err)
		}
	}
	for _, r := range rec.got() {
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Fatalf("a credential header without a key: %v", r.Header)
		}
	}
}

func TestListModelsPath(t *testing.T) {
	srv, rec := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"data":{"label":"x","usage":0}}`)
	})
	ids, status, err := New(t.TempDir()).ListModels(context.Background(),
		Provider{Protocol: "openai", BaseURL: srv.URL + "/v1", Key: "sk-or-1", Path: "/key", AllowPrivate: true})
	r := rec.got()[0]
	if ids != nil || status != 200 || err != nil || r.URL.Path != "/v1/key" || r.Header.Get("Authorization") != "Bearer sk-or-1" {
		t.Fatalf("ids %q, status %d, err %v, path %s", ids, status, err, r.URL.Path)
	}
}

func TestHostSide(t *testing.T) {
	if got := hostSide("http://host.docker.internal:11434/v1"); got != "http://localhost:11434/v1" {
		t.Fatalf("hostSide = %q", got)
	}
	for _, tc := range []struct {
		p    Provider
		want string
	}{
		{Provider{Protocol: "openai", BaseURL: "http://host.docker.internal:9/v1/"}, "http://localhost:9/v1/models"},
		{Provider{Protocol: "openai", BaseURL: "http://host.docker.internal:9/v1/", Path: "/key"}, "http://localhost:9/v1/key"},
		{Provider{Protocol: "anthropic", BaseURL: "https://api.anthropic.com/"}, "https://api.anthropic.com/v1/models"},
	} {
		if got := tc.p.URL(); got != tc.want {
			t.Fatalf("%+v: URL = %q, want %q", tc.p, got, tc.want)
		}
	}
	for _, host := range []string{"localhost", "127.0.0.1", "127.1.2.3", "[::1]"} {
		for _, port := range []string{"", ":11434"} {
			got, ok := ToContainer("http://" + host + port + "/v1")
			if want := "http://host.docker.internal" + port + "/v1"; got != want || !ok {
				t.Fatalf("ToContainer(%s%s) = %q, %v", host, port, got, ok)
			}
		}
	}
	if got, ok := ToContainer("https://api.openai.com/v1"); got != "https://api.openai.com/v1" || ok {
		t.Fatalf("ToContainer of a public URL = %q, %v", got, ok)
	}
}

func TestListModelsStatus(t *testing.T) {
	for _, code := range []int{401, 502} {
		srv, rec := serve(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = fmt.Fprint(w, `{"error":"sk-bad"}`)
		})
		ids, status, err := New(t.TempDir()).ListModels(context.Background(), Provider{Protocol: "openai", BaseURL: srv.URL, Key: "sk-bad", AllowPrivate: true})
		if ids != nil || status != code || err != nil || len(rec.got()) != 1 {
			t.Fatalf("%d: ids %q, status %d, err %v, %d requests", code, ids, status, err, len(rec.got()))
		}
	}
}

func TestListModelsNotAList(t *testing.T) {
	for _, body := range []string{`{}`, `[]`, `not json`, `{"data":[]}`} {
		srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, body) })
		ids, status, err := New(t.TempDir()).ListModels(context.Background(), Provider{Protocol: "openai", BaseURL: srv.URL, AllowPrivate: true})
		if want := "the model list at " + srv.URL + "/models is empty or not a list"; ids != nil || status != 200 || err == nil || err.Error() != want {
			t.Fatalf("%s: ids %q, status %d, err %v", body, ids, status, err)
		}
	}
}

func TestListModelsNoRedirect(t *testing.T) {
	second, secondRec := serve(t, models)
	srv, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+"/models", http.StatusFound)
	})
	ids, status, err := New(t.TempDir()).ListModels(context.Background(), Provider{Protocol: "openai", BaseURL: srv.URL, Key: "k", AllowPrivate: true})
	if ids != nil || status != 302 || err != nil || len(secondRec.got()) != 0 {
		t.Fatalf("ids %q, status %d, err %v, %d requests to the second server", ids, status, err, len(secondRec.got()))
	}
}

func TestClientTransport(t *testing.T) {
	h := New(t.TempDir())
	c := h.client(true, false, nil, "", "")
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil || !tr.DisableKeepAlives || tr.ForceAttemptHTTP2 || c.CheckRedirect == nil || c.Timeout != h.httpBound ||
		tr.TLSClientConfig == nil || tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("client %+v, transport %+v", c, tr)
	}
}

func TestListModelsPrivate(t *testing.T) {
	srv, rec := serve(t, models)
	_, _, err := New(t.TempDir()).ListModels(context.Background(), Provider{Protocol: "openai", BaseURL: srv.URL, Key: "k"})
	if err == nil || !strings.Contains(err.Error(), "127.0.0.1 is a private address, and the provider does not allow private networks") ||
		len(rec.got()) != 0 {
		t.Fatalf("err %v, %d requests", err, len(rec.got()))
	}
}

// TestRefuseAsGuard: the free check refuses what the proxy's guard (SEC-4)
// refuses: a forbidden address always, a private one unless the provider
// allows private networks.
func TestRefuseAsGuard(t *testing.T) {
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700::1111", "100.64.0.1", "100.100.100.200", "168.63.129.16",
		"198.18.0.1", "192.0.0.192", "0.1.2.3", "0.0.0.0", "64:ff9b::a00:1", "64:ff9b::808:808", "64:ff9b::a9fe:a9fe",
		"169.254.169.254", "127.0.0.1", "::1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "fc00::1", "fd00:ec2::254",
		"fd20:ce::254", "fd00:42::42", "fd00:a9fe:a9fe::1", "fd00:c1::a9fe:a9fe", "224.0.0.1", "240.0.0.1", "255.255.255.255",
		"2001::1", "2002::1", "192.0.2.1", "198.51.100.1", "203.0.113.1", "2001:db8::1", "3fff::1", "fe80::1", "fe80::1%eth0",
		"::", "::ffff:10.0.0.1", "::ffff:8.8.8.8", "ff02::1", "fec0::1", "64:ff9b:1::1", "::a00:1"} {
		a := netip.MustParseAddr(s)
		class := transport.Classify(a)
		if got, want := refuse(a, false) != nil, class != transport.Public; got != want {
			t.Errorf("%s without private networks: refused %v, the guard's class %d", s, got, class)
		}
		if got, want := refuse(a, true) != nil, class == transport.Forbidden; got != want {
			t.Errorf("%s with private networks: refused %v, the guard's class %d", s, got, class)
		}
	}
	if err := refuse(netip.MustParseAddr("169.254.169.254"), true); err == nil ||
		err.Error() != "169.254.169.254 is link-local, cloud metadata or reserved, and is never reached" {
		t.Errorf("err = %v", err)
	}
}

// hang answers nothing until the request ends.
func hang(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }

func TestListModelsBound(t *testing.T) {
	if New("x").httpBound != 30*time.Second {
		t.Fatalf("httpBound = %v, want 30s", New("x").httpBound)
	}
	srv, _ := serve(t, hang)
	h := New(t.TempDir())
	h.httpBound = 100 * time.Millisecond
	start := time.Now()
	_, _, err := h.ListModels(context.Background(), Provider{Protocol: "openai", BaseURL: srv.URL, Key: "sk-hang", AllowPrivate: true})
	noKey(t, err, "sk-hang")
	if err == nil || !strings.Contains(err.Error(), srv.URL+"/models") || time.Since(start) > 2*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
}

func TestListModelsRefused(t *testing.T) {
	srv, _ := serve(t, models)
	base := srv.URL
	srv.Close()
	_, _, err := New(t.TempDir()).ListModels(context.Background(), Provider{Protocol: "openai", BaseURL: base, Key: "sk-x", AllowPrivate: true})
	noKey(t, err, "sk-x")
	if err == nil || !strings.HasPrefix(err.Error(), "the model list at "+base+"/models was not read: ") {
		t.Fatalf("err %v", err)
	}
}

func TestListModelsLimit(t *testing.T) {
	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"data":[{"id":"`+strings.Repeat("x", 2<<20)+`"}]}`)
	})
	if ids, _, err := New(t.TempDir()).ListModels(context.Background(), Provider{Protocol: "openai", BaseURL: srv.URL, AllowPrivate: true}); err == nil || ids != nil {
		t.Fatalf("a 2 MiB answer: ids %d, err %v", len(ids), err)
	}
	ids, _, err := New(t.TempDir()).ListModels(context.Background(), Provider{Protocol: "openai", BaseURL: srv.URL, AllowPrivate: true, Limit: 8 << 20})
	if err != nil || len(ids) != 1 {
		t.Fatalf("a 2 MiB answer under an 8 MiB limit: ids %d, err %v", len(ids), err)
	}
	over, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, strings.Repeat("x", 8<<20+1)) })
	_, _, err = New(t.TempDir()).ListModels(context.Background(), Provider{Protocol: "openai", BaseURL: over.URL, AllowPrivate: true, Limit: 8 << 20})
	if err == nil || err.Error() != "the model list at "+over.URL+"/models was not read: the answer is over 8 MiB" {
		t.Fatalf("over 8 MiB: err %v", err)
	}
}

// readyHost is a clone whose .env publishes srv's port.
func readyHost(t *testing.T, srv *httptest.Server) *Host {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	h := New(t.TempDir())
	write(t, filepath.Join(h.Dir, ".env"), "STARGAITE_PORT="+u.Port()+"\n")
	return h
}

func TestReadyPlain(t *testing.T) {
	var code atomic.Int32 // the handler runs on the server's goroutine
	code.Store(200)
	srv, rec := serve(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(int(code.Load())) })
	h := readyHost(t, srv)
	if err := h.Ready(context.Background()); err != nil || rec.got()[0].URL.Path != "/health/ready" {
		t.Fatalf("200: %v", err)
	}
	code.Store(503)
	if err := h.Ready(context.Background()); err == nil {
		t.Fatal("503 was taken as ready")
	}
}

func TestReadyTry(t *testing.T) {
	if New("x").ready.try != 2*time.Second {
		t.Fatalf("ready.try = %v", New("x").ready.try)
	}
	srv, _ := serve(t, hang)
	h := readyHost(t, srv)
	h.ready.try = 100 * time.Millisecond
	start := time.Now()
	if err := h.Ready(context.Background()); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
}

func TestReadyTLS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	h := readyHost(t, srv)
	write(t, filepath.Join(h.Dir, "compose.override.yaml"), "# setup: tls_name=example.com\n# setup: tls_ca=cert.pem\nservices: {}\n")
	cert := filepath.Join(h.Dir, "tls", "cert.pem")
	write(t, cert, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})))
	if err := h.Ready(context.Background()); err != nil {
		t.Fatalf("the server's own certificate: %v", err)
	}
	write(t, cert, string(otherCert(t)))
	if err := h.Ready(context.Background()); err == nil {
		t.Fatal("a certificate outside tls/ was trusted")
	}
}

// otherCert is a self-signed certificate for example.com, in PEM.
func otherCert(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"example.com"}, IsCA: true,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// planeReq is one request a data plane got.
type planeReq struct{ method, path, auth, ctype, body string }

// planeLog is what a data plane got, and a signal for each request whose
// context had ended when its answer returned.
type planeLog struct {
	mu    sync.Mutex
	reqs  []planeReq
	ended chan struct{}
}

func (l *planeLog) got() []planeReq {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.reqs)
}

// logged wraps answer so that l records each request.
func (l *planeLog) logged(answer http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		l.mu.Lock()
		l.reqs = append(l.reqs, planeReq{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), string(body)})
		l.mu.Unlock()
		answer(w, r)
		if r.Context().Err() != nil {
			l.ended <- struct{}{}
		}
	}
}

// servePlane is a clone whose data plane answers with answer.
func servePlane(t *testing.T, answer http.HandlerFunc) (*Host, *planeLog) {
	t.Helper()
	l := &planeLog{ended: make(chan struct{}, 10)}
	srv, _ := serve(t, l.logged(answer))
	return readyHost(t, srv), l
}

// budget opens a paid budget of n calls on a fresh session.
func budget(t *testing.T, n int) *paid.Budget {
	t.Helper()
	b, err := paid.NewSession().Open(n)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// clean fails when err holds the key or a Bearer header.
func clean(t *testing.T, err error, key string) {
	t.Helper()
	noKey(t, err, key)
	if err != nil && strings.Contains(err.Error(), "Bearer") {
		t.Fatalf("the error holds the header: %v", err)
	}
}

const tryBody = `{"model":"a","max_tokens":256,"messages":[{"role":"user","content":"Say hello in one short sentence."}]}`

func TestTryCompletionRequest(t *testing.T) {
	h, l := servePlane(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Stargaite-Target", "acme/m")
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"content":"hi\u001b[2J"},"finish_reason":"stop"}]}`)
	})
	key := `k"1\2`
	got, err := h.TryCompletion(context.Background(), budget(t, 1), 1, key, "a")
	clean(t, err, key)
	reqs := l.got()
	if err != nil || len(reqs) != 1 {
		t.Fatalf("err %v, %d requests", err, len(reqs))
	}
	want := planeReq{"POST", "/v1/chat/completions", "Bearer " + key, "application/json", tryBody}
	if reqs[0] != want {
		t.Fatalf("request %+v\nwant    %+v", reqs[0], want)
	}
	if got.Status != 200 || got.Content != "hi[2J" || got.FinishReason != "stop" || got.Target != "acme/m" || got.Message != "" || got.Took <= 0 {
		t.Fatalf("completion %+v", got)
	}
}

func TestTryCompletionAnswers(t *testing.T) {
	h, l := servePlane(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = fmt.Fprint(w, `{"error":{"message":"the provider failed","code":"provider_error"}}`)
	})
	got, err := h.TryCompletion(context.Background(), budget(t, 1), 1, "sk-k", "a")
	if err != nil || got.Status != 502 || got.Message != "the provider failed" || got.Content != "" || len(l.got()) != 1 {
		t.Fatalf("completion %+v, err %v, %d requests", got, err, len(l.got()))
	}

	h, l = servePlane(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), httpLimit+1))
	})
	_, err = h.TryCompletion(context.Background(), budget(t, 1), 1, "sk-k", "a")
	if err == nil || err.Error() != "the test call did not complete: the answer is over 1 MiB" || len(l.got()) != 1 {
		t.Fatalf("an answer over 1 MiB: err %v, %d requests", err, len(l.got()))
	}
}

// TestTryCompletionTakesFirst: no budget, too small a one, or a spent one is
// refused before anything is sent.
func TestTryCompletionTakesFirst(t *testing.T) {
	h, l := servePlane(t, func(http.ResponseWriter, *http.Request) {})
	spent := budget(t, 1)
	if err := spent.Take(1); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		b     *paid.Budget
		calls int
		want  string
	}{
		{nil, 1, "no paid budget: this call was not announced"},
		{budget(t, 1), 2, "2 paid calls asked, 1 left of what this step stated"},
		{spent, 1, "1 paid calls asked, 0 left of what this step stated"},
	} {
		if _, err := h.TryCompletion(context.Background(), tc.b, tc.calls, "sk-k", "a"); err == nil || err.Error() != tc.want {
			t.Errorf("err %v, want %q", err, tc.want)
		}
	}
	if n := len(l.got()); n != 0 {
		t.Fatalf("%d requests without a budget", n)
	}
}

// TestTryCompletionBound: the whole request is bounded by httpBound, and the
// server sees it end.
func TestTryCompletionBound(t *testing.T) {
	h, l := servePlane(t, hang)
	h.httpBound = 50 * time.Millisecond
	start := time.Now()
	_, err := h.TryCompletion(context.Background(), budget(t, 1), 1, "sk-hang", "a")
	if err == nil || err.Error() != "the test call did not complete: no answer within 50ms" || time.Since(start) > 2*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
	select {
	case <-l.ended:
	case <-time.After(time.Second):
		t.Fatal("the server never saw the request end")
	}
	if n := len(l.got()); n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
}

// TestTryCompletionCancel: ctx's end cancels the request at the server.
func TestTryCompletionCancel(t *testing.T) {
	h, l := servePlane(t, hang)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := h.TryCompletion(ctx, budget(t, 1), 1, "sk-k", "a")
		done <- err
	}()
	for deadline := time.Now().Add(2 * time.Second); len(l.got()) == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the request never arrived")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("TryCompletion did not return")
	}
	select {
	case <-l.ended:
	case <-time.After(time.Second):
		t.Fatal("the server's request did not end within 1 s of the cancel")
	}
	if n := len(l.got()); n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
}

// TestTryCompletionTransport: no redirect followed, no proxy from the
// environment, only tls/<CA> trusted; no error holds the key.
func TestTryCompletionTransport(t *testing.T) {
	const key = "sk-transport"
	ctx := context.Background()
	second, secondLog := servePlane(t, func(http.ResponseWriter, *http.Request) {})
	sp, _ := second.Plane()
	h, _ := servePlane(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sp.BaseURL+"/chat/completions", http.StatusTemporaryRedirect)
	})
	got, err := h.TryCompletion(ctx, budget(t, 1), 1, key, "a")
	clean(t, err, key)
	if err != nil || got.Status != http.StatusTemporaryRedirect || len(secondLog.got()) != 0 {
		t.Fatalf("redirect: completion %+v, err %v, %d requests to the second server", got, err, len(secondLog.got()))
	}

	// Go never sends a loopback request through an environment proxy, so the
	// try's own client is looked at instead.
	if _, c, err := h.planeClient("127.0.0.1", true); err != nil || c.Transport.(*http.Transport).Proxy != nil {
		t.Fatalf("proxy: the try's client takes a proxy (err %v)", err)
	}

	l := &planeLog{ended: make(chan struct{}, 10)}
	srv := httptest.NewTLSServer(l.logged(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	h = readyHost(t, srv)
	write(t, filepath.Join(h.Dir, "compose.override.yaml"), "# setup: tls_name=example.com\n# setup: tls_ca=ca.pem\nservices: {}\n")
	ca := filepath.Join(h.Dir, "tls", "ca.pem")
	write(t, ca, string(otherCert(t)))
	_, err = h.TryCompletion(ctx, budget(t, 1), 1, key, "a")
	clean(t, err, key)
	if err == nil || !strings.HasPrefix(err.Error(), "the test call did not complete: ") || len(l.got()) != 0 {
		t.Fatalf("a certificate outside tls/: err %v, %d requests", err, len(l.got()))
	}
	write(t, ca, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})))
	if got, err := h.TryCompletion(ctx, budget(t, 1), 1, key, "a"); err != nil || got.Status != 200 || len(l.got()) != 1 {
		t.Fatalf("tls/ca.pem: completion %+v, err %v, %d requests", got, err, len(l.got()))
	}
}

// listenOn listens on addr. 127.0.0.2 and its like are on Linux's loopback;
// elsewhere they may not be configured, and the test is skipped.
func listenOn(t *testing.T, addr string) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", addr)
	if err != nil && runtime.GOOS != "linux" {
		t.Skipf("%s is not configured on this host's loopback", addr)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// tlsPlane serves /health/ready under TLS with files' pair on 127.0.0.1
// and 127.0.0.2 at one port, and logs the local address of each request.
func tlsPlane(t *testing.T, files TLSFiles) (port int, local func() []string) {
	t.Helper()
	pair, err := tls.X509KeyPair(files["cert.pem"], files["key.pem"])
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen []string
	srv := &http.Server{TLSConfig: &tls.Config{Certificates: []tls.Certificate{pair}},
		Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, r.Context().Value(http.LocalAddrContextKey).(net.Addr).String())
		})}
	one := listenOn(t, "127.0.0.1:0")
	port = one.Addr().(*net.TCPAddr).Port
	two := listenOn(t, "127.0.0.2:"+strconv.Itoa(port))
	for _, l := range []net.Listener{one, two} {
		go func() { _ = srv.ServeTLS(l, "", "") }()
	}
	t.Cleanup(func() { _ = srv.Close() })
	return port, func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(seen) }
}

func TestCheckHTTPS(t *testing.T) {
	files, err := SelfSigned([]string{"127.0.0.1", "127.0.0.2"})
	if err != nil {
		t.Fatal(err)
	}
	port, local := tlsPlane(t, files)
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:"+strconv.Itoa(freePort(t)))
	h := New(t.TempDir())
	write(t, filepath.Join(h.Dir, ".env"), "STARGAITE_PORT="+strconv.Itoa(port)+"\n")
	write(t, filepath.Join(h.Dir, "compose.override.yaml"), "# setup: tls_name=localhost\n# setup: tls_ca=cert.pem\nservices: {}\n")
	write(t, filepath.Join(h.Dir, "tls", "cert.pem"), string(files["cert.pem"]))
	p := strconv.Itoa(port)
	for _, tc := range []struct{ addr, at string }{{"127.0.0.1", "127.0.0.1:" + p}, {"127.0.0.2", "127.0.0.2:" + p}, {"0.0.0.0", "127.0.0.1:" + p}} {
		if err := h.CheckHTTPS(context.Background(), tc.addr); err != nil {
			t.Fatalf("%s: %v", tc.addr, err)
		}
		if got := local(); got[len(got)-1] != tc.at {
			t.Fatalf("%s: dialled %q, want %s", tc.addr, got, tc.at)
		}
	}
	other, err := SelfSigned([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(h.Dir, "tls", "cert.pem"), string(other["cert.pem"]))
	if err := h.CheckHTTPS(context.Background(), "127.0.0.1"); err == nil {
		t.Fatal("a certificate outside tls/ was trusted")
	}

	// The host's own addresses are no provider's: no guard refuses them.
	h.httpBound = 300 * time.Millisecond
	dial := "169.254.1.1:" + strconv.Itoa(freePort(t))
	_, unguarded := h.client(false, true, nil, dial, "").Get("https://x/health/ready")
	_, guarded := h.client(true, true, nil, dial, "").Get("https://x/health/ready")
	const refused = "is link-local, cloud metadata or reserved"
	if unguarded == nil || strings.Contains(unguarded.Error(), refused) || guarded == nil || !strings.Contains(guarded.Error(), refused) {
		t.Fatalf("unguarded %v, guarded %v", unguarded, guarded)
	}
}
