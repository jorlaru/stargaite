package task

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
)

func remoteSteps() []ui.Step {
	return []ui.Step{{ID: "where", Title: "addresses"}, {ID: "cert", Title: "certificate"}, {ID: "write", Title: "files and override"},
		{ID: "recreate", Title: "recreate and check"}}
}

// certificate is what Open to the network publishes with: the choice, the
// name the admin CLI reaches it by, the CA file in tls/ ("" for a public
// CA) and, for own, the reader's files.
type certificate struct {
	choice, name, ca string
	files            host.TLSFiles
}

// OpenToNetwork publishes the data plane on loopback and the addresses
// given, always with TLS (SEC-6), as setup.sh's remote did: the reader's
// certificate or a self-signed one, in tls/ readable by the container, one
// confirm naming every change to the override, then a forced recreate (a
// renewal changes no configuration) and an HTTPS check of each address. It
// makes no CLI call. Run again, it renews the certificate, keeping the
// addresses published.
func OpenToNetwork(ctx context.Context, u *ui.UI, _ *proxy.Proxy, h *host.Host) (string, error) {
	if err := remoteMachine(ctx, h); err != nil {
		return "", err
	}
	o, err := h.ReadOverride()
	if err != nil {
		return "", err
	}
	hostAddrs, _ := h.Addresses(ctx)
	if hostAddrs != "" && u.Asks() {
		say(ctx, u, "this host's addresses: "+hostAddrs)
	}
	addrs, err := askAddresses(ctx, u, o, hostAddrs)
	if err != nil {
		return "", err
	}
	// A self-signed certificate names each address given; 0.0.0.0, which
	// no client dials, publishes alone and stands for this host's.
	names := addrs
	if slices.Contains(addrs, "0.0.0.0") {
		addrs, names = []string{"0.0.0.0"}, append(names, strings.Fields(hostAddrs)...)
	}
	c, err := askCertificate(ctx, u, h, addrs[0] == "0.0.0.0" && hostAddrs == "")
	if err != nil {
		return "", err
	}
	gid := h.TLSGroup()
	if gid != "" && h.GroupShared(ctx, gid) {
		say(ctx, u, "warning: group "+gid+", which the container joins to read tls/, has other members, who can read its key too")
	}
	say(ctx, u, "tls/ gets the certificate and its key, and from now on the data plane speaks HTTPS only: "+
		"every client, local ones too, needs https:// and to trust the certificate")
	if err := o.Ours(); err != nil {
		return "", err
	}
	said, what := setOverride(ctx, u, o, [][2]string{{"tls", c.choice}, {"tls_name", c.name}, {"tls_ca", c.ca}, {"tls_group", gid},
		{"addresses", strings.Join(addrs, " ")}})
	if err := confirm(ctx, u, "write", said+recreates); err != nil {
		if usage := (*ui.UsageError)(nil); errors.As(err, &usage) {
			return "", &ui.UsageError{Msg: "missing --yes: " + what + recreates}
		}
		return "", err
	}
	if c.files == nil {
		if c.files, err = host.SelfSigned(names); err != nil {
			return "", err
		}
	}
	if err := writeRemote(ctx, u, h, o, c.files); err != nil {
		return "", err
	}
	if err := recreate(ctx, u, h, "recreate", true, "--force-recreate"); err != nil {
		if pressed(err) {
			return "", err
		}
		return "", fmt.Errorf("%w; ./setup.sh remote again repairs it", err)
	}
	return published(ctx, u, h, addrs, c.choice == "self-signed")
}

// remoteMachine refuses what Open to the network cannot publish: no
// container, the port taken while it is stopped, and Docker whose uids and
// groups are not this host's.
func remoteMachine(ctx context.Context, h *host.Host) error {
	exists, runs, err := container(ctx, h)
	switch {
	case err != nil:
		return err
	case !exists:
		return errNoContainer
	}
	p, err := h.Plane()
	switch {
	case err != nil:
		return err
	case !runs && host.PortTaken(p.Port):
		return fmt.Errorf("a free host port: 127.0.0.1:%d is taken; ./setup.sh install --port <n> publishes another", p.Port)
	}
	rootless, err := h.Rootless(ctx)
	switch {
	case err != nil:
		return err
	case rootless:
		return errors.New("rootless Docker or user-namespace remapping here: the container's uid and groups are not this host's, " +
			"so tls/ cannot be made readable to it; nothing was changed")
	}
	return nil
}

// askAddresses asks the addresses to publish on beside 127.0.0.1, those
// published by default, so a renewal keeps them.
func askAddresses(ctx context.Context, u *ui.UI, o *host.Override, hostAddrs string) ([]string, error) {
	published := slices.DeleteFunc(strings.Fields(o.Get("addresses")), func(a string) bool { return a == "127.0.0.1" })
	a, err := u.Text(ctx, ui.Question{ID: "address", Flag: "address", Step: "where",
		Prompt:     "the addresses to publish on beside 127.0.0.1, separated by commas; 127.0.0.1 alone for loopback only",
		HasDefault: true, Default: strings.Join(published, ","),
		Check: func(a string) error { _, err := addressList(a, hostAddrs); return err }})
	if err != nil {
		return nil, err
	}
	return addressList(a, hostAddrs)
}

// addressList is 127.0.0.1, then each address of answer (split on commas
// and spaces) once: an IPv4 address of this host, as hostAddrs lists them
// when known, or 0.0.0.0.
func addressList(answer, hostAddrs string) ([]string, error) {
	list := []string{"127.0.0.1"}
	for _, a := range strings.FieldsFunc(answer, func(r rune) bool { return r == ',' || r == ' ' }) {
		if ip, err := netip.ParseAddr(a); err != nil || !ip.Is4() {
			return nil, fmt.Errorf("%s is not an IPv4 address, such as 192.0.2.10", a)
		}
		switch {
		case slices.Contains(list, a):
		case a != "0.0.0.0" && hostAddrs != "" && !slices.Contains(strings.Fields(hostAddrs), a):
			return nil, fmt.Errorf("%s is not an address of this host, which has %s", a, hostAddrs)
		default:
			list = append(list, a)
		}
	}
	return list, nil
}

// askCertificate asks own or self-signed; for own, the files and the name,
// read and checked before anything is written. unnamed warns that a
// self-signed certificate for 0.0.0.0 names none of this host's addresses.
func askCertificate(ctx context.Context, u *ui.UI, h *host.Host, unnamed bool) (certificate, error) {
	choice, err := u.Pick(ctx, ui.Question{ID: "tls", Flag: "tls", Step: "cert",
		Prompt: "the certificate: own (yours, with its key) or self-signed (made here)",
		Options: []ui.Option{{Value: "own", Label: "your certificate and its key"},
			{Value: "self-signed", Label: "a self-signed one, made here"}}})
	if err != nil {
		return certificate{}, err
	}
	own := ownQuestions()
	if choice == "self-signed" {
		if slices.ContainsFunc(own, u.Given) {
			return certificate{}, &ui.UsageError{Msg: "--tls-cert, --tls-key, --tls-name and --tls-ca are for --tls own"}
		}
		if unnamed {
			say(ctx, u, "warning: this host's addresses are unknown here (no hostname -I), "+
				"so the certificate names only those given beside 0.0.0.0 in --address")
		}
		return certificate{choice: choice, name: "localhost", ca: "cert.pem"}, nil
	}
	a := make([]string, len(own)) // the certificate, its key, its name, the CA
	for i, q := range own {
		if a[i], err = u.Text(ctx, q); err != nil {
			return certificate{}, err
		}
	}
	c := certificate{choice: choice, name: a[2]}
	if a[3] != "" {
		c.ca = "ca.pem"
	}
	c.files, err = h.OwnTLS(a[0], a[1], a[3])
	return c, err
}

// ownQuestions are own's: the certificate, its key, its name and the
// private CA, in that order.
func ownQuestions() []ui.Question {
	abs := func(a string) error {
		if !filepath.IsAbs(a) {
			return fmt.Errorf("%s is not an absolute path", a)
		}
		return nil
	}
	q := func(id, prompt string, check func(string) error) ui.Question {
		return ui.Question{ID: id, Flag: id, Step: "cert", Prompt: prompt, Check: check}
	}
	ca := q("tls-ca", "the absolute path of the certificate of the private CA that signed it; empty for a public CA",
		func(a string) error {
			if a == "" {
				return nil
			}
			return abs(a)
		})
	ca.HasDefault = true
	return []ui.Question{
		q("tls-cert", "the absolute path of the certificate chain, in PEM", abs),
		q("tls-key", "the absolute path of its private key, in PEM", abs),
		q("tls-name", "the name on it that the admin CLI reaches it by", func(a string) error {
			if !regexp.MustCompile(`^[A-Za-z0-9.-]+$`).MatchString(a) {
				return fmt.Errorf("%s is not a host name, such as proxy.example.com", a)
			}
			return nil
		}),
		ca,
	}
}

// setOverride sets the TLS keys on o and shows each that changes on its
// own line, never cut as a said line is, so a long value is in the
// transcript whole; it returns what the confirm says, naming those lines,
// and what a usage error names, with each key.
func setOverride(ctx context.Context, u *ui.UI, o *host.Override, keys [][2]string) (said, what string) {
	var changed []string
	for _, kv := range keys {
		if o.Get(kv[0]) != kv[1] {
			changed = append(changed, kv[0]+"="+kv[1])
			_ = u.Show(ctx, "  "+kv[0]+"="+kv[1])
		}
		o.Set(kv[0], kv[1])
	}
	kept := ""
	switch {
	case len(changed) == 0:
		return "the certificate and key in tls/ are replaced", "the certificate and key in tls/ are replaced"
	case o.Exists():
		said, kept = "compose.override.yaml is rewritten with ", `, keeping its other "# setup:" lines`
	default:
		said = "writing compose.override.yaml with "
	}
	return said + "the lines above" + kept, said + strings.Join(changed, " ") + kept
}

// writeRemote writes the override, then tls/; when tls/ fails, its reason
// is said and the override put back as it was read, so the next run reads
// the old state.
func writeRemote(ctx context.Context, u *ui.UI, h *host.Host, o *host.Override, files host.TLSFiles) error {
	if err := h.WriteOverride(o); err != nil {
		return err
	}
	err := h.WriteTLS(ctx, files)
	if err == nil {
		return nil
	}
	say(ctx, u, err.Error())
	if h.RestoreOverride(o) != nil {
		return errors.New("tls/ was not written, and compose.override.yaml was not put back: move it away and run this again")
	}
	return errors.New("tls/ was not written, or not given its owner and modes; compose.override.yaml is as it was")
}

// published checks each address over HTTPS by the certificate's name, then
// says what clients need.
func published(ctx context.Context, u *ui.UI, h *host.Host, addrs []string, selfSigned bool) (string, error) {
	p, err := h.Plane()
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		if err := h.CheckHTTPS(ctx, a); err != nil {
			say(ctx, u, err.Error())
			return "", fmt.Errorf("the data plane did not answer over HTTPS on %s:%d", a, p.Port)
		}
		say(ctx, u, fmt.Sprintf("https on %s:%d: ready", a, p.Port))
	}
	beyond := strings.Join(slices.DeleteFunc(slices.Clone(addrs), func(a string) bool { return a == "127.0.0.1" }), " ")
	if beyond == "0.0.0.0" {
		beyond = "this host"
	}
	if beyond != "" {
		say(ctx, u, fmt.Sprintf("warning: Docker publishes past ufw and firewalld: every machine that reaches %s reaches port %d; "+
			"limit it in your network's firewall or in the DOCKER-USER chain", beyond, p.Port))
	}
	if expiry, err := h.CertExpiry(); err == nil {
		say(ctx, u, "the certificate expires on "+expiry.UTC().Format("2006-01-02 15:04")+" UTC; ./setup.sh remote again renews it")
	}
	if selfSigned {
		say(ctx, u, "clients trust this one certificate, tls/cert.pem copied to their machine, beside their own bundle: curl --cacert cert.pem;")
		say(ctx, u, `  Python (requests, httpx) verify="cert.pem"; Node.js NODE_EXTRA_CA_CERTS=cert.pem; or the system's trust store`)
	}
	cli, err := cliFromClone(h)
	if err != nil {
		return "", err
	}
	say(ctx, u, fmt.Sprintf("base URL %s here; elsewhere, https://<an address the certificate holds>:%d/v1", p.BaseURL, p.Port))
	say(ctx, u, "the CLI from this directory: alias stargaite='"+cli+"'")
	return "Done: the data plane answers over HTTPS on " + strings.Join(addrs, " ") + ".", nil
}
