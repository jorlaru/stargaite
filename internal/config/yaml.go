package config

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"strings"

	"go.yaml.in/yaml/v3"
)

const (
	// MaxYAMLBytes is the largest YAML document read. It is the body cap of
	// the apply route, so a file that validates offline is one apply can send.
	MaxYAMLBytes = 16 << 20
	// maxYAMLDepth is far above what a declaration needs: its deepest value
	// is a reasoning fragment, a few levels below level five.
	maxYAMLDepth = 32
)

// DecodeYAML reads a declaration written in YAML into v, through the strict
// decoder. Only the plain subset is accepted: one document of mappings with
// string keys, lists and scalars. An anchor, an alias, a merge key, a repeated
// key, a key that is not a string, a tagged collection and a second document
// are errors that name the line.
//
// A scalar is never converted by YAML's rules. It becomes a number, a boolean
// or null only when its text is that JSON literal; everything else is text. So
// a date stays a string, and 0x10, 1_000, .inf, True and ~ are strings that
// the strict decoder refuses where a number or a boolean is declared. An empty
// value is null, which reads as an absent member.
func DecodeYAML(data []byte, v any) error {
	doc, err := yamlToJSON(data)
	if err != nil {
		return err
	}
	return Decode(doc, v)
}

func yamlToJSON(data []byte) ([]byte, error) {
	if len(data) > MaxYAMLBytes {
		return nil, fmt.Errorf("yaml: the document is larger than %d MiB", MaxYAMLBytes>>20)
	}
	// The tree is read without resolving anything: the library's own guards
	// against repeated keys and alias bombs only run when it decodes into Go
	// values, so the walk below refuses both itself.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc, second yaml.Node
	switch err := dec.Decode(&doc); {
	case err == io.EOF:
		return nil, errors.New("yaml: the file holds no document")
	case err != nil:
		return nil, err
	}
	switch err := dec.Decode(&second); {
	case err == nil:
		return nil, fmt.Errorf("yaml: line %d: a second document is not allowed", second.Line)
	case err != io.EOF:
		return nil, err
	}
	value, err := fromYAML(doc.Content[0], 1)
	if err != nil {
		return nil, err
	}
	return Encode(value)
}

func refuse(n *yaml.Node, format string, args ...any) error {
	return fmt.Errorf("yaml: line %d: %s", n.Line, fmt.Sprintf(format, args...))
}

// fromYAML turns a node into the value Encode writes as JSON: a map, a list,
// a string, a boolean, nil or the raw text of a number.
func fromYAML(n *yaml.Node, depth int) (any, error) {
	switch {
	case n.Kind == yaml.AliasNode:
		return nil, refuse(n, "aliases are not supported")
	case n.Anchor != "":
		return nil, refuse(n, "anchors are not supported")
	case depth > maxYAMLDepth:
		return nil, refuse(n, "nesting is deeper than %d levels", maxYAMLDepth)
	case n.Kind == yaml.MappingNode && n.Tag != "!!map", n.Kind == yaml.SequenceNode && n.Tag != "!!seq":
		return nil, refuse(n, "tag %.64q is not supported", n.Tag)
	}
	switch n.Kind {
	case yaml.MappingNode:
		m := make(map[string]any, len(n.Content)/2)
		for i := 0; i < len(n.Content); i += 2 {
			k := n.Content[i]
			_, repeated := m[k.Value]
			switch {
			case k.Tag == "!!merge":
				return nil, refuse(k, "merge keys are not supported")
			case k.Kind != yaml.ScalarNode || k.Tag != "!!str":
				return nil, refuse(k, "a key must be a string: write it in quotes")
			case k.Anchor != "":
				return nil, refuse(k, "anchors are not supported")
			case repeated:
				return nil, refuse(k, "key %.64q is repeated", k.Value)
			}
			v, err := fromYAML(n.Content[i+1], depth+1)
			if err != nil {
				return nil, err
			}
			m[k.Value] = v
		}
		return m, nil
	case yaml.SequenceNode:
		list := make([]any, 0, len(n.Content))
		for _, item := range n.Content {
			v, err := fromYAML(item, depth+1)
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
		return list, nil
	}
	switch raw := jsontext.Value(n.Value); n.Tag {
	case "!!null":
		if n.Value == "" || n.Value == "null" {
			return nil, nil
		}
	case "!!bool":
		if n.Value == "true" || n.Value == "false" {
			return n.Value == "true", nil
		}
	case "!!int", "!!float":
		if raw.Kind() == '0' && raw.IsValid() {
			return raw, nil
		}
	}
	return n.Value, nil
}

// EncodeYAML writes v as YAML: the canonical encoding in block style, two
// spaces deep. Every string is tagged as one, so the emitter quotes those that
// would read back as something else (a date, a number, true, null).
func EncodeYAML(v any) ([]byte, error) {
	data, err := Encode(v)
	if err != nil {
		return nil, err
	}
	node, err := toYAML(jsontext.NewDecoder(bytes.NewReader(data)))
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(node); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

var yamlTags = map[jsontext.Kind]string{'"': "!!str", '0': "!!int", 't': "!!bool", 'f': "!!bool", 'n': "!!null"}

// toYAML reads the next JSON value into a node. A number keeps its text; the
// emitter writes its tag out only when YAML would not read that text as a
// number, as with one beyond the range of a float.
func toYAML(dec *jsontext.Decoder) (*yaml.Node, error) {
	tok, err := dec.ReadToken()
	if err != nil {
		return nil, err
	}
	kind := tok.Kind()
	if kind != '{' && kind != '[' {
		n := &yaml.Node{Kind: yaml.ScalarNode, Tag: yamlTags[kind], Value: tok.String()}
		if kind == '0' && strings.ContainsAny(n.Value, ".eE") {
			n.Tag = "!!float"
		}
		return n, nil
	}
	n := &yaml.Node{Kind: yaml.MappingNode}
	if kind == '[' {
		n.Kind = yaml.SequenceNode
	}
	// Names and values alternate inside an object, and a name is a string
	// token, so both take the same path.
	for next := dec.PeekKind(); next != '}' && next != ']'; next = dec.PeekKind() {
		child, err := toYAML(dec)
		if err != nil {
			return nil, err
		}
		n.Content = append(n.Content, child)
	}
	_, err = dec.ReadToken()
	return n, err
}
