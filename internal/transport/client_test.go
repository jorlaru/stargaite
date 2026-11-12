package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// host is the name every test URL uses; only the injected resolver knows it.
const host = "provider.test"

// mock serves a mock provider on loopback and returns it with its port.
func mock(t *testing.T) (*mockprovider.Server, string) {
	t.Helper()
	m := mockprovider.New()
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return m, port
}

// answer resolves host to addrs and fails the test on any other lookup.
func answer(t *testing.T, addrs ...string) Resolver {
	return func(_ context.Context, h string) ([]netip.Addr, error) {
		if h != host {
			t.Errorf("unexpected lookup of %q", h)
			return nil, errors.New("unexpected lookup")
		}
		out := []netip.Addr{}
		for _, a := range addrs {
			out = append(out, netip.MustParseAddr(a))
		}
		return out, nil
	}
}

// post bounds the call, so a broken guard fails the test instead of waiting
// on a connection.
func post(c *http.Client, url string, body []byte) (*http.Response, error) {
	c.Timeout = 5 * time.Second
	resp, err := c.Post(url, "application/json", bytes.NewReader(body))
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	return resp, err
}

// SEC-4 at dial time: every answer of the resolver is classified, and a
// literal address too, before anything is dialled.
func TestClientDialTimeVerdict(t *testing.T) {
	m, port := mock(t)
	for _, tt := range []struct {
		name     string
		allow    bool
		answers  []string
		url      string // default http://provider.test:<port>/chat/completions
		want     string // "" when the call reaches the mock
		resolver Resolver
	}{
		{name: "loopback with the flag", allow: true, answers: []string{"127.0.0.1"}},
		{name: "loopback without the flag", answers: []string{"127.0.0.1"}, want: "is private"},
		{name: "IPv4-mapped loopback without the flag", answers: []string{"::ffff:127.0.0.1"}, want: "is private"},
		{name: "one private address among public ones", answers: []string{"8.8.8.8", "127.0.0.1"}, want: host + ": address 127.0.0.1 is private"},
		{name: "link-local metadata with the flag", allow: true, answers: []string{"169.254.169.254"}, want: "never allowed"},
		{name: "listed metadata with the flag", allow: true, answers: []string{"168.63.129.16"}, want: "never allowed"},
		{name: "listed IPv6 metadata with the flag", allow: true, answers: []string{"fd00:ec2::254"}, want: "never allowed"},
		{name: "listed metadata in carrier-grade NAT with the flag", allow: true, answers: []string{"100.100.100.200"}, want: "never allowed"},
		{name: "link-local metadata behind NAT64 with the flag", allow: true, answers: []string{"64:ff9b::a9fe:a9fe"}, want: "never allowed"},
		{name: "metadata among loopback with the flag", allow: true, answers: []string{"127.0.0.1", "169.254.169.254"}, want: "never allowed"},
		{name: "empty answer", allow: true, answers: []string{}, want: "no address"},
		{name: "failed lookup", allow: true, want: "no such host",
			resolver: func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("no such host") }},
		{name: "literal metadata address with the flag", allow: true, url: "http://169.254.169.254/latest/meta-data/", want: "never allowed"},
		{name: "literal unspecified address with the flag", allow: true, url: "http://0.0.0.0:" + port + "/chat/completions", want: "never allowed"},
		{name: "literal loopback without the flag", url: "http://127.0.0.1:" + port + "/chat/completions", want: "is private"},
		{name: "literal loopback with the flag", allow: true, url: "http://127.0.0.1:" + port + "/chat/completions"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := len(m.Calls())
			resolve := tt.resolver
			if resolve == nil {
				resolve = answer(t, tt.answers...)
			}
			url := tt.url
			if url == "" {
				url = "http://" + host + ":" + port + "/chat/completions"
			}
			resp, err := post(NewClient(tt.allow, resolve), url, []byte(`{}`))
			reached := len(m.Calls()) - before
			switch {
			case tt.want == "" && (err != nil || resp.StatusCode != http.StatusOK || reached != 1):
				t.Errorf("err %v, %d calls", err, reached)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want) || reached != 0):
				t.Errorf("err %v, want %q; %d calls", err, tt.want, reached)
			}
		})
	}
}

// The dialer's own check, on the address actually connected to.
func TestDialerControl(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	ctx := context.Background()
	if _, err := dialer(false).DialContext(ctx, "tcp", ln.Addr().String()); err == nil || !strings.Contains(err.Error(), "is private") {
		t.Errorf("loopback without the flag: %v", err)
	}
	// 0.0.0.0 is Forbidden and, dialled, reaches the local host: the listener.
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	if _, err := dialer(true).DialContext(ctx, "tcp", "0.0.0.0:"+port); err == nil || !strings.Contains(err.Error(), "never allowed") {
		t.Errorf("unspecified address with the flag: %v", err)
	}
	conn, err := dialer(true).DialContext(ctx, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("loopback with the flag: %v", err)
	}
	_ = conn.Close()
}

// SEC-4: a 3xx is returned as the response, never followed.
func TestClientDoesNotFollowRedirects(t *testing.T) {
	m, port := mock(t)
	c := NewClient(true, answer(t, "127.0.0.1"))
	url := "http://" + host + ":" + port + "/chat/completions"
	for _, location := range []string{url, "http://169.254.169.254/latest/meta-data/"} {
		before := len(m.Calls())
		m.Script(mockprovider.Step{Status: http.StatusFound, Header: http.Header{"Location": {location}}})
		resp, err := post(c, url, []byte(`{}`))
		if err != nil || resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != location {
			t.Fatalf("%s: err %v, resp %v", location, err, resp)
		}
		if n := len(m.Calls()) - before; n != 1 {
			t.Errorf("%s: %d calls, want 1", location, n)
		}
	}
}

// SEC-4: proxy variables are ignored, so the verdict is on the destination.
// Checked on the field: http.ProxyFromEnvironment reads the variables once
// per process, so setting them in a test proves nothing.
func TestClientIgnoresProxyVariables(t *testing.T) {
	if p := NewClient(true, answer(t)).Transport.(*http.Transport).Proxy; p != nil {
		t.Error("the client has a proxy function")
	}
}

// Certificates are always verified: the test server's own is not trusted.
func TestClientVerifiesCertificates(t *testing.T) {
	m := mockprovider.New()
	srv := httptest.NewTLSServer(m)
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	_, err := post(NewClient(true, answer(t, "127.0.0.1")), "https://"+host+":"+port+"/v1/messages", []byte(`{}`))
	var verify *tls.CertificateVerificationError
	if !errors.As(err, &verify) || len(m.Calls()) != 0 {
		t.Errorf("err %v, %d calls", err, len(m.Calls()))
	}
}

// The two clients of phase 2, one per value of the flag, share no pool.
func TestClientsHaveSeparatePools(t *testing.T) {
	resolve := answer(t)
	if NewClient(false, resolve).Transport == NewClient(true, resolve).Transport {
		t.Error("the clients share a transport")
	}
}

func TestClientNeedsAResolver(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("no panic")
		}
	}()
	NewClient(false, nil)
}
