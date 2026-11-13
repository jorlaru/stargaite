package reasoning

import (
	"regexp"
	"strings"
	"testing"
)

// thinkBlock is the reference of RSN-12: every block, the shortest, and an
// unclosed one to the end of the text.
var thinkBlock = regexp.MustCompile(`(?s)<think>.*?(?:</think>|\z)`)

// strip feeds pieces to a new Stripper, checks that it never holds more than
// 7 bytes, and returns the visible text with what Flush released.
func strip(t *testing.T, pieces []string) (string, bool) {
	t.Helper()
	var s Stripper
	var out strings.Builder
	for _, p := range pieces {
		out.WriteString(s.Feed(p))
		if len(s.held) > 7 {
			t.Fatalf("%q: holds %q", pieces, s.held)
		}
	}
	out.WriteString(s.Flush())
	return out.String(), s.Stripped
}

// TestStripper is RSN-12 and AC-19 on decoded text: several blocks, an
// unclosed block, partial tags that turn out to be text, and each text split
// at every byte offset and fed one byte at a time, with the same result.
func TestStripper(t *testing.T) {
	for _, tt := range []struct {
		text, want string
		stripped   bool
	}{
		{"", "", false},
		{"plain text", "plain text", false},
		{"<think>plan</think>\n\nanswer", "\n\nanswer", true},
		{"a<think>1</think>b<think>2</think>c<think>3</think>", "abc", true},
		{"<think></think>", "", true},
		{"answer<think>never closed", "answer", true},
		{"answer<think>never closed</thi", "answer", true},
		{"a <thing> b", "a <thing> b", false},
		{"ends with <thi", "ends with <thi", false},
		{"ends with <", "ends with <", false},
		{"a</think>b", "a</think>b", false},
		{"<think>a<think>b</think>c</think>", "c</think>", true},
		{"<<think>x</think>>", "<>", true},
		{"<think>x</think </think>y", "y", true},
		{"é<think>ü</think>ñ", "éñ", true},
		{"<THINK>x</THINK>", "<THINK>x</THINK>", false},
	} {
		splits := [][]string{{tt.text}, strings.Split(tt.text, "")}
		for i := range len(tt.text) + 1 {
			splits = append(splits, []string{tt.text[:i], tt.text[i:]})
			for j := i; j <= len(tt.text); j++ {
				splits = append(splits, []string{tt.text[:i], tt.text[i:j], tt.text[j:]})
			}
		}
		for _, pieces := range splits {
			if got, stripped := strip(t, pieces); got != tt.want || stripped != tt.stripped {
				t.Fatalf("%q: %q, stripped %v; want %q, %v", pieces, got, stripped, tt.want, tt.stripped)
			}
		}
		if got := thinkBlock.ReplaceAllString(tt.text, ""); got != tt.want {
			t.Errorf("the reference gives %q for %q", got, tt.text)
		}
	}
}

// FuzzStripper feeds any text in pieces whose lengths come from cuts: the
// visible text is the reference's, at most 7 bytes are held, and Stripped
// says an opening tag was seen.
func FuzzStripper(f *testing.F) {
	f.Add("a<think>1</think>b<think>2</think>c", []byte{3, 1, 7})
	f.Add("answer<think>never closed</thi", []byte{6, 1, 1, 1, 1, 1, 1, 1})
	f.Add("x<<think>></think></think>y<thin", []byte{0, 2, 8})
	f.Add("é<think>ü</think>ñ", []byte{1})
	f.Fuzz(func(t *testing.T, text string, cuts []byte) {
		var pieces []string
		rest := text
		for _, c := range cuts {
			n := min(int(c)%10, len(rest))
			pieces, rest = append(pieces, rest[:n]), rest[n:]
		}
		pieces = append(pieces, rest)
		got, stripped := strip(t, pieces)
		if want := thinkBlock.ReplaceAllString(text, ""); got != want || stripped != strings.Contains(text, thinkOpen) {
			t.Fatalf("%q: %q, stripped %v; want %q", pieces, got, stripped, want)
		}
	})
}
