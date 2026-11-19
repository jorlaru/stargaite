package rules

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// warnCodes collects the code of every warning the files raise: argument 0
// of a call to the identifier warn (env.go's closure), argument 1 of a call
// to a selector .warn (report.warn). A string literal is taken as is, an
// identifier through the files' string constants; any other form is an
// error naming its file and line.
func warnCodes(fset *token.FileSet, files []*ast.File) (map[string]bool, error) {
	consts := map[string]string{}
	for _, f := range files {
		for _, decl := range f.Decls {
			d, ok := decl.(*ast.GenDecl)
			if !ok || d.Tok != token.CONST {
				continue
			}
			for _, spec := range d.Specs {
				v := spec.(*ast.ValueSpec)
				for i, name := range v.Names {
					if i < len(v.Values) {
						if lit, ok := v.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							consts[name.Name], _ = strconv.Unquote(lit.Value)
						}
					}
				}
			}
		}
	}
	codes := map[string]bool{}
	var bad error
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || bad != nil {
				return bad == nil
			}
			arg := -1
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				if fn.Name == "warn" {
					arg = 0
				}
			case *ast.SelectorExpr:
				if fn.Sel.Name == "warn" {
					arg = 1
				}
			}
			if arg < 0 {
				return true
			}
			where := fset.Position(call.Pos())
			if arg >= len(call.Args) {
				bad = fmt.Errorf("%s:%d: warn without a code", where.Filename, where.Line)
				return false
			}
			switch a := call.Args[arg].(type) {
			case *ast.BasicLit:
				code, err := strconv.Unquote(a.Value)
				if a.Kind != token.STRING || err != nil {
					bad = fmt.Errorf("%s:%d: the code is not a string", where.Filename, where.Line)
					return false
				}
				codes[code] = true
			case *ast.Ident:
				code, ok := consts[a.Name]
				if !ok {
					bad = fmt.Errorf("%s:%d: the code %s is not a string constant", where.Filename, where.Line, a.Name)
					return false
				}
				codes[code] = true
			default:
				bad = fmt.Errorf("%s:%d: the code is neither a string nor a constant", where.Filename, where.Line)
				return false
			}
			return true
		})
	}
	return codes, bad
}

// rowless lists the codes without a row, sorted.
func rowless(codes map[string]bool) []string {
	var out []string
	for _, c := range slices.Sorted(maps.Keys(codes)) {
		if _, ok := Lookup(c); !ok {
			out = append(out, c)
		}
	}
	return out
}

// TestEveryWarnCodeHasARow: every warning internal/config can raise has a
// row, and the rows are exactly those codes.
func TestEveryWarnCodeHasARow(t *testing.T) {
	dir := filepath.Join("..", "..", "config")
	names, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	codes, err := warnCodes(fset, files)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"credential_unresolved", "url_unresolved", "target_unverified", "unpriced", "no_fallback_prices", "reasoning_undeclared",
		"effort_dropped", "endpoint_unservable", "unknown_alias", "unknown_provider", "quota_beyond_retention",
		"env_file_unreadable", "env_file_malformed", "env_file_quoted",
	}
	if got := slices.Sorted(maps.Keys(codes)); !slices.Equal(got, slices.Sorted(slices.Values(want))) {
		t.Errorf("internal/config warns with %q, want %q", got, want)
	}
	if missing := rowless(codes); len(missing) > 0 {
		t.Errorf("codes without a row: %q", missing)
	}
	var rows []string
	for _, w := range Warnings() {
		rows = append(rows, w.Code)
	}
	if !slices.Equal(rows, want) {
		t.Errorf("rows %q, want %q in that order", rows, want)
	}

	// Negative controls: a new code is caught, and so is a code the
	// collector cannot read.
	f, err := parser.ParseFile(fset, "new.go", `package config
func f(r *report) { r.warn("X-1", "brand_new", "res", "", "a new warning") }`, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	codes, err = warnCodes(fset, []*ast.File{f})
	if err != nil || !slices.Equal(rowless(codes), []string{"brand_new"}) {
		t.Errorf("a new code: rowless %q, err %v", rowless(codes), err)
	}
	f, err = parser.ParseFile(fset, "computed.go", `package config
func f(r *report, c string) { r.warn("X-1", c+"_x", "res", "", "a computed code") }`, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := warnCodes(fset, []*ast.File{f}); err == nil || !strings.HasPrefix(err.Error(), "computed.go:2:") {
		t.Errorf("a computed code: err %v, want one naming computed.go:2", err)
	}
}

// TestRowsWellFormed: one row per code, a known step to hold until, and
// words that name the resource once.
func TestRowsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, w := range Warnings() {
		if seen[w.Code] {
			t.Errorf("%s: two rows", w.Code)
		}
		seen[w.Code] = true
		if !slices.Contains([]string{"", "recreate", "check"}, w.Held) {
			t.Errorf("%s: held until %q", w.Code, w.Held)
		}
		if w.Words != "" && (strings.Count(w.Words, "%") != 1 || !strings.Contains(w.Words, "%s")) {
			t.Errorf("%s: words %q do not hold exactly one %%s", w.Code, w.Words)
		}
	}
	held := map[string]string{}
	for _, w := range Warnings() {
		held[w.Code] = w.Held
	}
	if held["credential_unresolved"] != "recreate" || held["target_unverified"] != "check" || held["url_unresolved"] != "" {
		t.Errorf("held = %v", held)
	}
	// A fresh slice each call: a caller's change does not reach the table.
	w := Warnings()
	w[0].Code = "changed"
	if Warnings()[0].Code != "credential_unresolved" {
		t.Error("Warnings returned a shared slice")
	}
}

// TestPlain: a row with words says them with the resource's name; any other
// code says the CLI's own message and where to find it again.
func TestPlain(t *testing.T) {
	want := "providers/acme: its host name did not resolve from the container; the proxy tries again on each call, so check the URL if this lasts"
	if got := Plain("url_unresolved", "providers/acme", "x"); got != want {
		t.Errorf("url_unresolved: %q, want %q", got, want)
	}
	want = "providers/acme: its key is not in the container yet: Add providers writes it to .env and recreates the container"
	if got := Plain("credential_unresolved", "providers/acme", "x"); got != want {
		t.Errorf("credential_unresolved: %q, want %q", got, want)
	}
	msg := "pay-per-use target with neither prices nor a provider cost report: its usage has no cost"
	for _, code := range []string{"unpriced", "never_heard_of"} {
		if got := Plain(code, "targets/a", msg); got != msg+" (stargaite validate lists it)" {
			t.Errorf("%s: %q", code, got)
		}
	}
}
