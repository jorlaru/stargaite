package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The console's layers (docs/design/phase-7.md section 5). Each rule is a
// function over go list's output or the parsed sources, run on the real
// tree and on a hand-made input that breaks it.

const module = "github.com/jorlaru/stargaite/"

// root is the module's directory: go test runs in the package's.
func root(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// goList runs go list with args at the module root, one line per entry.
func goList(t *testing.T, args ...string) []string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list"}, args...)...)
	cmd.Dir = root(t)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %q: %v", args, err)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

// expect runs check on the real input, which must pass, and on a broken
// one, which must fail.
func expect(t *testing.T, check func([]string) error, real, broken []string) {
	t.Helper()
	if err := check(real); err != nil {
		t.Error(err)
	}
	if err := check(broken); err == nil {
		t.Errorf("the check passed on the broken input %q", broken)
	}
}

// noCharm: below the fronts nothing reaches a terminal library, Bubble Tea's
// or the terminal modules it is built on.
func noCharm(deps []string) error {
	for _, d := range deps {
		if strings.HasPrefix(d, "charm.land/") || strings.HasPrefix(d, "github.com/charmbracelet/") {
			return fmt.Errorf("%s is a dependency below the fronts", d)
		}
	}
	return nil
}

func TestNoCharmBelowFronts(t *testing.T) {
	deps := goList(t, "-deps", "./internal/console/ui", "./internal/console/ui/plain", "./internal/console/task",
		"./internal/console/proxy", "./internal/console/host", "./internal/console/paid", "./internal/console/rules")
	expect(t, noCharm, deps, append(deps, "charm.land/bubbletea/v2"))
	expect(t, noCharm, deps, append(deps, "github.com/charmbracelet/colorprofile"))
}

// pieceImports: a piece knows no task, proxy or host.
func pieceImports(imports []string) error {
	for _, i := range imports {
		for _, p := range []string{"task", "proxy", "host"} {
			if i == module+"internal/console/"+p {
				return fmt.Errorf("piece imports %s", i)
			}
		}
	}
	return nil
}

func TestPieceImports(t *testing.T) {
	imports := goList(t, "-f", `{{join .Imports "\n"}}`, "./internal/console/piece")
	expect(t, pieceImports, imports, append(imports, module+"internal/console/host"))
}

// importLines is "<import path> <import> <import>…" per package.
func importLines(t *testing.T, patterns ...string) []string {
	t.Helper()
	return goList(t, append([]string{"-f", `{{.ImportPath}} {{join .Imports " "}}`}, patterns...)...)
}

// onlyHost is the check that host is the one package importing path: the
// one that runs a command (os/exec) or speaks HTTP (net/http).
func onlyHost(path string) func(lines []string) error {
	return func(lines []string) error {
		for _, l := range lines {
			f := strings.Fields(l)
			for _, i := range f[1:] {
				if i == path && f[0] != module+"internal/console/host" {
					return fmt.Errorf("%s imports %s", f[0], path)
				}
			}
		}
		return nil
	}
}

func TestOnlyHostExecs(t *testing.T) {
	lines := importLines(t, "./internal/console/...", "./cmd/stargaite-console")
	expect(t, onlyHost("os/exec"), lines, append(lines, module+"internal/console/task os/exec"))
}

func TestOnlyHostHTTP(t *testing.T) {
	lines := importLines(t, "./internal/console/...", "./cmd/stargaite-console")
	expect(t, onlyHost("net/http"), lines, append(lines, module+"internal/console/task net/http"))
}

// noProxyPackages: the console reaches the proxy only through its CLI.
func noProxyPackages(lines []string) error {
	for _, l := range lines {
		f := strings.Fields(l)
		for _, i := range f[1:] {
			if strings.HasPrefix(i, module+"internal/") && !strings.HasPrefix(i, module+"internal/console/") {
				return fmt.Errorf("%s imports %s", f[0], i)
			}
		}
	}
	return nil
}

func TestNoProxyPackages(t *testing.T) {
	lines := importLines(t, "./internal/console/...", "./cmd/stargaite-console")
	expect(t, noProxyPackages, lines, append(lines, module+"cmd/stargaite-console "+module+"internal/config"))
}

// admintestOnlyInTests: the admin fixture is test code; the listing must be
// the whole module.
func admintestOnlyInTests(lines []string) error {
	cli := false
	for _, l := range lines {
		f := strings.Fields(l)
		cli = cli || f[0] == module+"internal/cli"
		for _, i := range f[1:] {
			if i == module+"internal/admintest" {
				return fmt.Errorf("%s imports admintest outside its tests", f[0])
			}
		}
	}
	if !cli {
		return fmt.Errorf("the listing has no %sinternal/cli: it is not the module", module)
	}
	return nil
}

func TestAdmintestOnlyInTests(t *testing.T) {
	lines := importLines(t, "./...")
	expect(t, admintestOnlyInTests, lines, append(lines, module+"internal/console/task "+module+"internal/admintest"))
	if admintestOnlyInTests([]string{module + "internal/console/ui context errors"}) == nil {
		t.Error("the check passed on a one-package listing")
	}
}

// parsed is the non-test sources of the console, by path from the root.
func parsed(t *testing.T) map[string]*ast.File {
	t.Helper()
	files := map[string]*ast.File{}
	fset := token.NewFileSet()
	for _, dir := range []string{"internal/console", "cmd/stargaite-console"} {
		err := filepath.WalkDir(filepath.Join(root(t), dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root(t), path)
			files[filepath.ToSlash(rel)] = f
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(files) < 10 {
		t.Fatalf("only %d files parsed", len(files))
	}
	return files
}

// source parses one hand-made file.
func source(t *testing.T, path, src string) map[string]*ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]*ast.File{path: f}
}

// oneGoStatement: the only go statement is in ui.Go, which recovers.
func oneGoStatement(files map[string]*ast.File) error {
	for path, f := range files {
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && path == "internal/console/ui/goroutine.go" && fn.Name.Name == "Go" && fn.Recv == nil {
				continue
			}
			var found error
			ast.Inspect(decl, func(n ast.Node) bool {
				if _, ok := n.(*ast.GoStmt); ok && found == nil {
					found = fmt.Errorf("%s has a go statement outside ui.Go", path)
				}
				return found == nil
			})
			if found != nil {
				return found
			}
		}
	}
	return nil
}

func TestOneGoStatement(t *testing.T) {
	if err := oneGoStatement(parsed(t)); err != nil {
		t.Error(err)
	}
	broken := source(t, "internal/console/task/x.go", "package task\nfunc f() { go func() {}() }\n")
	if oneGoStatement(broken) == nil {
		t.Error("a second go statement passed")
	}
	inGo := source(t, "internal/console/ui/goroutine.go", "package ui\nfunc Go(fn func()) { go fn() }\n")
	if err := oneGoStatement(inGo); err != nil {
		t.Errorf("ui.Go's own go statement: %v", err)
	}
}

// noPackageState: no init, and no package variable but a sentinel error,
// and main's link-time version.
func noPackageState(files map[string]*ast.File) error {
	for path, f := range files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Name.Name == "init" && d.Recv == nil {
					return fmt.Errorf("%s has func init", path)
				}
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				for _, spec := range d.Specs {
					v := spec.(*ast.ValueSpec)
					if path == "cmd/stargaite-console/main.go" && len(v.Names) == 1 && v.Names[0].Name == "version" {
						continue
					}
					if len(v.Values) != len(v.Names) {
						return fmt.Errorf("%s: package variable %s", path, v.Names[0].Name)
					}
					for _, val := range v.Values {
						if !isErrorsNew(val) {
							return fmt.Errorf("%s: package variable %s is not a sentinel error", path, v.Names[0].Name)
						}
					}
				}
			}
		}
	}
	return nil
}

// isErrorsNew reports whether e is a call errors.New(…).
func isErrorsNew(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "errors" && sel.Sel.Name == "New"
}

func TestNoPackageState(t *testing.T) {
	if err := noPackageState(parsed(t)); err != nil {
		t.Error(err)
	}
	for _, src := range []string{
		"package task\nfunc init() {}\n",
		"package task\nvar n int\n",
		"package task\nvar m = map[string]int{}\n",
		"package task\nvar version = \"dev\"\n",
	} {
		if noPackageState(source(t, "internal/console/task/x.go", src)) == nil {
			t.Errorf("passed: %q", src)
		}
	}
	ok := source(t, "internal/console/task/x.go", "package task\nimport \"errors\"\nvar ErrX = errors.New(\"x\")\n")
	if err := noPackageState(ok); err != nil {
		t.Errorf("a sentinel error: %v", err)
	}
}

// paidSessionPlace: only ui and main name paid.Session or paid.NewSession,
// so only they can open a paid step, whatever name paid is imported as; and
// only main names the host's CLI, so every other package runs the CLI
// through proxy, whose one door refuses the paid command.
func paidSessionPlace(files map[string]*ast.File) error {
	for path, f := range files {
		dir := filepath.Dir(path)
		names := map[string]bool{"paid": true}
		for _, spec := range f.Imports {
			if strings.HasSuffix(strings.Trim(spec.Path.Value, `"`), "/internal/console/paid") && spec.Name != nil {
				if spec.Name.Name == "." {
					return fmt.Errorf("%s dot-imports paid", path)
				}
				names[spec.Name.Name] = true
			}
		}
		var found error
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			switch {
			case dir == "cmd/stargaite-console":
			case sel.Sel.Name == "CLI":
				found = fmt.Errorf("%s names .CLI: only main hands the host's CLI to proxy", path)
			case dir == "internal/console/ui":
			case ok && names[pkg.Name] && (sel.Sel.Name == "Session" || sel.Sel.Name == "NewSession"):
				found = fmt.Errorf("%s names paid.%s", path, sel.Sel.Name)
			}
			return found == nil
		})
		if found != nil {
			return found
		}
	}
	return nil
}

func TestPaidSessionPlace(t *testing.T) {
	if err := paidSessionPlace(parsed(t)); err != nil {
		t.Error(err)
	}
	for _, src := range []string{
		"package task\nfunc f() { _ = paid.NewSession() }\n",
		"package task\nfunc f(s *paid.Session) {}\n",
		"package task\nimport budget \"github.com/jorlaru/stargaite/internal/console/paid\"\nfunc f() { s := budget.NewSession(); s.Open(12) }\n",
		"package task\nimport . \"github.com/jorlaru/stargaite/internal/console/paid\"\nfunc f() {}\n",
		"package task\nfunc f(h *host.Host) { h.CLI(nil, []string{\"targets\", \"verify\", \"a/m\"}, nil, nil, nil) }\n",
	} {
		if paidSessionPlace(source(t, "internal/console/task/x.go", src)) == nil {
			t.Errorf("passed: %q", src)
		}
	}
	for _, path := range []string{"internal/console/ui/x.go", "cmd/stargaite-console/x.go"} {
		if err := paidSessionPlace(source(t, path, "package x\nfunc f() { _ = paid.NewSession() }\n")); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
	if err := paidSessionPlace(source(t, "cmd/stargaite-console/x.go", "package x\nfunc f() { _ = proxy.New(h.CLI) }\n")); err != nil {
		t.Errorf("main naming h.CLI: %v", err)
	}
	if paidSessionPlace(source(t, "internal/console/ui/x.go", "package x\nfunc f() { _ = proxy.New(h.CLI) }\n")) == nil {
		t.Error("ui naming h.CLI passed")
	}
}
