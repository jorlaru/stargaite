// Package plain is the console's front without a terminal: it never asks.
// A question takes its stated default or fails with exit 2 naming its flag
// (OPS-11), and every line goes to standard output as it is said.
package plain

import (
	"context"
	"io"

	"github.com/jorlaru/stargaite/internal/console/ui"
)

// Front writes to standard output and answers from defaults.
type Front struct{ out io.Writer }

// New returns the plain front writing to stdout.
func New(stdout io.Writer) *Front { return &Front{out: stdout} }

// Ask answers a question without asking. A reveal writes its value alone on
// a line, so a script can capture it; a work question waits for its work to
// end; any other question takes its default, or is a usage error naming the
// flag that answers it, and its prompt.
func (f *Front) Ask(ctx context.Context, q ui.Question) (string, error) {
	switch {
	case q.Kind == ui.KindReveal:
		_, err := io.WriteString(f.out, q.Value+"\n")
		return "", err
	case q.Kind == ui.KindWork:
		<-ctx.Done()
		return "", ctx.Err()
	case q.HasDefault:
		return q.Default, nil
	case q.Prompt != "":
		return "", &ui.UsageError{Msg: "missing --" + q.Flag + ": " + q.Prompt}
	}
	return "", &ui.UsageError{Msg: "missing --" + q.Flag}
}

// Show writes text, already cleaned, as a line.
func (f *Front) Show(_ context.Context, text string) error {
	_, err := io.WriteString(f.out, text+"\n")
	return err
}
