package ui

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestOneLine(t *testing.T) {
	long := strings.Repeat("é", 400)
	for _, tc := range []struct{ in, want string }{
		{"id\x1b]52;c;aGk=\x07", "id]52;c;aGk="},
		{"a\x1b[2Jb", "a[2Jb"},
		{"x\u202ey", "xy"},
		{"a\nb\tc", "a b c"},
		{"\u200bz\ufeff", "z"},
		{"\u0085c1", "c1"},
		{"a\xffb", "ab"},
		{"a\r\x7fb\u061c\u2028\u2060\u2066\u2069c", "abc"},
		{"gpt\u20634\u00ad\u206a", "gpt4"}, // other invisible format characters
		{"anthropic/claude-x\U000E0078\U000E0041", "anthropic/claude-x"}, // tag characters
		{strings.Repeat("é", oneLineMax), strings.Repeat("é", oneLineMax)},
		{long, strings.Repeat("é", oneLineMax-1) + "…"},
	} {
		if got := OneLine(tc.in); got != tc.want {
			t.Errorf("OneLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := utf8.RuneCountInString(OneLine(long)); got != 300 {
		t.Fatalf("runes = %d, want 300", got)
	}
}

func TestMultiLine(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"a\nb", "a\nb"},
		{"a\tb", "a b"},
		{"a\x1b[2J\r\n\u202eb\xff", "a[2J\nb"},
		{"\u0085\u200b\ufeffz", "z"},
	} {
		if got := MultiLine(tc.in); got != tc.want {
			t.Errorf("MultiLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// 2-byte runes over an odd cap: the cut falls on a rune boundary.
	in := "x" + strings.Repeat("é", multiLineMax)
	got := MultiLine(in)
	if !strings.HasSuffix(got, "\n…") {
		t.Fatalf("no \\n… at the end of a capped text")
	}
	body := strings.TrimSuffix(got, "\n…")
	if len(body) > multiLineMax || len(body) < multiLineMax-1 || !utf8.ValidString(got) || multiLineMax != 65536 {
		t.Fatalf("capped body is %d bytes, valid %v", len(body), utf8.ValidString(got))
	}
	exact := strings.Repeat("a", multiLineMax)
	if MultiLine(exact) != exact {
		t.Fatal("a text at the cap was cut")
	}
}
