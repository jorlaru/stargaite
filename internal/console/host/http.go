package host

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jorlaru/stargaite/internal/console/ui"
)

const (
	// httpBound bounds an HTTP call with a secret: the free key check.
	httpBound = 30 * time.Second
	// httpLimit caps an answer read.
	httpLimit = 1 << 20
)

// Provider is what a free key check needs of a provider.
type Provider struct {
	Protocol     string // openai or anthropic
	BaseURL      string // as the proxy stores it; host.docker.internal is read from the host as localhost
	Key          string // "" sends no credential header
	Path         string // "" is the protocol's model list; otherwise the GET's path after the base URL
	AllowPrivate bool   // the provider's stored allow_private_network
	Limit        int    // the largest answer read, in bytes; 0 is httpLimit
}

// URL is the check's URL, as setup.sh's list_models builds it: the host-side
// base URL less one trailing slash, then Path or the protocol's model list.
func (p Provider) URL() string {
	base := strings.TrimSuffix(hostSide(p.BaseURL), "/")
	switch {
	case p.Path != "":
		return base + p.Path
	case p.Protocol == "anthropic":
		return base + "/v1/models"
	}
	return base + "/models"
}

// ListModels makes one GET of the provider's model list, or of Path, with
// the key in its header, never retried. A 2xx model list gives its ids; a
// 2xx answer at Path is read but not decoded, so its status alone decides.
// Any other status is returned with no error. An error never holds the key.
func (h *Host) ListModels(ctx context.Context, p Provider) (ids []string, status int, err error) {
	u := p.URL()
	query := ""
	if p.Path == "" && p.Protocol == "anthropic" {
		query = "?limit=1000"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u+query, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("the model list at %s was not read: the URL is not valid", u)
	}
	switch {
	case p.Protocol == "anthropic" && p.Path == "":
		req.Header.Set("anthropic-version", "2023-06-01")
		if p.Key != "" {
			req.Header.Set("x-api-key", p.Key)
		}
	case p.Key != "":
		req.Header.Set("Authorization", "Bearer "+p.Key)
	}
	body, status, err := h.get(h.client(p.AllowPrivate, nil, "", ""), req, cmp.Or(p.Limit, httpLimit))
	switch {
	case ctx.Err() != nil:
		return nil, 0, fmt.Errorf("the model list at %s was not read: %w", u, ctx.Err())
	case err != nil:
		return nil, 0, fmt.Errorf("the model list at %s was not read: %s", u, h.plain(err))
	case status/100 != 2 || p.Path != "":
		return nil, status, nil
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &list) != nil || len(list.Data) == 0 {
		return nil, status, fmt.Errorf("the model list at %s is empty or not a list", u)
	}
	for _, m := range list.Data {
		ids = append(ids, ui.OneLine(m.ID))
	}
	return ids, status, nil
}

// isReady is one readiness check of the data plane, as setup.sh's
// wait_ready makes it: on loopback, under TLS by the certificate's name,
// trusting only tls/<CA> when the override names one.
func (h *Host) isReady(ctx context.Context) error {
	p, err := h.Plane()
	if err != nil {
		return err
	}
	var pool *x509.CertPool
	if p.CA != "" {
		pem, err := os.ReadFile(filepath.Join(h.Dir, "tls", p.CA))
		if err != nil {
			return fmt.Errorf("tls/%s: %w", p.CA, err)
		}
		pool = x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return fmt.Errorf("tls/%s holds no certificate", p.CA)
		}
	}
	try, cancel := context.WithTimeout(ctx, h.ready.try)
	defer cancel()
	req, err := http.NewRequestWithContext(try, http.MethodGet, strings.TrimSuffix(p.BaseURL, "/v1")+"/health/ready", nil)
	if err != nil {
		return err
	}
	_, status, err := h.get(h.client(true, pool, "127.0.0.1:"+strconv.Itoa(p.Port), p.Name), req, httpLimit)
	switch {
	case err != nil:
		return fmt.Errorf("/health/ready: %w", err)
	case status/100 != 2:
		return fmt.Errorf("/health/ready answered %d", status)
	}
	return nil
}

// get sends req and reads at most limit bytes of the answer, a whole number
// of MiB.
func (h *Host) get(c *http.Client, req *http.Request, limit int) ([]byte, int, error) {
	resp, err := c.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	switch {
	case err != nil:
		return nil, 0, err
	case len(body) > limit:
		return nil, 0, fmt.Errorf("the answer is over %d MiB", limit>>20)
	}
	return body, resp.StatusCode, nil
}

// plain is a transport failure in plain words, without the request's URL.
func (h *Host) plain(err error) string {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "no answer within " + h.httpBound.String()
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return err.Error()
}

// client is the one HTTP client: bounded by httpBound, no redirect followed,
// no proxy from the environment, no keep-alive, HTTP/1.1. It refuses a
// private address unless allowPrivate; pool nil trusts the system's roots.
// With dial set, every connection goes to that address, with serverName as
// the name the certificate must hold.
func (h *Host) client(allowPrivate bool, pool *x509.CertPool, dial, serverName string) *http.Client {
	dialer := &net.Dialer{Control: func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			return err
		}
		return refuse(ap.Addr(), allowPrivate)
	}}
	transport := &http.Transport{
		Proxy:             nil,
		DisableKeepAlives: true,
		ForceAttemptHTTP2: false,
		TLSClientConfig:   &tls.Config{RootCAs: pool, ServerName: serverName},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if dial != "" {
				addr = dial
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}
	return &http.Client{
		Timeout:       h.httpBound,
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// refuse refuses an address the proxy's guard (SEC-4, internal/transport)
// would: a forbidden one always, a private one unless allowPrivate. The
// console never imports the proxy's packages, so the guard's lists are
// copied here; TestRefuseAsGuard compares the two verdicts.
func refuse(a netip.Addr, allowPrivate bool) error {
	forbidden, private := guardClass(a)
	switch {
	case forbidden:
		return fmt.Errorf("%s is link-local, cloud metadata or reserved, and is never reached", a)
	case private && !allowPrivate:
		return fmt.Errorf("%s is a private address, and the provider does not allow private networks", a.Unmap())
	}
	return nil
}

// guardClass is the guard's Classify: a zoned address is forbidden, a mapped
// one is its IPv4 address, a NAT64 one the IPv4 address it carries; then the
// metadata and forbidden lists, the private list, and IPv6 outside 2000::/3.
func guardClass(a netip.Addr) (forbidden, private bool) {
	if !a.IsValid() || a.Zone() != "" {
		return true, false
	}
	a = a.Unmap()
	if netip.MustParsePrefix("64:ff9b::/96").Contains(a) {
		b := a.As16()
		return guardClass(netip.AddrFrom4([4]byte(b[12:])))
	}
	in := func(list ...string) bool {
		return slices.ContainsFunc(list, func(p string) bool { return netip.MustParsePrefix(p).Contains(a) })
	}
	switch {
	case in("fd00:ec2::/32", "100.100.100.200/32", "168.63.129.16/32", "fd20:ce::254/128", "fd00:42::42/128", // metadata
		"fd00:a9fe:a9fe::1/128", "fd00:c1::a9fe:a9fe/128",
		"0.0.0.0/8", "169.254.0.0/16", "192.0.0.0/24", "224.0.0.0/4", "240.0.0.0/4", "2001::/23", "2002::/16"): // forbidden
		return true, false
	case in("127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "fc00::/7",
		"198.18.0.0/15", "192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24", "2001:db8::/32", "3fff::/20"):
		return false, true
	}
	return a.Is6() && !netip.MustParsePrefix("2000::/3").Contains(a), false
}

// ToContainer is a URL on the host's loopback as the container reaches it,
// on host.docker.internal (setup.sh's reach); ok is false for any other URL,
// returned unchanged.
func ToContainer(u string) (string, bool) {
	m := regexp.MustCompile(`^(https?://)(localhost|127\.[0-9.]+|\[::1\])(:[0-9]+)?(/.*)?$`).FindStringSubmatch(u)
	if m == nil {
		return u, false
	}
	return m[1] + "host.docker.internal" + m[3] + m[4], true
}

// hostSide is a stored URL as the host reaches it: host.docker.internal is
// the host's own localhost.
func hostSide(u string) string {
	return strings.Replace(u, "//host.docker.internal", "//localhost", 1)
}
