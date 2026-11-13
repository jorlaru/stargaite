package params

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"
)

var seeds = []string{
	`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"u"}}]}],"tools":[{"type":"function","function":{"name":"f","strict":true,"parameters":{"a":1,"a":2}}}],"response_format":{"type":"json_schema","json_schema":{"strict":true}},"stream_options":{"include_usage":true}}`,
	`{"model":"m","max_tokens":1024,"thinking":{"type":"enabled","budget_tokens":2048},"output_config":{"effort":"high","format":{"type":"json_schema"}},"messages":[{"role":"user","content":[{"type":"tool_result","content":[{"type":"image"}]}]}]}`,
	`{"choices":[{"message":{"content":"x","tool_calls":[{"id":"a"}]},"delta":{"tool_calls":[{"index":0}]}}],"usage":{"prompt_tokens_details":{"cached_tokens":1}}}`,
	`{"type":"message_start","message":{"usage":{"input_tokens":1}},"content_block":{"type":"text"},"delta":{"text":"t"},"error":{"type":"e"}}`,
	"{\"mod\\u0065l\":\"\\u00e9\\ud83d\\ude00\",\"\\\"\":[[], {}, null, true, false, -1.5e3]}",
	` { "a" : [ 1 , { "b" : { } } ] } `,
	`{"model":"a","model":"b"}`,
	`{"messages":[{"role":"a","role":"b"}]}`,
	"{\"a\":\"\xff\"}",
	`{"a":1} {}`,
	`[1]`,
	`{}`,
}

// escaped spells every UTF-16 unit of name as an escape sequence.
func escaped(name string) []byte {
	out := []byte{'"'}
	for _, u := range utf16.Encode([]rune(name)) {
		out = fmt.Appendf(out, "\\"+"u%04x", u)
	}
	return append(out, '"')
}

// FuzzScan checks Scan against the standard validator: it accepts exactly
// the JSON objects without a repeat in an indexed object (and within
// maxValues indexed values, each at least two bytes), refuses a
// differently escaped repeat of a top-level name, and Detect never panics.
func FuzzScan(f *testing.F) {
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		trimmed := bytes.TrimLeft(data, " \t\r\n")
		object := len(trimmed) > 0 && trimmed[0] == '{'
		valid := object && jsontext.Value(data).IsValid(jsontext.AllowDuplicateNames(true))
		unique := object && jsontext.Value(data).IsValid()
		for _, walk := range [][][]string{nil, Chat.Walk, Messages.Walk, ChatOut, ChatReply, MessagesReply} {
			b, e := Scan(data, walk)
			if e != nil {
				capped := strings.HasPrefix(e.Message, "the body has more than ") && len(data) > 2*maxValues
				if e.Code != "invalid_request" || e.Message == "" || (unique || valid && !strings.HasPrefix(e.Message, "repeated key ")) && !capped {
					t.Fatalf("walk %v: %q refused with %+v (valid %v, unique %v)", walk, data, e, valid, unique)
				}
				continue
			}
			if !valid {
				t.Fatalf("walk %v: accepted %q", walk, data)
			}
			keys := b.Keys()
			for i, k := range keys {
				v, ok := b.Get(k)
				if !ok || slices.Contains(keys[:i], k) || !jsontext.Value(v).IsValid(jsontext.AllowDuplicateNames(true)) {
					t.Fatalf("walk %v: key %q of %q: %q, %v", walk, k, data, v, ok)
				}
			}
			Detect("chat", b)
			Detect("messages", b)
			if len(keys) == 0 {
				continue
			}
			end := bytes.LastIndexByte(data, '}')
			dup := slices.Concat(data[:end], []byte(","), escaped(keys[0]), []byte(":0"), data[end:])
			if _, e := Scan(dup, walk); e == nil || e.Param != keys[0] {
				t.Fatalf("walk %v: escaped repeat %q accepted or misreported: %+v", walk, dup, e)
			}
		}
	})
}

// FuzzEdit checks one op on any scanned body: the result scans, means the
// body with the op applied, and differs from the body only at the op.
func FuzzEdit(f *testing.F) {
	for _, s := range seeds {
		for _, c := range []struct {
			path, value string
			del         bool
		}{
			{"model", `"x"`, false}, {"output_config/effort", `"high"`, false}, {"thinking", "", true},
			{"messages/0/content", `"hi"`, false}, {"a/1/b/c", `{"d":[1]}`, false}, {"messages/0", "", true},
		} {
			f.Add([]byte(s), c.path, []byte(c.value), c.del)
		}
	}
	f.Fuzz(func(t *testing.T, data []byte, path string, value []byte, del bool) {
		b, e := Scan(data, nil)
		if e != nil {
			return
		}
		p := strings.Split(path, "/")
		if del {
			value = nil
		}
		out, err := b.Edit(Op{Path: p, Value: value})
		if err != nil {
			return
		}
		if _, e := Scan(out, nil); e != nil {
			t.Fatalf("%q edited at %q with %q gave %q: %+v", data, path, value, out, e)
		}
		var before, after, v any
		if json.Unmarshal(data, &before, jsontext.AllowDuplicateNames(true)) != nil ||
			value != nil && json.Unmarshal(value, &v) != nil {
			return // a number beyond float64: no reference value
		}
		pattern, found := find(before, p)
		switch {
		case found && value != nil:
			wb, _ := Scan(data, [][]string{pattern[:len(pattern)-1]})
			old, ok := wb.Get(p...)
			if !ok || len(out)-len(data) != len(value)-len(old) || changed(data, out) > len(old) {
				t.Fatalf("replace in %q at %q with %q gave %q (old %q)", data, path, value, out, old)
			}
		case value != nil && changed(data, out) != 0:
			t.Fatalf("append to %q at %q with %q changed bytes: %q", data, path, value, out)
		case found && changed(out, data) != 0:
			t.Fatalf("delete in %q at %q changed other bytes: %q", data, path, out)
		case !found && value == nil && !bytes.Equal(out, data):
			t.Fatalf("delete of absent %q changed %q to %q", path, data, out)
		}
		if err := json.Unmarshal(out, &after, jsontext.AllowDuplicateNames(true)); err != nil {
			t.Fatal(err)
		}
		if want := apply(before, p, v, value == nil); !reflect.DeepEqual(want, after) {
			t.Fatalf("%q edited at %q with %q gave %q: means %v, want %v", data, path, value, out, after, want)
		}
	})
}

// changed is how many bytes of a, between their longest common prefix and
// suffix, differ from b: zero when b is a with bytes inserted at one place.
func changed(a, b []byte) int {
	n := min(len(a), len(b))
	pre := 0
	for pre < n && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < n-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	return len(a) - pre - suf
}

// find reports whether p exists in a decoded document, and its walk pattern.
func find(doc any, p []string) (pattern []string, found bool) {
	for _, s := range p {
		switch d := doc.(type) {
		case map[string]any:
			c, ok := d[s]
			if !ok {
				return nil, false
			}
			pattern, doc = append(pattern, s), c
		case []any:
			i, err := strconv.Atoi(s)
			if !isIndex(s) || err != nil || i >= len(d) {
				return nil, false
			}
			pattern, doc = append(pattern, "*"), d[i]
		default:
			return nil, false
		}
	}
	return pattern, true
}

// apply is the reference meaning of an op on a decoded document.
func apply(doc any, p []string, v any, del bool) any {
	switch d := doc.(type) {
	case map[string]any:
		if len(p) == 1 {
			if del {
				delete(d, p[0])
			} else {
				d[p[0]] = v
			}
			return d
		}
		c, ok := d[p[0]]
		if !ok {
			if del {
				return d
			}
			c = map[string]any{}
		}
		d[p[0]] = apply(c, p[1:], v, del)
	case []any:
		i, _ := strconv.Atoi(p[0])
		if len(p) == 1 && del {
			return slices.Delete(d, i, i+1)
		}
		if len(p) == 1 {
			d[i] = v
		} else {
			d[i] = apply(d[i], p[1:], v, del)
		}
	}
	return doc
}
