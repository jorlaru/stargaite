// Package telemetry exports spans over OTLP/HTTP with the JSON encoding
// (TEL-1): the span type, its encoder and the Exporter, with Test and Status
// (phase-3 design section 10). It uses the standard library only.
package telemetry

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"math"
	"time"
)

// Span kinds of OTLP.
const (
	KindInternal = 1
	KindServer   = 2
	KindClient   = 3
)

// Attr is one span attribute.
type Attr struct {
	Key   string
	Value any // string, bool, int64, float64 or []string
}

// Span is one span as the exporter encodes it.
type Span struct {
	Trace      [16]byte
	ID, Parent [8]byte
	Remote     bool // the parent came from a traceparent: flags 0x200
	Name       string
	Kind       int // KindInternal, KindServer or KindClient
	Start, End time.Time
	Attrs      []Attr
	Failed     bool   // status ERROR, with Message
	Message    string // already redacted
}

// random fills b from crypto/rand, never with all zeros, which W3C Trace
// Context and OTLP read as an invalid id.
func random(b []byte) {
	for {
		_, _ = rand.Read(b) // never fails (crypto/rand)
		for _, c := range b {
			if c != 0 {
				return
			}
		}
	}
}

// The OTLP 1.11.1 JSON encoding of ExportTraceServiceRequest: lowerCamelCase
// keys, ids as lowercase hex, enums as integers, 64-bit integers as decimal
// strings.
type (
	request struct {
		ResourceSpans []resourceSpans `json:"resourceSpans"`
	}
	resourceSpans struct {
		Resource   resource     `json:"resource"`
		ScopeSpans []scopeSpans `json:"scopeSpans"`
	}
	resource struct {
		Attributes []keyValue `json:"attributes"`
	}
	scopeSpans struct {
		Scope scope            `json:"scope"`
		Spans []jsontext.Value `json:"spans"`
	}
	scope struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	span struct {
		TraceID      string     `json:"traceId"`
		SpanID       string     `json:"spanId"`
		ParentSpanID string     `json:"parentSpanId,omitzero"`
		Flags        uint32     `json:"flags"`
		Name         string     `json:"name"`
		Kind         int        `json:"kind"`
		Start        uint64     `json:"startTimeUnixNano,string"`
		End          uint64     `json:"endTimeUnixNano,string"`
		Attributes   []keyValue `json:"attributes,omitzero"`
		Status       *status    `json:"status,omitzero"`
	}
	status struct {
		Message string `json:"message,omitzero"`
		Code    int    `json:"code"`
	}
	keyValue struct {
		Key   string `json:"key"`
		Value value  `json:"value"`
	}
	value struct {
		String *string `json:"stringValue,omitzero"`
		Bool   *bool   `json:"boolValue,omitzero"`
		Int    *int64  `json:"intValue,omitzero,string"`
		Double any     `json:"doubleValue,omitzero"` // a float64, or the string of a value JSON numbers cannot hold
		Array  *array  `json:"arrayValue,omitzero"`
	}
	array struct {
		Values []value `json:"values"`
	}
)

// Span flags: the sampled bit, and the bits that say whether the parent is
// remote (0x100 known, 0x200 remote).
const (
	flagsLocal  = 0x01 | 0x100
	flagsRemote = flagsLocal | 0x200
	statusError = 2
)

// encode returns the JSON of one span of an ExportTraceServiceRequest.
func encode(s Span) (jsontext.Value, error) {
	out := span{TraceID: hex.EncodeToString(s.Trace[:]), SpanID: hex.EncodeToString(s.ID[:]), Flags: flagsLocal,
		Name: s.Name, Kind: s.Kind, Start: unixNano(s.Start), End: unixNano(s.End)}
	if s.Parent != [8]byte{} {
		out.ParentSpanID = hex.EncodeToString(s.Parent[:])
	}
	if s.Remote {
		out.Flags = flagsRemote
	}
	if s.Failed {
		out.Status = &status{Message: s.Message, Code: statusError}
	}
	var err error
	if out.Attributes, err = attributes(s.Attrs); err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

func attributes(attrs []Attr) ([]keyValue, error) {
	var out []keyValue // nil without attributes, which the encoding omits
	for _, a := range attrs {
		var v value
		switch x := a.Value.(type) {
		case string:
			v.String = &x
		case bool:
			v.Bool = &x
		case int64:
			v.Int = &x
		case float64:
			// The proto3 JSON mapping's names for what a JSON number cannot hold.
			switch {
			case math.IsNaN(x):
				v.Double = "NaN"
			case math.IsInf(x, 1):
				v.Double = "Infinity"
			case math.IsInf(x, -1):
				v.Double = "-Infinity"
			default:
				v.Double = x
			}
		case []string:
			v.Array = &array{Values: make([]value, len(x))}
			for i := range x {
				v.Array.Values[i].String = &x[i]
			}
		default:
			return nil, fmt.Errorf("attribute %s has a value of type %T", a.Key, a.Value)
		}
		out = append(out, keyValue{a.Key, v})
	}
	return out, nil
}

// unixNano is t in nanoseconds since the Unix epoch; 0 before it.
func unixNano(t time.Time) uint64 {
	return uint64(max(t.UnixNano(), 0))
}

// body returns the ExportTraceServiceRequest of spans, already encoded, under
// one resource and one scope: the proxy and its version.
func body(version string, spans []jsontext.Value) ([]byte, error) {
	name, ver := "stargaite", version
	return json.Marshal(request{ResourceSpans: []resourceSpans{{
		Resource:   resource{Attributes: []keyValue{{"service.name", value{String: &name}}, {"service.version", value{String: &ver}}}},
		ScopeSpans: []scopeSpans{{Scope: scope{Name: name, Version: version}, Spans: spans}},
	}}})
}
