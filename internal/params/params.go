// Package params reads and edits request and response bodies in place
// (ADR-002, PAR-1, PAR-8): one pass of the standard JSON tokenizer indexes the
// top-level object and the nested objects a walk names, refuses a repeated
// key in any of them, and records the byte span of every member it indexes,
// so an edit leaves every other byte as the client sent it.
package params

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Error is a body the scanner refuses: not a JSON object, not valid JSON or
// UTF-8, a key repeated in an indexed object, or more than maxValues indexed
// values. Code is always
// invalid_request; Param is the path of the repeated key, such as
// "messages[0].role", and empty for a syntax error (and for a repeated empty
// top-level name).
type Error struct{ Code, Param, Message string }

// maxValues bounds the members and elements Scan indexes in one body, so the
// index of a body stays near the default max_body_bytes in memory however
// dense the body is.
const maxValues = 100_000

// Body is a scanned JSON object: the raw bytes, which must not be modified
// afterwards, and the byte spans of the indexed members.
type Body struct {
	raw  []byte
	root *node
}

// node is one value of the body. Members and elements are listed only for an
// indexed object or array, which is every value a walk path reaches.
type node struct {
	name       string // member name, unescaped; "" for an array element and the root
	key        int    // offset of the member's name; the value's start for an element
	start, end int    // the value's bytes
	obj        bool   // a JSON object
	indexed    bool   // kids lists every member or element, in order
	kids       []*node
}

// trie is a set of walk paths; "*" stands for each element of an array.
type trie map[string]trie

// add inserts path. With both set, a decimal step also inserts "*", so a
// concrete path such as {"choices","0","message"} reaches its object whether
// "0" turns out to be an array index or a member name.
func (t trie) add(path []string, both bool) {
	if len(path) == 0 {
		return
	}
	steps := []string{path[0]}
	if both && isIndex(path[0]) {
		steps = append(steps, "*")
	}
	for _, s := range steps {
		if t[s] == nil {
			t[s] = trie{}
		}
		t[s].add(path[1:], both)
	}
}

func isIndex(s string) bool {
	i, err := strconv.Atoi(s)
	return err == nil && i >= 0 && strconv.Itoa(i) == s
}

// Scan reads raw in one pass. The top-level object and every object or array
// a walk path reaches (its prefixes included) are indexed, and an indexed
// object that repeats a name, compared after unescaping, is refused. Every
// other value is checked for syntax and UTF-8 and skipped, so a repeat inside
// forwarded content is not an error (PAR-1).
func Scan(raw []byte, walk [][]string) (*Body, *Error) {
	t := trie{}
	for _, p := range walk {
		t.add(p, false)
	}
	return scan(raw, t)
}

func scan(raw []byte, t trie) (*Body, *Error) {
	d := jsontext.NewDecoder(bytes.NewReader(raw), jsontext.AllowDuplicateNames(true))
	if d.PeekKind() != '{' {
		return nil, &Error{Code: "invalid_request", Message: "the body must be a JSON object"}
	}
	left := maxValues
	root, e := value(d, t, "", &left)
	if e != nil {
		return nil, e
	}
	if _, err := d.ReadToken(); err != io.EOF {
		if err == nil {
			err = errors.New("data after the top-level object")
		}
		return nil, syntax(err)
	}
	return &Body{raw: raw, root: root}, nil
}

func syntax(err error) *Error {
	return &Error{Code: "invalid_request", Message: "the body is not valid JSON: " + strings.TrimPrefix(err.Error(), "jsontext: ")}
}

// value reads the next value. A non-nil t indexes an object or an array, path
// names the value in errors, and left counts the values still indexable.
func value(d *jsontext.Decoder, t trie, path string, left *int) (*node, *Error) {
	if k := d.PeekKind(); t == nil || k != '{' && k != '[' {
		v, err := d.ReadValue()
		if err != nil {
			return nil, syntax(err)
		}
		end := int(d.InputOffset())
		return &node{start: end - len(v), end: end}, nil
	}
	tok, err := d.ReadToken()
	if err != nil {
		return nil, syntax(err)
	}
	n := &node{start: int(d.InputOffset()) - 1, obj: tok.Kind() == '{', indexed: true}
	seen := map[string]bool{}
	for k := d.PeekKind(); k != '}' && k != ']'; k = d.PeekKind() {
		if *left--; *left < 0 {
			return nil, &Error{Code: "invalid_request", Message: fmt.Sprintf("the body has more than %d members or elements in the objects and arrays the proxy reads", maxValues)}
		}
		if !n.obj {
			c, e := value(d, t["*"], path+"["+strconv.Itoa(len(n.kids))+"]", left)
			if e != nil {
				return nil, e
			}
			c.key = c.start
			n.kids = append(n.kids, c)
			continue
		}
		raw, err := d.ReadValue()
		if err != nil {
			return nil, syntax(err)
		}
		key := int(d.InputOffset()) - len(raw)
		name, err := jsontext.AppendUnquote(nil, raw)
		if err != nil {
			return nil, syntax(err)
		}
		p := string(name)
		if path != "" {
			p = path + "." + p
		}
		if seen[string(name)] {
			return nil, &Error{Code: "invalid_request", Param: p, Message: "repeated key " + strconv.Quote(p)}
		}
		seen[string(name)] = true
		ct := t[string(name)]
		if string(name) == "*" {
			ct = nil // "*" in a walk stands for array elements only
		}
		c, e := value(d, ct, p, left)
		if e != nil {
			return nil, e
		}
		c.name, c.key = string(name), key
		n.kids = append(n.kids, c)
	}
	if _, err := d.ReadToken(); err != nil {
		return nil, syntax(err)
	}
	n.end = int(d.InputOffset())
	return n, nil
}

// child is the member or element of n that step names: a member name in an
// object, a decimal index in an array. Only an indexed value has children.
func (n *node) child(step string) *node {
	if !n.indexed {
		return nil
	}
	if n.obj {
		for _, k := range n.kids {
			if k.name == step {
				return k
			}
		}
		return nil
	}
	if !isIndex(step) {
		return nil
	}
	if i, _ := strconv.Atoi(step); i < len(n.kids) {
		return n.kids[i]
	}
	return nil
}

// Keys returns the top-level names, unescaped, in order of appearance.
func (b *Body) Keys() []string {
	keys := make([]string, len(b.root.kids))
	for i, k := range b.root.kids {
		keys[i] = k.name
	}
	return keys
}

// Get returns the exact bytes of the value at path, where an element is a
// member name or a decimal array index. It fails for a path that is absent or
// that leaves the indexed set, so nothing is read from an object that was not
// checked for repeats. The bytes are the body's own and must not be modified.
func (b *Body) Get(path ...string) (raw []byte, ok bool) {
	n := b.root
	for _, s := range path {
		if n = n.child(s); n == nil {
			return nil, false
		}
	}
	return b.raw[n.start:n.end:n.end], true
}

// Op is one edit: a nil Value deletes the member at Path; otherwise it
// replaces the value, or appends the member when absent, creating missing
// parent objects.
type Op struct {
	Path  []string
	Value []byte
}

// Edit returns a copy of the body with ops applied in order, each to the
// result of the previous one, so an op can write inside an object an earlier
// op wrote. Every byte outside the ops is unchanged: a replace swaps the value
// span, an append goes after the parent's last member, a delete removes the
// member and one separator. Each op locates its path with a scan that refuses
// a repeated key in every object on the path. A Value must be valid JSON
// without repeated names. Deleting an absent member changes nothing.
func (b *Body) Edit(ops ...Op) ([]byte, error) {
	out := bytes.Clone(b.raw)
	for _, op := range ops {
		var err error
		if out, err = edit(out, op); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func edit(raw []byte, op Op) ([]byte, error) {
	p := op.Path
	if len(p) == 0 {
		return nil, errors.New("params: an edit needs a path")
	}
	if op.Value != nil && !jsontext.Value(op.Value).IsValid() {
		return nil, fmt.Errorf("params: the value for %s is not valid JSON", strings.Join(p, "."))
	}
	t := trie{}
	t.add(p[:len(p)-1], true)
	b, e := scan(raw, t)
	if e != nil {
		return nil, fmt.Errorf("params: cannot edit %s: %s", strings.Join(p, "."), e.Message)
	}
	n := b.root
	for i, s := range p {
		c := n.child(s)
		if c == nil {
			if !n.obj {
				return nil, fmt.Errorf("params: cannot edit %s: %s is missing and its parent is not an object", strings.Join(p, "."), strings.Join(p[:i+1], "."))
			}
			if op.Value == nil {
				return raw, nil
			}
			return insert(raw, n, p[i:], op.Value)
		}
		if i < len(p)-1 {
			n = c
			continue
		}
		if op.Value != nil {
			return splice(raw, c.start, c.end, op.Value), nil
		}
		k := 0
		for n.kids[k] != c {
			k++
		}
		switch {
		case k+1 < len(n.kids):
			return splice(raw, c.key, n.kids[k+1].key, nil), nil
		case k > 0:
			return splice(raw, n.kids[k-1].end, c.end, nil), nil
		default:
			return splice(raw, c.key, c.end, nil), nil
		}
	}
	panic("unreachable")
}

// insert appends to object n the member path[0], nesting path[1:] as new
// objects around v.
func insert(raw []byte, n *node, path []string, v []byte) ([]byte, error) {
	for i := len(path) - 1; i >= 0; i-- {
		name, err := jsontext.AppendQuote(nil, path[i])
		if err != nil {
			return nil, fmt.Errorf("params: member name %q: %w", path[i], err)
		}
		member := append(append(name, ':'), v...)
		if i > 0 {
			member = append(append([]byte{'{'}, member...), '}')
		}
		v = member
	}
	at := n.start + 1
	if len(n.kids) > 0 {
		at = n.kids[len(n.kids)-1].end
		v = append([]byte{','}, v...)
	}
	return splice(raw, at, at, v), nil
}

func splice(raw []byte, from, to int, ins []byte) []byte {
	out := make([]byte, 0, len(raw)-(to-from)+len(ins))
	return append(append(append(out, raw[:from]...), ins...), raw[to:]...)
}
