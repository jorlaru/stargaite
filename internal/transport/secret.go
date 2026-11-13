package transport

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
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

// SetHeader sets name to prefix plus the value on an outbound request's
// header; the zero Secret, a provider without a credential, sets nothing.
func (s Secret) SetHeader(h http.Header, name, prefix string) {
	if s.value != nil {
		h.Set(name, prefix+*s.value)
	}
}

// Redact replaces the value in text, a provider's own message, with
// "[redacted]": a provider that echoes the credential cannot carry it into a
// log or a response.
func (s Secret) Redact(text string) string {
	if s.value == nil || *s.value == "" {
		return text
	}
	return strings.ReplaceAll(text, *s.value, redacted)
}
