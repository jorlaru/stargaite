// Package proxy is how the console reads and changes the proxy: one CLI
// command per call, always with --json, decoded into a typed reply. The
// console never talks to the admin API itself and never imports the proxy's
// packages (OPS-11: it changes the configuration only through the CLI). An
// error names the command and, at most, the CLI's own error message: the
// raw output is never quoted, since it may hold a secret.
package proxy

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jorlaru/stargaite/internal/console/ui"
)

const (
	// cliBound bounds one CLI call, as phase 6 bounds its own (90 s).
	cliBound = 90 * time.Second
	// captureLimit caps the answer of one call.
	captureLimit = 8 << 20
)

// Runner runs the CLI with args and returns its exit code. In production it
// is host.CLI; in tests the CLI in process.
type Runner func(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error)

// Reply is a decoded answer. Required names the top-level members the
// answer must have, so a proxy of another version is caught by name.
type Reply interface{ Required() []string }

// Proxy calls the CLI through a Runner.
type Proxy struct{ run Runner }

// New returns a Proxy that calls the CLI through run.
func New(run Runner) *Proxy { return &Proxy{run: run} }

// Call runs "stargaite <words> --json <flags> [-- <positionals>]" and decodes
// its answer into reply.
func (p *Proxy) Call(ctx context.Context, reply Reply, words string, flags []string, positionals ...string) error {
	ctx, cancel := context.WithTimeout(ctx, cliBound)
	defer cancel()
	args := append(strings.Fields(words), "--json")
	args = append(args, flags...)
	if len(positionals) > 0 {
		args = append(append(args, "--"), positionals...)
	}
	var out capped
	code, err := p.run(ctx, args, &out, io.Discard)
	if err == nil {
		err = ctx.Err()
	}
	switch {
	case err != nil:
		return fmt.Errorf("stargaite %s: %w", words, err)
	case out.over:
		return fmt.Errorf("stargaite %s: the answer is larger than 8 MiB", words)
	case code != 0:
		var envelope struct {
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(out.buf.Bytes(), &envelope) == nil && envelope.Error != nil && envelope.Error.Message != "" {
			return fmt.Errorf("stargaite %s: %s", words, ui.OneLine(envelope.Error.Message))
		}
		return fmt.Errorf("stargaite %s: failed with exit %d", words, code)
	}
	// The decoder's errors are dropped, not wrapped: they can quote the output.
	stale := "this console and the running proxy are different versions; rebuild the proxy from this clone"
	var members map[string]jsontext.Value
	if json.Unmarshal(out.buf.Bytes(), &members) != nil {
		return fmt.Errorf("stargaite %s: the answer was not understood: %s", words, stale)
	}
	for _, key := range reply.Required() {
		if _, ok := members[key]; !ok {
			return fmt.Errorf("stargaite %s: the answer has no %q: %s", words, key, stale)
		}
	}
	if json.Unmarshal(out.buf.Bytes(), reply) != nil {
		return fmt.Errorf("stargaite %s: the answer was not understood: %s", words, stale)
	}
	return nil
}

// capped keeps an answer up to captureLimit and notes an overrun.
type capped struct {
	buf  bytes.Buffer
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	if c.buf.Len()+len(p) > captureLimit {
		c.over = true
		return 0, errors.New("more than 8 MiB")
	}
	return c.buf.Write(p)
}
