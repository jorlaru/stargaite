package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/domain"
)

const (
	defaultAdminURL = "http://127.0.0.1:4001"
	// requestTimeout is above the admin listener's write timeout, so the
	// server, not the client, ends a slow call.
	requestTimeout = 90 * time.Second
	// codeUnreachable is the CLI's own error code when the admin API cannot
	// be reached, so --json output stays one JSON object (design section 13,
	// point 10).
	codeUnreachable = "admin_unreachable"
)

// client does not follow redirects: the admin key goes to the admin URL only.
var client = &http.Client{
	Timeout:       requestTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// fill puts the value of a body flag at its path. "--flag=" sends null, which
// a merge patch reads as "clear" (design section 13, point 20). The row, not
// the text, fixes the type of a scalar.
func fill(body map[string]any, f flagSpec, v string) error {
	path := slices.Clip(strings.Split(f.path, "."))
	if v == "" {
		put(body, path, nil)
		return nil
	}
	switch f.kind {
	case kindString:
		put(body, path, v)
	case kindBool:
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("%q is not true or false", v)
		}
		put(body, path, b)
	case kindNumber:
		if !number(v) {
			return fmt.Errorf("%q is not a number", v)
		}
		put(body, path, jsontext.Value(v))
	case kindTypedPairs, kindStringPairs:
		for _, pair := range strings.Split(v, ",") {
			name, value, ok := strings.Cut(pair, "=")
			if !ok || name == "" {
				return fmt.Errorf("%q is not name=value", pair)
			}
			var typed any = value
			if raw := jsontext.Value(value); f.kind == kindTypedPairs && raw.IsValid() && strings.ContainsRune("0tf", rune(raw.Kind())) {
				typed = raw
			}
			put(body, append(path, name), typed)
		}
	case kindQuota:
		window, limit, _ := strings.Cut(v, ":")
		unit, n, ok := strings.Cut(limit, "=")
		if !ok || window == "" || unit == "" || !number(n) {
			return fmt.Errorf("%q is not <window>:<unit>=<number>", v)
		}
		list, _ := body[f.path].([]any) // null after "--quota=", which this entry replaces
		body[f.path] = append(list, map[string]any{"window": window, unit: jsontext.Value(n)})
	}
	return nil
}

func number(v string) bool {
	raw := jsontext.Value(v)
	return raw.IsValid() && raw.Kind() == '0'
}

// put sets the member at path, making the objects on the way.
func put(body map[string]any, path []string, v any) {
	for _, name := range path[:len(path)-1] {
		next, ok := body[name].(map[string]any)
		if !ok {
			next = map[string]any{}
			body[name] = next
		}
		body = next
	}
	body[path[len(path)-1]] = v
}

// call sends the one request of a command and prints the answer.
func call(ctx context.Context, name string, in input, method, path string, body any, stdout, stderr io.Writer) int {
	base := cmp.Or(*in.flags["admin-url"], in.getenv("STARGAITE_ADMIN_URL"), defaultAdminURL)
	if u, err := url.Parse(base); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		say(stderr, "%s: the admin URL %q is not an http or https URL\n", name, base)
		return exitUsage
	}

	var data []byte
	contentType := "application/json"
	switch b := body.(type) {
	case nil:
	case yamlFile:
		if b == "" {
			say(stderr, "%s: -f <file> is required\n", name)
			return exitUsage
		}
		var err error
		if data, err = readFile(string(b)); err != nil {
			return failed(name, in, domain.NewError(domain.CodeInvalidRequest, "the file was not read: "+err.Error()), stdout, stderr)
		}
		contentType = "application/yaml"
	default:
		var err error
		if data, err = config.Encode(b); err != nil {
			return failed(name, in, domain.NewError(domain.CodeInternalError, err.Error()), stdout, stderr)
		}
	}

	key := in.getenv("STARGAITE_ADMIN_KEY")
	if key == "" {
		dir := cmp.Or(*in.flags["data-dir"], in.getenv("STARGAITE_DATA_DIR"), "./data")
		file, err := readFile(filepath.Join(dir, "admin.key"))
		if err != nil {
			return failed(name, in, unreachable("no admin key: STARGAITE_ADMIN_KEY is unset and "+err.Error()), stdout, stderr)
		}
		key = strings.TrimSpace(string(file))
	}

	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(base, "/")+"/admin/v1"+path, bytes.NewReader(data))
	if err != nil {
		return failed(name, in, unreachable(err.Error()), stdout, stderr)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if data != nil {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := client.Do(req)
	if err != nil {
		return failed(name, in, unreachable(err.Error()), stdout, stderr)
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		return failed(name, in, unreachable(err.Error()), stdout, stderr)
	}
	return show(name, in, resp, answer, stdout, stderr)
}

func unreachable(message string) *domain.Error {
	return &domain.Error{Message: "the admin API was not reached: " + message, Type: "api_error", Code: codeUnreachable}
}

// failed reports an error found on this side of the call: in the envelope
// with --json, so the output stays JSON, and as a message otherwise.
func failed(name string, in input, e *domain.Error, stdout, stderr io.Writer) int {
	if in.json {
		data, _ := json.Marshal(struct {
			Error *domain.Error `json:"error"`
		}{e})
		_, _ = stdout.Write(append(data, '\n'))
	} else {
		say(stderr, "%s: %s\n", name, printable(e.Message))
	}
	return exitFailure
}

// show prints an answer: the body unchanged with --json, otherwise rendered
// for a person, an error on stderr. A 2xx answer is exit 0, unless it holds
// error findings, as validate does on an invalid configuration.
func show(name string, in input, resp *http.Response, answer []byte, stdout, stderr io.Writer) int {
	var findings struct {
		Findings []domain.Finding `json:"findings"`
	}
	_ = json.Unmarshal(answer, &findings) // only the findings of a success matter here
	ok := resp.StatusCode/100 == 2
	code := exitOK
	if !ok || slices.ContainsFunc(findings.Findings, func(f domain.Finding) bool { return f.Severity == domain.SeverityError }) {
		code = exitFailure
	}

	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch {
	case in.json:
		_, _ = stdout.Write(answer)
	case !ok:
		var e struct {
			Error domain.Error `json:"error"`
		}
		if json.Unmarshal(answer, &e) != nil || e.Error.Message == "" {
			say(stderr, "%s: %s: %s\n", name, resp.Status, printable(string(answer)))
			break
		}
		say(stderr, "%s: %s\n", name, printable(e.Error.Message))
		for _, f := range e.Error.Findings {
			say(stderr, "%s\n", findingLine(f))
		}
	case mediaType != "application/json" || !jsontext.Value(answer).IsValid():
		_, _ = stdout.Write(answer) // the YAML of export
	default:
		if err := render(stdout, "", answer); err != nil {
			say(stderr, "%s: %v\n", name, err)
			return exitFailure
		}
	}
	return code
}

// render prints a JSON value for a person: an object as "name: value" lines
// in name order, a nested member under its dotted name, a list of objects as
// a table and any other list on one line. Text is quoted when it holds a
// control character, so the server's answer sends nothing to the terminal.
func render(w io.Writer, name string, v jsontext.Value) error {
	switch v.Kind() {
	case '{':
		var m map[string]jsontext.Value
		if err := json.Unmarshal(v, &m); err != nil {
			return err
		}
		for _, k := range slices.Sorted(maps.Keys(m)) {
			if err := render(w, strings.TrimPrefix(name+"."+k, "."), m[k]); err != nil {
				return err
			}
		}
		return nil
	case '[':
		var list []jsontext.Value
		if err := json.Unmarshal(v, &list); err != nil {
			return err
		}
		if len(list) > 0 && !slices.ContainsFunc(list, func(e jsontext.Value) bool { return e.Kind() != '{' }) {
			return table(w, name, list)
		}
		cells := []string{"-"}
		if len(list) > 0 {
			cells = cells[:0]
		}
		for _, e := range list {
			cells = append(cells, cell(e))
		}
		say(w, "%s: %s\n", printable(name), strings.Join(cells, ", "))
		return nil
	}
	say(w, "%s: %s\n", printable(name), cell(v))
	return nil
}

// table prints a list of objects, one column per member name.
func table(w io.Writer, name string, list []jsontext.Value) error {
	rows := make([]map[string]jsontext.Value, len(list))
	columns := map[string]bool{}
	for i, e := range list {
		if err := json.Unmarshal(e, &rows[i]); err != nil {
			return err
		}
		for k := range rows[i] {
			columns[k] = true
		}
	}
	names := slices.Sorted(maps.Keys(columns))
	say(w, "%s:\n", printable(name))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	header := make([]string, len(names))
	for i, k := range names {
		header[i] = printable(k)
	}
	say(tw, "  %s\n", strings.Join(header, "\t"))
	for _, row := range rows {
		cells := make([]string, len(names))
		for i, k := range names {
			cells[i] = "-"
			if v, ok := row[k]; ok {
				cells[i] = cell(v)
			}
		}
		say(tw, "  %s\n", strings.Join(cells, "\t"))
	}
	return tw.Flush()
}

// cell is a value as text: a string without its quotes, anything else as JSON.
func cell(v jsontext.Value) string {
	var s string
	if v.Kind() != '"' || json.Unmarshal(v, &s) != nil {
		s = string(v)
	}
	return printable(s)
}
