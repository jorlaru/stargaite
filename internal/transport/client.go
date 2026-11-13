package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// NewClient returns an HTTP client for outbound calls that applies SEC-4 at
// connection time. Its dial resolves the host through resolve, the resolver
// of set time, refuses the connection when any address is Forbidden, or
// Private while allowPrivate is false, and dials an address literal; the
// dialer checks again the address actually connected to. Proxy variables are
// ignored, redirects are returned as responses, certificates are verified.
// Each call has its own connection pool. Deadlines come from the request's
// context. The transport keeps a dial going after its request gave up, for a
// later request, so the dial and the TLS handshake also have a backstop of
// their own (dialBackstop), far above any connect timeout (RES-3): a provider
// that accepts and never completes the handshake cannot hold a dial forever.
func NewClient(allowPrivate bool, resolve Resolver) *http.Client {
	if resolve == nil {
		panic("transport: NewClient needs a resolver")
	}
	d := dialer(allowPrivate)
	return &http.Client{
		Transport: &http.Transport{
			TLSHandshakeTimeout: dialBackstop,
			Proxy:               nil, // SEC-4: the check would see the proxy's address
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(address)
				if err != nil {
					return nil, err
				}
				var addrs []netip.Addr
				if a, perr := netip.ParseAddr(host); perr == nil {
					addrs = append(addrs, a)
				} else if addrs, err = resolve(ctx, host); err != nil {
					return nil, err
				}
				if len(addrs) == 0 {
					return nil, fmt.Errorf("%s: no address", host)
				}
				for _, a := range addrs {
					if err := allowed(a, allowPrivate); err != nil {
						return nil, fmt.Errorf("%s: %w", host, err)
					}
				}
				var errs []error
				for _, a := range addrs {
					conn, err := d.DialContext(ctx, network, net.JoinHostPort(a.String(), port))
					if err == nil {
						return conn, nil
					}
					errs = append(errs, err)
				}
				return nil, errors.Join(errs...)
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// dialBackstop bounds a dial and a TLS handshake that no request waits for.
const dialBackstop = time.Minute

// dialer refuses, through Control, a connection to an address that SEC-4
// does not allow, whatever led the dial there.
func dialer(allowPrivate bool) *net.Dialer {
	return &net.Dialer{Timeout: dialBackstop, Control: func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			return err
		}
		return allowed(ap.Addr(), allowPrivate)
	}}
}

// allowed is the dial-time verdict on one address, the same as set time's.
func allowed(addr netip.Addr, allowPrivate bool) error {
	switch Classify(addr) {
	case Forbidden:
		return fmt.Errorf("address %s is link-local, cloud metadata or reserved, and is never allowed", addr)
	case Private:
		if !allowPrivate {
			return fmt.Errorf("address %s is private and allow_private_network is not set", addr)
		}
	}
	return nil
}
