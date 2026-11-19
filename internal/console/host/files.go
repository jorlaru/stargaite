package host

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/jorlaru/stargaite/internal/console/ui"
)

// The files the console shares with setup.sh: .env and compose.override.yaml.
// Each is written as bash writes it, byte for byte, through a temporary file
// beside it moved over it, so no half-written file is ever read
// (tests/setup/format_test.go runs both).

// OverrideMarker is the first line of an override setup.sh or the console
// wrote; an override without it is the reader's own and is never rewritten.
const OverrideMarker = `# Written by setup.sh from its "# setup:" lines below; it rewrites this file whole.`

// EnvGet is the value of name in .env, without one surrounding pair of quotes,
// from its first NAME= line, as setup.sh's env_get reads it. A missing .env
// or line is ok == false.
func (h *Host) EnvGet(name string) (value string, ok bool, err error) {
	data, err := os.ReadFile(filepath.Join(h.Dir, ".env"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf(".env: %w", err)
	}
	for _, line := range envLines(data) {
		if v, ok := strings.CutPrefix(line, name+"="); ok {
			if len(v) >= 2 && v[0] == v[len(v)-1] && (v[0] == '\'' || v[0] == '"') {
				v = v[1 : len(v)-1]
			}
			return v, true, nil
		}
	}
	return "", false, nil
}

// EnvSet writes NAME='VALUE' to .env, replacing every NAME= line or appended,
// as setup.sh's env_set does. The value must be one .env can hold; no error
// holds it.
func (h *Host) EnvSet(name, value string) error {
	if err := varName(name); err != nil {
		return err
	}
	if err := ui.Storable(name, value); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(h.Dir, ".env"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf(".env: %w", err)
	}
	var b strings.Builder
	found := false
	for _, line := range envLines(data) {
		if strings.HasPrefix(line, name+"=") {
			line, found = name+"='"+value+"'", true
		}
		b.WriteString(line + "\n")
	}
	if !found {
		b.WriteString(name + "='" + value + "'\n")
	}
	return h.replace(".env", ".env.", b.String())
}

// EnvUnset removes every NAME= line of .env, keeping every other byte. A
// missing .env, or one without the line, is removed false, and nothing is
// written.
func (h *Host) EnvUnset(name string) (removed bool, err error) {
	if err := varName(name); err != nil {
		return false, err
	}
	data, err := os.ReadFile(filepath.Join(h.Dir, ".env"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf(".env: %w", err)
	}
	var b strings.Builder
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if strings.HasPrefix(line, name+"=") {
			removed = true
			continue
		}
		b.WriteString(line)
	}
	if !removed {
		return false, nil
	}
	if err := h.replace(".env", ".env.", b.String()); err != nil {
		return false, err
	}
	return true, nil
}

// varName refuses what cannot be a variable's name in .env.
func varName(name string) error {
	if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(name) {
		return fmt.Errorf("%q is not a variable name", name)
	}
	return nil
}

// Override is compose.override.yaml as setup.sh's override_set reads it: its
// marker, its text, and its "# setup: KEY=VALUE" lines in file order.
type Override struct {
	exists, marked bool
	raw            string
	keys           []string // as found, a repeated key included, as bash keeps it
	values         map[string]string
}

// ReadOverride reads compose.override.yaml. Its "# setup:" lines are read
// whatever its marker says, as setup.sh's tls_state reads them; a missing
// file is an Override that does not exist.
func (h *Host) ReadOverride() (*Override, error) {
	o := &Override{values: map[string]string{}}
	data, err := os.ReadFile(filepath.Join(h.Dir, "compose.override.yaml"))
	if errors.Is(err, fs.ErrNotExist) {
		return o, nil
	}
	if err != nil {
		return nil, fmt.Errorf("compose.override.yaml: %w", err)
	}
	o.exists, o.raw = true, string(data)
	lines, _ := splitLines(o.raw) // bash's loop here reads no line after the last newline
	o.marked = len(lines) > 0 && lines[0] == OverrideMarker
	setup := regexp.MustCompile(`^# setup: ([a-z_]+)=(.*)$`)
	for _, line := range lines {
		if m := setup.FindStringSubmatch(line); m != nil {
			o.keys = append(o.keys, m[1])
			o.values[m[1]] = m[2]
		}
	}
	return o, nil
}

// Exists reports whether the file exists.
func (o *Override) Exists() bool { return o.exists }

// Marked reports whether setup.sh or the console wrote it.
func (o *Override) Marked() bool { return o.marked }

// Holds reports whether its text holds s.
func (o *Override) Holds(s string) bool { return strings.Contains(o.raw, s) }

// Get is the value of a "# setup:" key, or "".
func (o *Override) Get(key string) string { return o.values[key] }

// Set sets a key; "" drops it from the file written. A new key is written
// after the others.
func (o *Override) Set(key, value string) {
	if _, ok := o.values[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.values[key] = value
}

// WriteOverride writes the override whole from its keys, as setup.sh's
// override_set renders it. It refuses an override it did not write.
func (h *Host) WriteOverride(o *Override) error {
	if o.exists && !o.marked {
		return errors.New("compose.override.yaml was not written by setup.sh, so it is left as it is: move it away and run this again")
	}
	var b strings.Builder
	b.WriteString(OverrideMarker + "\n")
	for _, k := range o.keys {
		v := o.values[k]
		if strings.Contains(v, "\n") {
			return fmt.Errorf("compose.override.yaml: %s holds a newline", k)
		}
		if v != "" {
			b.WriteString("# setup: " + k + "=" + v + "\n")
		}
	}
	b.WriteString("services:\n  stargaite:\n")
	var hosts []string
	if o.values["host_gateway"] != "" {
		hosts = append(hosts, "host.docker.internal:host-gateway")
	}
	if o.values["tls"] != "" {
		hosts = append(hosts, o.values["tls_name"]+":127.0.0.1")
	}
	if len(hosts) > 0 {
		b.WriteString("    extra_hosts:\n")
		for _, x := range hosts {
			b.WriteString(`      - "` + x + "\"\n")
		}
	}
	if o.values["tls"] != "" {
		b.WriteString(`    command: ["serve", "--listen", "0.0.0.0:4000", "--tls-cert", "/tls/cert.pem", "--tls-key", "/tls/key.pem"]` +
			"\n    ports: !override\n")
		addrs := strings.Fields(o.values["addresses"])
		if len(addrs) == 0 {
			addrs = []string{""} // bash's printf writes its format once with no argument
		}
		for _, a := range addrs {
			b.WriteString(`      - "` + a + ":${STARGAITE_PORT:-4000}:4000\"\n")
		}
		b.WriteString("    volumes:\n      - ./tls:/tls:ro\n")
		if g := o.values["tls_group"]; g != "" {
			b.WriteString("    group_add:\n      - \"" + g + "\"\n")
		}
	}
	return h.replace("compose.override.yaml", "compose.override.", b.String())
}

// Plane is how the host reaches the proxy's data plane: on loopback at Port,
// under TLS by the certificate's Name, trusting tls/<CA> when CA is set.
type Plane struct {
	BaseURL, Name, CA string
	Port              int
}

// Plane reads the data plane from STARGAITE_PORT in .env and the override's
// TLS lines, as setup.sh's tls_state does.
func (h *Host) Plane() (Plane, error) {
	p := Plane{Port: 4000}
	v, ok, err := h.EnvGet("STARGAITE_PORT")
	if err != nil {
		return Plane{}, err
	}
	if ok {
		n, err := strconv.Atoi(v)
		if err != nil || !regexp.MustCompile(`^[1-9][0-9]{0,4}$`).MatchString(v) || n > 65535 {
			return Plane{}, errors.New("STARGAITE_PORT in .env is not a port number, 1 to 65535: write it as STARGAITE_PORT=<n>")
		}
		p.Port = n
	}
	if p.Name, p.CA, err = h.tlsNames(); err != nil {
		return Plane{}, err
	}
	p.BaseURL = "http://127.0.0.1:" + strconv.Itoa(p.Port) + "/v1"
	if p.Name != "" {
		p.BaseURL = "https://" + p.Name + ":" + strconv.Itoa(p.Port) + "/v1"
	}
	return p, nil
}

// tlsNames is the override's tls_name and tls_ca, checked, or "" for both
// when it names no tls_name.
func (h *Host) tlsNames() (name, ca string, err error) {
	o, err := h.ReadOverride()
	if err != nil {
		return "", "", err
	}
	name, ca = o.Get("tls_name"), o.Get("tls_ca")
	switch {
	case name == "":
		return "", "", nil
	case !regexp.MustCompile(`^[A-Za-z0-9.-]+$`).MatchString(name):
		return "", "", errors.New("compose.override.yaml: tls_name is not a host name")
	case ca != "" && !regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`).MatchString(ca):
		return "", "", errors.New("compose.override.yaml: tls_ca is not a file name in tls/")
	}
	return name, ca, nil
}

// replace writes content to file in the clone through a temporary file of
// prefix beside it (mode 0600), moved over it; the temporary file is removed
// on every failure.
func (h *Host) replace(file, prefix, content string) error {
	tmp, err := os.CreateTemp(h.Dir, prefix)
	if err != nil {
		return fmt.Errorf("no temporary file beside %s: %w", file, err)
	}
	_, err = tmp.WriteString(content)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), filepath.Join(h.Dir, file))
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("%s was not written: %w", file, err)
	}
	return nil
}

// envLines splits .env as bash's `while IFS= read -r line || [[ -n $line ]]`
// does: a "\r" stays in its line, and a last line without "\n" counts.
func envLines(data []byte) []string {
	lines, rest := splitLines(string(data))
	if rest != "" {
		lines = append(lines, rest)
	}
	return lines
}

// splitLines is the lines of s that end in "\n", and what follows the last.
func splitLines(s string) (lines []string, rest string) {
	parts := strings.Split(s, "\n")
	return parts[:len(parts)-1], parts[len(parts)-1]
}
