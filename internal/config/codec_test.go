package config

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/jorlaru/stargaite/internal/domain"
)

// backslash keeps every escape of this file in two pieces, so the source
// never holds one that an editor or a tool could decode on the way.
const backslash = `\`

// escapedE is the JSON escape of the letter "e".
const escapedE = backslash + "u0065"

func TestDecodeRefuses(t *testing.T) {
	type named struct {
		Model string `json:"model"`
	}
	tests := []struct {
		name string
		in   string
		v    any    // what to decode into; a document when nil
		want string // the message, or the part of it that is not the standard decoder's
	}{
		{name: "duplicate member", in: `{"version":1,"version":1}`, want: `member "/version" is repeated`},
		{name: "duplicate member, one name escaped", in: `{"model":"a","mod` + escapedE + `l":"b"}`, v: new(named),
			want: `member "/model" is repeated`},
		{name: "duplicate member, one name escaped, in a map", in: `{"mod` + escapedE + `l":1,"model":2}`, v: new(map[string]any),
			want: `member "/model" is repeated`},
		{name: "duplicate member three levels deep",
			in:   `{"version":1,"models":{"fast":{"reasoning":{"effort":"low","effort":"high"}}}}`,
			want: `member "/models/fast/reasoning/effort" is repeated`},
		{name: "duplicate resource name", in: `{"version":1,"keys":{"a":{"models":[]},"a":{"models":[]}}}`,
			want: `member "/keys/a" is repeated`},
		{name: "duplicate member inside a raw fragment",
			in:   `{"version":1,"targets":{"p/m":{"reasoning":{"on":{"thinking":{"type":"a","type":"b"}}}}}}`,
			want: `member "/targets/p~1m/reasoning/on/thinking/type" is repeated`},
		{name: "duplicate member inside a raw fragment, one name escaped",
			in:   `{"version":1,"targets":{"p/m":{"reasoning":{"on":{"thinking":{"typ` + escapedE + `":"a","type":"b"}}}}}}`,
			want: `member "/targets/p~1m/reasoning/on/thinking/type" is repeated`},
		{name: "unknown member", in: `{"version":1,"bogus":true}`, want: `unknown member "/bogus"`},
		{name: "unknown nested member", in: `{"version":1,"models":{"fast":{"max_output_token":1}}}`,
			want: `unknown member "/models/fast/max_output_token"`},
		{name: "member in another letter case", in: `{"Version":1}`, want: `unknown member "/Version"`},
		{name: "second value", in: `{"version":1}{"version":2}`, want: `after top-level value at "", byte 13`},
		{name: "trailing text", in: `{"version":1} x`, want: `after top-level value at "", byte 14`},
		{name: "invalid UTF-8 in a value", in: "{\"version\":1,\"providers\":{\"p\":{\"base_url\":\"\xff\"}}}",
			want: `invalid UTF-8 at "/providers/p/base_url", byte 43`},
		{name: "invalid UTF-8 in a name", in: "{\"version\":1,\"providers\":{\"\xff\":{}}}", want: `invalid UTF-8 at "/providers", byte 27`},
		{name: "invalid UTF-8 inside a raw fragment",
			in:   "{\"version\":1,\"targets\":{\"p/m\":{\"reasoning\":{\"off\":{\"k\":\"\xff\"}}}}}",
			want: `invalid UTF-8 at "/targets/p~1m/reasoning/off/k", byte 56`},
		{name: "half of a surrogate pair", in: `{"version":1,"providers":{"p":{"base_url":"` + backslash + `ud800"}}}`,
			want: `at "/providers/p/base_url", byte 43`},
		{name: "null", in: `null`, want: "a value is expected, not null"},
		{name: "null in white space", in: " null\n", want: "a value is expected, not null"},
		{name: "empty input", in: ``, want: `unexpected EOF at "", byte 0`},
		{name: "cut short", in: `{"version":1,"models":{"fast":`, want: `at "/models/fast", byte 30`},
		{name: "list", in: `[]`, want: `the value at "" is not valid: wrong type`},
		{name: "number written as text", in: `{"version":"1"}`, want: `the value at "/version" is not valid: wrong type`},
		{name: "text where a list is declared", in: `{"version":1,"models":{"fast":{"targets":"p/m"}}}`,
			want: `the value at "/models/fast/targets" is not valid: wrong type`},
		{name: "fraction where an integer is declared", in: `{"version":1,"settings":{"max_body_bytes":1.5}}`,
			want: `the value at "/settings/max_body_bytes" is not valid: `},
		{name: "integer out of range", in: `{"version":1,"settings":{"max_body_bytes":12345678901234567890}}`,
			want: `the value at "/settings/max_body_bytes" is not valid: `},
		{name: "date where a time is declared", in: `{"version":1,"keys":{"k":{"content_until":"2026-11-01"}}}`,
			want: `the value at "/keys/k/content_until" is not valid: `},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := tt.v
			if v == nil {
				v = new(domain.Document)
			}
			err := Decode([]byte(tt.in), v)
			if err == nil {
				t.Fatalf("Decode(%q) accepted the input, decoded %+v", tt.in, v)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Decode(%q) = %q, want a message with %q", tt.in, err, tt.want)
			}
			// What an operator reads names no Go type and does not change
			// from one process to the next, as the standard decoder's does.
			for _, leak := range []string{"Go ", "domain.", "unmarshal"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("Decode(%q) = %q, which holds %q", tt.in, err, leak)
				}
			}
		})
	}
}

func TestDecodeAccepts(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want domain.Document
	}{
		{name: "white space around the value", in: " \n{\"version\": 1}\t\r\n", want: domain.Document{Version: 1}},
		{name: "an escaped name is the member it spells", in: `{"v` + escapedE + `rsion":1}`, want: domain.Document{Version: 1}},
		{name: "a null member is an absent one", in: `{"version":1,"providers":null,"settings":{"budget":null}}`, want: domain.Document{Version: 1}},
		{name: "names that differ are not duplicates",
			in: `{"version":1,"models":{"a":{},"A":{}}}`, want: domain.Document{Version: 1, Models: map[string]domain.Model{"a": {}, "A": {}}}},
		{name: "a fragment keeps the digits of a number",
			in: `{"version":1,"targets":{"p/m":{"reasoning":{"off":{"n":12345678901234567890}}}}}`,
			want: domain.Document{Version: 1, Targets: map[string]domain.Target{"p/m": {
				Reasoning: &domain.Reasoning{Off: map[string]json.RawMessage{"n": json.RawMessage(`12345678901234567890`)}}}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got domain.Document
			if err := Decode([]byte(tt.in), &got); err != nil {
				t.Fatalf("Decode(%q): %v", tt.in, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Decode(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestEncodeCanonical(t *testing.T) {
	doc := domain.Document{
		Version: 1,
		Providers: map[string]domain.Provider{
			"b": {BaseURL: "https://example.com/v1?a=1&b=<2>"}, "a": {}, "B": {}, "10": {}, "9": {},
		},
		Targets: map[string]domain.Target{"a/m": {Reasoning: &domain.Reasoning{On: map[string]json.RawMessage{
			"z": json.RawMessage("{ \"b\" : 1.0,\n \"a\" : 12345678901234567890, \"c\": {\"y\": 1e400, \"x\": []} }"),
			"a": json.RawMessage(`[ 1 , 2 ]`),
		}}}},
	}
	// Names sorted by their bytes, the fragment compact with its members
	// sorted and its numbers untouched, and no HTML escaping.
	const want = `{"version":1,"providers":{"10":{},"9":{},"B":{},"a":{},"b":{"base_url":"https://example.com/v1?a=1&b=<2>"}},` +
		`"targets":{"a/m":{"reasoning":{"on":{"a":[1,2],"z":{"a":12345678901234567890,"b":1.0,"c":{"x":[],"y":1e400}}}}}}}`
	// Map iteration order changes from one run of the loop to the next.
	for range 20 {
		got, err := Encode(doc)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("Encode:\n got %s\nwant %s", got, want)
		}
	}
}

func TestEncodeTwoSpellingsOfOneFragment(t *testing.T) {
	var encoded []string
	for _, in := range []string{
		`{"on":{"thinking":{"type":"enabled","budget_tokens":1024}}}`,
		"{\"on\": {\"thinking\": {\n\t\"budget_tokens\": 1024, \"type\": \"enabled\"}}}",
	} {
		var r domain.Reasoning
		if err := Decode([]byte(in), &r); err != nil {
			t.Fatal(err)
		}
		out, err := Encode(r)
		if err != nil {
			t.Fatal(err)
		}
		encoded = append(encoded, string(out))
	}
	if encoded[0] != encoded[1] {
		t.Errorf("one declaration, two encodings:\n%s\n%s", encoded[0], encoded[1])
	}
}

func TestEncodeRefusesInvalidUTF8(t *testing.T) {
	if out, err := Encode(domain.Provider{BaseURL: "https://example.com/\xff"}); err == nil {
		t.Errorf("Encode wrote %q, which Decode would refuse", out)
	}
}

// annexBJSON is annex B of the kick-off as the domain package keeps it.
func annexBJSON(t *testing.T) domain.Document {
	t.Helper()
	data, err := os.ReadFile("../domain/testdata/annex-b.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc domain.Document
	if err := Decode(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestCodecFixedPoint(t *testing.T) {
	doc := annexBJSON(t)
	first, err := Encode(doc)
	if err != nil {
		t.Fatal(err)
	}
	var again domain.Document
	if err := Decode(first, &again); err != nil {
		t.Fatalf("Decode of the canonical encoding: %v", err)
	}
	if !reflect.DeepEqual(again, doc) {
		t.Errorf("decoding the canonical encoding changed the document:\n got %+v\nwant %+v", again, doc)
	}
	second, err := Encode(again)
	if err != nil {
		t.Fatal(err)
	}
	if string(second) != string(first) {
		t.Errorf("the canonical encoding is not a fixed point:\n%s\n%s", first, second)
	}
}
