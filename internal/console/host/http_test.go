package host

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	c := h.client(false, nil, "", "")
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
	if err := h.isReady(context.Background()); err != nil || rec.got()[0].URL.Path != "/health/ready" {
		t.Fatalf("200: %v", err)
	}
	code.Store(503)
	if err := h.isReady(context.Background()); err == nil {
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
	if err := h.isReady(context.Background()); err == nil || time.Since(start) > 2*time.Second {
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
	if err := h.isReady(context.Background()); err != nil {
		t.Fatalf("the server's own certificate: %v", err)
	}
	write(t, cert, string(otherCert(t)))
	if err := h.isReady(context.Background()); err == nil {
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
