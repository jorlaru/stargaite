package task

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
	"github.com/jorlaru/stargaite/internal/console/ui/plain"
)

// remoteWorld is Open to the network's fixture: a clone whose docker fake
// answers compose ps, info, the forced recreate and logs, logging each call;
// fakes of hostname, chown, getent, id and openssl; and a TLS data plane at
// .env's port on 127.0.0.1, 127.0.0.2 and 127.0.0.3 (or 0.0.0.0), serving
// the pair in tls/ at each handshake and logging each request's local
// address and name.
type remoteWorld struct {
	t     *testing.T
	h     *host.Host
	bin   string
	port  int
	p     string // port, as text
	mu    sync.Mutex
	seen  []string // "<local address> <SNI>" per request
	calls []string // CLI calls
}

// remoteDocker is the docker fake's script for ps (compose ps -q), info and
// up (the forced recreate).
func (w *remoteWorld) docker(ps, info, up string) {
	writeFile(w.t, filepath.Join(w.bin, "docker"), "#!/bin/sh\necho \"$*\" >>'"+w.bin+"/log'\ncase \"$*\" in\n"+
		"'compose ps -q') "+ps+" ;;\n'compose ps -a -q') echo c0ffee ;;\n'info --format {{.SecurityOptions}}') "+info+" ;;\n"+
		"'compose up -d --no-build --force-recreate') "+up+" ;;\n'compose logs --tail 30 stargaite') echo 'a log line' ;;\n"+
		"*) exit 9 ;;\nesac\n", 0o755)
}

func tlsWorld(t *testing.T) *remoteWorld { return tlsWorldOn(t, "127.0.0.1", "127.0.0.2", "127.0.0.3") }

// tlsWorldOn is tlsWorld with the data plane listening on addrs, the first
// choosing the port.
func tlsWorldOn(t *testing.T, addrs ...string) *remoteWorld {
	t.Helper()
	w := &remoteWorld{t: t, h: host.New(t.TempDir()), bin: t.TempDir()}
	w.docker("echo c0ffee", "echo '[name=seccomp]'", ":")
	gid := strconv.Itoa(os.Getegid())
	for name, body := range map[string]string{
		"hostname": "[ \"$*\" = -I ] || exit 9\nprintf '%s' \"$FAKE_HOSTNAME\"\n",
		"chown":    "echo \"$*\" >>'" + w.bin + "/chown.log'\n",
		"getent":   "echo \"$*\" >>'" + w.bin + "/getent.log'\ncase \"$1\" in passwd) printf '%s' \"$FAKE_PASSWD\";; group) printf '%s' \"$FAKE_GROUP\";; esac\n",
		"id":       "[ \"$*\" = -un ] || exit 9\necho me\n",
		"openssl":  "echo \"$*\" >>'" + w.bin + "/openssl.log'\nexit 1\n",
	} {
		writeFile(t, filepath.Join(w.bin, name), "#!/bin/sh\n"+body, 0o755)
	}
	t.Setenv("PATH", w.bin)
	t.Setenv("FAKE_HOSTNAME", "127.0.0.2 127.0.0.3 ")
	t.Setenv("FAKE_PASSWD", "me:x:1000:"+gid+"::/home/me:/bin/sh\n")
	t.Setenv("FAKE_GROUP", "me:x:"+gid+":\n")
	srv := &http.Server{
		TLSConfig: &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			pair, err := tls.LoadX509KeyPair(filepath.Join(w.h.Dir, "tls", "cert.pem"), filepath.Join(w.h.Dir, "tls", "key.pem"))
			return &pair, err
		}},
		Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.seen = append(w.seen, r.Context().Value(http.LocalAddrContextKey).(net.Addr).String()+" "+r.TLS.ServerName)
		}),
		ErrorLog: log.New(io.Discard, "", 0), // the handshakes before tls/ exists fail
	}
	for i, a := range addrs {
		port := "0"
		if i > 0 {
			port = w.p
		}
		l, err := net.Listen("tcp", a+":"+port)
		if err != nil && runtime.GOOS != "linux" {
			t.Skipf("%s is not configured on this host's loopback", a)
		}
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			w.port = l.Addr().(*net.TCPAddr).Port
			w.p = strconv.Itoa(w.port)
		}
		go func() { _ = srv.ServeTLS(l, "", "") }()
	}
	t.Cleanup(func() { _ = srv.Close() })
	writeFile(t, filepath.Join(w.h.Dir, ".env"), "STARGAITE_PORT="+w.p+"\n", 0o600)
	return w
}

// open runs Open to the network with answers and the plain front.
func (w *remoteWorld) open(answers map[string]string) (string, string, error) {
	var out bytes.Buffer
	res, err := w.openWith(ui.New(plain.New(&out), answers))
	return res, out.String(), err
}

func (w *remoteWorld) openWith(u *ui.UI) (string, error) {
	var calls []string
	defer func() { w.calls = append(w.calls, calls...) }()
	return u.Run(context.Background(), remoteSteps(), func(ctx context.Context) (string, error) {
		return OpenToNetwork(ctx, u, proxy.New(cannedRunner(w.t, nil, &calls)), w.h)
	})
}

func (w *remoteWorld) log(name string) string { return readFile(filepath.Join(w.bin, name)) }

func (w *remoteWorld) ups() int {
	return strings.Count(w.log("log"), "compose up -d --no-build --force-recreate\n")
}

// served is the requests the data plane got, "<local address> <SNI>".
func (w *remoteWorld) served() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.seen)
}

// file is a file of the clone, "" when missing.
func (w *remoteWorld) file(name string) string { return readFile(filepath.Join(w.h.Dir, name)) }

// untouched fails unless nothing was written: the override as before, no
// tls/, no up, no CLI call and no openssl.
func (w *remoteWorld) untouched(override string) {
	w.t.Helper()
	if _, err := os.Lstat(filepath.Join(w.h.Dir, "tls")); w.file("compose.override.yaml") != override || !errors.Is(err, os.ErrNotExist) ||
		w.ups() != 0 || len(w.calls) != 0 || w.log("openssl.log") != "" {
		w.t.Fatalf("changed: override %q, tls/ %v, %d up(s), calls %q, openssl %q", w.file("compose.override.yaml"), err, w.ups(), w.calls,
			w.log("openssl.log"))
	}
}

// leafOf parses the first certificate of a PEM text.
func leafOf(t *testing.T, data string) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode([]byte(data))
	if block == nil {
		t.Fatalf("no certificate in %q", data)
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func ipsOf(c *x509.Certificate) string {
	var ips []string
	for _, ip := range c.IPAddresses {
		ips = append(ips, ip.String())
	}
	return strings.Join(ips, " ")
}

// leafPair is a self-signed P-256 leaf for name and ip, valid until notAfter,
// and its key: a pair that SelfSigned would not make.
func leafPair(t *testing.T, name, ip string, notAfter time.Time) host.TLSFiles {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		IPAddresses: []net.IP{net.ParseIP(ip)}, NotBefore: notAfter.Add(-48 * time.Hour), NotAfter: notAfter, BasicConstraintsValid: true,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return host.TLSFiles{"cert.pem": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		"key.pem": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})}
}

// ownPair writes a pair for proxy.example.com and 127.0.0.2 outside the
// clone and returns the certificate's and the key's paths and the pair.
func ownPair(t *testing.T) (cert, key string, pair host.TLSFiles) {
	t.Helper()
	pair = leafPair(t, "proxy.example.com", "127.0.0.2", time.Now().Add(time.Hour))
	dir := t.TempDir()
	cert, key = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	writeFile(t, cert, string(pair["cert.pem"]), 0o600)
	writeFile(t, key, string(pair["key.pem"]), 0o600)
	return cert, key, pair
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

// servedOn reports whether the data plane got a request on addr by name.
func (w *remoteWorld) servedOn(addr, name string) bool {
	return slices.Contains(w.served(), addr+":"+w.p+" "+name)
}

const recreatedToRead = ", and the container is then recreated to read it: requests in flight are given time to finish, and the rate-limit windows restart"

// TestOpenToNetworkSelfSigned is R1: a self-signed certificate on loopback
// and two addresses of this host, confirmed first; the override, tls/ and
// the certificate exactly; one forced recreate; each address checked over
// HTTPS by the certificate's name; no CLI call.
func TestOpenToNetworkSelfSigned(t *testing.T) {
	w := tlsWorld(t)
	gid := strconv.Itoa(os.Getegid())
	answers := map[string]string{"address": "127.0.0.2,127.0.0.3,127.0.0.2", "tls": "self-signed"}
	_, out, err := w.open(answers)
	change := "tls=self-signed tls_name=localhost tls_ca=cert.pem tls_group=" + gid + " addresses=127.0.0.1 127.0.0.2 127.0.0.3"
	var usage *ui.UsageError
	if !errors.As(err, &usage) || err.Error() != "missing --yes: writing compose.override.yaml with "+change+recreatedToRead {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if !inOrder(out, "  tls=self-signed", "  tls_name=localhost", "  tls_ca=cert.pem", "  tls_group="+gid,
		"  addresses=127.0.0.1 127.0.0.2 127.0.0.3", "writing compose.override.yaml with the lines above"+recreatedToRead) || strings.Contains(out, change) {
		t.Fatalf("the changed keys are not said before the confirm:\n%s", out)
	}
	w.untouched("")

	answers["yes"] = "yes"
	res, out, err := w.open(answers)
	if err != nil || res != "Done: the data plane answers over HTTPS on 127.0.0.1 127.0.0.2 127.0.0.3." {
		t.Fatalf("res %q, err %v\n%s", res, err, out)
	}
	want := host.OverrideMarker + "\n# setup: tls=self-signed\n# setup: tls_name=localhost\n# setup: tls_ca=cert.pem\n# setup: tls_group=" + gid +
		"\n# setup: addresses=127.0.0.1 127.0.0.2 127.0.0.3\nservices:\n  stargaite:\n    extra_hosts:\n      - \"localhost:127.0.0.1\"\n" +
		`    command: ["serve", "--listen", "0.0.0.0:4000", "--tls-cert", "/tls/cert.pem", "--tls-key", "/tls/key.pem"]` + "\n" +
		"    ports: !override\n      - \"127.0.0.1:${STARGAITE_PORT:-4000}:4000\"\n      - \"127.0.0.2:${STARGAITE_PORT:-4000}:4000\"\n" +
		"      - \"127.0.0.3:${STARGAITE_PORT:-4000}:4000\"\n    volumes:\n      - ./tls:/tls:ro\n    group_add:\n      - \"" + gid + "\"\n"
	if got := w.file("compose.override.yaml"); got != want {
		t.Fatalf("override:\n%s\nwant:\n%s", got, want)
	}
	tlsDir := filepath.Join(w.h.Dir, "tls")
	if mode(t, tlsDir) != 0o750 || mode(t, filepath.Join(tlsDir, "cert.pem")) != 0o640 || mode(t, filepath.Join(tlsDir, "key.pem")) != 0o640 ||
		w.log("chown.log") != "-R :"+gid+" tls\n" {
		t.Fatalf("tls/ modes or owner: chown %q", w.log("chown.log"))
	}
	c := leafOf(t, w.file("tls/cert.pem"))
	hostName, _ := os.Hostname()
	if c.Subject.CommonName != "localhost" || !slices.Equal(c.DNSNames, []string{"localhost", hostName}) ||
		ipsOf(c) != "127.0.0.1 127.0.0.2 127.0.0.3" || !c.BasicConstraintsValid || c.IsCA ||
		!slices.Equal(c.ExtKeyUsage, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}) || c.NotAfter.Sub(c.NotBefore) != 825*24*time.Hour {
		t.Fatalf("certificate: %+v", c)
	}
	for _, a := range []string{"127.0.0.1", "127.0.0.2", "127.0.0.3"} {
		if !w.servedOn(a, "localhost") {
			t.Fatalf("no HTTPS request by localhost on %s: %q", a, w.served())
		}
	}
	hasLines(t, out,
		"tls/ gets the certificate and its key, and from now on the data plane speaks HTTPS only: every client, local ones too, needs https:// and to trust the certificate",
		"the files are written: if the recreate stops before the container is recreated, docker compose up -d --no-build --force-recreate in this clone does it",
		"https on 127.0.0.1:"+w.p+": ready", "https on 127.0.0.2:"+w.p+": ready", "https on 127.0.0.3:"+w.p+": ready",
		"warning: Docker publishes past ufw and firewalld: every machine that reaches 127.0.0.2 127.0.0.3 reaches port "+w.p+
			"; limit it in your network's firewall or in the DOCKER-USER chain",
		"the certificate expires on "+c.NotAfter.UTC().Format("2006-01-02 15:04")+" UTC; ./setup.sh remote again renews it",
		"clients trust this one certificate, tls/cert.pem copied to their machine, beside their own bundle: curl --cacert cert.pem;",
		`  Python (requests, httpx) verify="cert.pem"; Node.js NODE_EXTRA_CA_CERTS=cert.pem; or the system's trust store`,
		"base URL https://localhost:"+w.p+"/v1 here; elsewhere, https://<an address the certificate holds>:"+w.p+"/v1",
		"the CLI from this directory: alias stargaite='docker compose exec -T -e STARGAITE_ADMIN_URL=https://localhost:4001 -e SSL_CERT_FILE=/tls/cert.pem stargaite /stargaite'",
	)
	if w.ups() != 1 || len(w.calls) != 0 || w.log("openssl.log") != "" {
		t.Fatalf("%d up(s), calls %q, openssl %q", w.ups(), w.calls, w.log("openssl.log"))
	}

	// Twenty addresses: each is in the transcript whole, on its own line,
	// which the confirm names; the usage error names them all.
	w = tlsWorld(t)
	var many []string
	for i := 2; i <= 21; i++ {
		many = append(many, "127.0.0."+strconv.Itoa(i))
	}
	t.Setenv("FAKE_HOSTNAME", strings.Join(many, " ")+" ")
	_, out, err = w.open(map[string]string{"address": strings.Join(many, ","), "tls": "self-signed"})
	if !errors.As(err, &usage) || !strings.Contains(err.Error(), "addresses=127.0.0.1 "+strings.Join(many, " ")+recreatedToRead) {
		t.Fatalf("err = %v", err)
	}
	if !slices.Contains(strings.Split(out, "\n"), "writing compose.override.yaml with the lines above"+recreatedToRead) ||
		!slices.Contains(strings.Split(out, "\n"), "  addresses=127.0.0.1 "+strings.Join(many, " ")) {
		t.Fatalf("twenty addresses:\n%s", out)
	}
	w.untouched("")
	// Twenty of fifteen characters: over a said line's 300 runes, still whole.
	w = tlsWorld(t)
	many = nil
	for i := 101; i <= 120; i++ {
		many = append(many, "127.100.100."+strconv.Itoa(i))
	}
	t.Setenv("FAKE_HOSTNAME", strings.Join(many, " ")+" ")
	_, out, _ = w.open(map[string]string{"address": strings.Join(many, ","), "tls": "self-signed"})
	if !slices.Contains(strings.Split(out, "\n"), "  addresses=127.0.0.1 "+strings.Join(many, " ")) {
		t.Fatalf("twenty long addresses:\n%s", out)
	}
	w.untouched("")

	// 127.0.0.1 alone: loopback only, never refused as not this host's.
	w = tlsWorld(t)
	t.Setenv("FAKE_HOSTNAME", "127.0.0.2 127.0.0.3 ")
	res, out, err = w.open(map[string]string{"address": "127.0.0.1", "tls": "self-signed", "yes": "yes"})
	if err != nil || res != "Done: the data plane answers over HTTPS on 127.0.0.1." || strings.Contains(out, "warning: Docker publishes") ||
		!strings.Contains(w.file("compose.override.yaml"), "# setup: addresses=127.0.0.1\n") ||
		!slices.Contains(strings.Split(out, "\n"), "https on 127.0.0.1:"+w.p+": ready") {
		t.Fatalf("loopback: res %q, err %v\n%s", res, err, out)
	}
}

// TestOpenToNetworkRenew is R1's renewal and R8: run again, it keeps the
// addresses published and the override, replaces the pair and recreates,
// even when the pair in tls/ has expired.
func TestOpenToNetworkRenew(t *testing.T) {
	w := tlsWorld(t)
	if _, out, err := w.open(map[string]string{"address": "127.0.0.2", "tls": "self-signed", "yes": "yes"}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	override, first := w.file("compose.override.yaml"), w.file("tls/cert.pem")
	_, out, err := w.open(map[string]string{"tls": "self-signed"})
	if err == nil || err.Error() != "missing --yes: the certificate and key in tls/ are replaced"+recreatedToRead || w.file("tls/cert.pem") != first ||
		w.ups() != 1 {
		t.Fatalf("err = %v, %d up(s)\n%s", err, w.ups(), out)
	}
	before := len(w.served())
	_, out, err = w.open(map[string]string{"tls": "self-signed", "yes": "yes"})
	renewed := w.file("tls/cert.pem")
	if err != nil || renewed == first || leafOf(t, renewed).SerialNumber.Cmp(leafOf(t, first).SerialNumber) == 0 ||
		w.file("compose.override.yaml") != override || w.ups() != 2 || !slices.Contains(strings.Split(out, "\n"), "https on 127.0.0.2:"+w.p+": ready") ||
		len(w.served()) <= before {
		t.Fatalf("renewal: err %v, %d up(s)\n%s", err, w.ups(), out)
	}

	// An expired pair in tls/: a client that trusts it refuses the plane,
	// and the renewal mends it.
	expired := leafPair(t, "localhost", "127.0.0.1", time.Now().Add(-time.Hour))
	writeFile(t, filepath.Join(w.h.Dir, "tls", "cert.pem"), string(expired["cert.pem"]), 0o640)
	writeFile(t, filepath.Join(w.h.Dir, "tls", "key.pem"), string(expired["key.pem"]), 0o640)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(expired["cert.pem"])
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "localhost"}}}
	if resp, err := client.Get("https://127.0.0.1:" + w.p + "/health/ready"); err == nil {
		_ = resp.Body.Close()
		t.Fatal("an expired certificate was trusted")
	}
	_, out, err = w.open(map[string]string{"tls": "self-signed", "yes": "yes"})
	if err != nil || !slices.Contains(strings.Split(out, "\n"), "https on 127.0.0.2:"+w.p+": ready") || w.ups() != 3 {
		t.Fatalf("expired: err %v\n%s", err, out)
	}
}

// TestOpenToNetworkRewrites: run again with another address, the override
// is rewritten; without --yes the usage error names the changed key and
// nothing is written; with it the confirm names the lines above.
func TestOpenToNetworkRewrites(t *testing.T) {
	w := tlsWorld(t)
	if _, out, err := w.open(map[string]string{"address": "127.0.0.2", "tls": "self-signed", "yes": "yes"}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	override, first := w.file("compose.override.yaml"), w.file("tls/cert.pem")
	answers := map[string]string{"address": "127.0.0.3", "tls": "self-signed"}
	_, out, err := w.open(answers)
	var usage *ui.UsageError
	if !errors.As(err, &usage) || err.Error() != `missing --yes: compose.override.yaml is rewritten with addresses=127.0.0.1 127.0.0.3, keeping its other "# setup:" lines`+recreatedToRead ||
		w.file("compose.override.yaml") != override || w.file("tls/cert.pem") != first || w.ups() != 1 {
		t.Fatalf("err = %v, %d up(s)\n%s", err, w.ups(), out)
	}
	answers["yes"] = "yes"
	_, out, err = w.open(answers)
	if err != nil || !inOrder(out, "  addresses=127.0.0.1 127.0.0.3",
		`compose.override.yaml is rewritten with the lines above, keeping its other "# setup:" lines`+recreatedToRead) ||
		!strings.Contains(w.file("compose.override.yaml"), "\n# setup: addresses=127.0.0.1 127.0.0.3\n") || w.ups() != 2 {
		t.Fatalf("err %v, %d up(s)\n%s\n%s", err, w.ups(), out, w.file("compose.override.yaml"))
	}
}

// TestOpenToNetworkOwn is R2, as root: the reader's pair copied byte for
// byte, owned by the container's uid, with a private CA trusted from tls/,
// or with a public CA trusted only from the system's roots.
func TestOpenToNetworkOwn(t *testing.T) {
	cert, key, pair := ownPair(t)
	for _, private := range []bool{true, false} {
		w := tlsWorld(t)
		w.h.UID = 0
		answers := map[string]string{"address": "127.0.0.2", "tls": "own", "tls-cert": cert, "tls-key": key, "tls-name": "proxy.example.com",
			"yes": "yes"}
		if private {
			answers["tls-ca"] = cert
		} else {
			w.h.ShortenWaits(200 * time.Millisecond)
		}
		res, out, err := w.open(answers)
		o := w.file("compose.override.yaml")
		tlsDir := filepath.Join(w.h.Dir, "tls")
		if !strings.Contains(o, "# setup: tls=own\n# setup: tls_name=proxy.example.com\n") ||
			!strings.Contains(o, "# setup: addresses=127.0.0.1 127.0.0.2\n") || strings.Contains(o, "tls_group") || strings.Contains(o, "group_add") ||
			w.file("tls/cert.pem") != string(pair["cert.pem"]) || w.file("tls/key.pem") != string(pair["key.pem"]) ||
			mode(t, tlsDir) != 0o700 || mode(t, filepath.Join(tlsDir, "cert.pem")) != 0o600 || mode(t, filepath.Join(tlsDir, "key.pem")) != 0o600 ||
			w.log("chown.log") != "-R 65532:65532 tls\n" || w.ups() != 1 || strings.Contains(out, "NODE_EXTRA_CA_CERTS") {
			t.Fatalf("private %v: override\n%s\nchown %q, %d up(s)\n%s", private, o, w.log("chown.log"), w.ups(), out)
		}
		if private {
			if err != nil || res != "Done: the data plane answers over HTTPS on 127.0.0.1 127.0.0.2." || !strings.Contains(o, "# setup: tls_ca=ca.pem\n") ||
				w.file("tls/ca.pem") != string(pair["cert.pem"]) || !w.servedOn("127.0.0.1", "proxy.example.com") ||
				!w.servedOn("127.0.0.2", "proxy.example.com") || !strings.Contains(out, "\nthe certificate expires on ") ||
				!slices.Contains(strings.Split(out, "\n"), "the CLI from this directory: alias stargaite='docker compose exec -T "+
					"-e STARGAITE_ADMIN_URL=https://proxy.example.com:4001 -e SSL_CERT_FILE=/tls/ca.pem stargaite /stargaite'") {
				t.Fatalf("private CA: res %q, err %v, served %q\n%s", res, err, w.served(), out)
			}
			continue
		}
		admin, aerr := w.h.AdminEnv()
		if err == nil || err.Error() != "the proxy did not become ready; ./setup.sh remote again repairs it" || strings.Contains(out, ": ready") ||
			strings.Contains(o, "tls_ca") || w.file("tls/ca.pem") != "" || aerr != nil ||
			!slices.Equal(admin, []string{"-e", "STARGAITE_ADMIN_URL=https://proxy.example.com:4001"}) {
			t.Fatalf("public CA: err %v, admin %q\n%s", err, admin, out)
		}
	}
}

// TestOpenToNetworkAnyAddress is R3 and R6: 0.0.0.0 publishes every address
// alone, checked at 127.0.0.1, and the certificate names this host's
// addresses as hostname -I gives them, or none without it.
func TestOpenToNetworkAnyAddress(t *testing.T) {
	for _, tc := range []struct {
		hostname, address, ips string
		known                  bool
	}{
		{"127.0.0.2 127.0.0.3 ", "127.0.0.3,0.0.0.0", "127.0.0.1 127.0.0.3 127.0.0.2", true},
		{"192.0.2.10 2001:db8::1 198.51.100.7 ", "0.0.0.0", "127.0.0.1 192.0.2.10 198.51.100.7", true},
		{"", "0.0.0.0", "127.0.0.1", false},
	} {
		w := tlsWorldOn(t, "0.0.0.0")
		t.Setenv("FAKE_HOSTNAME", tc.hostname)
		if !tc.known {
			_ = os.Remove(filepath.Join(w.bin, "hostname"))
			_ = os.Remove(filepath.Join(w.bin, "getent"))
		}
		res, out, err := w.open(map[string]string{"address": tc.address, "tls": "self-signed", "yes": "yes"})
		o := w.file("compose.override.yaml")
		warned := slices.Contains(strings.Split(out, "\n"), "warning: this host's addresses are unknown here (no hostname -I), "+
			"so the certificate names only those given beside 0.0.0.0 in --address")
		if err != nil || res != "Done: the data plane answers over HTTPS on 0.0.0.0." ||
			!strings.Contains(o, "# setup: addresses=0.0.0.0\n") ||
			!strings.Contains(o, "    ports: !override\n      - \"0.0.0.0:${STARGAITE_PORT:-4000}:4000\"\n    volumes:\n") ||
			!slices.Contains(strings.Split(out, "\n"), "https on 0.0.0.0:"+w.p+": ready") || strings.Contains(out, "https on 127.0.0.3") ||
			ipsOf(leafOf(t, w.file("tls/cert.pem"))) != tc.ips || warned == tc.known || w.log("getent.log") != "" && !tc.known ||
			!strings.Contains(out, "every machine that reaches this host reaches port "+w.p+";") {
			t.Fatalf("%q %q: res %q, err %v, IPs %q\n%s\n%s", tc.hostname, tc.address, res, err, ipsOf(leafOf(t, w.file("tls/cert.pem"))), o, out)
		}
	}
}

// TestOpenToNetworkRefusals is R9, R6's first half, O2 and O3: each refused
// before anything is written, a usage error exit 2, any other exit 1.
func TestOpenToNetworkRefusals(t *testing.T) {
	cert, key, _ := ownPair(t)
	_, otherKey, _ := ownPair(t)
	notPEM := filepath.Join(t.TempDir(), "not.pem")
	writeFile(t, notPEM, "not a certificate\n", 0o600)
	own := func(more ...string) map[string]string {
		a := map[string]string{"address": "127.0.0.2", "tls": "own", "tls-cert": cert, "tls-key": key, "tls-name": "proxy.example.com", "yes": "yes"}
		for i := 0; i+1 < len(more); i += 2 {
			a[more[i]] = more[i+1]
		}
		return a
	}
	selfSigned := map[string]string{"address": "127.0.0.2", "tls": "self-signed", "yes": "yes"}
	for _, tc := range []struct {
		name    string
		answers map[string]string
		ps      string // compose ps -q
		info    string
		mine    string // the reader's own override
		err     string // exact, or a prefix ending "("
		usage   bool
	}{
		{"no tls", map[string]string{"address": "127.0.0.2", "yes": "yes"}, "", "", "",
			"missing --tls: the certificate: own (yours, with its key) or self-signed (made here)", true},
		{"no tls-cert", map[string]string{"address": "127.0.0.2", "tls": "own", "yes": "yes"}, "", "", "",
			"missing --tls-cert: the absolute path of the certificate chain, in PEM", true},
		{"unreadable", own("tls-cert", "/nonexistent/nope.pem"), "", "", "", "/nonexistent/nope.pem is not a file the console can read; nothing was changed", false},
		{"relative", own("tls-key", "setup.sh"), "", "", "", "--tls-key: setup.sh is not an absolute path", true},
		{"three octets", map[string]string{"address": "192.0.2", "tls": "self-signed"}, "", "", "", "--address: 192.0.2 is not an IPv4 address, such as 192.0.2.10", true},
		{"octet 256", map[string]string{"address": "192.0.2.256"}, "", "", "", "--address: 192.0.2.256 is not an IPv4 address, such as 192.0.2.10", true},
		{"not this host's", map[string]string{"address": "127.0.0.2,203.0.113.5"}, "", "", "",
			"--address: 203.0.113.5 is not an address of this host, which has 127.0.0.2 127.0.0.3", true},
		{"plain", map[string]string{"address": "127.0.0.2", "tls": "plain"}, "", "", "", "--tls: plain is not one of own, self-signed", true},
		{"name", own("tls-name", "a b"), "", "", "", "--tls-name: a b is not a host name, such as proxy.example.com", true},
		{"own flags", map[string]string{"address": "127.0.0.2", "tls": "self-signed", "tls-name": "x", "yes": "yes"}, "", "", "",
			"--tls-cert, --tls-key, --tls-name and --tls-ca are for --tls own", true},
		{"rootless", selfSigned, "", "echo '[name=seccomp name=rootless]'", "", "rootless Docker or user-namespace remapping here: the container's uid " +
			"and groups are not this host's, so tls/ cannot be made readable to it; nothing was changed", false},
		{"userns", selfSigned, "", "echo '[name=userns]'", "", "rootless Docker or user-namespace remapping here: the container's uid " +
			"and groups are not this host's, so tls/ cannot be made readable to it; nothing was changed", false},
		{"not PEM", own("tls-cert", notPEM, "tls-key", notPEM), "", "", "", "the certificate and key do not match (", false},
		{"another key", own("tls-key", otherKey), "", "", "", "the certificate and key do not match (", false},
		{"own override", selfSigned, "", "", "services: {}\n",
			"compose.override.yaml was not written by setup.sh, so it is left as it is: move it away and run this again", false},
		{"no container", selfSigned, "", "", "", "no container here: ./setup.sh install makes it", false},
		{"stopped, port taken", selfSigned, ":", "", "", "", false},
	} {
		w := tlsWorld(t)
		ps, info := "echo c0ffee", "echo '[name=seccomp]'"
		if tc.ps != "" {
			ps = tc.ps
		}
		if tc.info != "" {
			info = tc.info
		}
		w.docker(ps, info, ":")
		if tc.name == "no container" {
			writeFile(t, filepath.Join(w.bin, "docker"), "#!/bin/sh\necho \"$*\" >>'"+w.bin+"/log'\n", 0o755)
		}
		if tc.name == "stopped, port taken" {
			tc.err = "a free host port: 127.0.0.1:" + w.p + " is taken; ./setup.sh install --port <n> publishes another"
		}
		if tc.mine != "" {
			writeFile(t, filepath.Join(w.h.Dir, "compose.override.yaml"), tc.mine, 0o600)
		}
		_, out, err := w.open(tc.answers)
		var usage *ui.UsageError
		exact := !strings.HasSuffix(tc.err, "(")
		if err == nil || exact && err.Error() != tc.err || !exact && !strings.HasPrefix(err.Error(), tc.err) || errors.As(err, &usage) != tc.usage {
			t.Fatalf("%s: err = %v, want %q (usage %v)\n%s", tc.name, err, tc.err, tc.usage, out)
		}
		w.untouched(tc.mine)
	}
}

// TestOpenToNetworkTLSWriteFails is R7: tls/ that cannot be written puts
// the override back byte for byte, or removes it when there was none.
func TestOpenToNetworkTLSWriteFails(t *testing.T) {
	marked := host.OverrideMarker + "\n# setup: host_gateway=1\nservices:\n  stargaite:\n    extra_hosts:\n      - \"host.docker.internal:host-gateway\"\n"
	for _, before := range []string{"", marked} {
		w := tlsWorld(t)
		writeFile(t, filepath.Join(w.h.Dir, "tls"), "a file\n", 0o600)
		if before != "" {
			writeFile(t, filepath.Join(w.h.Dir, "compose.override.yaml"), before, 0o600)
		}
		_, out, err := w.open(map[string]string{"address": "127.0.0.2", "tls": "self-signed", "yes": "yes"})
		_, statErr := os.Stat(filepath.Join(w.h.Dir, "compose.override.yaml"))
		if err == nil || err.Error() != "tls/ was not written, or not given its owner and modes; compose.override.yaml is as it was" ||
			!strings.Contains(out, "\nno temporary file beside tls/") ||
			w.file("compose.override.yaml") != before || before == "" && !errors.Is(statErr, os.ErrNotExist) || w.ups() != 0 {
			t.Fatalf("before %q: err %v, override %q\n%s", before, err, w.file("compose.override.yaml"), out)
		}
	}
}

// TestOpenToNetworkNotAnswering: an address that does not answer over
// HTTPS says why, then O14's error.
func TestOpenToNetworkNotAnswering(t *testing.T) {
	w := tlsWorldOn(t, "127.0.0.1", "127.0.0.2")
	_, out, err := w.open(map[string]string{"address": "127.0.0.2,127.0.0.3", "tls": "self-signed", "yes": "yes"})
	if err == nil || err.Error() != "the data plane did not answer over HTTPS on 127.0.0.3:"+w.p ||
		!inOrder(out, "https on 127.0.0.2:"+w.p+": ready", `/health/ready: Get "https://localhost:`+w.p+`/health/ready": dial tcp 127.0.0.3:`+w.p+
			": connect: connection refused") {
		t.Fatalf("err %v\n%s", err, out)
	}
}

// TestOpenToNetworkGroup is R5: the group the container joins is warned of
// when another user has it as primary group or it lists another member.
func TestOpenToNetworkGroup(t *testing.T) {
	gid := strconv.Itoa(os.Getegid())
	warning := "warning: group " + gid + ", which the container joins to read tls/, has other members, who can read its key too"
	me := "me:x:1000:" + gid + "::/home/me:/bin/sh\n"
	for _, tc := range []struct {
		passwd, group string
		warn          bool
	}{
		{me, "me:x:" + gid + ":\n", false},
		{me + "you:x:1001:" + gid + "::/home/you:/bin/sh\n", "me:x:" + gid + ":\n", true},
		{me, "users:x:" + gid + ":you\n", true},
		{me, "users:x:" + gid + ":me\n", false},
		{me, "users:x:" + gid + ":me,you\n", true},
	} {
		w := tlsWorld(t)
		t.Setenv("FAKE_PASSWD", tc.passwd)
		t.Setenv("FAKE_GROUP", tc.group)
		_, out, err := w.open(map[string]string{"address": "127.0.0.2", "tls": "self-signed"})
		if err == nil || slices.Contains(strings.Split(out, "\n"), warning) != tc.warn {
			t.Fatalf("%q %q: err %v, want warning %v\n%s", tc.passwd, tc.group, err, tc.warn, out)
		}
	}
}

// TestOpenToNetworkUpFails is R3's failed start and TestSetupBounds' "up
// outlives its bound": the recreate's error, then the repair; no HTTPS
// check and no CLI call.
func TestOpenToNetworkUpFails(t *testing.T) {
	for _, tc := range []struct{ up, err string }{
		{"exit 1", "docker compose up failed or was interrupted; its output is above; ./setup.sh remote again repairs it"},
		{"/bin/sleep 30 >/dev/null 2>&1", "docker compose up was not done within 20 minutes; its output is above; ./setup.sh remote again repairs it"},
	} {
		w := tlsWorld(t)
		w.docker("echo c0ffee", "echo '[name=seccomp]'", tc.up)
		w.h.ShortenWaits(200 * time.Millisecond)
		start := time.Now()
		_, out, err := w.open(map[string]string{"address": "127.0.0.2", "tls": "self-signed", "yes": "yes"})
		if err == nil || err.Error() != tc.err || time.Since(start) > 12*time.Second || len(w.served()) != 0 || len(w.calls) != 0 {
			t.Fatalf("%s: err %v after %s, served %q\n%s", tc.up, err, time.Since(start), w.served(), out)
		}
	}
}

// TestOpenToNetworkEsc: Esc at the addresses or at the confirm leaves the
// task with nothing written.
func TestOpenToNetworkEsc(t *testing.T) {
	for _, answers := range []map[string][]string{
		{"address": {leave}},
		{"address": {""}, "tls": {"self-signed"}, "yes": {leave}},
	} {
		w := tlsWorld(t)
		s := &scripted{answers: answers}
		_, err := w.openWith(ui.New(s, nil))
		if !errors.Is(err, ui.ErrLeave) {
			t.Fatalf("%v: err = %v\n%s", answers, err, s.output())
		}
		if !strings.HasPrefix(s.output(), "this host's addresses: 127.0.0.2 127.0.0.3\n") {
			t.Fatalf("the host's addresses are not said first:\n%s", s.output())
		}
		w.untouched("")
	}
}
