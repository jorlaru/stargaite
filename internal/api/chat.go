package api

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptrace"
	"time"

	"github.com/jorlaru/stargaite/internal/adapters/openai"
	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/params"
	"github.com/jorlaru/stargaite/internal/plan"
	"github.com/jorlaru/stargaite/internal/transport"
)

// The PAR-9 headers besides the request id.
const (
	traceHeader   = "X-Stargaite-Trace-Id"
	targetHeader  = "X-Stargaite-Target"
	effortHeader  = "X-Stargaite-Reasoning-Effort"
	changedHeader = "X-Stargaite-Params-Changed"
)

// The causes that end an attempt early: the RES-3 timers, a stream that ends
// before its last event, and a client that went away (RES-5).
var (
	errConnectTimeout   = errors.New("connect timeout")
	errTotalTimeout     = errors.New("total timeout")
	errFirstByteTimeout = errors.New("first byte timeout")
	errIdleTimeout      = errors.New("idle timeout")
	errStreamEnded      = errors.New("the stream ended before its last event")
	errClientGone       = errors.New("client gone")
)

// request is one authenticated inference request (design section 4).
type request struct {
	id, traceID string           // traceID: 16 random bytes in hex until phase 3 reads traceparent
	ep          domain.Endpoint  // chat
	snap        *config.Snapshot // loaded once (RES-7)
	key         *config.Key
	alias       string // the body's "model" string, set before the lookup; "" when the body named none
	model       *config.Model
	body        *params.Body
	client      *plan.Client
	attempts    []*attempt
	err         *domain.Error // what the client got; nil on success
	started     time.Time
}

// attempt is one provider call.
type attempt struct {
	n                         int
	target                    *config.Target
	out                       *plan.Outgoing
	status                    int             // provider status; 0 when no response arrived
	cause                     error           // what ended it early: a timer, client gone, a transport, read or relay failure; nil otherwise
	obs                       domain.Observed // design section 11
	written                   bool            // a byte reached the client: no other answer from here
	started, firstByte, ended time.Time       // firstByte: when the response headers arrived
}

// chat serves POST /v1/chat/completions (design section 4, steps 1 to 8),
// synchronous and streamed, toward openai targets. done runs on every exit
// once the key is authenticated, a panic included. A stream that fails once
// committed has already written its terminal event, so it gets no envelope.
func (h *handler) chat(w http.ResponseWriter, r *http.Request) {
	rq := &request{id: w.Header().Get(domain.RequestIDHeader), ep: domain.EndpointChat, snap: h.Config.Snapshot(), started: time.Now()}
	if rq.key, rq.traceID = h.authenticate(w, r, rq.snap); rq.key == nil {
		return
	}
	defer h.done(rq)
	if rq.err = h.check(w, r, rq); rq.err == nil {
		rq.err = h.route(w, r, rq)
	}
	if rq.err != nil && (len(rq.attempts) == 0 || !rq.attempts[len(rq.attempts)-1].written) {
		fail(w, rq.err)
	}
}

// check is steps 2 and 3: the bounded read of the body (LIM-6), the scan
// (PAR-1), the alias, the key's access to it (SEC-2) and plan.Check.
func (h *handler) check(w http.ResponseWriter, r *http.Request, rq *request) *domain.Error {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, rq.snap.Settings.MaxBodyBytes))
	if tooLarge := new(http.MaxBytesError); errors.As(err, &tooLarge) {
		return domain.NewError(domain.CodeRequestTooLarge, fmt.Sprintf("the body is larger than max_body_bytes, %d", tooLarge.Limit))
	} else if err != nil {
		return domain.NewError(domain.CodeInvalidRequest, "the request body could not be read")
	}
	// The body is read: from here the server's ReadTimeout would only cancel
	// a request that lasts longer than it (design section 10).
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})

	var pe *params.Error
	if rq.body, pe = params.Scan(raw, params.Chat.Walk); pe != nil {
		e := domain.NewError(pe.Code, pe.Message)
		e.Param = pe.Param
		return e
	}
	v, _ := rq.body.Get("model")
	alias, ok := unquote(v)
	if !ok {
		e := domain.NewError(domain.CodeInvalidRequest, "model must be a string naming a model alias")
		e.Param = "model"
		return e
	}
	rq.alias = alias
	if rq.model = rq.snap.Models[alias]; rq.model == nil {
		return domain.NewError(domain.CodeModelNotFound, fmt.Sprintf("model %.200q does not exist", alias))
	}
	if !rq.key.Allows(alias) {
		return domain.NewError(domain.CodeModelNotAllowed, fmt.Sprintf("this key may not use model %.200q", alias))
	}
	var e *domain.Error
	rq.client, e = plan.Check(rq.ep, rq.body, &rq.model.Spec)
	return e
}

// route is steps 5 to 8: per target in order, Build (a static Skip) and the
// dynamic exclusion of a provider whose credential does not resolve, then
// one call. With no target left it answers RES-10: the Skip of the first
// target when every one was skipped statically, else 503
// no_target_available, as for an alias with no published target. Fallback
// and the circuit breaker arrive with step C20, so the first call is the
// last.
func (h *handler) route(w http.ResponseWriter, r *http.Request, rq *request) *domain.Error {
	var static []plan.Skip
	excluded := false
	for i, t := range rq.model.Targets {
		out, skip := plan.Build(plan.Input{Endpoint: rq.ep, Body: rq.body, Model: &rq.model.Spec,
			Upstream: t.Upstream, Target: &t.Spec, Provider: &t.Provider.Spec, Client: rq.client})
		if skip != nil {
			if skip.Code == domain.CodeInternalError {
				h.Log.Error("build failed", "request_id", rq.id, "target", t.ID, "error", skip.Param)
			}
			static = append(static, *skip)
			continue
		}
		if t.Provider.Disabled {
			excluded = true
			continue
		}
		return h.call(w, r, rq, t, out, i+1)
	}
	if excluded || len(static) == 0 {
		return domain.NewError(domain.CodeNoTargetAvailable, fmt.Sprintf("no target of model %.200q is available", rq.alias))
	}
	s := static[0]
	e := domain.NewError(s.Code, fmt.Sprintf("no target of model %.200q can serve this request: %s", rq.alias, s.Code))
	if s.Code != domain.CodeInternalError { // its Param is the proxy's own fault, logged above
		e.Param = s.Param
	}
	return e
}

// call is step 7 for one target: the credential header, the RES-3 timers
// (connect, then total for a synchronous call, or first byte and idle for a
// stream), the write deadline, the call and the answer. A synchronous body is
// read whole up to max_response_bytes (RES-4) and, for a 2xx the adapter can
// read, relayed unchanged; a 2xx stream is relayed as it arrives (relay). The
// PAR-9 headers go out with the status line. It returns the section 15 error
// of the call, nil once the response is relayed.
func (h *handler) call(w http.ResponseWriter, r *http.Request, rq *request, t *config.Target, out *plan.Outgoing, n int) *domain.Error {
	a := &attempt{n: n, target: t, out: out, started: time.Now()}
	defer h.finish(rq, a)
	rq.attempts = append(rq.attempts, a)
	w.Header().Set(targetHeader, t.ID)
	if out.Effort.Sent != nil {
		w.Header().Set(effortHeader, *out.Effort.Sent)
	}
	if c := plan.Header(out.Changes); c != "" {
		w.Header().Set(changedHeader, c)
	}

	tm, limit := rq.snap.Settings.Timeouts, rq.snap.Settings.MaxResponseBytes
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	connect := time.AfterFunc(ms(tm.ConnectMS), func() { cancel(errConnectTimeout) })
	defer connect.Stop()
	// GotConn comes after the dial and the TLS handshake.
	trace := &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { connect.Stop() }}
	var timer, idle *time.Timer
	wait := ms(tm.TotalMS)
	if rq.client.Stream {
		timer = time.AfterFunc(ms(tm.FirstByteMS), func() { cancel(errFirstByteTimeout) })
		// The idle timer runs from the first byte (RES-3), so it bounds the
		// rest of the headers, a 1xx and the read of an error body too.
		idle = time.AfterFunc(ms(tm.StreamIdleMS), func() { cancel(errIdleTimeout) })
		idle.Stop()
		defer idle.Stop()
		trace.GotFirstResponseByte = func() { timer.Stop(); idle.Reset(ms(tm.StreamIdleMS)) }
		wait = ms(tm.FirstByteMS + tm.StreamIdleMS)
	} else {
		timer = time.AfterFunc(wait, func() { cancel(errTotalTimeout) })
	}
	defer timer.Stop()
	// The server's WriteTimeout would otherwise cut an answer that comes later
	// than it (design section 10); a stream moves it again per event.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(wait + 5*time.Second))

	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodPost, out.URL, bytes.NewReader(out.Body))
	var resp *http.Response
	if err == nil {
		// Only the headers Build wrote and the credential reach the provider (PAR-14).
		req.Header = out.Header.Clone()
		t.Provider.Credential.SetHeader(req.Header, "Authorization", "Bearer ")
		client := h.Clients[0]
		if t.Provider.Spec.AllowPrivateNetwork {
			client = h.Clients[1]
		}
		resp, err = client.Do(req)
	}
	if err != nil {
		return failed(ctx, a, err)
	}
	// Closing the body before it ends drops the connection, so the provider
	// stops sending.
	defer func() { _ = resp.Body.Close() }()
	a.status, a.firstByte = resp.StatusCode, time.Now()
	if rq.client.Stream && a.status/100 == 2 {
		return h.relay(ctx, w, rq, a, resp, idle)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err == nil && int64(len(body)) > limit && a.status/100 == 2 {
		err = fmt.Errorf("the response is larger than max_response_bytes, %d", limit)
	}
	if err != nil {
		return failed(ctx, a, err)
	}

	switch s := a.status; {
	case s/100 == 2:
		if a.obs, err = openai.Observe(body); err != nil {
			a.cause = err
			return domain.NewError(domain.CodeUpstreamError, "the provider's answer is not a chat completion")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		a.written = true
		if _, err := w.Write(body); err != nil {
			a.cause = fmt.Errorf("%w: %v", errClientGone, err)
		}
		return nil
	case s == http.StatusUnauthorized || s == http.StatusForbidden:
		// The operator's credential, not the client's: the provider's text
		// goes only to the log, rate-limited (kick-off section 15).
		if n, ok := h.upstream.tick(h.Config.Now()); ok {
			h.Log.Warn("provider refused the credential", "request_id", rq.id, "target", t.ID, "status", s,
				"message", message(t, s, body), "refusals", n)
		}
		return domain.NewError(domain.CodeUpstreamAuthError, "the provider refused the operator's credential")
	case s == http.StatusBadRequest || s == http.StatusNotFound || s == http.StatusRequestEntityTooLarge ||
		s == http.StatusUnprocessableEntity:
		return domain.UpstreamError(s, message(t, s, body), resp.Header.Get("Retry-After"))
	case s == http.StatusTooManyRequests:
		// Status and Retry-After only: the provider's text names the
		// operator's account and limits (kick-off section 15).
		return domain.UpstreamError(s, "the provider answered 429", resp.Header.Get("Retry-After"))
	}
	return domain.UpstreamError(a.status, fmt.Sprintf("the provider answered %d", a.status), "")
}

// ms is a duration in milliseconds, as the settings state them.
func ms(v int64) time.Duration { return time.Duration(v) * time.Millisecond }

// failed is the section 15 error of a call that got no usable answer: a
// RES-3 timer is 504 upstream_timeout; a transport failure or a body cut
// while read is 502 upstream_error. The cause goes to the attempt.
func failed(ctx context.Context, a *attempt, err error) *domain.Error {
	if a.cause = err; ctx.Err() != nil {
		a.cause = context.Cause(ctx)
	}
	for _, timer := range []error{errConnectTimeout, errTotalTimeout, errFirstByteTimeout, errIdleTimeout} {
		if errors.Is(a.cause, timer) {
			return domain.NewError(domain.CodeUpstreamTimeout, "the provider did not answer in time: "+a.cause.Error())
		}
	}
	return domain.NewError(domain.CodeUpstreamError, "the provider call failed")
}

// relay sends a 2xx stream to the client as it arrives (design section 9,
// RES-1, RES-4). Each upstream event resets the idle timer and moves the
// write deadline. Comments before the commit are dropped; the first event
// the adapter emits commits the response: status 200, the SSE headers and
// the PAR-9 headers already set. Every event is written and flushed at once,
// with the credential replaced in it, and nothing is buffered beyond the
// event being read. A failure before the commit is the section 15 error the
// caller answers; after it, the terminal event, written here: an upstream
// error chunk is relayed as it is, being in the client's format, any other
// failure is data: {"error":{...}} with no [DONE] after it. A failed write
// ends the attempt with nothing more to answer: the client is gone.
func (h *handler) relay(ctx context.Context, w http.ResponseWriter, rq *request, a *attempt, resp *http.Response, idle *time.Timer) *domain.Error {
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt != "text/event-stream" {
		a.cause = fmt.Errorf("the answer is %.100q, not an event stream", resp.Header.Get("Content-Type"))
		return domain.NewError(domain.CodeUpstreamError, "the provider's answer is not an event stream")
	}
	rc := http.NewResponseController(w)
	credential := a.target.Provider.Credential
	write := func(e transport.Event) error {
		if !a.written {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			a.written = true
		}
		e.Data = []byte(credential.Redact(string(e.Data)))
		err := transport.WriteEvent(w, e)
		if err == nil {
			err = rc.Flush()
		}
		if err != nil {
			return fmt.Errorf("%w: %v", errClientGone, err)
		}
		return nil
	}
	emit := func(name string, data []byte) error { return write(transport.Event{Name: name, Data: data}) }

	var s openai.Stream
	events := transport.NewEventReader(resp.Body, rq.snap.Settings.MaxResponseBytes)
	silence := ms(rq.snap.Settings.Timeouts.StreamIdleMS)
	var ev transport.Event
	var err error
	for err == nil {
		if ev, err = events.Next(); err == io.EOF {
			err = errStreamEnded
		}
		// A write blocked on a slow client is the write deadline's to bound,
		// not the provider's silence.
		idle.Stop()
		_ = rc.SetWriteDeadline(time.Now().Add(silence + 5*time.Second))
		switch {
		case err != nil:
		case ev.Comment:
			if a.written {
				err = write(ev)
			}
		default:
			err = s.Event(ev.Name, ev.Data, emit)
		}
		idle.Reset(silence)
	}
	a.obs = s.Observed()
	if err == io.EOF { // [DONE] was relayed
		return nil
	}

	if a.cause = err; ctx.Err() != nil {
		a.cause = context.Cause(ctx)
	}
	var e *domain.Error
	var chunk *openai.ErrorChunk
	switch {
	case errors.Is(a.cause, errClientGone) || errors.Is(a.cause, context.Canceled):
		a.cause = errClientGone
		return nil
	case errors.As(err, &chunk):
		a.cause = errors.New(credential.Redact(chunk.Error()))
		e = domain.NewError(domain.CodeUpstreamError, "the provider's stream reported an error")
		if a.written {
			_ = emit("", ev.Data)
		}
		return e
	case errors.Is(a.cause, errIdleTimeout):
		e = domain.NewError(domain.CodeUpstreamTimeout, "the provider did not answer in time: "+a.cause.Error())
	default:
		e = domain.NewError(domain.CodeUpstreamError, "the provider's stream failed")
	}
	if a.written {
		data, _ := config.Encode(struct {
			Error *domain.Error `json:"error"`
		}{e})
		_ = write(transport.Event{Data: data})
	}
	return e
}

// message is the error.message of a provider's error body, without the
// credential; else a fixed text with the status.
func message(t *config.Target, status int, body []byte) string {
	if b, e := params.Scan(body, [][]string{{"error"}}); e == nil {
		v, _ := b.Get("error", "message")
		if s, ok := unquote(v); ok && s != "" {
			return t.Provider.Credential.Redact(s)
		}
	}
	return fmt.Sprintf("the provider answered %d", status)
}

// finish ends one provider call (design section 11, point 2) after its last
// byte, its failure or a panic: one log line. Phase 3 adds the usage record,
// the counters and the attempt span here.
func (h *handler) finish(rq *request, a *attempt) {
	a.ended = time.Now()
	attrs := []any{"request_id", rq.id, "trace_id", rq.traceID, "key", rq.key.Name, "alias", rq.alias, "attempt", a.n,
		"target", a.target.ID, "status", a.status, "duration_ms", a.ended.Sub(a.started).Milliseconds()}
	if a.cause != nil {
		attrs = append(attrs, "cause", a.cause.Error())
	}
	if !a.firstByte.IsZero() {
		attrs = append(attrs, "first_byte_ms", a.firstByte.Sub(a.started).Milliseconds())
	}
	// What was sent, as Build read it back from the body (AC-09, AC-18).
	if c := plan.Header(a.out.Changes); c != "" {
		attrs = append(attrs, "params_changed", c)
	}
	if a.out.Effort.Sent != nil {
		attrs = append(attrs, "effort_sent", *a.out.Effort.Sent)
	}
	if a.out.MaxOutput.Sent != nil {
		attrs = append(attrs, "max_output_sent", *a.out.MaxOutput.Sent)
	}
	attrs = append(attrs, "response_model", a.obs.ResponseModel, "finish_reason", a.obs.FinishReason)
	if a.obs.Usage != nil {
		attrs = append(attrs, "usage", a.obs.Usage)
	}
	h.Log.Info("attempt", append(attrs, "visible_bytes", a.obs.VisibleBytes, "tool_calls", a.obs.ToolCalls, "stripped", a.obs.Stripped)...)
}

// done ends one authenticated request (design section 11, point 3) after
// its last finish: the request line (OPS-9). Phase 3 releases the admission
// slots and ends the request span here.
func (h *handler) done(rq *request) {
	attrs := []any{"request_id", rq.id, "trace_id", rq.traceID, "key", rq.key.Name}
	if rq.alias != "" {
		attrs = append(attrs, "alias", fmt.Sprintf("%.200s", rq.alias)) // an unknown alias is as long as the body allows
	}
	attrs = append(attrs, "attempts", len(rq.attempts))
	if rq.err != nil {
		attrs = append(attrs, "status", rq.err.Status, "code", rq.err.Code)
	} else {
		attrs = append(attrs, "status", http.StatusOK)
	}
	h.Log.Info("request", append(attrs, "duration_ms", time.Since(rq.started).Milliseconds())...)
}

// unquote is the JSON string v, unescaped; ok is false for any other value.
func unquote(v []byte) (string, bool) {
	if len(v) == 0 || v[0] != '"' {
		return "", false
	}
	s, err := jsontext.AppendUnquote(nil, v)
	return string(s), err == nil
}
