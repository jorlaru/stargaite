package presets_test

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"flag"
	"os"
	"slices"
	"testing"

	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/presets"
)

var update = flag.Bool("update", false, "rewrite the golden file")

const golden = "testdata/catalog.json"

// TestCatalogGolden fixes the data of every preset (AC-53).
func TestCatalogGolden(t *testing.T) {
	got, err := json.Marshal(presets.Catalog, json.Deterministic(true), jsontext.Multiline(true))
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if *update {
		if err := os.WriteFile(golden, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the catalog differs from %s (run the test with -update after a deliberate change):\n%s", golden, got)
	}
}

// TestCatalog: the eighteen presets of kick-off section 7.2 in its order, the
// last eight marked untested, none setting allow_private_network (AC-53), and
// each one a valid provider but for the flag the kick-off says it needs.
func TestCatalog(t *testing.T) {
	names := []string{
		"openai", "anthropic", "openrouter", "deepinfra", "minimax", "zai", "moonshot", "dashscope", "ollama", "tei",
		"deepseek", "gemini", "xai", "mistral", "groq", "together", "fireworks", "cerebras",
	}
	// What a provider created from the preset alone is refused for.
	needs := map[string][]string{
		"dashscope": {"SEC-4 required base_url"},
		"tei":       {"SEC-4 required base_url"},
		"ollama":    {"SEC-4 url_refused base_url"},
	}
	var got []string
	for i, p := range presets.Catalog {
		got = append(got, p.Name)
		if p.Untested != (i >= 10) {
			t.Errorf("%s: untested %v", p.Name, p.Untested)
		}
		if p.AllowPrivateNetwork {
			t.Errorf("%s sets allow_private_network", p.Name)
		}
		doc := &domain.Document{Version: 1, Providers: map[string]domain.Provider{p.Name: p.Provider}}
		var refused []string
		for _, f := range config.Validate(doc) {
			if f.Severity == domain.SeverityError {
				refused = append(refused, f.Rule+" "+f.Code+" "+f.Field)
			}
		}
		if !slices.Equal(refused, needs[p.Name]) {
			t.Errorf("%s: refused for %q, want %q", p.Name, refused, needs[p.Name])
		}
	}
	if !slices.Equal(got, names) {
		t.Errorf("presets %q, want %q", got, names)
	}
}
