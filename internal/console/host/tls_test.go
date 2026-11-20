package host

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// leaf parses the first PEM block of cert.
func leaf(t *testing.T, cert []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(cert)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("no certificate in %q", cert)
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSelfSigned(t *testing.T) {
	files, err := SelfSigned([]string{"127.0.0.1", "127.0.0.2", "0.0.0.0", "127.0.0.2", "2001:db8::1"})
	if err != nil || len(files) != 2 {
		t.Fatalf("%d files, err %v", len(files), err)
	}
	c := leaf(t, files["cert.pem"])
	dns := []string{"localhost"}
	if name, _ := os.Hostname(); name != "" {
		dns = append(dns, name)
	}
	var ips []string
	for _, ip := range c.IPAddresses {
		ips = append(ips, ip.String())
	}
	if c.Subject.CommonName != "localhost" || !slices.Equal(c.DNSNames, dns) || !slices.Equal(ips, []string{"127.0.0.1", "127.0.0.2"}) {
		t.Fatalf("CN %q, DNS %q, IPs %q", c.Subject.CommonName, c.DNSNames, ips)
	}
	critical := false
	for _, e := range c.Extensions {
		critical = critical || (e.Id.Equal(asn1.ObjectIdentifier{2, 5, 29, 19}) && e.Critical)
	}
	if !c.BasicConstraintsValid || c.IsCA || !critical || !slices.Equal(c.ExtKeyUsage, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}) ||
		c.KeyUsage != 0 || c.NotAfter.Sub(c.NotBefore) != 825*24*time.Hour {
		t.Fatalf("basic %v, CA %v, critical %v, EKU %v, KU %v, valid %v", c.BasicConstraintsValid, c.IsCA, critical, c.ExtKeyUsage,
			c.KeyUsage, c.NotAfter.Sub(c.NotBefore))
	}
	if _, err := tls.X509KeyPair(files["cert.pem"], files["key.pem"]); err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(files["key.pem"])
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if ec, ok := key.(*ecdsa.PrivateKey); err != nil || block.Type != "PRIVATE KEY" || !ok || ec.Curve != elliptic.P256() {
		t.Fatalf("key %T, %q, %v", key, block.Type, err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(c)
	if _, err := c.Verify(x509.VerifyOptions{Roots: roots, DNSName: "localhost"}); err != nil {
		t.Fatal(err)
	}
	again, err := SelfSigned([]string{"127.0.0.1"})
	if err != nil || leaf(t, again["cert.pem"]).SerialNumber.Cmp(c.SerialNumber) == 0 {
		t.Fatalf("a second certificate: %v, the same serial", err)
	}
}

// pairFiles writes a SelfSigned pair to files and returns their paths.
func pairFiles(t *testing.T) (cert, key string, files TLSFiles) {
	t.Helper()
	files, err := SelfSigned([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cert, key = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	write(t, cert, string(files["cert.pem"]))
	write(t, key, string(files["key.pem"]))
	return cert, key, files
}

func TestOwnTLS(t *testing.T) {
	h := New(t.TempDir())
	cert, key, pair := pairFiles(t)
	got, err := h.OwnTLS(cert, key, cert)
	if err != nil || len(got) != 3 || string(got["cert.pem"]) != string(pair["cert.pem"]) || string(got["key.pem"]) != string(pair["key.pem"]) ||
		string(got["ca.pem"]) != string(pair["cert.pem"]) {
		t.Fatalf("with a CA: %d files, %v", len(got), err)
	}
	if got, err := h.OwnTLS(cert, key, ""); err != nil || len(got) != 2 || got["ca.pem"] != nil {
		t.Fatalf("without a CA: %d files, %v", len(got), err)
	}
	dir := t.TempDir()
	if _, err := h.OwnTLS(dir, key, ""); err == nil || err.Error() != dir+" is not a file the console can read; nothing was changed" {
		t.Fatalf("a directory: %v", err)
	}
	_, otherKey, _ := pairFiles(t)
	if _, err := h.OwnTLS(cert, otherKey, ""); err == nil || !strings.HasPrefix(err.Error(), "the certificate and key do not match (") ||
		!strings.HasSuffix(err.Error(), "); nothing was changed") {
		t.Fatalf("another key: %v", err)
	}
	if os.Getuid() == 0 {
		t.Skip("root reads a file of mode 000")
	}
	if err := os.Chmod(key, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := h.OwnTLS(cert, key, ""); err == nil || err.Error() != key+" is not a file the console can read; nothing was changed" {
		t.Fatalf("unreadable: %v", err)
	}
}

// modes is the mode of tls/ and of each of its files, by name.
func modes(t *testing.T, dir string) map[string]os.FileMode {
	t.Helper()
	got := map[string]os.FileMode{}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	got["."] = info.Mode().Perm()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		got[e.Name()] = info.Mode().Perm()
	}
	return got
}

func TestWriteTLS(t *testing.T) {
	logs := t.TempDir()
	log := filepath.Join(logs, "argv")
	fakes(t, map[string]string{"chown": `echo "$*" >> ` + log + "\n"})
	files, err := SelfSigned([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		uid              int
		chown            string
		dirMode, fileMod os.FileMode
	}{
		{1000, "-R :" + strconv.Itoa(os.Getegid()) + " tls\n", 0o750, 0o640},
		{0, "-R 65532:65532 tls\n", 0o700, 0o600},
	} {
		_ = os.Remove(log)
		h := New(t.TempDir())
		h.UID = tc.uid
		if err := h.WriteTLS(context.Background(), files); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(h.Dir, "tls")
		want := map[string]os.FileMode{".": tc.dirMode, "cert.pem": tc.fileMod, "key.pem": tc.fileMod}
		if got := modes(t, dir); len(got) != 3 || got["."] != want["."] || got["cert.pem"] != want["cert.pem"] || got["key.pem"] != want["key.pem"] ||
			read(t, log) != tc.chown {
			t.Fatalf("uid %d: modes %v, chown %q", tc.uid, got, read(t, log))
		}
		if read(t, filepath.Join(dir, "cert.pem")) != string(files["cert.pem"]) || read(t, filepath.Join(dir, "key.pem")) != string(files["key.pem"]) {
			t.Fatalf("uid %d: the files are not the bytes given", tc.uid)
		}
		renewed, err := SelfSigned([]string{"127.0.0.1"})
		if err != nil {
			t.Fatal(err)
		}
		if err := h.WriteTLS(context.Background(), renewed); err != nil {
			t.Fatal(err)
		}
		if got := modes(t, dir); len(got) != 3 || read(t, filepath.Join(dir, "cert.pem")) != string(renewed["cert.pem"]) {
			t.Fatalf("uid %d: after a second write: %v", tc.uid, got)
		}
	}
	// A renewal whose chown fails leaves the pair in tls/ as it was, still
	// readable by the container, and no temporary file.
	kept := New(t.TempDir())
	if err := kept.WriteTLS(context.Background(), files); err != nil {
		t.Fatal(err)
	}
	fakes(t, map[string]string{"chown": "exit 1\n"})
	renewed, err := SelfSigned([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(kept.Dir, "tls")
	want := map[string]os.FileMode{".": 0o750, "cert.pem": 0o640, "key.pem": 0o640}
	if err := kept.WriteTLS(context.Background(), renewed); err == nil || !maps.Equal(modes(t, dir), want) ||
		read(t, filepath.Join(dir, "cert.pem")) != string(files["cert.pem"]) || read(t, filepath.Join(dir, "key.pem")) != string(files["key.pem"]) {
		t.Fatalf("chown fails: %v, modes %v", err, modes(t, dir))
	}
	fakes(t, map[string]string{"chown": `echo "$*" >> ` + log + "\n"})
	_ = os.Remove(log)
	h := New(t.TempDir())
	write(t, filepath.Join(h.Dir, "tls"), "a file\n")
	if err := h.WriteTLS(context.Background(), files); err == nil || read(t, filepath.Join(h.Dir, "tls")) != "a file\n" || read(t, log) != "" {
		t.Fatalf("tls a file: %v, chown %q", err, read(t, log))
	}
	if entries, _ := os.ReadDir(h.Dir); len(entries) != 1 {
		t.Fatalf("left in the clone: %v", entries)
	}
}

func TestAddressesAndRootless(t *testing.T) {
	fakes(t, map[string]string{
		"hostname": "[ \"$*\" = -I ] || exit 9\nprintf '127.0.0.2 127.0.0.3 \\n'\n",
		"docker":   "[ \"$*\" = 'info --format {{.SecurityOptions}}' ] || exit 9\n[ -n \"$FAKE\" ] || exit 1\necho \"$FAKE\"\n",
	})
	h := New(t.TempDir())
	ctx := context.Background()
	if words, ok := h.Addresses(ctx); words != "127.0.0.2 127.0.0.3" || !ok {
		t.Fatalf("Addresses = %q, %v", words, ok)
	}
	for _, tc := range []struct {
		info string
		want bool
	}{{"[name=seccomp,profile=builtin name=rootless]", true}, {"[name=userns]", true}, {"[name=seccomp,profile=builtin name=cgroupns]", false}} {
		t.Setenv("FAKE", tc.info)
		if got, err := h.Rootless(ctx); got != tc.want || err != nil {
			t.Fatalf("%s: %v, %v", tc.info, got, err)
		}
	}
	t.Setenv("FAKE", "")
	if _, err := h.Rootless(ctx); err == nil {
		t.Fatal("a failing docker info is no error")
	}
	fakes(t, map[string]string{})
	if words, ok := h.Addresses(ctx); words != "" || ok {
		t.Fatalf("no hostname: %q, %v", words, ok)
	}
}

func TestGroupShared(t *testing.T) {
	gid := "4242"
	fakes(t, map[string]string{
		"getent": "case \"$1\" in passwd) printf '%s' \"$FAKE_PASSWD\";; group) [ \"$2\" = 4242 ] && printf '%s' \"$FAKE_GROUP\";; esac\n",
		"id":     "[ \"$*\" = -un ] || exit 9\n[ -z \"$FAKE_NONAME\" ] || { echo 1000; exit 1; }\necho me\n",
	})
	h := New(t.TempDir())
	me := "me:x:1000:" + gid + "::/home/me:/bin/sh\n"
	for _, tc := range []struct {
		passwd, group, noname string
		want                  bool
	}{
		{me, "me:x:" + gid + ":\n", "", false},
		{me + "you:x:1001:" + gid + "::/home/you:/bin/sh\n", "me:x:" + gid + ":\n", "", true},
		{me, "users:x:" + gid + ":you\n", "", true},
		{me, "users:x:" + gid + ":me\n", "", false},
		{me, "users:x:" + gid + ":me,you\n", "", true},
		{me, "users:x:" + gid + ":me\n", "yes", true}, // id -un fails: no name is excluded
	} {
		t.Setenv("FAKE_PASSWD", tc.passwd)
		t.Setenv("FAKE_GROUP", tc.group)
		t.Setenv("FAKE_NONAME", tc.noname)
		if got := h.GroupShared(context.Background(), gid); got != tc.want {
			t.Fatalf("%q %q %q: %v", tc.passwd, tc.group, tc.noname, got)
		}
	}
	fakes(t, map[string]string{"id": "echo me\n"})
	if h.GroupShared(context.Background(), gid) {
		t.Fatal("no getent: shared")
	}
}

func TestCertExpiry(t *testing.T) {
	files, err := SelfSigned([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	h := New(t.TempDir())
	write(t, filepath.Join(h.Dir, "tls", "cert.pem"), string(files["cert.pem"]))
	got, err := h.CertExpiry()
	if want := leaf(t, files["cert.pem"]).NotAfter; err != nil || !got.Equal(want) || time.Until(got) < 824*24*time.Hour {
		t.Fatalf("CertExpiry = %v, %v; want %v", got, err, want)
	}
	write(t, filepath.Join(h.Dir, "tls", "cert.pem"), "not PEM\n")
	if _, err := h.CertExpiry(); err == nil {
		t.Fatal("not PEM: no error")
	}
}
