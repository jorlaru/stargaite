package ui

import (
	"errors"
	"strings"
)

// Storable refuses a secret .env cannot hold as NAME='VALUE', which bash's
// env_set writes and both setup.sh and Compose read: an empty value, a ' or
// a newline, or a trailing backslash. name is what the error names; the
// value is never in it.
func Storable(name, v string) error {
	switch {
	case v == "":
		return errors.New(name + " is empty")
	case strings.ContainsAny(v, "'\n") || strings.HasSuffix(v, `\`):
		return errors.New(name + ` holds ' or a newline, or ends in \: .env cannot hold it, so nothing was written`)
	}
	return nil
}
