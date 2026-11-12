package transport

import (
	"net/netip"
	"strings"
	"testing"
)

// TestClassify fixes every range of SEC-4: both ends of each prefix, the
// addresses just outside it, and the forms that carry another address inside.
// Each row is also checked through CheckURL as a literal, so a Forbidden row
// stays refused with allow_private_network set.
func TestClassify(t *testing.T) {
	rows := []struct {
		addr string
		want Class
	}{
		// Public: ordinary addresses and the neighbours of every special range.
		{"8.8.8.8", Public},
		{"1.0.0.0", Public},
		{"9.255.255.255", Public},
		{"11.0.0.0", Public},
		{"100.63.255.255", Public},
		{"100.128.0.0", Public},
		{"126.255.255.255", Public},
		{"128.0.0.0", Public},
		{"168.63.129.15", Public},
		{"168.63.129.17", Public},
		{"169.253.255.255", Public},
		{"169.255.0.0", Public},
		{"172.15.255.255", Public},
		{"172.32.0.0", Public},
		{"191.255.255.255", Public},
		{"192.0.1.0", Public},
		{"192.0.1.255", Public},
		{"192.0.3.0", Public},
		{"192.167.255.255", Public},
		{"192.169.0.0", Public},
		{"198.17.255.255", Public},
		{"198.20.0.0", Public},
		{"198.51.99.255", Public},
		{"198.51.101.0", Public},
		{"203.0.112.255", Public},
		{"203.0.114.0", Public},
		{"223.255.255.255", Public},
		{"2000::", Public},
		{"2001:200::", Public},
		{"2001:db7:ffff:ffff:ffff:ffff:ffff:ffff", Public},
		{"2001:db9::", Public},
		{"2003::", Public},
		{"2606:4700:4700::1111", Public},
		{"3ffe:ffff:ffff:ffff:ffff:ffff:ffff:ffff", Public},
		{"3fff:1000::", Public},
		{"3fff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", Public},
		{"::ffff:8.8.8.8", Public},   // IPv4-mapped, public inside
		{"64:ff9b::808:808", Public}, // NAT64, 8.8.8.8 inside

		// Private: loopback.
		{"127.0.0.0", Private},
		{"127.0.0.1", Private},
		{"127.255.255.255", Private},
		{"::1", Private},
		// Private: RFC 1918.
		{"10.0.0.0", Private},
		{"10.255.255.255", Private},
		{"172.16.0.0", Private},
		{"172.31.255.255", Private},
		{"192.168.0.0", Private},
		{"192.168.255.255", Private},
		// Private: carrier-grade NAT, around the metadata address it holds.
		{"100.64.0.0", Private},
		{"100.100.100.199", Private},
		{"100.100.100.201", Private},
		{"100.127.255.255", Private},
		// Private: unique-local, around the metadata prefixes it holds.
		{"fc00::", Private},
		{"fd00:42::41", Private},
		{"fd00:42::43", Private},
		{"fd00:ec1:ffff:ffff:ffff:ffff:ffff:ffff", Private},
		{"fd00:ec3::", Private},
		{"fd20:ce::253", Private},
		{"fd20:ce::255", Private},
		{"fdff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", Private},
		// Private: benchmarking.
		{"198.18.0.0", Private},
		{"198.19.255.255", Private},
		// Private: documentation.
		{"192.0.2.0", Private},
		{"192.0.2.255", Private},
		{"198.51.100.0", Private},
		{"198.51.100.255", Private},
		{"203.0.113.0", Private},
		{"203.0.113.255", Private},
		{"2001:db8::", Private},
		{"2001:db8:ffff:ffff:ffff:ffff:ffff:ffff", Private},
		{"3fff::", Private},
		{"3fff:fff:ffff:ffff:ffff:ffff:ffff:ffff", Private},
		// Private inside another form.
		{"::ffff:127.0.0.1", Private},      // IPv4-mapped loopback
		{"::ffff:10.0.0.1", Private},       // IPv4-mapped RFC 1918
		{"::ffff:100.64.0.1", Private},     // IPv4-mapped carrier-grade NAT
		{"64:ff9b::7f00:1", Private},       // NAT64, 127.0.0.1 inside
		{"64:ff9b::c0a8:101", Private},     // NAT64, 192.168.1.1 inside
		{"64:ff9b::127.0.0.1", Private},    // the same prefix in mixed notation
		{"64:ff9b::6464:64c7", Private},    // NAT64, 100.100.100.199 inside
		{"64:ff9b::198.51.100.1", Private}, // NAT64, documentation inside

		// Forbidden: unspecified and "this network".
		{"0.0.0.0", Forbidden},
		{"0.0.0.1", Forbidden},
		{"0.255.255.255", Forbidden},
		{"::", Forbidden},
		// Forbidden: link-local in both families.
		{"169.254.0.0", Forbidden},
		{"169.254.169.254", Forbidden},
		{"169.254.255.255", Forbidden},
		{"fe80::", Forbidden},
		{"fe80::a9fe:a9fe", Forbidden},
		{"febf:ffff:ffff:ffff:ffff:ffff:ffff:ffff", Forbidden},
		// Forbidden: multicast.
		{"224.0.0.0", Forbidden},
		{"239.255.255.255", Forbidden},
		{"ff00::", Forbidden},
		{"ff02::1", Forbidden},
		{"ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", Forbidden},
		// Forbidden: reserved and broadcast.
		{"240.0.0.0", Forbidden},
		{"255.255.255.254", Forbidden},
		{"255.255.255.255", Forbidden},
		// Forbidden: IETF protocol assignments.
		{"192.0.0.0", Forbidden},
		{"192.0.0.192", Forbidden},
		{"192.0.0.255", Forbidden},
		// Forbidden: the metadata list.
		{"fd00:ec2::", Forbidden},
		{"fd00:ec2::254", Forbidden},
		{"fd00:ec2:ffff:ffff:ffff:ffff:ffff:ffff", Forbidden},
		{"100.100.100.200", Forbidden},
		{"168.63.129.16", Forbidden},
		{"fd20:ce::254", Forbidden},
		{"fd00:42::42", Forbidden},
		{"fd00:a9fe:a9fe::1", Forbidden},
		{"fd00:c1::a9fe:a9fe", Forbidden},
		// Forbidden inside another form.
		{"::ffff:169.254.169.254", Forbidden}, // IPv4-mapped link-local
		{"::ffff:0.0.0.0", Forbidden},         // IPv4-mapped unspecified
		{"::ffff:168.63.129.16", Forbidden},   // IPv4-mapped listed address
		{"::ffff:100.100.100.200", Forbidden}, // IPv4-mapped listed address
		{"64:ff9b::a9fe:a9fe", Forbidden},     // NAT64, 169.254.169.254 inside
		{"64:ff9b::6464:64c8", Forbidden},     // NAT64, 100.100.100.200 inside
		{"64:ff9b::a83f:8110", Forbidden},     // NAT64, 168.63.129.16 inside
		{"64:ff9b::", Forbidden},              // NAT64, 0.0.0.0 inside
		{"64:ff9b::ffff:ffff", Forbidden},     // NAT64, broadcast inside
		// Forbidden: IPv6 outside 2000::/3 that no rule names.
		{"::2", Forbidden},
		{"::127.0.0.1", Forbidden},        // IPv4-compatible, deprecated
		{"::8.8.8.8", Forbidden},          // IPv4-compatible, public inside
		{"::ffff:0:127.0.0.1", Forbidden}, // IPv4-translated
		{"::ffff:0:8.8.8.8", Forbidden},   // IPv4-translated, public inside
		{"64:ff9a:ffff:ffff:ffff:ffff:ffff:ffff", Forbidden},
		{"64:ff9b:0:1::", Forbidden},     // just past the NAT64 /96
		{"64:ff9b:1::7f00:1", Forbidden}, // local-use NAT64
		{"64:ff9b:1::808:808", Forbidden},
		{"100::1", Forbidden}, // discard prefix
		{"1fff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", Forbidden},
		{"4000::", Forbidden},
		{"fbff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", Forbidden},
		{"fe00::", Forbidden},
		{"fec0::1", Forbidden}, // site-local, deprecated
		{"feff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", Forbidden},
		// Forbidden: tunnel forms inside 2000::/3.
		{"2001::", Forbidden},
		{"2001::1", Forbidden}, // Teredo
		{"2001:0:7f00:1::1", Forbidden},
		{"2001:1ff:ffff:ffff:ffff:ffff:ffff:ffff", Forbidden},
		{"2002::", Forbidden},
		{"2002:7f00:1::1", Forbidden},    // 6to4, 127.0.0.1 inside
		{"2002:a9fe:a9fe::1", Forbidden}, // 6to4, 169.254.169.254 inside
		{"2002:808:808::1", Forbidden},   // 6to4, public inside
		{"2002:ffff:ffff:ffff:ffff:ffff:ffff:ffff", Forbidden},
		// Forbidden: any zone, whatever the address.
		{"fe80::1%eth0", Forbidden},
		{"::1%lo", Forbidden},
		{"fd12::1%eth0", Forbidden},
		{"2606:4700:4700::1111%eth0", Forbidden},
		{"::ffff:8.8.8.8%eth0", Forbidden},
	}
	for _, row := range rows {
		t.Run("SEC-4 "+row.addr, func(t *testing.T) {
			addr := netip.MustParseAddr(row.addr)
			if got := Classify(addr); got != row.want {
				t.Fatalf("Classify(%s) = %d, want %d", row.addr, got, row.want)
			}
			raw := "https://" + strings.ReplaceAll(netip.AddrPortFrom(addr, 443).String(), "%", "%25") + "/v1"
			for _, allowPrivate := range []bool{false, true} {
				err := CheckURL(raw, allowPrivate)
				if refused := row.want == Forbidden || row.want == Private && !allowPrivate; (err != nil) != refused {
					t.Errorf("CheckURL(%q, %v) = %v, want refused %v", raw, allowPrivate, err, refused)
				}
			}
		})
	}

	if got := Classify(netip.Addr{}); got != Forbidden {
		t.Errorf("Classify(zero address) = %d, want %d", got, Forbidden)
	}
}

// TestCheckURL fixes every refusal of SEC-4 that needs no network, with its
// reason, and the accepted forms. No error may quote the planted password.
func TestCheckURL(t *testing.T) {
	const (
		parse    = "cannot be parsed"
		scheme   = "scheme must be http or https"
		creds    = "must not carry credentials"
		fragment = "must not have a fragment"
		noHost   = "needs a host"
		port     = "port must be between"
		badName  = "not a valid host name"
		number   = "host is a number"
		never    = "never allowed"
		private  = "address is private"
		https    = "must use https"
	)
	rows := []struct {
		name         string
		raw          string
		allowPrivate bool
		want         string // part of the error; empty when the URL is accepted
	}{
		{"https name", "https://api.example.com", false, ""},
		{"https name with path and query", "https://api.example.com/v1?version=2", false, ""},
		{"upper-case scheme and host", "HTTPS://API.EXAMPLE.COM/v1", false, ""},
		{"port", "https://api.example.com:8443/v1", false, ""},
		{"highest port", "https://api.example.com:65535", false, ""},
		{"empty port means the default one", "https://api.example.com:/v1", false, ""},
		{"fully qualified name", "https://api.example.com./v1", false, ""},
		{"single-label name", "https://gateway/v1", false, ""},
		{"underscore in a name", "https://my_host.example.com", false, ""},
		{"label starting with a digit", "https://2fast.example.com", false, ""},
		{"digits before a named label", "https://127.0.0.1.example.com", false, ""},
		{"last label with digits and letters", "https://example.1e3", false, ""},
		{"punycode name", "https://xn--bcher-kva.example", false, ""},
		{"public IPv4 literal", "https://8.8.8.8/v1", false, ""},
		{"public IPv6 literal", "https://[2606:4700:4700::1111]:8443/v1", false, ""},
		{"flag: plain http to a public name", "http://api.example.com/v1", true, ""},
		{"flag: http localhost", "http://localhost:11434", true, ""},
		{"flag: name under localhost", "http://tei.localhost:8080", true, ""},
		{"flag: http loopback literal", "http://127.0.0.1:11434/v1", true, ""},
		{"flag: http IPv6 loopback", "http://[::1]:8080", true, ""},
		{"flag: https RFC 1918", "https://10.1.2.3", true, ""},
		{"flag: unique-local", "https://[fd12:3456::1]", true, ""},
		{"flag: carrier-grade NAT", "http://100.64.0.7:8000", true, ""},
		{"flag: single-label name over http", "http://ollama:11434", true, ""},

		{"empty", "", false, scheme},
		{"no scheme", "api.example.com/v1", false, scheme},
		{"scheme-relative", "//api.example.com/v1", false, scheme},
		{"host and port without scheme", "localhost:11434", false, scheme},
		{"ftp", "ftp://example.com", false, scheme},
		{"file", "file:///etc/passwd", true, scheme},
		{"unix", "unix:///var/run/docker.sock", true, scheme},
		{"websocket", "wss://example.com", true, scheme},
		{"missing scheme name", "://example.com", false, parse},
		{"space in host", "https://exa mple.com", false, parse},
		{"control character", "https://example.com/\x00", false, parse},
		{"percent-encoded ASCII in host", "https://local%68ost", true, parse},
		{"percent-encoded address", "https://%31%32%37.0.0.1", true, parse},
		{"non-numeric port", "https://example.com:http", false, parse},

		{"user and password", "https://user:hunter2@example.com/v1", false, creds},
		{"user and password with the flag", "http://user:hunter2@127.0.0.1", true, creds},
		{"user only", "https://user@example.com", false, creds},
		{"password only", "https://:hunter2@example.com", false, creds},
		{"empty userinfo", "https://@example.com", false, creds},
		{"password in an unparsable url", "https://user:hunter2@exa mple.com", false, parse},
		{"password cut by a slash", "https://user:hunter2/x@example.com", false, parse},
		{"password with a bad escape", "https://user:hunter2%zz@example.com", false, parse},

		{"fragment", "https://example.com/v1#section", false, fragment},
		{"empty fragment", "https://example.com/v1#", false, fragment},
		{"fragment hiding the host", "https://example.com#@127.0.0.1/", true, fragment},

		{"empty host", "https:///v1", false, noHost},
		{"empty host with port", "https://:443/v1", false, noHost},
		{"nothing after the scheme", "https://", false, noHost},
		{"opaque", "https:example.com", false, noHost},
		{"empty brackets", "https://[]/v1", true, parse},

		{"port zero", "https://example.com:0", false, port},
		{"port above the range", "https://example.com:65536", false, port},
		{"port beyond any integer", "https://example.com:99999999999999999999", false, port},

		{"plain http", "http://api.example.com/v1", false, https},
		{"plain http to a public address", "http://8.8.8.8", false, https},

		{"loopback", "https://127.0.0.1", false, private},
		{"loopback over http", "http://127.0.0.1:11434", false, private},
		{"IPv6 loopback", "https://[::1]", false, private},
		{"RFC 1918", "https://192.168.1.10:11434", false, private},
		{"carrier-grade NAT", "https://100.64.0.7", false, private},
		{"unique-local", "https://[fd12:3456::1]", false, private},
		{"IPv4-mapped loopback", "https://[::ffff:127.0.0.1]", false, private},
		{"NAT64 loopback", "https://[64:ff9b::7f00:1]", false, private},
		{"localhost", "https://localhost", false, private},
		{"localhost over http", "http://localhost:11434", false, private},
		{"localhost in upper case", "https://LOCALHOST", false, private},
		{"localhost in mixed case with a dot", "https://LocalHost.:8080", false, private},
		{"localhost with a trailing dot", "https://localhost./v1", false, private},
		{"name under localhost", "https://api.localhost", false, private},
		{"deep name under localhost", "https://a.b.LOCALHOST.", false, private},

		{"flag: metadata over http", "http://169.254.169.254/latest/meta-data/", true, never},
		{"metadata without the flag", "https://169.254.169.254", false, never},
		{"flag: IPv6 link-local", "https://[fe80::1]", true, never},
		{"flag: zone", "https://[fe80::1%25eth0]", true, never},
		{"flag: zone on loopback", "http://[::1%25lo]:8080", true, never},
		{"flag: unspecified", "http://0.0.0.0:8080", true, never},
		{"flag: IPv6 unspecified", "http://[::]:8080", true, never},
		{"flag: this network", "http://0.1.2.3", true, never},
		{"flag: multicast", "https://224.0.0.1", true, never},
		{"flag: IPv6 multicast", "https://[ff02::1]", true, never},
		{"flag: broadcast", "https://255.255.255.255", true, never},
		{"flag: reserved", "https://240.0.0.1", true, never},
		{"flag: listed fd00:ec2::/32", "http://[fd00:ec2::254]/latest", true, never},
		{"flag: listed 100.100.100.200", "http://100.100.100.200/latest", true, never},
		{"flag: listed 168.63.129.16", "http://168.63.129.16/machine", true, never},
		{"listed 168.63.129.16 without the flag", "https://168.63.129.16", false, never},
		{"flag: listed fd20:ce::254", "http://[fd20:ce::254]", true, never},
		{"flag: listed fd00:42::42", "http://[fd00:42::42]", true, never},
		{"flag: listed fd00:a9fe:a9fe::1", "http://[fd00:a9fe:a9fe::1]/v1", true, never},
		{"flag: listed fd00:c1::a9fe:a9fe", "http://[fd00:c1::a9fe:a9fe]/opc/v2", true, never},
		{"flag: 192.0.0.0/24", "http://192.0.0.192/latest", true, never},
		{"flag: IPv4-mapped metadata", "http://[::ffff:169.254.169.254]", true, never},
		{"flag: IPv4-mapped metadata in hex", "http://[::ffff:a9fe:a9fe]", true, never},
		{"flag: NAT64 metadata", "http://[64:ff9b::a9fe:a9fe]", true, never},
		{"flag: IPv4-compatible loopback", "http://[::127.0.0.1]", true, never},
		{"flag: 6to4 metadata", "http://[2002:a9fe:a9fe::1]", true, never},
		{"flag: Teredo", "http://[2001::1]", true, never},
		{"flag: site-local", "http://[fec0::1]", true, never},

		{"decimal integer", "https://2130706433", false, number},
		{"flag: decimal integer", "http://2130706433", true, number},
		{"flag: hex part and short form", "http://0x7f.1", true, number},
		{"flag: octal integer", "http://017700000001", true, number},
		{"flag: hex integer", "http://0x7f000001", true, number},
		{"flag: hex integer in upper case", "http://0X7F000001", true, number},
		{"flag: short dotted form", "http://127.1", true, number},
		{"flag: three parts", "http://127.0.1", true, number},
		{"flag: octal part", "http://0177.0.0.1", true, number},
		{"flag: leading zeros", "http://127.000.000.001", true, number},
		{"flag: hex parts", "http://0x7f.0x0.0x0.0x1", true, number},
		{"flag: address with a trailing dot", "http://127.0.0.1.", true, number},
		{"flag: metadata as an integer", "http://2852039166", true, number},
		{"flag: five parts", "http://1.2.3.4.5", true, number},
		{"flag: integer beyond 64 bits", "http://99999999999999999999999", true, number},
		{"flag: number as the last label", "http://example.0x10", true, number},
		{"flag: numeric last label", "http://example.com.1", true, number},

		{"flag: full-width digits", "http://１２７.０.０.１", true, badName},
		{"flag: percent-encoded full-width digit", "http://%ef%bc%91%ef%bc%92%ef%bc%97.0.0.1", true, badName},
		{"flag: circled letter", "http://ⓛocalhost", true, badName},
		{"flag: kelvin sign lower-cases to k", "http://example.Kom", true, badName},
		{"unicode name", "https://exämple.com", false, badName},
		{"only a dot", "https://./v1", false, badName},
		{"flag: IPv6 without brackets", "http://::1/", true, parse},
		{"character outside a host name", "https://exa$mple.com", false, badName},
	}
	for _, row := range rows {
		t.Run("SEC-4 "+row.name, func(t *testing.T) {
			err := CheckURL(row.raw, row.allowPrivate)
			switch {
			case err == nil && row.want != "":
				t.Fatalf("CheckURL(%q, %v) accepted, want an error with %q", row.raw, row.allowPrivate, row.want)
			case err == nil:
				return
			case row.want == "" || !strings.Contains(err.Error(), row.want):
				t.Errorf("CheckURL(%q, %v) = %q, want %q", row.raw, row.allowPrivate, err, row.want)
			}
			if msg := err.Error(); strings.Contains(msg, "hunter2") || msg != strings.ToLower(msg) {
				t.Errorf("error %q quotes the password or is not lower-case", msg)
			}
		})
	}
}
