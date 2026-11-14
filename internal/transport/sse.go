package transport

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// ErrEventTooLarge is the error of an event longer than the reader's bound.
var ErrEventTooLarge = errors.New("an event of the stream is larger than max_response_bytes")

// Event is one server-sent event (phase-2 design section 9).
type Event struct {
	Name    string // the event: field; "" when absent (OpenAI)
	Data    []byte // the data: lines joined by "\n"
	Comment bool   // a ":" line outside an event, relayed as a keepalive; Data holds the text after the colon
}

// EventReader reads server-sent events, each bounded in size, without
// holding more than the event being read.
type EventReader struct {
	r   *bufio.Reader
	max int64
}

// NewEventReader reads events from r; max bounds the bytes of one event,
// its lines and their ends included (settings.max_response_bytes).
func NewEventReader(r io.Reader, max int64) *EventReader {
	return &EventReader{r: bufio.NewReader(r), max: max}
}

// Next returns the next event: one with data, or a comment line read
// between events. Lines end in LF or CRLF; the id and retry fields, a field
// of any other name and a comment inside an event are dropped, and so is an
// event without data, as the SSE standard dispatches none. The error is
// io.EOF at a clean end, between events; io.ErrUnexpectedEOF when the stream
// ends inside an event; ErrEventTooLarge; or the read's own error.
func (r *EventReader) Next() (Event, error) {
	var e Event
	var data []byte
	fields, n := false, int64(0) // fields: a field line was read since the last event
	for {
		line, err := r.line(&n)
		if err != nil {
			if errors.Is(err, io.EOF) && (fields || len(line) > 0) {
				err = io.ErrUnexpectedEOF
			}
			return Event{}, err
		}
		if len(line) == 0 {
			if data != nil {
				e.Data = data
				return e, nil
			}
			e, fields, n = Event{}, false, 0
			continue
		}
		name, value, _ := bytes.Cut(line, []byte(":"))
		if len(name) == 0 { // a comment
			if !fields {
				return Event{Data: value, Comment: true}, nil
			}
			continue
		}
		fields = true
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(name) {
		case "data":
			if data == nil {
				data = []byte{}
			} else {
				data = append(data, '\n')
			}
			data = append(data, value...)
		case "event":
			e.Name = string(value)
		}
	}
}

// line reads one line without its end, adding its bytes to n. At the end of
// the stream it returns io.EOF and the bytes of an unterminated line.
func (r *EventReader) line(n *int64) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.r.ReadSlice('\n')
		if *n += int64(len(chunk)); *n > r.max {
			return nil, ErrEventTooLarge
		}
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			return line, err
		}
		line = line[:len(line)-1]
		return bytes.TrimSuffix(line, []byte("\r")), nil
	}
}

// WriteEvent writes e in one call: a comment line, or the event's name and
// one data line per line of its data, then the blank line that ends it. A
// line of data ends at a CRLF, an LF or a bare CR, as a client that follows
// the SSE standard splits it, and clients join data lines with LF: a bare CR
// read inside an event's data (the reader splits only at LF) reaches every
// client as an LF, harmless JSON whitespace, never as a line the relay did
// not check. The caller flushes and moves the write deadline.
func WriteEvent(w io.Writer, e Event) error {
	var b []byte
	if e.Comment {
		// A client that ends lines at a bare CR, as the SSE standard does,
		// would read what follows one as an event never seen here.
		text, _, _ := bytes.Cut(e.Data, []byte("\r"))
		b = append(append([]byte(":"), text...), '\n', '\n')
	} else {
		if e.Name != "" {
			b = append(append([]byte("event: "), e.Name...), '\n')
		}
		data := bytes.ReplaceAll(bytes.ReplaceAll(e.Data, []byte("\r\n"), []byte("\n")), []byte("\r"), []byte("\n"))
		for line := range bytes.SplitSeq(data, []byte("\n")) {
			b = append(append(append(b, "data: "...), line...), '\n')
		}
		b = append(b, '\n')
	}
	_, err := w.Write(b)
	return err
}
