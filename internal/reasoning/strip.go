package reasoning

import "strings"

// The think tags of RSN-12, matched on decoded text.
const (
	thinkOpen  = "<think>"
	thinkClose = "</think>"
)

// Stripper removes every <think>...</think> block from one text fed in
// pieces (RSN-12, design section 6): the content of one choice, or one text
// block. Text is decoded, so a JSON-escaped spelling of a tag matches. An
// unclosed block is removed to the end of the text. It holds back at most 7
// bytes, a partial tag at the end of a piece, and releases them with the next
// piece or by Flush. The zero Stripper is ready; it is not safe for
// concurrent use.
type Stripper struct {
	Stripped bool   // a block was removed
	inside   bool   // within a block
	held     string // the end of the last piece that could start the next tag
}

// Feed returns the visible part of the next piece of text.
func (s *Stripper) Feed(text string) string {
	text, s.held = s.held+text, ""
	var out strings.Builder
	for {
		tag := s.tag()
		i := strings.Index(text, tag)
		if i < 0 {
			break
		}
		if !s.inside {
			out.WriteString(text[:i])
		}
		s.inside, s.Stripped = !s.inside, true
		text = text[i+len(tag):]
	}
	tag := s.tag()
	n := min(len(tag)-1, len(text))
	for n > 0 && !strings.HasSuffix(text, tag[:n]) {
		n--
	}
	if s.held = text[len(text)-n:]; !s.inside {
		out.WriteString(text[:len(text)-n])
	}
	return out.String()
}

// Flush ends the text: it returns the bytes held outside a block, which no
// tag followed; inside an unclosed block they are removed with it.
func (s *Stripper) Flush() string {
	held := s.held
	if s.held = ""; s.inside {
		return ""
	}
	return held
}

// tag is the tag that changes the state next.
func (s *Stripper) tag() string {
	if s.inside {
		return thinkClose
	}
	return thinkOpen
}
