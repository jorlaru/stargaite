package transport

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

// stream holds every shape the reader meets: a comment between events and
// one inside an event, LF and CRLF ends, a field without a space or without
// a colon, multi-line data, an event name, the id and retry fields, a field
// of another name, an event without data and blank lines in a row.
const stream = ": hello\n\n" +
	"data: {\"a\":1}\n\n" +
	"id: 7\r\nevent: delta\r\ndata:two\r\ndata:  lines\r\n\r\n" +
	"data\n\n" +
	"retry: 10\nother: x\n\n\n\n" +
	"data: x\n: inside\ndata: y\n\n" +
	"data: [DONE]\n\n"

var events = []Event{
	{Data: []byte(" hello"), Comment: true},
	{Data: []byte(`{"a":1}`)},
	{Name: "delta", Data: []byte("two\n lines")},
	{Data: []byte{}},
	{Data: []byte("x\ny")},
	{Data: []byte("[DONE]")},
}

func readAll(t *testing.T, r io.Reader, max int64) ([]Event, error) {
	t.Helper()
	er := NewEventReader(r, max)
	var got []Event
	for {
		e, err := er.Next()
		if err != nil {
			return got, err
		}
		got = append(got, e)
	}
}

// TestEventReader replays the stream split at every byte offset and one
// byte at a time: the events are the same each time, and the end is io.EOF.
func TestEventReader(t *testing.T) {
	for i := 0; i <= len(stream); i++ {
		r := io.MultiReader(strings.NewReader(stream[:i]), strings.NewReader(stream[i:]))
		if got, err := readAll(t, iotest.HalfReader(r), 1<<10); err != io.EOF || !reflect.DeepEqual(got, events) {
			t.Fatalf("split at %d: %+v, %v", i, got, err)
		}
	}
	if got, err := readAll(t, iotest.OneByteReader(strings.NewReader(stream)), 1<<10); err != io.EOF || !reflect.DeepEqual(got, events) {
		t.Fatalf("one byte at a time: %+v, %v", got, err)
	}
}

// TestEventReaderEnds: a stream that stops inside an event, a line or an
// event without data is cut; a read error passes through.
func TestEventReaderEnds(t *testing.T) {
	for _, s := range []string{"data: x", "data: x\n", "event: e\n", "data: x\n\ndata: y\n", ": c"} {
		if _, err := readAll(t, strings.NewReader(s), 1<<10); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("%q: %v", s, err)
		}
	}
	for _, s := range []string{"", "\n\n", "data: x\n\n", ": c\n"} {
		if _, err := readAll(t, strings.NewReader(s), 1<<10); err != io.EOF {
			t.Errorf("%q: %v", s, err)
		}
	}
	boom := errors.New("boom")
	if _, err := readAll(t, io.MultiReader(strings.NewReader("data: x\n\ndata: "), iotest.ErrReader(boom)), 1<<10); !errors.Is(err, boom) {
		t.Errorf("read error: %v", err)
	}
}

// TestEventReaderBound: an event is bounded as a whole, its lines and their
// ends included, and a line longer than the reader's buffer is read whole.
func TestEventReaderBound(t *testing.T) {
	long := strings.Repeat("x", 10_000)
	got, err := readAll(t, strings.NewReader("data: "+long+"\n\n"), 10_008)
	if err != io.EOF || len(got) != 1 || string(got[0].Data) != long {
		t.Errorf("an event of exactly the bound: %d events, %v", len(got), err)
	}
	for _, s := range []string{"data: " + long + "\n\n", "data: " + long + long, "data: " + long[:5000] + "\ndata: " + long[:5000] + "\n\n", ": " + long + long + "\n"} {
		if _, err := readAll(t, strings.NewReader(s), 10_007); !errors.Is(err, ErrEventTooLarge) {
			t.Errorf("%d bytes: %v", len(s), err)
		}
	}
	// The bound is per event, not per stream.
	if got, err := readAll(t, strings.NewReader(strings.Repeat("data: "+long[:9000]+"\n\n", 5)), 10_000); err != io.EOF || len(got) != 5 {
		t.Errorf("five events under the bound: %d, %v", len(got), err)
	}
}

// TestWriteEvent: one write per event, read back as the same event.
func TestWriteEvent(t *testing.T) {
	for _, tc := range []struct {
		e    Event
		want string
	}{
		{Event{Data: []byte(`{"a":1}`)}, "data: {\"a\":1}\n\n"},
		{Event{Name: "delta", Data: []byte("two\nlines")}, "event: delta\ndata: two\ndata: lines\n\n"},
		{Event{Data: []byte{}}, "data: \n\n"},
		{Event{Data: []byte(" ping"), Comment: true}, ": ping\n\n"},
	} {
		var b bytes.Buffer
		w := &countWriter{w: &b}
		if err := WriteEvent(w, tc.e); err != nil || b.String() != tc.want || w.n != 1 {
			t.Errorf("%q: %q in %d writes, %v", tc.e.Data, b.String(), w.n, err)
		}
		if got, err := NewEventReader(&b, 100).Next(); err != nil || !reflect.DeepEqual(got, tc.e) {
			t.Errorf("read back %q: %+v, %v", tc.want, got, err)
		}
	}
	boom := errors.New("boom")
	if err := WriteEvent(errWriter{boom}, Event{Data: []byte("x")}); !errors.Is(err, boom) {
		t.Errorf("write error: %v", err)
	}
}

// TestWriteEventCR: a CR, a CRLF or an LF inside the data ends a data line,
// so a client that ends lines at a bare CR, as the SSE standard allows, reads
// the same event as one that ends them at LF: the data with each line end an
// LF. Written as it was read, a bare CR would end a line that client reads
// and the relay never checked.
func TestWriteEventCR(t *testing.T) {
	var b bytes.Buffer
	if err := WriteEvent(&b, Event{Name: "e", Data: []byte("{\r\"a\":\r\n1,\n\r\r\"b\":2}")}); err != nil {
		t.Fatal(err)
	}
	const want = "event: e\ndata: {\ndata: \"a\":\ndata: 1,\ndata: \ndata: \ndata: \"b\":2}\n\n"
	if b.String() != want {
		t.Fatalf("%q, want %q", b.String(), want)
	}
	// The bytes hold no CR, so every SSE client splits them alike.
	got, err := NewEventReader(&b, 100).Next()
	if err != nil || got.Name != "e" || string(got.Data) != "{\n\"a\":\n1,\n\n\n\"b\":2}" {
		t.Errorf("read back: %+v, %v", got, err)
	}
}

type countWriter struct {
	w io.Writer
	n int
}

func (c *countWriter) Write(p []byte) (int, error) { c.n++; return c.w.Write(p) }

type errWriter struct{ err error }

func (e errWriter) Write([]byte) (int, error) { return 0, e.err }
