package task

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
	"io"
	"maps"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
	"github.com/jorlaru/stargaite/internal/console/ui/plain"
)

// receiver is an OTLP receiver on 127.0.0.1: it answers every span with its
// status and records each span's headers. The plane's exporter dials
// 127.0.0.1 for every name, so host.docker.internal:<port> reaches it.
type receiver struct {
	port  string
	mu    sync.Mutex
	spans []http.Header
}

func receive(t *testing.T, status int) *receiver {
	t.Helper()
	rc := &receiver{}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v1/traces") {
			rc.mu.Lock()
			rc.spans = append(rc.spans, r.Header.Clone())
			rc.mu.Unlock()
		}
		rw.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	rc.port = u.Port()
	return rc
}

// headers is the headers of each span received.
func (rc *receiver) headers() []http.Header {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return slices.Clone(rc.spans)
}

// sendTraces runs Send traces with u.
func (w *world) sendTraces(u *ui.UI) (string, error) {
	return u.Run(context.Background(), traceSteps(), func(ctx context.Context) (string, error) {
		return SendTraces(ctx, u, w.proxy(), w.h)
	})
}

// tracesPlain runs Send traces with the plain front, answers and environ;
// it returns the result, the output and the error.
func (w *world) tracesPlain(answers map[string]string, environ ...string) (string, string, error) {
	var out bytes.Buffer
	res, err := w.sendTraces(ui.New(plain.New(&out), answers).WithEnv(environ))
	return res, out.String(), err
}

// tracesAt runs Send traces with the scripted answers.
func (w *world) tracesAt(answers map[string][]string) (string, *scripted, error) {
	s := &scripted{answers: answers}
	res, err := w.sendTraces(ui.New(s, nil))
	return res, s, err
}

// telemetryNowIs is the endpoint telemetry status answers, "" for none.
func (w *world) telemetryNowIs() string {
	w.t.Helper()
	var st proxy.TelemetryReply
	w.decoded(&st, "telemetry", "status")
	if st.Telemetry == nil {
		return ""
	}
	return st.Telemetry.OTLPEndpoint
}

// setLangfuse sets a Langfuse block toward port through the CLI, before
// the run, and returns its endpoint.
func (w *world) setLangfuse(port string) string {
	w.t.Helper()
	ep := "http://host.docker.internal:" + port + "/api/public/otel"
	w.must("telemetry", "set", "--otlp-endpoint", ep, "--preset", "langfuse", "--allow-private-network")
	return ep
}

// gatewayDone writes the override's host-gateway line, as a run before this one did.
func (w *world) gatewayDone() {
	w.t.Helper()
	o, err := w.h.ReadOverride()
	if err == nil {
		o.Set("host_gateway", "1")
		err = w.h.WriteOverride(o)
	}
	if err != nil {
		w.t.Fatal(err)
	}
}

// t6 is the line said for an endpoint on the host's loopback, moved to ep.
func t6(ep string) string {
	return "localhost inside the container is the container: telemetry is reached at " + ep +
		", the host's bridge address, so it must listen beyond the host's loopback"
}

var accepted = regexp.MustCompile(`(?m)^credentials: resolved · reachable: yes · authorized: yes · span: accepted \(200\) · trace_id=[0-9a-f]{32}$`)

func TestSendTracesLangfuse(t *testing.T) {
	w, _, _ := keyWorld(t)
	rc := receive(t, 200)
	ep := "http://host.docker.internal:" + rc.port + "/api/public/otel"
	res, out, err := w.tracesPlain(map[string]string{"backend": "langfuse", "endpoint": "http://localhost:" + rc.port + "/api/public/otel",
		"langfuse-public-env": "PK", "langfuse-secret-env": "SK", "yes": "yes"}, "PK=pk-lf-x", "SK=sk-lf-y")
	if err != nil || res != "Done: traces go to "+ep+"." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	want := []string{"telemetry status --json", "telemetry set --json --otlp-endpoint " + ep + " --preset langfuse --allow-private-network",
		"telemetry status --json", "telemetry test --json"}
	if got := w.cli(); !slices.Equal(got, want) {
		t.Errorf("CLI calls %q\nwant %q", got, want)
	}
	env := w.file(".env")
	if !strings.Contains(env, "\nLANGFUSE_PUBLIC_KEY='pk-lf-x'\n") || !strings.Contains(env, "\nLANGFUSE_SECRET_KEY='sk-lf-y'\n") {
		t.Errorf(".env %q", env)
	}
	if fi, err := os.Stat(filepath.Join(w.h.Dir, ".env")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf(".env mode: %v, %v", fi, err)
	}
	if o := w.file("compose.override.yaml"); !strings.Contains(o, "# setup: host_gateway=1\n") || w.ups() != 1 {
		t.Errorf("override %q, %d up(s)", o, w.ups())
	}
	hasLines(t, out, t6(ep), "telemetry test: one span to "+ep)
	if strings.Contains(out, "Ollama") || !accepted.MatchString(out) {
		t.Errorf("output:\n%s", out)
	}
	spans := rc.headers()
	if len(spans) != 1 || spans[0].Get("X-Langfuse-Ingestion-Version") != "4" || !strings.HasPrefix(spans[0].Get("Authorization"), "Basic ") {
		t.Errorf("spans %v", spans)
	}
	for _, secret := range []string{"pk-lf-x", "sk-lf-y"} {
		if where := w.leaks(secret, out); len(where) > 0 {
			t.Errorf("%s found in %q", secret, where)
		}
	}
}

func TestSendTracesReplace(t *testing.T) {
	w, _, _ := keyWorld(t)
	rc1, rc2 := receive(t, 200), receive(t, 200)
	ep1, ep2 := "http://host.docker.internal:"+rc1.port+"/api/public/otel", "http://host.docker.internal:"+rc2.port
	if _, out, err := w.tracesPlain(map[string]string{"backend": "langfuse", "endpoint": "http://localhost:" + rc1.port + "/api/public/otel",
		"langfuse-public-env": "PK", "langfuse-secret-env": "SK", "yes": "yes"}, "PK=pk", "SK=sk"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	env := w.file(".env")
	otlp := map[string]string{"backend": "otlp", "endpoint": "http://localhost:" + rc2.port}
	before := len(w.cli())
	_, out, err := w.tracesPlain(otlp)
	var usage *ui.UsageError
	if !errors.As(err, &usage) || usage.Msg != "missing --yes: telemetry toward "+ep1+
		" is replaced whole, none of its credentials or headers kept: it is unset first, so a change the CLI refuses leaves it unset" ||
		!slices.Equal(w.cli()[before:], []string{"telemetry status --json"}) || w.telemetryNowIs() != ep1 {
		t.Fatalf("without yes: %v, CLI %q\n%s", err, w.cli()[before:], out)
	}
	before = len(w.cli())
	otlp["yes"] = "yes"
	res, out, err := w.tracesPlain(otlp)
	if err != nil || res != "Done: traces go to "+ep2+"." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	want := []string{"telemetry status --json", "telemetry unset --json --yes", "telemetry set --json --otlp-endpoint " + ep2 + " --allow-private-network",
		"telemetry status --json", "telemetry test --json"}
	if got := w.cli()[before:]; !slices.Equal(got, want) {
		t.Errorf("CLI calls %q\nwant %q", got, want)
	}
	spans := rc2.headers()
	if len(spans) != 1 || spans[0].Get("X-Langfuse-Ingestion-Version") != "" || spans[0].Get("Authorization") != "" {
		t.Errorf("the old block's headers reached the new endpoint: %v", spans)
	}
	if w.file(".env") != env {
		t.Errorf(".env changed: %q", w.file(".env"))
	}

	// A header's credential, resolved in the plane (admintest.AnnexBEnv), is
	// sent with each span.
	res, out, err = w.tracesPlain(map[string]string{"backend": "otlp", "endpoint": "http://localhost:" + rc2.port, "header-name": "Authorization",
		"key-env": "TOK", "yes": "yes"}, "TOK=Bearer t0k")
	if spans := rc2.headers(); err != nil || len(spans) != 2 || spans[1].Get("Authorization") != "x" || !accepted.MatchString(out) {
		t.Errorf("with a header: %q, %v, spans %v\n%s", res, err, spans, out)
	}
	if !strings.Contains(w.file(".env"), "\nOTLP_AUTHORIZATION='Bearer t0k'\n") {
		t.Errorf(".env %q", w.file(".env"))
	}
}

func TestSendTracesRefusedAfterUnset(t *testing.T) {
	w, _, _ := keyWorld(t)
	old := w.setLangfuse("4318")
	env := w.file(".env")
	_, out, err := w.tracesPlain(map[string]string{"backend": "otlp", "endpoint": "192.0.2.10:4318", "yes": "yes"})
	var usage *ui.UsageError
	if err == nil || errors.As(err, &usage) {
		t.Fatalf("err = %v\n%s", err, out)
	}
	hasLines(t, out, "telemetry is now unset; it was toward "+old)
	if !strings.Contains(err.Error(), "the proxy would refuse these traces:\nsettings: telemetry.otlp_endpoint: url cannot be parsed") {
		t.Errorf("the refusal's finding is not said: %v", err)
	}
	if w.telemetryNowIs() != "" || w.file(".env") != env {
		t.Errorf("telemetry %q, .env %q", w.telemetryNowIs(), w.file(".env"))
	}

	w, _, _ = keyWorld(t)
	_, out, err = w.tracesPlain(map[string]string{"backend": "otlp", "endpoint": "192.0.2.10:4318", "yes": "yes"})
	if err == nil {
		t.Fatalf("none set: no error\n%s", out)
	}
	hasLines(t, out, "telemetry is not set")
}

func TestSendTracesUsage(t *testing.T) {
	const ep = "https://192.0.2.10/api/public/otel"
	status := []string{"telemetry status --json"}
	for _, row := range []struct {
		answers map[string]string
		environ []string
		set     bool // a block set before the run
		msg     string
		cli     []string
	}{
		{map[string]string{}, nil, false, "missing --backend: Where do traces go?", nil},
		{map[string]string{"backend": "zipkin"}, nil, false, "--backend: zipkin is not one of langfuse, otlp, off", nil},
		{map[string]string{"backend": "langfuse"}, nil, false, "missing --endpoint: the Langfuse endpoint, https://<host>/api/public/otel", nil},
		{map[string]string{"backend": "langfuse", "endpoint": ep, "header-name": "X"}, nil, false, "--header-name and --key-env are for --backend otlp", nil},
		{map[string]string{"backend": "otlp", "endpoint": ep, "key-env": "V"}, []string{"V=v"}, false,
			"--key-env holds the credential of --header-name: give both", nil},
		{map[string]string{"backend": "langfuse", "endpoint": "https://pk:sk@192.0.2.10/api/public/otel", "yes": "yes"}, nil, true,
			"--endpoint: the endpoint must not hold a user or a password: its credentials are asked as keys, hidden", nil},
		{map[string]string{"backend": "langfuse", "endpoint": "https://pk:s/k@192.0.2.10/otel", "yes": "yes"}, nil, true,
			"--endpoint: the endpoint must not hold a user or a password: its credentials are asked as keys, hidden", nil},
		{map[string]string{"backend": "langfuse", "endpoint": "pk:sk@192.0.2.10/otel", "yes": "yes"}, nil, true,
			"--endpoint: the endpoint must not hold a user or a password: its credentials are asked as keys, hidden", nil},
		{map[string]string{"backend": "langfuse", "endpoint": "https://pk:100%pure@192.0.2.10/otel", "yes": "yes"}, nil, true,
			"--endpoint: the endpoint must not hold a user or a password: its credentials are asked as keys, hidden", nil},
		{map[string]string{"backend": "langfuse", "endpoint": " https://pk:sk@192.0.2.10/otel", "yes": "yes"}, nil, true,
			"--endpoint: the endpoint must not hold a user or a password: its credentials are asked as keys, hidden", nil},
		{map[string]string{"backend": "langfuse", "endpoint": ep, "key-env": "V"}, nil, false, "--header-name and --key-env are for --backend otlp", nil},
		{map[string]string{"backend": "langfuse", "endpoint": ep}, nil, false, "missing --langfuse-public-env: LANGFUSE_PUBLIC_KEY", status},
		{map[string]string{"backend": "otlp", "endpoint": ep, "header-name": "Authorization"}, nil, false, "missing --key-env: OTLP_AUTHORIZATION", status},
		{map[string]string{"backend": "otlp", "endpoint": ep, "header-name": "Authorization", "key-env": "V"}, []string{"V=v"}, false,
			"missing --yes: writing OTLP_AUTHORIZATION to .env, and the container is then recreated", status},
	} {
		w, _, _ := keyWorld(t)
		old := ""
		if row.set {
			old = w.setLangfuse("4318")
		}
		files := w.files()
		_, out, err := w.tracesPlain(row.answers, row.environ...)
		var usage *ui.UsageError
		if !errors.As(err, &usage) || !strings.HasPrefix(usage.Msg, row.msg) {
			t.Errorf("%v: %v; want %q\n%s", row.answers, err, row.msg, out)
		}
		if got := w.cli(); !slices.Equal(got, row.cli) {
			t.Errorf("%v: CLI calls %q; want %q", row.answers, got, row.cli)
		}
		if got := w.files(); !maps.Equal(got, files) || w.telemetryNowIs() != old || strings.Contains(out, "@192.0.2.10") {
			t.Errorf("%v: files %v, telemetry %q\n%s", row.answers, got, w.telemetryNowIs(), out)
		}
	}
}

func TestSendTracesSecretRefused(t *testing.T) {
	answers := map[string]string{"backend": "otlp", "endpoint": "https://192.0.2.10:4318", "header-name": "Authorization", "key-env": "V", "yes": "yes"}
	for _, v := range []string{"it's", "a\nb", `x\`} {
		w, _, _ := keyWorld(t)
		env := w.file(".env")
		_, out, err := w.tracesPlain(answers, "V="+v)
		var usage *ui.UsageError
		if err == nil || errors.As(err, &usage) || err.Error() != `OTLP_AUTHORIZATION holds ' or a newline, or ends in \: .env cannot hold it, so nothing was written` {
			t.Errorf("%q: %v\n%s", v, err, out)
		}
		if w.file(".env") != env || len(changing(w.cli())) != 0 {
			t.Errorf("%q: .env %q, changes %q", v, w.file(".env"), changing(w.cli()))
		}
	}
	for _, name := range []string{"sk-pasted-value", "NOT_EXPORTED"} {
		w, _, _ := keyWorld(t)
		pasted := map[string]string{}
		maps.Copy(pasted, answers)
		pasted["key-env"] = name
		_, out, err := w.tracesPlain(pasted, "V=v")
		var usage *ui.UsageError
		if !errors.As(err, &usage) || usage.Msg != "--key-env takes the name of a variable exported to the console" || strings.Contains(out, name) {
			t.Errorf("%q: %v\n%s", name, err, out)
		}
	}
}

func TestSendTracesKeptInEnv(t *testing.T) {
	w, _, _ := keyWorld(t)
	rc := receive(t, 200)
	ep := "http://host.docker.internal:" + rc.port
	writeFile(t, filepath.Join(w.h.Dir, ".env"), w.file(".env")+"OTLP_X_TOK='v'\n", 0o600)
	w.gatewayDone()
	const t8 = "the container is recreated if it has not read OTLP_X_TOK yet: requests in flight are given time to finish, and the rate-limit windows restart"
	answers := map[string]string{"backend": "otlp", "endpoint": "http://localhost:" + rc.port, "header-name": "X-Tok"}
	_, out, err := w.tracesPlain(answers)
	var usage *ui.UsageError
	if !errors.As(err, &usage) || usage.Msg != "missing --yes: "+t8 || len(changing(w.cli())) != 0 || w.telemetryNowIs() != "" {
		t.Fatalf("without yes: %v, changes %q\n%s", err, changing(w.cli()), out)
	}
	answers["yes"] = "yes"
	_, out, err = w.tracesPlain(answers)
	if err == nil || err.Error() != "the test span was not accepted: telemetry stays set toward "+ep {
		t.Fatalf("err = %v\n%s", err, out)
	}
	hasLines(t, out, "OTLP_X_TOK is in .env; kept", t8)
	if !regexp.MustCompile(`(?m)^credentials: unresolved · `).MatchString(out) || w.ups() != 1 || strings.Contains(out, "missing --key-env") {
		t.Errorf("%d up(s)\n%s", w.ups(), out)
	}

	answers["replace"], answers["key-env"] = "yes", "V"
	_, out, _ = w.tracesPlain(answers, "V=new")
	if env := w.file(".env"); !strings.Contains(env, "\nOTLP_X_TOK='new'\n") {
		t.Errorf(".env %q\n%s", env, out)
	}
}

func TestSendTracesLocalNoHeader(t *testing.T) {
	w, _, _ := keyWorld(t)
	w.onMock("acme", "--protocol", "openai", "--billing", "pay_per_use")
	w.target("acme/m1", "--kind", "chat") // unpriced and unchecked: not telemetry's to say
	rc := receive(t, 200)
	ep := "http://host.docker.internal:" + rc.port
	answers := map[string]string{"backend": "otlp", "endpoint": "http://127.0.0.1:" + rc.port}
	_, out, err := w.tracesPlain(answers)
	var usage *ui.UsageError
	if !errors.As(err, &usage) || !strings.HasPrefix(usage.Msg, "missing --yes: writing compose.override.yaml with host_gateway=1") ||
		w.file("compose.override.yaml") != "" {
		t.Fatalf("without yes: %v, override %q\n%s", err, w.file("compose.override.yaml"), out)
	}
	before := len(w.cli())
	answers["yes"] = "yes"
	res, out, err := w.tracesPlain(answers)
	if err != nil || res != "Done: traces go to "+ep+"." {
		t.Fatalf("%q, %v\n%s", res, err, out)
	}
	want := []string{"telemetry status --json", "telemetry set --json --otlp-endpoint " + ep + " --allow-private-network", "telemetry status --json", "telemetry test --json"}
	if got := w.cli()[before:]; !slices.Equal(got, want) {
		t.Errorf("CLI calls %q", got)
	}
	if o := w.file("compose.override.yaml"); !strings.Contains(o, "\n      - \"host.docker.internal:host-gateway\"\n") {
		t.Errorf("override %q", o)
	}
	hasLines(t, out, t6(ep), "telemetry test: one span to "+ep)
	if !accepted.MatchString(out) || strings.Contains(out, "Ollama") || strings.Contains(out, "acme/m1") || len(rc.headers()) != 1 {
		t.Errorf("%d span(s)\n%s", len(rc.headers()), out)
	}
}

// localhostTLS serves next over TLS on 127.0.0.1 with a self-signed
// certificate for localhost; it returns the port and the certificate in PEM.
func localhostTLS(t *testing.T, next http.Handler) (string, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"localhost"}, IsCA: true, BasicConstraintsValid: true,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(next)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "https://127.0.0.1:"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestSendTracesOverride is TestSetupOverride's successor (design section
// 10): an override the reader wrote is never rewritten, and a marked one
// keeps its TLS lines when the host-gateway line joins.
func TestSendTracesOverride(t *testing.T) {
	answers := func(rc *receiver) map[string]string {
		return map[string]string{"backend": "otlp", "endpoint": "http://127.0.0.1:" + rc.port, "yes": "yes"}
	}
	mine := "services:\n  stargaite:\n    environment:\n      MINE: \"1\"\n"
	w, _, _ := keyWorld(t)
	writeFile(t, filepath.Join(w.h.Dir, "compose.override.yaml"), mine, 0o600)
	_, out, err := w.tracesPlain(answers(receive(t, 200)))
	if err == nil || err.Error() != `compose.override.yaml was not written by setup.sh, so it is left as it is: add "host.docker.internal:host-gateway" `+
		`to the extra_hosts of its stargaite service yourself, or move it away and run this again` ||
		w.file("compose.override.yaml") != mine || len(changing(w.cli())) != 0 {
		t.Errorf("mine: %v, override %q, changes %q\n%s", err, w.file("compose.override.yaml"), changing(w.cli()), out)
	}

	theirs := mine + "    extra_hosts:\n      - host.docker.internal:host-gateway\n"
	w, _, _ = keyWorld(t)
	writeFile(t, filepath.Join(w.h.Dir, "compose.override.yaml"), theirs, 0o600)
	rc := receive(t, 200)
	res, out, err := w.tracesPlain(answers(rc))
	if err != nil || res != "Done: traces go to http://host.docker.internal:"+rc.port+"." || w.file("compose.override.yaml") != theirs || len(rc.headers()) != 1 {
		t.Errorf("theirs: %q, %v, override %q\n%s", res, err, w.file("compose.override.yaml"), out)
	}

	// The control: a marked override with TLS, the data plane behind it.
	w, _, port := keyWorld(t)
	plane, _ := url.Parse("http://127.0.0.1:" + port)
	tlsPort, cert := localhostTLS(t, httputil.NewSingleHostReverseProxy(plane))
	writeFile(t, filepath.Join(w.h.Dir, ".env"), "STARGAITE_PORT="+tlsPort+"\n", 0o600)
	writeFile(t, filepath.Join(w.h.Dir, "tls", "cert.pem"), string(cert), 0o600)
	marker := "# Written by setup.sh from its \"# setup:\" lines below; it rewrites this file whole.\n"
	setup := "# setup: tls=self-signed\n# setup: tls_name=localhost\n# setup: tls_ca=cert.pem\n# setup: addresses=127.0.0.1\n"
	tlsBlock := "    command: [\"serve\", \"--listen\", \"0.0.0.0:4000\", \"--tls-cert\", \"/tls/cert.pem\", \"--tls-key\", \"/tls/key.pem\"]\n" +
		"    ports: !override\n      - \"127.0.0.1:${STARGAITE_PORT:-4000}:4000\"\n    volumes:\n      - ./tls:/tls:ro\n"
	writeFile(t, filepath.Join(w.h.Dir, "compose.override.yaml"),
		marker+setup+"services:\n  stargaite:\n    extra_hosts:\n      - \"localhost:127.0.0.1\"\n"+tlsBlock, 0o600)
	rc = receive(t, 200)
	ep := "http://host.docker.internal:" + rc.port
	res, out, err = w.tracesPlain(answers(rc))
	if err != nil || res != "Done: traces go to "+ep+"." || len(rc.headers()) != 1 {
		t.Fatalf("control: %q, %v, %d span(s)\n%s", res, err, len(rc.headers()), out)
	}
	want := marker + setup + "# setup: host_gateway=1\nservices:\n  stargaite:\n    extra_hosts:\n      - \"host.docker.internal:host-gateway\"\n" +
		"      - \"localhost:127.0.0.1\"\n" + tlsBlock
	if got := w.file("compose.override.yaml"); got != want {
		t.Errorf("override:\n%s\nwant:\n%s", got, want)
	}
	if !slices.Contains(w.cli(), "telemetry set --json --otlp-endpoint "+ep+" --allow-private-network") {
		t.Errorf("CLI calls %q", w.cli())
	}
}

func TestSendTracesTestFails(t *testing.T) {
	w, _, _ := keyWorld(t)
	rc := receive(t, 401)
	ep := "http://host.docker.internal:" + rc.port
	_, out, err := w.tracesPlain(map[string]string{"backend": "otlp", "endpoint": "http://localhost:" + rc.port, "yes": "yes"})
	if err == nil || err.Error() != "the test span was not accepted: telemetry stays set toward "+ep {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if !regexp.MustCompile(`(?m)^credentials: resolved · reachable: yes · authorized: no \(401\)`).MatchString(out) || strings.Contains(out, "failed with exit") {
		t.Errorf("output:\n%s", out)
	}
}

func TestSendTracesOff(t *testing.T) {
	const off = "telemetry toward http://host.docker.internal:4318/api/public/otel is turned off: no span is sent, and the variables of its credentials stay in .env"
	for _, yes := range []string{"yes", "no"} {
		w, _, _ := keyWorld(t)
		w.setLangfuse("4318")
		env := w.file(".env")
		res, s, err := w.tracesAt(map[string][]string{"backend": {"off"}, "yes": {yes}})
		hasLines(t, s.output(), off)
		switch {
		case yes == "yes" && (err != nil || res != "Done: telemetry is off." || !slices.Equal(changing(w.cli()), []string{"telemetry unset --json --yes"}) ||
			w.telemetryNowIs() != "" || w.file(".env") != env):
			t.Errorf("off: %q, %v, changes %q\n%s", res, err, changing(w.cli()), s.output())
		case yes == "no" && (err == nil || err.Error() != "stopped; nothing more was changed" || len(changing(w.cli())) != 0):
			t.Errorf("declined: %v, changes %q", err, changing(w.cli()))
		}
	}

	w, _, _ := keyWorld(t)
	if res, out, err := w.tracesPlain(map[string]string{"backend": "off"}); err != nil || res != "Telemetry is not set; nothing was changed." || len(changing(w.cli())) != 0 {
		t.Errorf("none set: %q, %v, changes %q\n%s", res, err, changing(w.cli()), out)
	}
	w.setLangfuse("4318")
	_, out, err := w.tracesPlain(map[string]string{"backend": "off"})
	var usage *ui.UsageError
	if !errors.As(err, &usage) || usage.Msg != "missing --yes: "+off {
		t.Errorf("flags without yes: %v\n%s", err, out)
	}
	if res, out, err := w.tracesPlain(map[string]string{"backend": "off", "yes": "yes"}); err != nil || res != "Done: telemetry is off." {
		t.Errorf("flags: %q, %v\n%s", res, err, out)
	}
}

// TestSendTracesEscBeforeWrite: Esc at any question before the write leaves
// .env, the override and telemetry as they were (design Review Focus 5).
func TestSendTracesEscBeforeWrite(t *testing.T) {
	// The replace of the block set first is agreed; Esc at the secret, then at the write.
	for _, at := range []map[string][]string{{"key-env": {leave}, "yes": {"yes"}}, {"key-env": {"Bearer t0k"}, "yes": {"yes", leave}}} {
		w, _, _ := keyWorld(t)
		old := w.setLangfuse("4318")
		rc := receive(t, 200)
		answers := map[string][]string{"backend": {"otlp"}, "endpoint": {"http://localhost:" + rc.port}, "header-name": {"Authorization"}}
		maps.Copy(answers, at)
		files := w.files()
		_, s, err := w.tracesAt(answers)
		if !errors.Is(err, ui.ErrLeave) || len(changing(w.cli())) != 0 || strings.Contains(s.output(), "already changed") ||
			!maps.Equal(w.files(), files) || w.telemetryNowIs() != old {
			t.Errorf("%v: %v, changes %q, telemetry %q\n%s", at, err, changing(w.cli()), w.telemetryNowIs(), s.output())
		}
	}
}

func TestSendTracesNotRunning(t *testing.T) {
	w := newWorld(t)
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "docker"), "#!/bin/sh\n[ \"$*\" = \"compose ps -q\" ] || exit 9\n", 0o755)
	t.Setenv("PATH", bin)
	_, out, err := w.tracesPlain(map[string]string{"backend": "off"})
	if err == nil || err.Error() != "no container runs here: ./setup.sh install starts it" || len(w.cli()) != 0 {
		t.Fatalf("err = %v, CLI %q\n%s", err, w.cli(), out)
	}
}
