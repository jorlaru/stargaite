package plan

import (
	"go/build"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestImports enforces the import rule of design section 3, and of phase-3
// design section 3 for usage, admission and telemetry: params, reasoning, the
// adapters, plan, routing and usage never reach store, transport, config or
// api; admission reaches no package of the module but domain; telemetry never
// reaches api, admin or cli; directly or through another package of the module.
func TestImports(t *testing.T) {
	forbidden := []string{"internal/store", "internal/transport", "internal/config", "internal/api"}
	for _, pkg := range []string{"internal/params", "internal/reasoning", "internal/plan", "internal/routing", "internal/adapters/openai", "internal/adapters/anthropic", "internal/usage"} {
		reach(t, pkg, func(rel string) bool { return slices.Contains(forbidden, rel) })
	}
	reach(t, "internal/admission", func(rel string) bool { return rel != "internal/domain" })
	reach(t, "internal/telemetry", func(rel string) bool {
		return slices.Contains([]string{"internal/api", "internal/admin", "internal/cli"}, rel)
	})
}

// reach reports every package of the module that pkg reaches, directly or
// through another, and that bad refuses.
func reach(t *testing.T, pkg string, bad func(rel string) bool) {
	t.Helper()
	const module = "github.com/jorlaru/stargaite/"
	root := filepath.Join("..", "..")
	if _, err := os.Stat(filepath.Join(root, pkg)); err != nil {
		return // a package of a later step
	}
	seen := map[string]bool{}
	var walk func(dir string, path []string)
	walk = func(dir string, path []string) {
		p, err := build.ImportDir(filepath.Join(root, dir), 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range p.Imports {
			rel, ok := strings.CutPrefix(imp, module)
			if !ok || seen[rel] {
				continue
			}
			seen[rel] = true
			chain := slices.Concat(path, []string{rel})
			if bad(rel) {
				t.Errorf("%s reaches %s", pkg, strings.Join(chain, " -> "))
			}
			walk(rel, chain)
		}
	}
	walk(pkg, []string{pkg})
}
