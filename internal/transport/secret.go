package transport

import (
	"fmt"
	"io"
	"log/slog"
)

const redacted = "[redacted]"

// Secret holds a credential value. Only this package reads the value, when it
// adds the credential to an outbound request; no exported method returns it.
// Every fmt verb, JSON and slog write "[redacted]" instead.
//
// The value sits behind a pointer. fmt calls no method on an unexported
// struct field, nor on any field while it reports a wrong verb (%s on a
// pointer to a struct that holds a Secret prints that struct with its
// methods off); it then prints the struct's own fields, and a pointer below
// the top level only as an address. So the value is not printed even there.
type Secret struct {
	value *string
}

// NewSecret wraps a credential value. Only config calls it, when a reference
// resolves.
func NewSecret(value string) Secret { return Secret{value: &value} }

// Format writes "[redacted]" for every verb, %#v included.
func (Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// MarshalJSON writes the string "[redacted]".
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// LogValue writes "[redacted]" in a log record.
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }
