package telemetry

import (
	"encoding/json/jsontext"
	"math"
	"strings"
	"testing"
	"time"
)

// The encoder writes OTLP 1.11.1 JSON: lowerCamelCase keys, hex ids, integer
// enums, 64-bit integers as decimal strings, a string array as arrayValue.
func TestEncode(t *testing.T) {
	full := Span{Trace: [16]byte{15: 1}, ID: [8]byte{7: 2}, Parent: [8]byte{0: 0xab, 7: 3}, Remote: true, Name: "chat fast", Kind: KindServer,
		Start: time.Unix(1, 5), End: time.Unix(2, 0), Failed: true, Message: "upstream failed",
		Attrs: []Attr{{"s", "x"}, {"b", true}, {"i", int64(-3)}, {"big", int64(math.MaxInt64)}, {"f", 1.5}, {"z", 0.0}, {"nan", math.NaN()},
			{"inf", math.Inf(-1)}, {"a", []string{"p", "q"}}, {"empty", ""}}}
	for _, tt := range []struct {
		name string
		span Span
		want string
	}{
		{"every field and attribute type", full, `{"traceId":"00000000000000000000000000000001","spanId":"0000000000000002",` +
			`"parentSpanId":"ab00000000000003","flags":769,"name":"chat fast","kind":2,"startTimeUnixNano":"1000000005","endTimeUnixNano":"2000000000",` +
			`"attributes":[{"key":"s","value":{"stringValue":"x"}},{"key":"b","value":{"boolValue":true}},{"key":"i","value":{"intValue":"-3"}},` +
			`{"key":"big","value":{"intValue":"9223372036854775807"}},{"key":"f","value":{"doubleValue":1.5}},{"key":"z","value":{"doubleValue":0}},` +
			`{"key":"nan","value":{"doubleValue":"NaN"}},{"key":"inf","value":{"doubleValue":"-Infinity"}},` +
			`{"key":"a","value":{"arrayValue":{"values":[{"stringValue":"p"},{"stringValue":"q"}]}}},{"key":"empty","value":{"stringValue":""}}],` +
			`"status":{"message":"upstream failed","code":2}}`},
		{"a local root without attributes", Span{Trace: [16]byte{0: 0xff}, ID: [8]byte{0: 1}, Name: "t", Kind: KindInternal, Start: time.Unix(0, 0).Add(-time.Second), End: time.Unix(0, 7)},
			`{"traceId":"ff000000000000000000000000000000","spanId":"0100000000000000","flags":257,"name":"t","kind":1,` +
				`"startTimeUnixNano":"0","endTimeUnixNano":"7"}`},
	} {
		got, err := encode(tt.span)
		if err != nil || string(got) != tt.want {
			t.Errorf("%s: %v\n got %s\nwant %s", tt.name, err, got, tt.want)
		}
	}
	if _, err := encode(Span{Attrs: []Attr{{"n", 3}}}); err == nil || !strings.Contains(err.Error(), "int") {
		t.Errorf("an attribute of type int: %v", err)
	}

	span, _ := encode(full)
	got, err := body("v1.2.3", []jsontext.Value{span, span})
	want := `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"stargaite"}},` +
		`{"key":"service.version","value":{"stringValue":"v1.2.3"}}]},"scopeSpans":[{"scope":{"name":"stargaite","version":"v1.2.3"},` +
		`"spans":[` + string(span) + `,` + string(span) + `]}]}]}`
	if err != nil || string(got) != want {
		t.Errorf("body: %v\n got %s\nwant %s", err, got, want)
	}
}

// random never answers all zeros, the invalid id.
func TestRandom(t *testing.T) {
	seen := map[[8]byte]bool{}
	for range 100 {
		var id [8]byte
		random(id[:])
		if id == [8]byte{} || seen[id] {
			t.Fatalf("id %x", id)
		}
		seen[id] = true
	}
}
