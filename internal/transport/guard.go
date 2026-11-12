// Package transport guards outbound connections: it classifies addresses and
// checks the URLs a configuration may point at (SEC-4).
package transport

import (
	"context"
	"errors"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// Class says how far an outbound address may be reached. The values grow with
// the restriction, so the verdict for several addresses is the largest one.
type Class int

// The classes, from the least to the most restricted.
const (
	Public    Class = iota // reachable by any outbound URL
	Private                // reachable only with allow_private_network
	Forbidden              // never reachable
)

// Resolver looks a host name up. It is a function so that set time and dial
// time ask the same source, and so that tests inject the answers.
type Resolver func(ctx context.Context, host string) ([]netip.Addr, error)

// metadata lists the cloud metadata services that sit outside link-local, so
// no range rule catches them. Checked before anything else.
var metadata = prefixes(
	"fd00:ec2::/32",          // Amazon EC2 instance services over IPv6, inside unique-local
	"100.100.100.200/32",     // Alibaba Cloud, inside carrier-grade NAT
	"168.63.129.16/32",       // Azure platform endpoint, in public address space: only this list refuses it
	"fd20:ce::254/128",       // Google Compute Engine on IPv6-only instances, inside unique-local
	"fd00:42::42/128",        // Scaleway, inside unique-local
	"fd00:a9fe:a9fe::1/128",  // Akamai (Linode), inside unique-local
	"fd00:c1::a9fe:a9fe/128", // Oracle Cloud on IPv6-only instances (OS Management Hub troubleshooting guide), inside unique-local
)

// forbidden holds the ranges no flag opens. The IPv6 ranges outside 2000::/3
// are not listed: Classify refuses that whole space unless a rule names it.
var forbidden = prefixes(
	"0.0.0.0/8",      // "this network": 0.0.0.0 is the unspecified address and reaches the local host on some systems
	"169.254.0.0/16", // link-local, where most metadata services live
	"192.0.0.0/24",   // IETF protocol assignments: no provider lives here and one cloud serves metadata at 192.0.0.192
	"224.0.0.0/4",    // multicast
	"240.0.0.0/4",    // reserved, and the broadcast address 255.255.255.255
	"2001::/23",      // IETF protocol assignments, Teredo (2001::/32) included: a tunnel form that carries IPv4 addresses
	"2002::/16",      // 6to4, deprecated: a tunnel endpoint delivers to the IPv4 address in bits 16 to 48
)

// private holds what allow_private_network opens. Beyond the ranges meant for
// private networks it holds every range that is not globally routed and is
// still used inside networks, so that none of them counts as public.
var private = prefixes(
	// Loopback.
	"127.0.0.0/8", "::1/128",
	// RFC 1918.
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
	// Carrier-grade NAT, also used by overlay networks.
	"100.64.0.0/10",
	// Unique-local.
	"fc00::/7",
	// Benchmarking: lab networks and local proxies answer here.
	"198.18.0.0/15",
	// Documentation: not routed, yet found inside networks.
	"192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24", "2001:db8::/32", "3fff::/20",
)

const hostChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.-_"

var (
	nat64   = netip.MustParsePrefix("64:ff9b::/96")
	global6 = netip.MustParsePrefix("2000::/3")
)

func prefixes(list ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(list))
	for i, s := range list {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

func within(list []netip.Prefix, addr netip.Addr) bool {
	return slices.ContainsFunc(list, func(p netip.Prefix) bool { return p.Contains(addr) })
}

// Classify says whether an outbound connection may reach addr. It is the one
// verdict used when a URL is set and again when a connection is dialled.
func Classify(addr netip.Addr) Class {
	// A zone names an interface, which only link-local addressing needs, and a
	// prefix never matches a zoned address: refusing is the only safe reading.
	if !addr.IsValid() || addr.Zone() != "" {
		return Forbidden
	}
	// ::ffff:a.b.c.d reaches a.b.c.d, and an IPv4 prefix does not match it until unmapped.
	addr = addr.Unmap()
	if nat64.Contains(addr) {
		// A translating gateway delivers to the IPv4 address in the last four bytes.
		b := addr.As16()
		return Classify(netip.AddrFrom4([4]byte(b[12:])))
	}
	switch {
	case within(metadata, addr), within(forbidden, addr):
		return Forbidden
	case within(private, addr):
		return Private
	case addr.Is6() && !global6.Contains(addr):
		// Only 2000::/3 is allocated as global unicast. What is left here is
		// unspecified, link-local (fe80::/10), multicast (ff00::/8), deprecated
		// site-local (fec0::/10), IPv4-compatible (::/96) and IPv4-translated
		// (::ffff:0:0:0/96) forms, local-use NAT64 (64:ff9b:1::/48, whose IPv4
		// position depends on a prefix length that is not known here), the
		// discard prefix and unallocated space: no provider lives in any of it.
		return Forbidden
	}
	return Public
}

// CheckURL applies the checks an outbound URL allows without the network. A
// host name that passes is still resolved and classified by the caller. The
// errors never quote the URL, which may hold a password.
func CheckURL(raw string, allowPrivate bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("url cannot be parsed")
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return errors.New("url scheme must be http or https")
	case u.User != nil:
		return errors.New("url must not carry credentials")
	case strings.Contains(raw, "#"):
		return errors.New("url must not have a fragment")
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("url needs a host")
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.ParseUint(p, 10, 16); err != nil || n == 0 {
			return errors.New("url port must be between 1 and 65535")
		}
	}

	// Checked in this order: a literal is classified; what is neither a literal
	// nor a host name is refused; the rest goes to the resolver, except the
	// names that are loopback by definition.
	class := Public
	name := strings.ToLower(strings.TrimSuffix(host, "."))
	addr, err := netip.ParseAddr(host)
	switch {
	case err == nil:
		class = Classify(addr)
	// A non-ASCII host could be mapped to an address literal by the HTTP
	// client's name normalisation, after this check. The test reads host, not
	// name: lower-casing turns some non-ASCII letters into ASCII ones.
	case name == "" || strings.Trim(host, hostChars) != "":
		return errors.New("url host is not a valid host name")
	case endsInNumber(name):
		return errors.New("url host is a number: write the address in full dotted form")
	// RFC 6761: localhost and every name under it are loopback.
	case name == "localhost" || strings.HasSuffix(name, ".localhost"):
		class = Private
	}

	switch {
	case class == Forbidden:
		return errors.New("url address is link-local, cloud metadata or reserved, and is never allowed")
	case class == Private && !allowPrivate:
		return errors.New("url address is private: set allow_private_network to allow it")
	case u.Scheme == "http" && !allowPrivate:
		return errors.New("url must use https: set allow_private_network to allow plain http")
	}
	return nil
}

// endsInNumber reports whether the last label of name is an integer in any
// spelling. "2130706433", "0x7f.1", "017700000001" and "127.1" are no names:
// some resolvers read them as addresses. As in the URL standard, such a host
// must be an address, and only the full dotted form is taken as one.
func endsInNumber(name string) bool {
	_, err := strconv.ParseUint(name[strings.LastIndexByte(name, '.')+1:], 0, 64)
	return err == nil || errors.Is(err, strconv.ErrRange)
}
