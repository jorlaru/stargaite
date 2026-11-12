// Package config reads, checks and publishes the configuration of an
// instance. Every declaration, whether it comes from a YAML file, an admin
// request or a store row, passes through the one strict decoder of this file.
package config

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
)

// Decode reads the single JSON value in data into v, which must be a pointer
// to a zero value (the decoder merges into what a map or a struct already
// holds). It is strict on purpose, because a typo that is silently dropped
// becomes a limit that is not enforced:
//
//   - a member the Go type does not have is an error, and names match in
//     their exact letter case;
//   - a name repeated inside one object is an error, at any depth and inside a
//     raw fragment too; names are compared after unescaping;
//   - invalid UTF-8 and anything but white space after the value are errors;
//   - a top-level null is an error: the standard decoder would leave v
//     untouched and report success.
//
// An error names the place as a JSON pointer, such as "/models/fast/targets".
func Decode(data []byte, v any) error {
	if string(bytes.TrimSpace(data)) == "null" {
		return errors.New("a value is expected, not null")
	}
	err := json.Unmarshal(data, v, json.RejectUnknownMembers(true))
	// The standard decoder names Go types and, on purpose, words its messages
	// differently from one process to the next. An operator reads this error
	// and tests compare it, so it is spelled here from the error's fields.
	var sem *json.SemanticError
	var syn *jsontext.SyntacticError
	switch {
	case errors.As(err, &sem) && sem.Err == json.ErrUnknownName:
		return fmt.Errorf("unknown member %q", sem.JSONPointer)
	case errors.As(err, &sem) && sem.Err == nil:
		return fmt.Errorf("the value at %q is not valid: wrong type", sem.JSONPointer)
	case errors.As(err, &sem):
		return fmt.Errorf("the value at %q is not valid: %v", sem.JSONPointer, sem.Err)
	case errors.As(err, &syn) && syn.Err == jsontext.ErrDuplicateName:
		return fmt.Errorf("member %q is repeated", syn.JSONPointer)
	case errors.As(err, &syn):
		return fmt.Errorf("%v at %q, byte %d", syn.Err, syn.JSONPointer, syn.ByteOffset)
	}
	return err
}

// Encode writes v in the canonical encoding: struct members in declaration
// order, map entries sorted by name, no insignificant white space. A raw
// fragment is compacted and its members sorted as well, so two spellings of
// one declaration encode to the same bytes; every number keeps its digits.
func Encode(v any) ([]byte, error) {
	return json.Marshal(v, json.Deterministic(true), jsontext.ReorderRawObjects(true))
}
