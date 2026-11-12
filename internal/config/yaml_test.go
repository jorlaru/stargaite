package config

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/jorlaru/stargaite/internal/domain"
)

func TestYAMLScalars(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string // the JSON it becomes
	}{
		// Text that YAML would convert and a declaration must keep.
		{"a date stays text", "v: 2026-11-01", `{"v":"2026-11-01"}`},
		{"a timestamp stays text", "v: 2026-11-01T10:00:00Z", `{"v":"2026-11-01T10:00:00Z"}`},
		{"a clock time stays text", "v: 12:30:00", `{"v":"12:30:00"}`},
		{"yes, no, on, off", "v: [yes, no, on, off, y, n, Yes, NO]", `{"v":["yes","no","on","off","y","n","Yes","NO"]}`},
		{"capitalised booleans", "v: [True, TRUE, False, FALSE]", `{"v":["True","TRUE","False","FALSE"]}`},
		{"hexadecimal, octal and binary", "v: [0x10, 0o17, 017, 0b11]", `{"v":["0x10","0o17","017","0b11"]}`},
		{"digit separators", "v: 1_000", `{"v":"1_000"}`},
		{"infinity and not-a-number", "v: [.inf, -.inf, .Inf, .nan, .NaN]", `{"v":[".inf","-.inf",".Inf",".nan",".NaN"]}`},
		{"tilde and capitalised null", "v: [~, Null, NULL]", `{"v":["~","Null","NULL"]}`},
		{"a plus sign, a bare point", "v: [+1, .5, 1., +1.5]", `{"v":["+1",".5","1.","+1.5"]}`},
		{"a float beyond the range", "v: 1e999", `{"v":"1e999"}`},

		// What is JSON already keeps its type, digit for digit.
		{"booleans", "v: [true, false]", `{"v":[true,false]}`},
		{"null", "v: null", `{"v":null}`},
		{"an empty value is null", "v:\nw: 1", `{"v":null,"w":1}`},
		{"an empty list item is null", "v:\n  -\n  - a", `{"v":[null,"a"]}`},
		{"integers", "v: [0, 12, -3, -0]", `{"v":[0,12,-3,-0]}`},
		{"an integer beyond 64 bits", "v: [12345678901234567890, 123456789012345678901234567890]", `{"v":[12345678901234567890,123456789012345678901234567890]}`},
		{"floats keep their digits", "v: [0.60, 3.00, 1e3, 1E+2, -1.5e-7]", `{"v":[0.60,3.00,1e3,1E+2,-1.5e-7]}`},

		// Quoting makes a string of anything.
		{"quoted scalars", `v: ["123", '1.5', "true", 'null', "", "~"]`, `{"v":["123","1.5","true","null","","~"]}`},
		{"block scalars", "v: |\n  a\n  b\nw: >\n  c\n  d\n", `{"v":"a\nb\n","w":"c d\n"}`},
		{"text outside ASCII", "v: \xc3\xa9\xf0\x9f\x98\x80", "{\"v\":\"\xc3\xa9\xf0\x9f\x98\x80\"}"},

		// An explicit tag follows the same rule: the type only when the text is that JSON literal.
		{"tagged as a string", "v: !!str 123", `{"v":"123"}`},
		{"tagged as an integer", `v: [!!int "12", !!int 0x10, !!int true]`, `{"v":[12,"0x10","true"]}`},
		{"tagged as a float", "v: [!!float 1, !!float 1e400, !!float .inf]", `{"v":[1,1e400,".inf"]}`},
		{"tagged as a boolean", `v: [!!bool "true", !!bool yes]`, `{"v":[true,"yes"]}`},
		{"tagged as null", `v: [!!null "", !!null "null", !!null x]`, `{"v":[null,null,"x"]}`},
		{"other tags are text", "v: [!custom x, !!binary aGk=, !!timestamp 2026-11-01]", `{"v":["x","aGk=","2026-11-01"]}`},

		// Shape.
		{"flow style", "{a: [1, {b: c}], d: {}}", `{"a":[1,{"b":"c"}],"d":{}}`},
		{"keys come out sorted", "b: 1\na: 2\n\"10\": 3\n'9': 4", `{"10":3,"9":4,"a":2,"b":1}`},
		{"a quoted key may be anything", "\"<<\": 1\n'1': 2\n\"true\": 3\n\"\": 4\n'~': 5", `{"":4,"1":2,"<<":1,"true":3,"~":5}`},
		{"comments and the document markers", "# c\n---\na: 1 # c\n...\n", `{"a":1}`},
		{"a scalar document", "text", `"text"`},
		{"an empty document", "---\n", `null`},
		{"a nested list", "- - a\n  - b\n- c", `[["a","b"],"c"]`},
		{"nesting at the bound", strings.Repeat("[", maxYAMLDepth) + strings.Repeat("]", maxYAMLDepth),
			strings.Repeat("[", maxYAMLDepth) + strings.Repeat("]", maxYAMLDepth)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := yamlToJSON([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("yamlToJSON(%q): %v", tt.yaml, err)
			}
			if string(got) != tt.want {
				t.Errorf("yamlToJSON(%q)\n got %s\nwant %s", tt.yaml, got, tt.want)
			}
		})
	}
}

// aliasBomb is nine references to nine references, sixteen levels deep:
// a few hundred bytes that expand to 9^17 scalars.
func aliasBomb() []byte {
	var b strings.Builder
	b.WriteString("a: &a [x,x,x,x,x,x,x,x,x]\n")
	for c := 'b'; c <= 'q'; c++ {
		ref := "*" + string(c-1)
		b.WriteString(string(c) + ": &" + string(c) + " [" + strings.Repeat(ref+",", 8) + ref + "]\n")
	}
	return []byte(b.String())
}

func TestYAMLRefuses(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{"anchor on a value", "a: 1\nb: &x 2\n", "line 2: anchors are not supported"},
		{"anchor on a key", "a: 1\n&x b: 2\n", "line 2: anchors are not supported"},
		{"anchor on a list", "a:\n  b: &x [1]\n", "line 2: anchors are not supported"},
		{"anchor on the document", "&x\na: 1\n", "line 1: anchors are not supported"},
		{"anchor and alias", "a: &x 1\nb: *x\n", "line 1: anchors are not supported"},
		{"alias bomb", string(aliasBomb()), "line 1: anchors are not supported"},
		{"merge key", "a: 1\nb:\n  <<: {c: 1}\n", "line 3: merge keys are not supported"},
		{"repeated key", "a: 1\nb: 2\na: 3\n", `line 3: key "a" is repeated`},
		{"repeated key, one quoted", "a: 1\n\"a\": 2\n", `line 2: key "a" is repeated`},
		{"repeated key, one escaped", "a: 1\n\"" + backslash + "x61\": 2\n", `line 2: key "a" is repeated`},
		{"repeated key three levels deep", "a:\n  b:\n    c:\n      d: 1\n      e: 2\n      d: 3\n", `line 6: key "d" is repeated`},
		{"repeated key in flow style", "a: {b: 1, b: 2}\n", `line 1: key "b" is repeated`},
		{"integer key", "a: 1\n1: b\n", "line 2: a key must be a string"},
		{"float key", "1.5: b\n", "line 1: a key must be a string"},
		{"boolean key", "true: b\n", "line 1: a key must be a string"},
		{"null key", "~: b\n", "line 1: a key must be a string"},
		{"empty key", "? \n: b\n", "line 1: a key must be a string"},
		{"date key", "2026-11-01: b\n", "line 1: a key must be a string"},
		{"list key", "? [a, b]\n: c\n", "line 1: a key must be a string"},
		{"mapping key", "? {a: 1}\n: c\n", "line 1: a key must be a string"},
		{"second document", "a: 1\n---\nb: 2\n", "a second document is not allowed"},
		{"empty second document", "a: 1\n---\n", "a second document is not allowed"},
		{"no document", "", "the file holds no document"},
		{"only comments", "# a: 1\n\n", "the file holds no document"},
		{"nesting past the bound", strings.Repeat("[", maxYAMLDepth+1) + strings.Repeat("]", maxYAMLDepth+1), "line 1: nesting is deeper than 32 levels"},
		{"mapping nested past the bound", strings.Repeat("{a: ", maxYAMLDepth) + "1" + strings.Repeat("}", maxYAMLDepth), "line 1: nesting is deeper than 32 levels"},
		{"larger than the bound", "a: 1\n#" + strings.Repeat("x", MaxYAMLBytes), "the document is larger than 16 MiB"},
		{"set", "a: !!set {x, y}\n", `line 1: tag "!!set" is not supported`},
		{"ordered map", "a: 1\nb: !!omap [x: 1]\n", `line 2: tag "!!omap" is not supported`},
		{"custom tag on a mapping", "a: !thing {x: 1}\n", `line 1: tag "!thing" is not supported`},
		{"tag with a line break and an escape", "a: !x%0A%1B%5B31mFAKE {x: 1}\n", `line 1: tag "!x\n\x1b[31mFAKE" is not supported`},
		{"repeated key with a line break", "\"a\\nb\": 1\n\"a\\nb\": 2\n", `line 2: key "a\nb" is repeated`},
		{"long repeated key is cut", strings.Repeat("k", 100) + ": 1\n" + strings.Repeat("k", 100) + ": 2\n", `line 2: key "` + strings.Repeat("k", 64) + `" is repeated`},
		{"syntax error", "a: [1\n", "did not find expected ',' or ']'"},
		{"syntax error in a second document", "a: 1\n---\nb: [1\n", "did not find expected ',' or ']'"},
		{"unknown alias", "a: *x\n", "unknown anchor"},
		{"invalid UTF-8", "a: \xff\n", "invalid leading UTF-8 octet"},
		{"control character", "a: \x00\n", "control characters are not allowed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := yamlToJSON([]byte(tt.yaml))
			if err == nil {
				t.Fatalf("accepted, as %s", got)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q, want one with %q", err, tt.want)
			}
		})
	}
}

// An alias always follows its anchor, and the walk stops at the anchor, so no
// document reaches the refusal of an alias. It is the net under that order.
func TestYAMLRefusesAliasNode(t *testing.T) {
	_, err := fromYAML(&yaml.Node{Kind: yaml.AliasNode, Line: 7, Alias: &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "x"}}, 1)
	if err == nil || err.Error() != "yaml: line 7: aliases are not supported" {
		t.Errorf("fromYAML(alias) = %v", err)
	}
}

// Design, section 4, "to confirm, C2": the library refuses an alias bomb and
// a repeated key only when it decodes into Go values. Reading into a Node
// runs neither guard, so the converter's own refusals are what protects.
func TestYAMLNodeBypassesTheLibraryGuards(t *testing.T) {
	var into any
	if err := yaml.Unmarshal(aliasBomb(), &into); err == nil || !strings.Contains(err.Error(), "excessive aliasing") {
		t.Errorf("decoding the bomb into a Go value: %v, want the library's refusal", err)
	}
	if err := yaml.Unmarshal([]byte("a: 1\na: 2\n"), &into); err == nil || !strings.Contains(err.Error(), "already defined") {
		t.Errorf("decoding a repeated key into a Go value: %v, want the library's refusal", err)
	}
	var node yaml.Node
	if err := yaml.Unmarshal(aliasBomb(), &node); err != nil {
		t.Errorf("the library now refuses the bomb for a Node too (%v): this test and the comment in yamlToJSON need an update", err)
	}
	if err := yaml.Unmarshal([]byte("a: 1\na: 2\n"), &node); err != nil {
		t.Errorf("the library now refuses a repeated key for a Node too (%v): this test and the comment in yamlToJSON need an update", err)
	}

	start := time.Now()
	if _, err := yamlToJSON(aliasBomb()); err == nil {
		t.Error("the converter accepted the bomb")
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("refusing the bomb took %v: something expanded it", took)
	}
}

func TestDecodeYAML(t *testing.T) {
	t.Run("declared values", func(t *testing.T) {
		const in = `
version: 1
providers:            # nothing declared yet
keys:
  agent: { models: [] }
targets:
  p/m:
    capabilities: { tools: false }
    pricing: { input_per_million: 0, as_of: 2026-11-01 }
    reasoning:
      off: {}
      on: { thinking: { type: enabled, budget_tokens: 1024 } }
`
		want := domain.Document{
			Version: 1,
			Keys:    map[string]domain.Key{"agent": {Models: []string{}}},
			Targets: map[string]domain.Target{"p/m": {
				Capabilities: domain.Capabilities{Tools: new(false)},
				Pricing:      &domain.Pricing{InputPerMillion: new(0.0), AsOf: "2026-11-01"},
				Reasoning: &domain.Reasoning{
					Off: map[string]json.RawMessage{},
					On:  map[string]json.RawMessage{"thinking": json.RawMessage(`{"budget_tokens":1024,"type":"enabled"}`)},
				},
			}},
		}
		var got domain.Document
		if err := DecodeYAML([]byte(in), &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("DecodeYAML:\n got %+v\nwant %+v", got, want)
		}
	})

	refused := []struct {
		name string
		yaml string
		want string
	}{
		{"unknown member", "version: 1\nbogus: 2\n", `unknown member "/bogus"`},
		{"unknown nested member", "models:\n  fast:\n    max_output_token: 1\n", `unknown member "/models/fast/max_output_token"`},
		{"hexadecimal where a number is declared", "version: 0x1\n", `the value at "/version" is not valid: wrong type`},
		{"digit separators where a number is declared", "settings: { max_body_bytes: 10_485_760 }\n", `the value at "/settings/max_body_bytes" is not valid: wrong type`},
		{"quoted number", "version: \"1\"\n", `the value at "/version" is not valid: wrong type`},
		{"yes where a boolean is declared", "targets: { p/m: { capabilities: { tools: yes } } }\n", `the value at "/targets/p~1m/capabilities/tools" is not valid: wrong type`},
		{"infinity where a number is declared", "models: { m: { trace_sample_rate: .inf } }\n", `the value at "/models/m/trace_sample_rate" is not valid: wrong type`},
		{"number where text is declared", "targets: { p/m: { pricing: { as_of: 2026 } } }\n", `the value at "/targets/p~1m/pricing/as_of" is not valid: wrong type`},
		{"empty document", "---\n", "not null"},
		{"text document", "version\n", `the value at "" is not valid: wrong type`},
		{"list document", "- version: 1\n", `the value at "" is not valid: wrong type`},
		{"refused by the converter", "version: 1\nversion: 1\n", `line 2: key "version" is repeated`},
	}
	for _, tt := range refused {
		t.Run(tt.name, func(t *testing.T) {
			var doc domain.Document
			err := DecodeYAML([]byte(tt.yaml), &doc)
			if err == nil {
				t.Fatalf("accepted, as %+v", doc)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q, want one with %q", err, tt.want)
			}
		})
	}
}

func TestEncodeYAML(t *testing.T) {
	doc := domain.Document{
		Version:  1,
		Settings: domain.Settings{MaxBodyBytes: 10485760, Telemetry: &domain.Telemetry{Headers: map[string]string{"x-version": "4"}}},
		Targets: map[string]domain.Target{"p/m": {
			Kind:            domain.KindChat,
			Capabilities:    domain.Capabilities{Tools: new(true), Vision: new(false)},
			MaxOutputTokens: 64000,
			Reasoning: &domain.Reasoning{
				Levels:     []string{"low", "high"},
				CanDisable: new(false),
				On:         map[string]json.RawMessage{"thinking": json.RawMessage(`{"type":"enabled","budget_tokens":1e3,"big":1e400}`)},
				Off:        map[string]json.RawMessage{},
			},
			Pricing: &domain.Pricing{InputPerMillion: new(0.15), OutputPerMillion: new(0.0), AsOf: "2026-11-01"},
		}},
		Models: map[string]domain.Model{
			"b":    {Targets: []string{"p/m", "p/n"}, Params: map[string]string{"true": "drop", "seed": "forward"}},
			"2024": {TraceSampleRate: new(0.0)},
			"a":    {},
		},
		Keys: map[string]domain.Key{"k": {Models: []string{}}, "all": {Models: []string{"*"}}},
	}
	const want = `version: 1
settings:
  max_body_bytes: 10485760
  telemetry:
    headers:
      x-version: "4"
targets:
  p/m:
    kind: chat
    capabilities:
      tools: true
      vision: false
    max_output_tokens: 64000
    reasoning:
      levels:
        - low
        - high
      can_disable: false
      on:
        thinking:
          big: !!float 1e400
          budget_tokens: 1e3
          type: enabled
      off: {}
    pricing:
      input_per_million: 0.15
      output_per_million: 0
      as_of: "2026-11-01"
models:
  "2024":
    trace_sample_rate: 0
  a: {}
  b:
    targets:
      - p/m
      - p/n
    params:
      seed: forward
      "true": drop
keys:
  all:
    models:
      - '*'
  k:
    models: []
`
	got, err := EncodeYAML(doc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("EncodeYAML:\n%s\nwant:\n%s", got, want)
	}
	var back domain.Document
	if err := DecodeYAML(got, &back); err != nil {
		t.Fatalf("DecodeYAML of the export: %v", err)
	}
	if a, b := mustEncode(t, back), mustEncode(t, doc); a != b {
		t.Errorf("export then import changed the document:\n got %s\nwant %s", a, b)
	}
}

func mustEncode(t *testing.T, v any) string {
	t.Helper()
	out, err := Encode(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// Every string must come back from an export as the string it was, as a
// value, as a list item and as a key.
func TestYAMLStringsSurviveExport(t *testing.T) {
	tricky := []string{
		"", " ", "  ", "\t", "\n", "\n\n", "\r", "\r\n", "a\n", "a\n\n", "\na", "a\nb", "a\n b", " a\nb", "a \nb", "a\n\nb", "a\r\nb", "a\tb",
		" lead", "trail ", "\ttab", "a  b",
		"~", "null", "Null", "NULL", "true", "True", "TRUE", "false", "yes", "no", "on", "off", "y", "n", "Y", "N",
		"0", "1", "-1", "+1", "007", "1.5", "1.0", "1e3", "1E3", ".5", "1.", "0x10", "0o7", "0b1", "1_000", ".inf", "-.inf", ".nan",
		"12345678901234567890", "1e400",
		"2026-11-01", "2026-11-01T10:00:00Z", "2026-11-01 10:00:00", "12:30", "12:30:00",
		"#", "# c", "a #b", "a# b", ":", "a: b", "a:b", "a :b", "- a", "-", "--", "---", "--- a", "...", "... a",
		"?", "? a", ",", "[", "]", "{", "}", "[a]", "{a: 1}", "*", "*a", "&a", "!", "!a", "!!str a", "<<", "=", "|", ">", "| a", "%", "%a", "@a", "`a",
		"'", "''", "'a'", "\"", "\"a\"", backslash, "a" + backslash + "nb", backslash + "x41", "$HOME", "${A}",
		"\x00", "\x01", "\x1b[0m", "\x7f",
		"\xc3\xa9", "\xc2\x85", "\xc2\xa0", "\xe2\x80\xa8", "\xe2\x80\xa9", "\xef\xbb\xbf", "\xef\xbb\xbfa", "\xef\xbf\xbd", "\xf0\x9f\x98\x80",
		"a\xe2\x80\xa8b", "a\xc2\x85b",
		strings.Repeat("word ", 100), strings.Repeat("x", 500), strings.Repeat("line\n", 50),
	}
	asValue := map[string]string{}
	asKey := map[string]string{}
	for i, s := range tricky {
		asValue[string(rune('a'+i/26))+string(rune('a'+i%26))] = s
		asKey[s] = "v"
	}
	if len(asKey) != len(tricky) {
		t.Fatal("the list repeats a string")
	}
	for name, in := range map[string]any{"values": asValue, "list": tricky, "keys": asKey} {
		t.Run(name, func(t *testing.T) {
			out, err := EncodeYAML(in)
			if err != nil {
				t.Fatal(err)
			}
			back := reflect.New(reflect.TypeOf(in))
			if err := DecodeYAML(out, back.Interface()); err != nil {
				t.Fatalf("DecodeYAML of the export: %v\n%s", err, out)
			}
			if !reflect.DeepEqual(back.Elem().Interface(), in) {
				t.Errorf("export then import changed a string:\n got %q\nwant %q\nyaml:\n%s", back.Elem().Interface(), in, out)
			}
		})
	}
}

// Numbers inside a fragment are not Go numbers: export must not round them.
func TestYAMLNumbersSurviveExport(t *testing.T) {
	// The last one is an integer that no float holds: YAML reads it as text
	// unless the export tags it.
	in := domain.Reasoning{Off: map[string]json.RawMessage{
		"n": json.RawMessage(`[0,-0,1.0,0.60,1e3,1E+2,1e+21,1e-7,1e400,-1E-400,12345678901234567890,123456789012345678901234567890,true,false,null,1` + strings.Repeat("0", 400) + `]`),
	}}
	out, err := EncodeYAML(in)
	if err != nil {
		t.Fatal(err)
	}
	var back domain.Reasoning
	if err := DecodeYAML(out, &back); err != nil {
		t.Fatalf("DecodeYAML of the export: %v\n%s", err, out)
	}
	if a, b := mustEncode(t, back), mustEncode(t, in); a != b {
		t.Errorf("export then import changed a number:\n got %s\nwant %s\nyaml:\n%s", a, b, out)
	}
}

// annexBYAML is the example configuration as the kick-off prints it, so the
// file the project documents is the file these tests read.
func annexBYAML(t *testing.T) []byte {
	t.Helper()
	text, err := os.ReadFile("../../kick-off.md")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, found := strings.Cut(string(text), "\n## Annex B.")
	_, rest, opened := strings.Cut(rest, "\n```yaml\n")
	block, _, closed := strings.Cut(rest, "\n```\n")
	if !found || !opened || !closed {
		t.Fatal("annex B of the kick-off has no yaml block")
	}
	return []byte(block)
}

func TestYAMLAnnexB(t *testing.T) {
	want := mustEncode(t, annexBJSON(t))

	var read domain.Document
	if err := DecodeYAML(annexBYAML(t), &read); err != nil {
		t.Fatalf("DecodeYAML(annex B): %v", err)
	}
	if got := mustEncode(t, read); got != want {
		t.Errorf("annex B read from YAML differs from the JSON fixture:\n got %s\nwant %s", got, want)
	}

	// Property (OPS-3, AC-39): export, then import, gives the same document,
	// and a second export gives the same text.
	exported, err := EncodeYAML(read)
	if err != nil {
		t.Fatal(err)
	}
	var back domain.Document
	if err := DecodeYAML(exported, &back); err != nil {
		t.Fatalf("DecodeYAML of the export: %v\n%s", err, exported)
	}
	if got := mustEncode(t, back); got != want {
		t.Errorf("export then import changed annex B:\n got %s\nwant %s", got, want)
	}
	if again, err := EncodeYAML(back); err != nil || !bytes.Equal(again, exported) {
		t.Errorf("a second export differs (error %v):\n%s\nfirst:\n%s", err, again, exported)
	}

	// Block style only, date-like and number-like strings quoted, and the
	// members of the document in their declared order.
	for _, line := range []string{
		"\n      as_of: \"2026-11-01\"\n",
		"\n      x-langfuse-ingestion-version: \"4\"\n",
		"\n    trace_sample_rate: 0\n",
		"\n      - window: 5h\n        requests: 1500\n",
		"\n    reasoning:\n      effort: medium\n      mode: default\n",
	} {
		if !bytes.Contains(exported, []byte(line)) {
			t.Errorf("the export lacks %q:\n%s", line, exported)
		}
	}
	if i := bytes.IndexAny(exported, "{}[]&*!#"); i >= 0 {
		t.Errorf("the export holds %q, which block style without tags, anchors or comments never writes:\n%s", exported[i], exported)
	}
	var order []string
	for line := range strings.Lines(string(exported)) {
		if !strings.HasPrefix(line, " ") {
			order = append(order, strings.TrimSpace(line))
		}
	}
	if got := strings.Join(order, " "); got != "version: 1 settings: providers: targets: models: keys:" {
		t.Errorf("top-level members of the export: %s", got)
	}
}
