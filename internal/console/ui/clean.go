package ui

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Caps on what a front prints of a text it did not write.
const (
	oneLineMax   = 300   // runes in a line, the last one "…" when cut
	multiLineMax = 65536 // bytes in a text, "\n…" appended when cut
)

// OneLine makes untrusted text safe to print on one line: tabs and newlines
// become spaces, and terminal controls, direction overrides, invisible
// characters and invalid UTF-8 are dropped, so a proxy string cannot move
// the cursor, write the clipboard or disguise itself.
func OneLine(s string) string {
	s = clean(s, false)
	if utf8.RuneCountInString(s) <= oneLineMax {
		return s
	}
	runes := []rune(s)
	return string(runes[:oneLineMax-1]) + "…"
}

// MultiLine is OneLine for a text of several lines: it keeps newlines.
func MultiLine(s string) string {
	s = clean(s, true)
	if len(s) <= multiLineMax {
		return s
	}
	cut := multiLineMax
	for !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n…"
}

// clean drops what stripped names and invalid bytes; a tab, and a newline
// unless keepNewline, become a space.
func clean(s string, keepNewline bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		switch {
		case r == utf8.RuneError && size == 1:
		case r == '\n' && keepNewline:
			b.WriteByte('\n')
		case r == '\n' || r == '\t':
			b.WriteByte(' ')
		case stripped(r):
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// stripped reports the runes no front prints: C0 controls, DEL, C1 controls,
// the line and paragraph separators, and every format character (Unicode
// category Cf: the bidirectional, zero-width and tag characters among them).
func stripped(r rune) bool {
	return r < 0x20 || (r >= 0x7f && r <= 0x9f) || r == 0x2028 || r == 0x2029 || unicode.Is(unicode.Cf, r)
}
