package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/transport"
)

// Env is what a check or a snapshot build needs from outside the declaration:
// the values behind credential references and the addresses behind host names.
type Env struct {
	Lookup   func(name string) (string, bool) // process environment first, then the env file
	Resolve  transport.Resolver               // injected in tests
	Findings []domain.Finding                 // warnings from this read of the env file
}

const (
	// maxEnvFileBytes is far above any file of credentials; it keeps a wrong
	// path from being loaded into memory on every change.
	maxEnvFileBytes = 1 << 20
	// maxEnvFileWarnings bounds what a wrong file adds to every response.
	maxEnvFileWarnings = 10
)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// NewEnv reads envFile, unless it is empty, and returns an Env that looks a
// name up in environ and then in the file (OPS-6). A variable set in the
// process environment wins even when it is empty. Each call reads the file
// again, so a credential added to it resolves on the next call.
//
// A file that cannot be read and a malformed line are warnings in Findings;
// neither is an error, and a warning never carries any content of the file.
func NewEnv(environ func(name string) (string, bool), envFile string, resolve transport.Resolver) Env {
	var file map[string]string
	var findings []domain.Finding
	if envFile != "" {
		file, findings = readEnvFile(envFile)
	}
	return Env{
		Lookup: func(name string) (string, bool) {
			if v, ok := environ(name); ok {
				return v, true
			}
			v, ok := file[name]
			return v, ok
		},
		Resolve:  resolve,
		Findings: findings,
	}
}

// readEnvFile parses NAME=value lines. There is no quoting and no
// interpolation: the value is the rest of the line, less the white space
// around it. Blank lines and lines that start with "#" are skipped; a "#"
// further on belongs to the value. The last line that sets a name wins.
func readEnvFile(path string) (map[string]string, []domain.Finding) {
	var findings []domain.Finding
	warn := func(code, format string, args ...any) {
		findings = append(findings, domain.Finding{
			Severity: domain.SeverityWarning, Rule: "OPS-6", Code: code,
			Resource: "env_file", Message: fmt.Sprintf(format, args...),
		})
	}

	// Only a regular file is opened: a pipe would block here and could be
	// read just once, while the file is read again on every change.
	var data []byte
	info, err := os.Stat(path)
	switch {
	case err != nil:
	case !info.Mode().IsRegular():
		err = errors.New("not a regular file")
	case info.Size() > maxEnvFileBytes:
		err = fmt.Errorf("larger than %d MiB", maxEnvFileBytes>>20)
	default:
		data, err = os.ReadFile(path)
	}
	if err != nil {
		// The path stays out of the message: the operator knows it, and a
		// finding travels in admin responses.
		if pathErr := (*fs.PathError)(nil); errors.As(err, &pathErr) {
			err = pathErr.Err
		}
		warn("env_file_unreadable", "the env file was not read: %v", err)
		return nil, findings
	}

	vars := map[string]string{}
	malformed := 0
	text := strings.TrimPrefix(string(data), "\xef\xbb\xbf") // a byte order mark some editors add
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line) // takes the "\r" of a CRLF line end too
		if line == "" || line[0] == '#' {
			continue
		}
		name, value, found := strings.Cut(line, "=")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if !found || !envName.MatchString(name) || !utf8.ValidString(value) || strings.ContainsFunc(value, unicode.IsControl) {
			if malformed++; malformed <= maxEnvFileWarnings {
				warn("env_file_malformed", "line %d of the env file is not NAME=value and was skipped", i+1)
			}
			continue
		}
		if len(value) > 1 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			warn("env_file_quoted", "line %d of the env file has its value in quotes, and the quotes are part of the value", i+1)
		}
		vars[name] = value
	}
	if malformed > maxEnvFileWarnings {
		warn("env_file_malformed", "%d more lines of the env file are not NAME=value and were skipped", malformed-maxEnvFileWarnings)
	}
	return vars, findings
}
