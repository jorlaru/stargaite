// Package proxy is how the console reads and changes the proxy: one CLI
// command per call, always with --json, decoded into a typed reply. The
// console never talks to the admin API itself and never imports the proxy's
// packages (OPS-11: it changes the configuration only through the CLI). An
// error names the command and, at most, the CLI's own error message: the
// raw output is never quoted, since it may hold a secret.
package proxy

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jorlaru/stargaite/internal/console/paid"
	"github.com/jorlaru/stargaite/internal/console/ui"
)

const (
	// cliBound bounds one CLI call, as phase 6 bounds its own (90 s).
	cliBound = 90 * time.Second
	// captureLimit caps the answer of one call.
	captureLimit = 8 << 20
	// backupBound bounds a backup, the copy of the whole store.
	backupBound = 10 * time.Minute
	// stopGrace is how long a stopped check has, once its standard input is
	// closed, before its command is cancelled.
	stopGrace = 10 * time.Second
	// stale ends every error that says the answer was not understood.
	stale = "this console and the running proxy are different versions; Rebuild to this clone's version (./setup.sh upgrade --rebuild) mends it"
)

// Runner runs the CLI with args and returns its exit code. A nil stdin is
// none. In production it is host.CLI; in tests the CLI in process.
type Runner func(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error)

// Reply is a decoded answer. Required names the top-level members the
// answer must have, so a proxy of another version is caught by name.
type Reply interface{ Required() []string }

// Proxy calls the CLI through a Runner.
type Proxy struct {
	run         Runner
	grace       time.Duration // stopGrace; tests shorten the fields, never the constants
	backupBound time.Duration // backupBound
}

// New returns a Proxy that calls the CLI through run.
func New(run Runner) *Proxy { return &Proxy{run: run, grace: stopGrace, backupBound: backupBound} }

// Call runs "stargaite <words> --json <flags> [-- <positionals>]" and decodes
// its answer into reply. It refuses targets verify (isVerify).
func (p *Proxy) Call(ctx context.Context, reply Reply, words string, flags []string, positionals ...string) error {
	if isVerify(words) {
		return errors.New("a paid command goes through Verify")
	}
	out, code, err := p.call(ctx, words, flags, nil, positionals...)
	switch {
	case err != nil:
		return fmt.Errorf("stargaite %s: %w", words, err)
	case code != 0:
		return failure(words, out, code)
	}
	return decode(words, out, reply)
}

// CallRefusable is Call, but a refusal (exit 1, an error envelope holding
// findings: nothing was published) returns those findings and no error, and
// leaves reply untouched. An accepted change returns no findings.
func (p *Proxy) CallRefusable(ctx context.Context, reply Reply, words string, flags []string, positionals ...string) ([]Finding, error) {
	if isVerify(words) {
		return nil, errors.New("a paid command goes through Verify")
	}
	out, code, err := p.call(ctx, words, flags, nil, positionals...)
	if err != nil {
		return nil, fmt.Errorf("stargaite %s: %w", words, err)
	}
	return settle(words, out, code, reply)
}

// NewSecret runs keys create or keys rotate (words) for key name and returns
// its new secret, as given, and the findings. Once the command may have run,
// an error that is not the admin API's own refusal says the secret may be
// lost (design section 5), never quoting the answer.
func (p *Proxy) NewSecret(ctx context.Context, words string, flags []string, name string) (string, []Finding, error) {
	const lost = "key %s may have been made (or rotated); if so its secret is lost: Change a key rotates it"
	out, code, err := p.call(ctx, words, flags, nil, name)
	switch {
	case err != nil:
		return "", nil, fmt.Errorf("stargaite %s: %w: "+lost, words, err, name)
	case code != 0 && answered(out):
		return "", nil, failure(words, out, code)
	case code != 0:
		return "", nil, fmt.Errorf("%w: "+lost, failure(words, out, code), name)
	}
	var reply KeyChange
	if decode(words, out, &reply) != nil || reply.Secrets[name] == "" {
		return "", nil, fmt.Errorf("stargaite %s: the answer was not understood, so the new secret of key %s is lost: Change a key rotates it (%s)", words, name, stale)
	}
	return reply.Secrets[name], reply.Findings, nil
}

// answered reports whether out is the admin API's own error answer, which
// changed nothing: an error envelope with a code, other than the CLI's
// admin_unreachable (internal/cli/client.go), which it gives when no answer
// was read.
func answered(out []byte) bool {
	var envelope struct {
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	return json.Unmarshal(out, &envelope) == nil && envelope.Error != nil && envelope.Error.Code != "" && envelope.Error.Code != "admin_unreachable"
}

// isVerify reports whether words run targets verify, the paid command, which
// only Verify runs: the CLI matches a command by its leading words, so any
// words that start with those two.
func isVerify(words string) bool {
	f := strings.Fields(words)
	return len(f) >= 2 && f[0] == "targets" && f[1] == "verify"
}

// DryRun asks what applying targets, new chat targets, and alias's whole
// declaration would find, and publishes nothing: "apply --dry-run -f -"
// with the document on standard input. A refusal is no error: its findings,
// every finding of the document it would lead to, are the answer.
func (p *Proxy) DryRun(ctx context.Context, targets []NewTarget, alias string, decl AliasDecl) ([]Finding, error) {
	const words = "apply"
	doc, err := dryRunDoc(targets, alias, decl)
	if err != nil {
		return nil, fmt.Errorf("stargaite %s: %w", words, err)
	}
	out, code, err := p.call(ctx, words, []string{"--dry-run", "-f", "-"}, bytes.NewReader(doc))
	if err != nil {
		return nil, fmt.Errorf("stargaite %s: %w", words, err)
	}
	var reply Change
	if f, err := settle(words, out, code, &reply); f != nil || err != nil {
		return f, err
	}
	return reply.Findings, nil
}

// settle reads an answer that may be a refusal: at exit 1, an error envelope
// holding findings gives them; anything else follows Call's rule.
func settle(words string, out []byte, code int, reply Reply) ([]Finding, error) {
	if f := refusal(out); code == 1 && len(f) > 0 {
		return f, nil
	}
	if code != 0 {
		return nil, failure(words, out, code)
	}
	return nil, decode(words, out, reply)
}

// dryRunDoc is the document DryRun sends: version 1 (the only one apply
// takes), the new chat targets, and the alias's declaration without its
// name. Settings are not in it, and apply without --prune keeps them.
func dryRunDoc(targets []NewTarget, alias string, decl AliasDecl) ([]byte, error) {
	type target struct {
		Kind    string   `json:"kind"`
		Pricing *Pricing `json:"pricing,omitzero"`
	}
	model := maps.Clone(decl)
	delete(model, "name")
	doc := struct {
		Version int                  `json:"version"`
		Targets map[string]target    `json:"targets,omitempty"`
		Models  map[string]AliasDecl `json:"models"`
	}{Version: 1, Targets: map[string]target{}, Models: map[string]AliasDecl{alias: model}}
	for _, t := range targets {
		doc.Targets[t.ID] = target{Kind: "chat", Pricing: t.Pricing}
	}
	return json.Marshal(doc)
}

// refusal is the findings of a refused change's error envelope, or none.
func refusal(out []byte) []Finding {
	var envelope struct {
		Error *struct {
			Findings []Finding `json:"findings"`
		} `json:"error"`
	}
	if json.Unmarshal(out, &envelope) != nil || envelope.Error == nil {
		return nil
	}
	return envelope.Error.Findings
}

// call runs "stargaite <words> --json <flags> [-- <positionals>]" with
// stdin (nil for none), bounded by cliBound, and returns its answer and
// exit code.
func (p *Proxy) call(ctx context.Context, words string, flags []string, stdin io.Reader, positionals ...string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, cliBound)
	defer cancel()
	args := append(strings.Fields(words), "--json")
	args = append(args, flags...)
	if len(positionals) > 0 {
		args = append(append(args, "--"), positionals...)
	}
	return p.exec(ctx, args, stdin)
}

// Verify runs one paid check of target, which states calls calls, after
// taking them from b. Its standard input is a pipe: when ctx ends or bound
// passes, the pipe is closed first, so the CLI ends its request and the
// server starts no further call; the command is cancelled only if it has not
// returned grace later. A ctx that has already ended starts nothing and
// takes nothing.
func (p *Proxy) Verify(ctx context.Context, b *paid.Budget, target string, calls int, bound time.Duration) (*VerifyReply, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("the check of %s was not started: %w", target, err)
	}
	if err := b.Take(calls); err != nil {
		return nil, err
	}
	const words = "targets verify"
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("stargaite %s: %w", words, err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	run, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	returned, stopped := make(chan struct{}), make(chan error, 1)
	ui.Go(func() { stopped <- p.watch(ctx, target, bound, w, returned, cancel) },
		func(v any) { cancel(); stopped <- fmt.Errorf("the check of %s lost its watch: %v", target, v) })
	args := []string{"targets", "verify", "--json", "--max-output-tokens", "256", "--until-stdin-closes", "--", target}
	out, code, err := p.exec(run, args, r)
	close(returned)
	if why := <-stopped; why != nil {
		return nil, why
	}
	if err != nil {
		return nil, fmt.Errorf("stargaite %s: %w", words, err)
	}
	return verdict(words, target, calls, out, code)
}

// watch waits for the run to return, ctx to end or bound to pass. On either
// of the last two it closes w, the run's standard input, gives the run grace
// to return, then cancels it, and says why the check was stopped.
func (p *Proxy) watch(ctx context.Context, target string, bound time.Duration, w *os.File, returned <-chan struct{}, cancel context.CancelFunc) error {
	expiry := time.NewTimer(bound)
	defer expiry.Stop()
	var why error
	select {
	case <-returned:
		return nil
	case <-ctx.Done():
		why = stopped{"the check of " + target + " was stopped: the call in flight may still be billed", ctx.Err()}
	case <-expiry.C:
		why = fmt.Errorf("the check of %s did not end within %s: the call in flight may still be billed", target, bound)
	}
	_ = w.Close()
	grace := time.NewTimer(p.grace)
	defer grace.Stop()
	select {
	case <-returned:
	case <-grace.C:
		cancel()
	}
	return why
}

// stopped is a check stopped by its context: its words for the reader, its
// cause for errors.Is, without Go's "context canceled" in the text.
type stopped struct {
	msg   string
	cause error
}

func (s stopped) Error() string { return s.msg }
func (s stopped) Unwrap() error { return s.cause }

// TelemetryTest runs telemetry test, which sends one span to the trace
// backend (no provider is called), and returns how far it got: an answer of
// exit 0 or 1 is the reply, whatever its outcome.
func (p *Proxy) TelemetryTest(ctx context.Context) (*TelemetryTestReply, error) {
	const words = "telemetry test"
	out, code, err := p.call(ctx, words, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("stargaite %s: %w", words, err)
	}
	var reply TelemetryTestReply
	if err := decodeAnswer(words, out, code, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// Help is the CLI's --help, its standard output whole, bounded as Call.
func (p *Proxy) Help(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, cliBound)
	defer cancel()
	out, code, err := p.exec(ctx, []string{"--help"}, nil)
	switch {
	case err != nil:
		return "", fmt.Errorf("stargaite --help: %w", err)
	case code != 0:
		return "", fmt.Errorf("stargaite --help: failed with exit %d", code)
	}
	return string(out), nil
}

// Validate runs validate, which exits 1 when a finding is an error and
// still reports every finding: an answer of exit 0 or 1 that decodes is the
// findings.
func (p *Proxy) Validate(ctx context.Context) ([]Finding, error) {
	const words = "validate"
	out, code, err := p.call(ctx, words, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("stargaite %s: %w", words, err)
	}
	var reply Change
	if err := decodeAnswer(words, out, code, &reply); err != nil {
		return nil, err
	}
	return reply.Findings, nil
}

// ValidateExit is validate's exit code, its answer unread, as setup.sh's
// upgrade compared it, so an answer this console cannot decode still gives
// a verdict; the error is only that the CLI did not run.
func (p *Proxy) ValidateExit(ctx context.Context) (int, error) {
	_, code, err := p.call(ctx, "validate", nil, nil)
	if err != nil {
		return 0, fmt.Errorf("stargaite validate: %w", err)
	}
	return code, nil
}

// Backup copies the store to w through "backup - --json" (phase 6's argv:
// the copy on standard output, never a terminal, OPS-3), within
// backupBound, and returns its size from the summary the CLI writes to
// standard error. The error is the bare reason, for the caller's own
// sentence: the CLI's error message, cleaned; "exit N"; or "no summary".
func (p *Proxy) Backup(ctx context.Context, w io.Writer) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, p.backupBound)
	defer cancel()
	var errOut capped
	code, err := p.run(ctx, []string{"backup", "-", "--json"}, nil, w, &errOut)
	switch {
	case err != nil:
		return 0, err
	case ctx.Err() != nil:
		return 0, ctx.Err()
	}
	lines := bytes.Split(bytes.TrimSpace(errOut.buf.Bytes()), []byte("\n"))
	var last struct {
		Bytes *int64 `json:"bytes"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(lines[len(lines)-1], &last)
	switch {
	case code != 0 && last.Error != nil && last.Error.Message != "":
		return 0, errors.New(ui.OneLine(last.Error.Message))
	case code != 0:
		return 0, fmt.Errorf("exit %d", code)
	case last.Bytes == nil:
		return 0, errors.New("no summary")
	}
	return *last.Bytes, nil
}

// decodeAnswer decodes the answer of a command that exits 1 on an outcome it
// still reports, as targets verify and telemetry test do: an answer of exit 0
// or 1 that decodes is reply; anything else follows Call's rule.
func decodeAnswer(words string, out []byte, code int, reply Reply) error {
	if code > 1 {
		return failure(words, out, code)
	}
	if err := decode(words, out, reply); err != nil {
		if code != 0 {
			return failure(words, out, code)
		}
		return err
	}
	return nil
}

// verdict reads a check's answer. An answer of exit 0 or 1 is a VerifyReply;
// more calls than stated, or a failed check, is an error; anything else
// follows Call's rule.
func verdict(words, target string, calls int, out []byte, code int) (*VerifyReply, error) {
	var reply VerifyReply
	if err := decodeAnswer(words, out, code, &reply); err != nil {
		return nil, err
	}
	if len(reply.Calls) > calls {
		return nil, fmt.Errorf("stargaite %s %s: the answer has %d calls, %d were stated: stop and report this", words, target, len(reply.Calls), calls)
	}
	if !reply.OK {
		var lines []string
		for _, c := range reply.Calls {
			lines = append(lines, c.line())
		}
		return nil, errors.New(strings.Join(append(lines, "target "+target+" failed its check"), "\n"))
	}
	return &reply, nil
}

// line is one call of a failed check: its level, status and reason.
func (c VerifyCall) line() string {
	effort, reason := cmp.Or(c.Effort, "call"), c.ProviderMessage
	if reason == "" && c.Error != nil {
		reason = c.Error.Message
	}
	return ui.OneLine(effort) + ": status " + strconv.Itoa(c.Status) + ", " + ui.OneLine(cmp.Or(reason, "no answer"))
}

// exec runs the CLI with args and stdin, and returns its answer and exit
// code. A runner error, the context ending or an answer over 8 MiB is an
// error, whatever the exit code.
func (p *Proxy) exec(ctx context.Context, args []string, stdin io.Reader) ([]byte, int, error) {
	var out capped
	code, err := p.run(ctx, args, stdin, &out, io.Discard)
	switch {
	case err != nil:
		return nil, 0, err
	case ctx.Err() != nil:
		return nil, 0, ctx.Err()
	case out.over:
		return nil, 0, errors.New("the answer is larger than 8 MiB")
	}
	return out.buf.Bytes(), code, nil
}

// failure is the error of a non-zero exit: the CLI's own error message,
// cleaned, never the raw output.
func failure(words string, out []byte, code int) error {
	var envelope struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(out, &envelope) == nil && envelope.Error != nil && envelope.Error.Message != "" {
		return fmt.Errorf("stargaite %s: %s", words, ui.OneLine(envelope.Error.Message))
	}
	return fmt.Errorf("stargaite %s: failed with exit %d", words, code)
}

// decode decodes an answer into reply, which must have every member reply
// requires. The decoder's errors are dropped, not wrapped: they can quote
// the output.
func decode(words string, out []byte, reply Reply) error {
	var members map[string]jsontext.Value
	if json.Unmarshal(out, &members) != nil {
		return fmt.Errorf("stargaite %s: the answer was not understood: %s", words, stale)
	}
	for _, key := range reply.Required() {
		if _, ok := members[key]; !ok {
			return fmt.Errorf("stargaite %s: the answer has no %q: %s", words, key, stale)
		}
	}
	if json.Unmarshal(out, reply) != nil {
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
