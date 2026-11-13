package plan

import (
	"go/build"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestImports enforces the import rule of design section 3: params,
// reasoning, the adapters, plan and routing never reach store, transport,
// config or api, directly or through another package of the module.
func TestImports(t *testing.T) {
	const module = "github.com/jorlaru/stargaite/"
	root := filepath.Join("..", "..")
	forbidden := []string{"internal/store", "internal/transport", "internal/config", "internal/api"}
	for _, pkg := range []string{"internal/params", "internal/reasoning", "internal/plan", "internal/routing", "internal/adapters/openai", "internal/adapters/anthropic"} {
		if _, err := os.Stat(filepath.Join(root, pkg)); err != nil {
			continue // a package of a later step
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
				for _, f := range forbidden {
					if rel == f {
						t.Errorf("%s reaches %s", pkg, strings.Join(chain, " -> "))
					}
				}
				walk(rel, chain)
			}
		}
		walk(pkg, []string{pkg})
	}
}
