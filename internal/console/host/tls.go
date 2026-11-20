package host

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// nssBound bounds each read of the host's users and groups: getent and id.
const nssBound = 10 * time.Second

// TLSFiles is what tls/ gets, by file name: cert.pem, key.pem and, for a
// private CA, ca.pem.
type TLSFiles map[string][]byte

// Addresses is this host's addresses as hostname -I gives them, within
// h.bound, the trailing space cut; ok is false when hostname is missing or
// fails.
func (h *Host) Addresses(ctx context.Context) (words string, ok bool) {
	bounded, cancel := context.WithTimeout(ctx, h.bound)
	defer cancel()
	out, code, err := h.Capture(bounded, "hostname", "-I")
	if err != nil || code != 0 {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// Rootless reports whether Docker runs rootless or with user-namespace
// remapping, where the container's uid and groups are not this host's.
func (h *Host) Rootless(ctx context.Context) (bool, error) {
	bounded, cancel := context.WithTimeout(ctx, h.bound)
	defer cancel()
	out, code, err := h.Capture(bounded, "docker", "info", "--format", "{{.SecurityOptions}}")
	switch {
	case err != nil:
		return false, err
	case code != 0:
		return false, fmt.Errorf("docker info failed with exit %d", code)
	}
	return regexp.MustCompile(`name=(rootless|userns)`).Match(out), nil
}

// TLSGroup is the group the container joins to read tls/: this user's
// effective group, or none as root, where tls/ is the container's own.
func (h *Host) TLSGroup() string {
	if h.UID == 0 {
		return ""
	}
	return strconv.Itoa(os.Getegid())
}

// GroupShared reports whether group gid has members other than this user,
// as setup.sh's remote reads them: more than one user whose primary group it
// is, or a member listed that is not this user's name. Without getent it
// is false; when id -un fails, no name is this user's.
func (h *Host) GroupShared(ctx context.Context, gid string) bool {
	passwd, _, err := h.nss(ctx, "getent", "passwd")
	if err != nil {
		return false
	}
	users := 0
	for _, line := range strings.Split(passwd, "\n") {
		if f := strings.Split(line, ":"); len(f) > 3 && f[3] == gid {
			users++
		}
	}
	if users > 1 {
		return true
	}
	group, code, err := h.nss(ctx, "getent", "group", gid)
	f := strings.SplitN(strings.SplitN(group, "\n", 2)[0], ":", 4)
	if err != nil || code != 0 || len(f) < 4 {
		return false
	}
	me, code, err := h.nss(ctx, "id", "-un")
	if me = strings.TrimSpace(me); err != nil || code != 0 {
		me = ""
	}
	return slices.ContainsFunc(strings.Split(f[3], ","), func(m string) bool { return m != "" && m != me })
}

// nss runs one read of the host's users or groups within nssBound.
func (h *Host) nss(ctx context.Context, name string, args ...string) (string, int, error) {
	bounded, cancel := context.WithTimeout(ctx, nssBound)
	defer cancel()
	out, code, err := h.Capture(bounded, name, args...)
	return string(out), code, err
}

// SelfSigned makes a self-signed leaf for 825 days, as setup.sh asked
// openssl for one: a P-256 key in PKCS#8, CN localhost, the names
// localhost and this host's name, and each IPv4 address of ips but 0.0.0.0,
// once; not a CA, for servers only. KeyUsage is left unset, as openssl's.
func SelfSigned(ips []string) (TLSFiles, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"},
		BasicConstraintsValid: true, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if name, _ := os.Hostname(); name != "" {
		tmpl.DNSNames = append(tmpl.DNSNames, name)
	}
	var seen []netip.Addr
	for _, s := range ips {
		if a, err := netip.ParseAddr(s); err == nil && a.Is4() && !a.IsUnspecified() && !slices.Contains(seen, a) {
			seen = append(seen, a)
			tmpl.IPAddresses = append(tmpl.IPAddresses, net.IP(a.AsSlice()))
		}
	}
	tmpl.NotBefore = time.Now()
	tmpl.NotAfter = tmpl.NotBefore.Add(825 * 24 * time.Hour)
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key) // a nil serial is made at random
	if err != nil {
		return nil, err
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return TLSFiles{"cert.pem": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		"key.pem": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})}, nil
}

// OwnTLS reads the reader's certificate, its key and, when ca is not "",
// the private CA's certificate, each a regular file, and checks that the
// certificate and key match, before anything is written.
func (h *Host) OwnTLS(cert, key, ca string) (TLSFiles, error) {
	files := TLSFiles{}
	for _, f := range [][2]string{{"cert.pem", cert}, {"key.pem", key}, {"ca.pem", ca}} {
		if f[1] == "" && f[0] == "ca.pem" {
			continue
		}
		data, err := readRegular(f[1])
		if err != nil {
			return nil, fmt.Errorf("%s is not a file the console can read; nothing was changed", f[1])
		}
		files[f[0]] = data
	}
	if _, err := tls.X509KeyPair(files["cert.pem"], files["key.pem"]); err != nil {
		return nil, fmt.Errorf("the certificate and key do not match (%s); nothing was changed", err)
	}
	return files, nil
}

// readRegular reads path when it is a regular file, so that no device or
// pipe is read.
func readRegular(path string) ([]byte, error) {
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	return os.ReadFile(path)
}

// WriteTLS writes files into tls/, each through a temporary file given its
// owner and mode before it is moved over its name, so each name holds the
// old file or a new one the container reads, explicitly: as root the
// container's uid 65532 owns tls/, 0700 and 0600; otherwise TLSGroup's
// group, 0750 and 0640, so the container that joins it reads them.
func (h *Host) WriteTLS(ctx context.Context, files TLSFiles) error {
	dir := filepath.Join(h.Dir, "tls")
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	temps := map[string]string{} // by name, the temporary files not yet moved
	defer func() {
		for _, tmp := range temps {
			_ = os.Remove(tmp)
		}
	}()
	for name, data := range files {
		tmp, err := h.temporary(filepath.Join("tls", name), name+".", string(data))
		if err != nil {
			return err
		}
		temps[name] = tmp
	}
	owner, dirMode, fileMode := ":"+h.TLSGroup(), fs.FileMode(0o750), fs.FileMode(0o640)
	if h.UID == 0 {
		owner, dirMode, fileMode = "65532:65532", 0o700, 0o600
	}
	bounded, cancel := context.WithTimeout(ctx, h.bound)
	defer cancel()
	if _, code, err := h.Capture(bounded, "chown", "-R", owner, "tls"); err != nil || code != 0 {
		return fmt.Errorf("chown -R %s tls failed", owner)
	}
	if err := os.Chmod(dir, dirMode); err != nil {
		return err
	}
	for _, tmp := range temps {
		if err := os.Chmod(tmp, fileMode); err != nil {
			return err
		}
	}
	for name, tmp := range temps {
		if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("tls/%s was not written: %w", name, err)
		}
		delete(temps, name)
	}
	return nil
}

// CertExpiry is when tls/cert.pem's first certificate expires.
func (h *Host) CertExpiry() (time.Time, error) {
	data, err := os.ReadFile(filepath.Join(h.Dir, "tls", "cert.pem"))
	if err != nil {
		return time.Time{}, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return time.Time{}, errors.New("tls/cert.pem holds no certificate")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, err
	}
	return c.NotAfter, nil
}
