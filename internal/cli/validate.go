package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/domain"
)

// resolve is the system resolver. Tests replace it, so that no test depends
// on the network.
var resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// lookupTimeout bounds all the name lookups of one run.
const lookupTimeout = 30 * time.Second

// validateFile is "validate -f": the checks of a change, run in process on a
// file, with the verdict apply would give on an empty instance (OPS-5).
func validateFile(ctx context.Context, in input, stdout io.Writer) error {
	path := *in.flags["f"]
	if path == "" {
		return fmt.Errorf("%w: -f <file> is required; validate without -f asks a server, which this version cannot do yet", errUsage)
	}
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	findings := checkFile(ctx, path, *in.flags["env-file"], in.getenv)

	errs := 0
	for _, f := range findings {
		if f.Severity == domain.SeverityError {
			errs++
		}
	}
	if in.json {
		if err := json.NewEncoder(stdout).Encode(struct {
			Findings []domain.Finding `json:"findings"`
		}{append([]domain.Finding{}, findings...)}); err != nil { // [] rather than null when clean
			return err
		}
	} else {
		if len(findings) == 0 {
			say(stdout, "no findings\n")
		}
		for _, f := range findings {
			field := ""
			if f.Field != "" {
				field = " " + printable(f.Field)
			}
			if _, err := fmt.Fprintf(stdout, "%s %s %s%s: %s\n", f.Severity, f.Rule, printable(f.Resource), field, printable(f.Message)); err != nil {
				return err
			}
		}
	}
	if errs > 0 {
		return fmt.Errorf("%d of %d findings are errors", errs, len(findings))
	}
	return nil
}

// printable quotes s when it holds a line break, an escape or another
// non-graphic rune, which a file can put in a name: a finding stays one line
// and sends nothing to the terminal.
func printable(s string) string {
	if strings.ContainsFunc(s, func(r rune) bool { return !unicode.IsGraphic(r) }) {
		return strconv.Quote(s)
	}
	return s
}

// checkFile returns the findings on the file at path; a file that cannot be
// read or decoded is one error finding, so --json output stays one object.
func checkFile(ctx context.Context, path, envFile string, getenv func(string) string) []domain.Finding {
	fail := func(code string, err error) []domain.Finding {
		return []domain.Finding{{Severity: domain.SeverityError, Rule: "OPS-5", Code: code, Resource: "file", Message: err.Error()}}
	}
	data, err := readFile(path)
	if err != nil {
		return fail("file_unreadable", err)
	}
	var doc domain.Document
	if err := config.DecodeYAML(data, &doc); err != nil {
		return fail("invalid_file", err)
	}
	// getenv cannot tell an empty variable from an unset one; an empty one
	// does not resolve anyway, so it falls through to the env file.
	environ := func(name string) (string, bool) {
		v := getenv(name)
		return v, v != ""
	}
	env := config.NewEnv(environ, envFile, resolve)
	return append(config.Validate(&doc), config.CheckResolved(ctx, &doc, config.OutboundURLs(&doc), env)...)
}

// readFile reads a regular file only, so that standard input, a pipe or a
// device is never read, and no more than one byte past what DecodeYAML takes.
func readFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, config.MaxYAMLBytes+1))
}
