// Package families groups a provider's model ids by vendor and family, from
// one table of name prefixes, so a reader picks a model from a sorted list
// of chat models (docs/design/phase-7.md section 3, Models). An id the table
// does not know goes to Other; nothing is ever an error.
package families

import (
	"cmp"
	"slices"
	"strings"
)

// other is the vendor and family of an id the table does not place.
const other = "Other"

// Model is one id with its vendor and family.
type Model struct{ ID, Vendor, Family string }

type vendor struct {
	name       string
	namespaces []string // the lower-case id namespaces that name it ("meta-llama")
	families   []family // in order, the more specific first
}

type family struct {
	name     string
	prefixes []string // lower-case prefixes of the model's name
}

// table is the vendors in their order. It returns fresh values: no package
// state.
func table() []vendor {
	return []vendor{
		{"OpenAI", []string{"openai"}, []family{
			{"gpt-oss", []string{"gpt-oss"}}, {"GPT", []string{"gpt-", "chatgpt-"}}, {"o-series", []string{"o1", "o3", "o4"}}}},
		{"Anthropic", []string{"anthropic"}, []family{
			{"Claude Opus", []string{"claude-opus", "claude-3-opus"}},
			{"Claude Sonnet", []string{"claude-sonnet", "claude-3-5-sonnet", "claude-3.5-sonnet", "claude-3-7-sonnet",
				"claude-3.7-sonnet", "claude-3-sonnet"}},
			{"Claude Haiku", []string{"claude-haiku", "claude-3-5-haiku", "claude-3.5-haiku", "claude-3-haiku"}},
			{"Claude", []string{"claude"}}}},
		{"Google", []string{"google"}, []family{{"Gemini", []string{"gemini"}}, {"Gemma", []string{"gemma"}}}},
		{"xAI", []string{"x-ai", "xai"}, []family{{"Grok", []string{"grok"}}}},
		{"Mistral", []string{"mistralai", "mistral"}, []family{
			{"Codestral", []string{"codestral"}},
			{"Mistral", []string{"mistral", "ministral", "magistral", "devstral", "pixtral", "mixtral", "open-mistral",
				"open-mixtral"}}}},
		{"DeepSeek", []string{"deepseek", "deepseek-ai"}, []family{{"DeepSeek", []string{"deepseek"}}}},
		{"Qwen", []string{"qwen"}, []family{{"Qwen", []string{"qwen", "qwq"}}}},
		{"Moonshot", []string{"moonshotai", "moonshot"}, []family{{"Kimi", []string{"kimi", "moonshot"}}}},
		{"MiniMax", []string{"minimax"}, []family{{"MiniMax", []string{"minimax"}}}},
		{"Z.ai", []string{"z-ai", "zai-org", "zai", "thudm"}, []family{{"GLM", []string{"glm", "zai-glm"}}}},
		{"Meta", []string{"meta-llama", "meta"}, []family{{"Llama", []string{"llama", "meta-llama"}}}},
	}
}

// nonChat is the substrings of a model's name that mark it as no chat model
// ("image" also marks "imagen").
func nonChat() []string {
	return []string{"embed", "rerank", "bge-", "whisper", "tts", "transcribe", "speech", "audio", "realtime", "dall-e",
		"image", "moderation"}
}

// Sort groups ids by vendor and family, chat models only unless all; hidden
// counts the non-chat ids left out. Vendors come in table order, then
// unknown namespaces alphabetically, then Other; families in table order,
// then Other; ids case-insensitively. Each id is kept as given.
func Sort(ids []string, all bool) (models []Model, hidden int) {
	vendors := table()
	for _, id := range ids {
		ns, name := split(id)
		if !all && slices.ContainsFunc(nonChat(), func(m string) bool { return strings.Contains(name, m) }) {
			hidden++
			continue
		}
		v, f := place(vendors, ns, name)
		models = append(models, Model{ID: id, Vendor: v, Family: f})
	}
	slices.SortStableFunc(models, func(a, b Model) int {
		return cmp.Or(
			cmp.Compare(vendorRank(vendors, a.Vendor), vendorRank(vendors, b.Vendor)),
			strings.Compare(a.Vendor, b.Vendor), // unknown namespaces
			cmp.Compare(familyRank(vendors, a), familyRank(vendors, b)),
			strings.Compare(strings.ToLower(a.ID), strings.ToLower(b.ID)))
	})
	return models, hidden
}

// split is an id's namespace, the segment before its last "/" ("" for none,
// or for the path words "models" and "accounts"; a leading "~" cut), and its name, the rest
// without a ":tag"; both lower-case, for matching only.
func split(id string) (ns, name string) {
	path, name := "", strings.ToLower(id)
	if i := strings.LastIndex(name, "/"); i >= 0 {
		path, name = name[:i], name[i+1:]
	}
	ns = strings.TrimPrefix(path[strings.LastIndex(path, "/")+1:], "~") // OpenRouter's "~vendor/...-latest"
	if ns == "models" || ns == "accounts" {
		ns = ""
	}
	name, _, _ = strings.Cut(name, ":")
	return ns, name
}

// place is the vendor and family of a namespace and name. A namespace a
// vendor lists names that vendor; an unknown one is itself the vendor; with
// none, the first family with a prefix of the name names both.
func place(vendors []vendor, ns, name string) (string, string) {
	if ns != "" {
		for _, v := range vendors {
			if slices.Contains(v.namespaces, ns) {
				return v.name, familyOf(v, name)
			}
		}
		return ns, other
	}
	for _, v := range vendors {
		if f := familyOf(v, name); f != other {
			return v.name, f
		}
	}
	return other, other
}

// familyOf is v's first family with a prefix of name, else Other.
func familyOf(v vendor, name string) string {
	for _, f := range v.families {
		if slices.ContainsFunc(f.prefixes, func(p string) bool { return strings.HasPrefix(name, p) }) {
			return f.name
		}
	}
	return other
}

// vendorRank is a vendor's place: the table's, then unknown namespaces, then
// Other.
func vendorRank(vendors []vendor, name string) int {
	switch i := slices.IndexFunc(vendors, func(v vendor) bool { return v.name == name }); {
	case i >= 0:
		return i
	case name == other:
		return len(vendors) + 1
	}
	return len(vendors)
}

// familyRank is m's family's place within its vendor: the table's, then
// Other.
func familyRank(vendors []vendor, m Model) int {
	i := slices.IndexFunc(vendors, func(v vendor) bool { return v.name == m.Vendor })
	if i < 0 {
		return 0
	}
	fs := vendors[i].families
	if j := slices.IndexFunc(fs, func(f family) bool { return f.name == m.Family }); j >= 0 {
		return j
	}
	return len(fs)
}
