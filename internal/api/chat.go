package api

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptrace"
	"slices"
	"sync"
	"time"

	"github.com/jorlaru/stargaite/internal/adapters/anthropic"
	"github.com/jorlaru/stargaite/internal/adapters/openai"
	"github.com/jorlaru/stargaite/internal/config"
	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/internal/params"
	"github.com/jorlaru/stargaite/internal/plan"
	"github.com/jorlaru/stargaite/internal/routing"
	"github.com/jorlaru/stargaite/internal/transport"
)

// The PAR-9 headers besides the request id.
const (
	traceHeader   = "X-Stargaite-Trace-Id"
	targetHeader  = "X-Stargaite-Target"
	effortHeader  = "X-Stargaite-Reasoning-Effort"
	changedHeader = "X-Stargaite-Params-Changed"
	// errorCodeHeader repeats the code of an error on /v1/messages, whose
	// envelope has no code member (kick-off section 15).
	errorCodeHeader = "X-Stargaite-Error-Code"
)

// The causes that end an attempt early: the RES-3 timers, a stream that ends
// before its last event, a client that went away (RES-5) and a stream
// request answered without a stream; timers are the first four. ErrStopping
// is the other (RES-6).
var (
	errConnectTimeout   = errors.New("connect timeout")
	errTotalTimeout     = errors.New("total timeout")
	errFirstByteTimeout = errors.New("first byte timeout")
	errIdleTimeout      = errors.New("idle timeout")
	errStreamEnded      = errors.New("the stream ended before its last event")
	errClientGone       = errors.New("client gone")
	errNotStream        = errors.New("not an event stream") // a 2xx answer to a stream request

	timers = []error{errConnectTimeout, errTotalTimeout, errFirstByteTimeout, errIdleTimeout}
)

// statusClientGone is the request line's status for a client that went away
// (RES-5), the one nginx logs for a request its client closed; no client
// ever reads it.
const statusClientGone = 499

// request is one authenticated inference request (design section 4).
type request struct {
	id, traceID string           // traceID: 16 random bytes in hex until phase 3 reads traceparent
	ep          domain.Endpoint  // chat or messages
	snap        *config.Snapshot // loaded once (RES-7)
	key         *config.Key
	alias       string // the body's "model" string, set before the lookup; "" when the body named none
	model       *config.Model
	body        *params.Body
	header      http.Header // anthropic-version and anthropic-beta, from /v1/messages only (PAR-14)
	client      *plan.Client
	attempts    []*attempt
	err         *domain.Error // what the client got; nil on success
	started     time.Time
	deadline    sync.Mutex // held by infer's deadline hook and by writeDeadline
	capped      bool       // the hook ran: the deadlines it set stay
}

// writeDeadline moves the write deadline of w to d unless infer's hook has
// capped it, which the mutex makes atomic with the hook (design section 10).
func (rq *request) writeDeadline(w http.ResponseWriter, d time.Time) {
	rq.deadline.Lock()
	defer rq.deadline.Unlock()
	if !rq.capped {
		_ = http.NewResponseController(w).SetWriteDeadline(d)
	}
}

// attempt is one provider call.
type attempt struct {
	n                         int
	gen                       uint64 // of the breaker permit, for Release
	target                    *config.Target
	out                       *plan.Outgoing
	status                    int             // provider status; 0 when no response arrived
	end                       verdict         // how it ended (classify)
	cause                     error           // what ended it early: a timer, client gone, a transport, read or relay failure; nil otherwise
	obs                       domain.Observed // design section 11
	written                   bool            // a byte reached the client: no other answer from here
	started, firstByte, ended time.Time       // firstByte: when the response headers arrived
}

// verdict is how an attempt ended (design section 10): whether the next
// target may be tried, what the target's breaker is told, and the section 15
// error answered when no target is left; nil on success and when the client
// is gone. The zero verdict is the client gone's.
type verdict struct {
	eligible bool            // RES-1 kind of failure: try the next target when nothing was written
	breaker  routing.Verdict // Pass, Fail, None
	err      *domain.Error
}

// relay is how one attempt's answer reaches the client (design section 9),
// chosen by the (endpoint, protocol) pair: sync maps a 2xx body, event and
// observed relay a 2xx stream. observed stands in for end, since every
// adapter emits its last bytes inside event (settled in C18).
type relay struct {
	sync     func(body []byte, now time.Time) ([]byte, domain.Observed, error)
	event    func(name string, data []byte, emit func(name string, data []byte) error) error
	observed func() domain.Observed
}

// relayFor is the relay of an attempt on endpoint ep toward t for client c:
// messages toward anthropic is the native Messages relay; chat toward
// openai is the native chat relay, chat toward anthropic the translated one,
// whose chunks take created from now. Build skips messages toward openai.
func relayFor(ep domain.Endpoint, t *config.Target, c *plan.Client, now func() time.Time) relay {
	tg, p := &t.Spec, &t.Provider.Spec
	switch {
	case ep == domain.EndpointMessages:
		s := anthropic.NewStream(tg, p)
		return relay{
			sync:     func(body []byte, _ time.Time) ([]byte, domain.Observed, error) { return anthropic.Observe(tg, p, body) },
			event:    s.Event,
			observed: s.Observed,
		}
	case p.Protocol == domain.ProtocolAnthropic:
		s := anthropic.NewChatStream(tg, p, c.Usage, now)
		return relay{
			sync: func(body []byte, now time.Time) ([]byte, domain.Observed, error) {
				return anthropic.ToChat(tg, p, body, now)
			},
			event:    s.Event,
			observed: s.Observed,
		}
	}
	s := openai.NewStream(tg, p)
	return relay{
		sync:     func(body []byte, _ time.Time) ([]byte, domain.Observed, error) { return openai.Observe(tg, p, body) },
		event:    s.Event,
		observed: s.Observed,
	}
}

// infer serves POST /v1/chat/completions and POST /v1/messages (design
// sections 4 and 8, steps 1 to 8), synchronous and streamed. done runs on
// every exit once the key is authenticated, a panic included. Every error is
// in the envelope of ep; a stream that fails once committed has already
// written its terminal event, so it gets no envelope. Only /v1/messages
// keeps the client's anthropic-version and anthropic-beta, for Build to
// forward toward an anthropic target (PAR-14).
func (h *handler) infer(ep domain.Endpoint) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rq := &request{id: w.Header().Get(domain.RequestIDHeader), ep: ep, snap: h.Config.Snapshot(), started: time.Now()}
		if rq.key, rq.traceID = h.authenticate(w, r, rq.snap, ep); rq.key == nil {
			return
		}
		h.Flight.Add(1)
		defer h.done(rq)
		// A body read, or a write to a client that stopped reading, does not
		// end with the request's context: once that ends (the shutdown, or the
		// client gone), the read stops at once and a write gets 1 s, so the
		// request ends within the drain wait (design section 10). The
		// handler waits for a started f: on HTTP/2 the writer must not be
		// touched once the handler has returned. No later move of the write
		// deadline undoes the 1 s (writeDeadline).
		fired := make(chan struct{})
		stop := context.AfterFunc(r.Context(), func() {
			defer close(fired)
			rq.deadline.Lock()
			defer rq.deadline.Unlock()
			rq.capped = true
			rc := http.NewResponseController(w)
			_ = rc.SetReadDeadline(time.Now())
			_ = rc.SetWriteDeadline(time.Now().Add(time.Second))
		})
		defer func() {
			if !stop() {
				<-fired
			}
		}()
		if ep == domain.EndpointMessages {
			rq.header = http.Header{}
			for _, name := range []string{"Anthropic-Version", "Anthropic-Beta"} {
				if v := r.Header.Values(name); v != nil {
					rq.header[name] = v
				}
			}
		}
		if rq.err = h.check(w, r, rq); rq.err == nil {
			rq.err = h.route(w, r, rq)
		}
		if rq.err != nil && (len(rq.attempts) == 0 || !rq.attempts[len(rq.attempts)-1].written) {
			fail(w, ep, rq.err)
		}
	}
}

// check is steps 2 and 3: the bounded read of the body (LIM-6), the scan
// (PAR-1), the alias, the key's access to it (SEC-2) and plan.Check.
func (h *handler) check(w http.ResponseWriter, r *http.Request, rq *request) *domain.Error {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, rq.snap.Settings.MaxBodyBytes))
	if tooLarge := new(http.MaxBytesError); errors.As(err, &tooLarge) {
		return domain.NewError(domain.CodeRequestTooLarge, fmt.Sprintf("the body is larger than max_body_bytes, %d", tooLarge.Limit))
	} else if err != nil && errors.Is(context.Cause(r.Context()), ErrStopping) {
		return domain.NewError(domain.CodeInternalError, ErrStopping.Error())
	} else if err != nil {
		return domain.NewError(domain.CodeInvalidRequest, "the request body could not be read")
	}
	// The body is read: from here the server's ReadTimeout would only cancel
	// a request that lasts longer than it (design section 10).
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})

	walk := params.Chat.Walk
	if rq.ep == domain.EndpointMessages {
		walk = params.Messages.Walk
	}
	var pe *params.Error
	if rq.body, pe = params.Scan(raw, walk); pe != nil {
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

// route is steps 5 to 8: per target in order, Build (a static Skip), then
// exclude (a dynamic exclusion), then one call; a failure eligible for
// fallback with nothing written moves to the next target (RES-1), each
// attempt with its own Build, so effort and cap are resolved again for it
// (RSN-10). With no target left it answers the section 15 error of the last
// call when a provider was called; else RES-10: the Skip of the first target
// when every one was skipped statically, otherwise the first dynamic reason
// (503 no_target_available, as for an alias with no published target).
func (h *handler) route(w http.ResponseWriter, r *http.Request, rq *request) *domain.Error {
	var static []plan.Skip
	dynamic := ""
	for i, t := range rq.model.Targets {
		out, skip := plan.Build(plan.Input{Endpoint: rq.ep, Body: rq.body, Header: rq.header, Model: &rq.model.Spec,
			Upstream: t.Upstream, Target: &t.Spec, Provider: &t.Provider.Spec, Client: rq.client})
		if skip != nil {
			if skip.Code == domain.CodeInternalError {
				h.Log.Error("build failed", "request_id", rq.id, "target", t.ID, "error", skip.Param)
			}
			static = append(static, *skip)
			continue
		}
		gen, code := h.exclude(rq, t)
		if code != "" {
			dynamic = cmp.Or(dynamic, code)
			continue
		}
		if a := h.call(w, r, rq, t, out, i+1, gen); a.end.err == nil || a.written || !a.end.eligible {
			return a.end.err
		}
	}
	if n := len(rq.attempts); n > 0 {
		return rq.attempts[n-1].end.err
	}
	if dynamic != "" || len(static) == 0 {
		return domain.NewError(cmp.Or(dynamic, domain.CodeNoTargetAvailable), fmt.Sprintf("no target of model %.200q is available", rq.alias))
	}
	s := static[0]
	e := domain.NewError(s.Code, fmt.Sprintf("no target of model %.200q can serve this request: %s", rq.alias, s.Code))
	if s.Code != domain.CodeInternalError { // its Param is the proxy's own fault, logged above
		e.Param = s.Param
	}
	return e
}

// exclude is the dynamic exclusion of t (step 6), a RES-10 code or "": a
// provider whose credential does not resolve, then an open circuit. A
// circuit's permit, once taken, goes back with its gen in the finish of the
// call that follows at once. Phase 3 inserts quota, budget and usage-store
// checks before both, which read rq.
func (h *handler) exclude(rq *request, t *config.Target) (gen uint64, code string) {
	if t.Provider.Disabled {
		return 0, domain.CodeNoTargetAvailable
	}
	if gen, ok := h.breakers.Acquire(t.ID); ok {
		return gen, ""
	}
	return 0, domain.CodeNoTargetAvailable
}

// call is step 7 for one target: the credential header, the RES-3 timers
// (connect, then total for a synchronous call, or first byte and idle for a
// stream), the write deadline, the call and the answer. A synchronous body is
// read whole up to max_response_bytes (RES-4) and, for a 2xx the adapter can
// read, relayed as the adapter maps it: unchanged but for the think-tag edits
// (RSN-12) on the native paths, translated for chat toward anthropic; a 2xx
// stream is relayed as it arrives (stream). The PAR-9 headers of this
// attempt, replacing those of an earlier one, go out with the status line.
// The attempt it returns holds how it ended (settle).
func (h *handler) call(w http.ResponseWriter, r *http.Request, rq *request, t *config.Target, out *plan.Outgoing, n int, gen uint64) *attempt {
	a := &attempt{n: n, gen: gen, target: t, out: out, started: time.Now()}
	defer h.finish(rq, a)
	rl := relayFor(rq.ep, t, rq.client, h.Config.Now)
	rq.attempts = append(rq.attempts, a)
	w.Header().Set(targetHeader, t.ID)
	w.Header().Del(effortHeader)
	w.Header().Del(changedHeader)
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
	rq.writeDeadline(w, time.Now().Add(wait+5*time.Second))

	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodPost, out.URL, bytes.NewReader(out.Body))
	var resp *http.Response
	if err == nil {
		// Only the headers Build wrote and the credential reach the provider (PAR-14).
		req.Header = out.Header.Clone()
		if t.Provider.Spec.Protocol == domain.ProtocolAnthropic {
			t.Provider.Credential.SetHeader(req.Header, "x-api-key", "")
		} else {
			t.Provider.Credential.SetHeader(req.Header, "Authorization", "Bearer ")
		}
		client := h.Clients[0]
		if t.Provider.Spec.AllowPrivateNetwork {
			client = h.Clients[1]
		}
		resp, err = client.Do(req)
	}
	if err != nil {
		settle(ctx, a, false, nil, nil, err, nil)
		return a
	}
	// Closing the body before it ends drops the connection, so the provider
	// stops sending.
	defer func() { _ = resp.Body.Close() }()
	a.status, a.firstByte = resp.StatusCode, time.Now()
	if rq.client.Stream && a.status/100 == 2 {
		h.stream(ctx, w, rq, a, rl, resp, idle)
		return a
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	var relayErr error
	if err == nil && a.status/100 == 2 {
		var answer []byte
		if int64(len(body)) > limit {
			relayErr = fmt.Errorf("the response is larger than max_response_bytes, %d", limit)
		} else if answer, a.obs, relayErr = rl.sync(body, h.Config.Now()); relayErr == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			a.written = true
			if _, err = w.Write(answer); err != nil {
				err = fmt.Errorf("%w: %v", errClientGone, err)
			}
		}
	}
	settle(ctx, a, false, resp.Header, body, err, relayErr)
	if a.end.err != nil && a.end.err.Code == domain.CodeUpstreamAuthError {
		// The operator's credential, not the client's: the provider's text
		// goes only to the log, rate-limited (kick-off section 15).
		if n, ok := h.upstream.tick(h.Config.Now()); ok {
			h.Log.Warn("provider refused the credential", "request_id", rq.id, "target", t.ID, "status", a.status,
				"message", t.Provider.Credential.Redact(message(a.status, body)), "refusals", n)
		}
	}
	return a
}

// ms is a duration in milliseconds, as the settings state them.
func ms(v int64) time.Duration { return time.Duration(v) * time.Millisecond }

// settle records how a ended. err is what failed the call, a read or a
// write; when the attempt's context had ended, its cause stands instead (a
// RES-3 timer, the shutdown, or the client gone), except for a failed write
// to the client: a timer that fired while a slow client stalled the write
// says nothing of the target. relayErr is what the relay found in a 2xx
// answer. The cause is err, else relayErr; the verdict is classify's, with
// the credential replaced in the provider's text it passes on.
func settle(ctx context.Context, a *attempt, stream bool, header http.Header, body []byte, err, relayErr error) {
	if err != nil && ctx.Err() != nil && !errors.Is(err, errClientGone) {
		err = context.Cause(ctx)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, errClientGone) {
		err = errClientGone
	}
	a.cause = cmp.Or(err, relayErr)
	a.end = classify(stream, a.status, a.cause, header, body, relayErr)
	if a.end.err != nil {
		a.end.err.Message = a.target.Provider.Credential.Redact(a.end.err.Message)
	}
}

// classify is how an attempt ended (design section 10, RES-1, RES-2,
// kick-off section 15): whether the next target may be tried, what the
// target's breaker is told and the error answered when no target is left.
// Nothing else decides them. status is the provider's, 0 without a
// response; cause what ended the attempt early; header and body the
// provider's (Retry-After, the error message); relayErr what the relay found
// in a 2xx answer; stream whether that answer was relayed as a stream,
// which only words the message. The client gone is no verdict, with nothing
// to answer; the shutdown neither tries the next target nor tells the
// breaker, and answers 500 internal_error "server stopping" (RES-6, section
// 15 point 15). A timer, a failed call, a stream cut, 429 and any 5xx are
// eligible and fail the breaker: 504 upstream_timeout for a timer, the 429
// with its Retry-After, else 502. An upstream error event is eligible when
// Anthropic documents 429 or a 5xx for its type (section 15 point 15); an
// OpenAI error chunk never is. Everything else passes the breaker and is not
// eligible: a 2xx the relay cannot use, 400, 404, 413 and 422 with the
// provider's status and message, 401 and 403 as 502 upstream_auth_error, any
// other status as 502.
func classify(stream bool, status int, cause error, header http.Header, body []byte, relayErr error) verdict {
	fail := func(e *domain.Error) verdict { return verdict{eligible: true, breaker: routing.Fail, err: e} }
	pass := func(e *domain.Error) verdict { return verdict{breaker: routing.Pass, err: e} }
	upstream := func(message string) *domain.Error { return domain.NewError(domain.CodeUpstreamError, message) }
	var chunk *openai.ErrorChunk
	var event *anthropic.ErrorEvent
	switch {
	case errors.Is(cause, errClientGone):
		return verdict{}
	case errors.Is(cause, ErrStopping):
		return verdict{err: domain.NewError(domain.CodeInternalError, ErrStopping.Error())}
	case slices.ContainsFunc(timers, func(timer error) bool { return errors.Is(cause, timer) }):
		return fail(domain.NewError(domain.CodeUpstreamTimeout, "the provider did not answer in time: "+cause.Error()))
	case errors.As(relayErr, &event):
		if event.Status == http.StatusTooManyRequests || event.Status/100 == 5 {
			return fail(upstream("the provider's stream reported an error"))
		}
		return pass(upstream("the provider's stream reported an error"))
	case errors.As(relayErr, &chunk):
		return pass(upstream("the provider's stream reported an error"))
	case errors.Is(relayErr, errNotStream):
		return pass(upstream("the provider's answer is not an event stream"))
	case relayErr != nil && stream:
		return pass(upstream("the provider's stream failed"))
	case relayErr != nil:
		return pass(upstream("the provider's answer is not usable"))
	case cause != nil && stream:
		return fail(upstream("the provider's stream failed"))
	case cause != nil:
		return fail(upstream("the provider call failed"))
	}
	switch s := status; {
	case s/100 == 2:
		return pass(nil)
	case s == http.StatusUnauthorized || s == http.StatusForbidden:
		return pass(domain.NewError(domain.CodeUpstreamAuthError, "the provider refused the operator's credential"))
	case s == http.StatusBadRequest || s == http.StatusNotFound || s == http.StatusRequestEntityTooLarge ||
		s == http.StatusUnprocessableEntity:
		return pass(domain.UpstreamError(s, message(s, body), header.Get("Retry-After")))
	case s == http.StatusTooManyRequests:
		// Status and Retry-After only: the provider's text names the
		// operator's account and limits (kick-off section 15).
		return fail(domain.UpstreamError(s, "the provider answered 429", header.Get("Retry-After")))
	case s/100 == 5:
		return fail(domain.UpstreamError(s, fmt.Sprintf("the provider answered %d", s), ""))
	}
	return pass(domain.UpstreamError(status, fmt.Sprintf("the provider answered %d", status), ""))
}

// stream sends a 2xx stream to the client as it arrives (design section 9,
// RES-1, RES-4). Each upstream event resets the idle timer and moves the
// write deadline, also one that emits nothing, such as a thinking delta.
// Comments before the commit are dropped, those the adapter emits (an
// Anthropic ping) included; the first event the adapter emits commits the
// response: status 200, the SSE headers and the PAR-9 headers already set.
// Every event is written and flushed at once, with the credential replaced
// in it, and nothing is buffered beyond the event being read and the at most
// 7 bytes of a think-tag stripper (RSN-12). A failure before the commit is
// left to the caller (settle); after it, the terminal event is written here,
// always the proxy's own in the envelope of the endpoint: data: {"error":
// {...}} with no [DONE] after it on chat, event: error with the Anthropic
// envelope on messages. An upstream error event never crosses with its text:
// on messages its type does when Anthropic documents it (else api_error),
// with a fixed message naming the request id; on chat toward anthropic the
// fixed message names that type; an OpenAI error chunk gets the fixed
// message alone. A failed write ends the attempt with nothing more to
// answer: the client is gone.
func (h *handler) stream(ctx context.Context, w http.ResponseWriter, rq *request, a *attempt, rl relay, resp *http.Response, idle *time.Timer) {
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt != "text/event-stream" {
		settle(ctx, a, true, resp.Header, nil, nil, fmt.Errorf("the answer is %.100q, %w", resp.Header.Get("Content-Type"), errNotStream))
		return
	}
	rc := http.NewResponseController(w)
	credential := a.target.Provider.Credential
	write := func(e transport.Event) error {
		if e.Comment && !a.written {
			return nil // a comment, an Anthropic ping included, keeps the stream alive but commits nothing
		}
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
	emit := func(name string, data []byte) error {
		return write(transport.Event{Name: name, Data: data, Comment: name == anthropic.Comment})
	}

	// err is a failed read (a cut) or write (the client gone); relayErr what
	// the stream holds: an event over max_response_bytes, malformed data, an
	// upstream error event, or io.EOF once the last event is relayed.
	events := transport.NewEventReader(resp.Body, rq.snap.Settings.MaxResponseBytes)
	silence := ms(rq.snap.Settings.Timeouts.StreamIdleMS)
	var ev transport.Event
	var err, relayErr error
	for err == nil && relayErr == nil {
		if ev, err = events.Next(); err == io.EOF {
			err = errStreamEnded
		}
		// A write blocked on a slow client is the write deadline's to bound,
		// not the provider's silence; once the context ends, infer's 1 s stays.
		idle.Stop()
		if ctx.Err() == nil {
			rq.writeDeadline(w, time.Now().Add(silence+5*time.Second))
		}
		switch {
		case errors.Is(err, transport.ErrEventTooLarge):
			err, relayErr = nil, err
		case err != nil:
		case ev.Comment:
			err = write(ev)
		default:
			if relayErr = rl.event(ev.Name, ev.Data, emit); errors.Is(relayErr, errClientGone) {
				err, relayErr = relayErr, nil
			}
		}
		idle.Reset(silence)
	}
	a.obs = rl.observed()
	if relayErr == io.EOF { // the last event was relayed
		relayErr = nil
	}
	settle(ctx, a, true, resp.Header, nil, err, relayErr)
	if !a.written || a.end.err == nil {
		return
	}
	name := ""
	if rq.ep == domain.EndpointMessages {
		name = "error"
	}
	// After the commit an upstream error's type may cross when Anthropic
	// documents it (a closed list); the provider's text never does, it stays
	// in the cause (the owner's decision of 2026-11-14).
	e := *a.end.err
	var event *anthropic.ErrorEvent
	switch {
	case !errors.As(relayErr, &event):
	case rq.ep == domain.EndpointMessages:
		e.Message = "the provider's stream failed; request id " + rq.id
		if event.Status != 0 {
			e.Type = event.Type
		}
	case event.Status != 0:
		e.Message = "the provider's stream failed: " + event.Type
	}
	data, _ := config.Encode(envelope(rq.ep, &e))
	_ = write(transport.Event{Name: name, Data: data})
}

// message is the error.message of a provider's error body, else a fixed
// text with the status. It may hold the credential: the caller redacts it.
func message(status int, body []byte) string {
	if b, e := params.Scan(body, [][]string{{"error"}}); e == nil {
		v, _ := b.Get("error", "message")
		if s, ok := unquote(v); ok && s != "" {
			return s
		}
	}
	return fmt.Sprintf("the provider answered %d", status)
}

// finish ends one provider call (design section 11, point 2) after its last
// byte, its failure or a panic: the circuit's permit goes back with the
// call's verdict (None on a panic), and one log line, without the
// credential. Phase 3 adds the usage record, the counters and the attempt
// span here.
func (h *handler) finish(rq *request, a *attempt) {
	a.ended = time.Now()
	h.breakers.Release(a.target.ID, a.gen, a.end.breaker)
	attrs := []any{"request_id", rq.id, "trace_id", rq.traceID, "key", rq.key.Name, "alias", rq.alias, "attempt", a.n,
		"target", a.target.ID, "status", a.status, "duration_ms", a.ended.Sub(a.started).Milliseconds()}
	if a.cause != nil {
		attrs = append(attrs, "cause", a.target.Provider.Credential.Redact(a.cause.Error()))
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
// its last finish: the request line (OPS-9), then Flight. Its status and
// code are those of the error the client got, also as the terminal event of
// a committed stream (so 500 internal_error for a request the shutdown
// stopped); 499 client_gone when the client went away, whatever was written
// before; else 200. Phase 3 releases the admission slots and ends the
// request span here.
func (h *handler) done(rq *request) {
	defer h.Flight.Done()
	attrs := []any{"request_id", rq.id, "trace_id", rq.traceID, "key", rq.key.Name}
	if rq.alias != "" {
		attrs = append(attrs, "alias", fmt.Sprintf("%.200s", rq.alias)) // an unknown alias is as long as the body allows
	}
	n := len(rq.attempts)
	attrs = append(attrs, "attempts", n)
	switch {
	case rq.err != nil:
		attrs = append(attrs, "status", rq.err.Status, "code", rq.err.Code)
	case n > 0 && errors.Is(rq.attempts[n-1].cause, errClientGone):
		attrs = append(attrs, "status", statusClientGone, "code", "client_gone")
	default:
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
