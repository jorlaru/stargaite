package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/domain"
	"github.com/jorlaru/stargaite/tests/mockprovider"
)

// streamConfig has RES-3 stream timers in milliseconds, so no test waits.
const streamConfig = `version: 1
settings: { timeouts: { first_byte_ms: 300, stream_idle_ms: 300 }, max_response_bytes: 4096 }
providers:
  mock: { protocol: openai, base_url: MOCK, api_key_env: MOCK_KEY, billing: none, allow_private_network: true }
targets:
  mock/up-1: { kind: chat, reasoning: { dialect: reasoning_effort, levels: [low, medium, high], can_disable: true } }
  mock/up-2: { kind: chat }
models:
  fast: { targets: [mock/up-1], reasoning: { effort: high, mode: default }, max_output_tokens: 500 }
  plain: { targets: [mock/up-2] }
keys:
  agent: { models: [fast, plain] }
`

// streamFixture is the data plane on a real listener, so responses are
// flushed and a client can go away, with the mock provider behind it.
type streamFixture struct {
	*fixture
	mock       *mockprovider.Server
	mockURL    string
	url, token string
}

func newStreamFixture(t *testing.T, yaml string) *streamFixture {
	t.Helper()
	f := newFixture(t)
	mock := mockprovider.New()
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	tokens := f.apply(t, strings.Replace(yaml, "MOCK", srv.URL, 1))
	data := httptest.NewServer(f.data)
	t.Cleanup(data.Close)
	return &streamFixture{fixture: f, mock: mock, mockURL: srv.URL, url: data.URL, token: tokens["agent"]}
}

// post sends a chat completion and reads the whole answer.
func (s *streamFixture) post(t *testing.T, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, s.url+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v after %q", err, got)
	}
	return resp, string(got)
}

func streamBody(alias string) string { return chatBody(alias, `,"stream":true`) }

func chunk(content string) string {
	return `{"id":"c","object":"chat.completion.chunk","created":1,"model":"up-2","choices":[{"index":0,"delta":{"content":"` + content + `"},"finish_reason":null}]}`
}

func sse(data ...string) [][]byte {
	var out [][]byte
	for _, d := range data {
		out = append(out, []byte("data: "+d+"\n\n"))
	}
	return out
}

func terminal(message, code string) string {
	return `data: {"error":{"message":"` + message + `","type":"api_error","code":"` + code + `"}}` + "\n\n"
}

// TestChatStream: the mock's default stream reaches the client byte for
// byte with the SSE and PAR-9 headers (AC-12), the provider gets "stream":
// true with the alias's edits, and the attempt observes the usage chunk the
// provider sent unasked. The fixture of upstream bytes, split at every byte
// offset and one byte per chunk, gives the same client bytes each time.
func TestChatStream(t *testing.T) {
	s := newStreamFixture(t, streamConfig)
	resp, got := s.post(t, streamBody("fast"))
	h := resp.Header
	if resp.StatusCode != http.StatusOK || h.Get("Content-Type") != "text/event-stream" || h.Get("Cache-Control") != "no-cache" ||
		h.Get(domain.RequestIDHeader) == "" || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(h.Get(traceHeader)) ||
		h.Get(targetHeader) != "mock/up-1" || h.Get(effortHeader) != "high" || h.Get(changedHeader) != "max_tokens:added,reasoning_effort:added" {
		t.Fatalf("status %d, headers %v", resp.StatusCode, h)
	}
	direct, err := http.Post(s.mockURL+"/chat/completions", "application/json", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := io.ReadAll(direct.Body)
	_ = direct.Body.Close()
	if got != string(want) || !strings.HasSuffix(got, "data: [DONE]\n\n") {
		t.Errorf("body %q, the mock sends %q", got, want)
	}
	if calls := s.mock.Calls(); len(calls) != 2 || string(calls[0].Body) != chatBody("up-1", `,"stream":true,"max_tokens":500,"reasoning_effort":"high"`) {
		t.Errorf("calls %+v", calls)
	}
	if log := s.log.String(); !strings.Contains(log, `"response_model":"mock","finish_reason":"stop","usage":{"input":1,"cache_read":0,"cache_write":0,"output":2},"visible_bytes":10`) ||
		!strings.Contains(log, `"alias":"fast","attempts":1,"status":200`) {
		t.Errorf("log %s", log)
	}

	up, err := os.ReadFile("testdata/openai-stream.up.sse")
	if err != nil {
		t.Fatal(err)
	}
	down, err := os.ReadFile("testdata/openai-stream.down.sse")
	if err != nil {
		t.Fatal(err)
	}
	var bytewise [][]byte
	for i := range up {
		bytewise = append(bytewise, up[i:i+1])
	}
	splits := [][][]byte{bytewise}
	for i := 0; i <= len(up); i++ {
		splits = append(splits, [][]byte{up[:i], up[i:]})
	}
	for _, chunks := range splits {
		s.mock.Script(mockprovider.Step{Chunks: chunks})
		if resp, got := s.post(t, streamBody("plain")); resp.StatusCode != http.StatusOK || got != string(down) {
			t.Fatalf("split %d: %d %q", len(chunks[0]), resp.StatusCode, got)
		}
	}
	lines := strings.Split(strings.TrimSpace(s.log.String()), "\n")
	if !strings.Contains(lines[len(lines)-2], `"response_model":"up-2","finish_reason":"tool_calls",`+
		`"usage":{"input":12,"cache_read":0,"cache_write":0,"output":7,"reasoning":3},"visible_bytes":16,"tool_calls":1`) ||
		!strings.Contains(lines[len(lines)-1], `"msg":"request"`) {
		t.Errorf("last lines %s", lines[len(lines)-2:])
	}
}

// TestSDKStreamRequests is AC-01 for streams: the stream requests recorded
// from the OpenAI Python SDK 3.50.0 and Node SDK 7.56.0, which ask for the
// usage chunk, reach the provider with only the alias's edits, and every
// event of the answer holds the fields the SDKs' ChatCompletionChunk
// requires, up to [DONE].
func TestSDKStreamRequests(t *testing.T) {
	s := newStreamFixture(t, streamConfig)
	for _, name := range []string{"python-stream", "node-stream"} {
		raw, err := os.ReadFile("testdata/openai-" + name + ".http")
		if err != nil {
			t.Fatal(err)
		}
		recorded, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(raw)))
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(recorded.Body)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(recorded.Method, s.url+recorded.URL.Path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header = recorded.Header.Clone()
		req.Header.Set("Authorization", "Bearer "+s.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" || !strings.HasSuffix(string(got), "data: [DONE]\n\n") {
			t.Fatalf("%s: %d %q %v", name, resp.StatusCode, got, err)
		}
		events := strings.Split(strings.TrimSuffix(string(got), "data: [DONE]\n\n"), "\n\n")
		usage := false
		for _, e := range events[:len(events)-1] {
			var c struct {
				ID, Object, Model *string
				Created           *int64
				Choices           []struct{ Index *int }
				Usage             *struct {
					TotalTokens *int `json:"total_tokens"`
				}
			}
			data, ok := strings.CutPrefix(e, "data: ")
			if err := json.Unmarshal([]byte(data), &c); !ok || err != nil || c.ID == nil || c.Object == nil || *c.Object != "chat.completion.chunk" ||
				c.Model == nil || c.Created == nil || c.Choices == nil {
				t.Errorf("%s: event %q", name, e)
			}
			for _, ch := range c.Choices {
				if ch.Index == nil {
					t.Errorf("%s: choice without index in %q", name, e)
				}
			}
			usage = usage || c.Usage != nil && c.Usage.TotalTokens != nil
		}
		if !usage {
			t.Errorf("%s: no usage chunk in %q", name, got)
		}
		want := strings.Replace(strings.TrimSuffix(string(body), "}"), `"model":"fast"`, `"model":"up-1"`, 1) + `,"max_tokens":500,"reasoning_effort":"high"}`
		if calls := s.mock.Calls(); len(calls) == 0 || string(calls[len(calls)-1].Body) != want || calls[len(calls)-1].Header.Get("X-Stainless-Lang") != "" {
			t.Errorf("%s: the provider got %+v", name, calls)
		}
	}
}

// TestChatStreamFaults: a stream that fails before its first event commits
// is an envelope (section 15); after it, the client gets what was relayed and
// one terminal event, with no [DONE] (RES-4). Comments keep a stream alive
// and are dropped before the commit. The credential reaches neither the
// client nor the log. Each call has one attempt line and each request one
// request line.
func TestChatStreamFaults(t *testing.T) {
	s := newStreamFixture(t, streamConfig)
	echo := `{"error":{"message":"bad key ` + planted + `MOCK_KEY","type":"server_error"}}`
	big := `{"x":"` + strings.Repeat("x", 5000) + `"}`
	first := "data: " + chunk("a") + "\n\n"
	for _, tc := range []struct {
		name          string
		step          mockprovider.Step
		status        int
		code, message string // the envelope's, before the commit
		body          string // the client's bytes, after the commit
		cause         string
	}{
		{name: "an answer that is not a stream", step: mockprovider.Step{Body: []byte(mockReply), Header: http.Header{"Content-Type": {"application/json"}}},
			status: 502, code: "upstream_error", message: "the provider's answer is not an event stream", cause: "not an event stream"},
		{name: "an error chunk first", step: mockprovider.Step{Chunks: sse(echo)},
			status: 502, code: "upstream_error", message: "the provider's stream reported an error", cause: "bad key [redacted]"},
		{name: "cut before the first event", step: mockprovider.Step{Chunks: [][]byte{[]byte(": c\n\n")}, Cut: true},
			status: 502, code: "upstream_error", message: "the provider's stream failed", cause: "unexpected EOF"},
		{name: "ended before the first event", step: mockprovider.Step{Chunks: [][]byte{[]byte(": c\n\n")}},
			status: 502, code: "upstream_error", message: "the provider's stream failed", cause: "the stream ended before its last event"},
		{name: "a late first byte", step: mockprovider.Step{Chunks: sse(chunk("a"), "[DONE]"), Delay: time.Second},
			status: 504, code: "upstream_timeout", message: "the provider did not answer in time: first byte timeout", cause: "first byte timeout"},
		{name: "silent before the first event", step: mockprovider.Step{Chunks: [][]byte{[]byte(": c\n\n"), []byte(first)}, Gap: time.Second},
			status: 504, code: "upstream_timeout", message: "the provider did not answer in time: idle timeout", cause: "idle timeout"},
		{name: "an event over max_response_bytes first", step: mockprovider.Step{Chunks: sse(big)},
			status: 502, code: "upstream_error", message: "the provider's stream failed", cause: "larger than max_response_bytes"},
		{name: "provider 429", step: mockprovider.Step{Status: 429, Header: http.Header{"Retry-After": {"3"}}, Body: []byte(echo)},
			status: 429, code: "upstream_error", message: "the provider answered 429"},
		{name: "provider 400", step: mockprovider.Step{Status: 400, Body: []byte(echo)},
			status: 400, code: "upstream_error", message: "bad key [redacted]"},
		{name: "provider 500 holding its body", step: mockprovider.Step{Status: 500, Body: []byte("{"), Hang: true},
			status: 504, code: "upstream_timeout", message: "idle timeout", cause: "idle timeout"},

		{name: "comments keep it alive", step: mockprovider.Step{Chunks: [][]byte{[]byte(": a\n\n"), []byte(": b\n\n"), []byte(": c\n\n"), []byte(first),
			[]byte(": d\n\n"), []byte("data: [DONE]\n\n")}, Gap: 150 * time.Millisecond}, status: 200, body: first + ": d\n\ndata: [DONE]\n\n"},
		{name: "a comment hiding an event behind a bare CR", step: mockprovider.Step{Chunks: [][]byte{[]byte(first),
			[]byte(": ping\rdata: " + chunk("b") + "\n\n"), []byte("data: [DONE]\n\n")}}, status: 200, body: first + ": ping\n\ndata: [DONE]\n\n"},
		{name: "cut after the commit", step: mockprovider.Step{Chunks: [][]byte{[]byte(first)}, Cut: true},
			status: 200, body: first + terminal("the provider's stream failed", "upstream_error"), cause: "unexpected EOF"},
		{name: "ended before [DONE]", step: mockprovider.Step{Chunks: [][]byte{[]byte(first)}},
			status: 200, body: first + terminal("the provider's stream failed", "upstream_error"), cause: "the stream ended before its last event"},
		{name: "silent after the commit", step: mockprovider.Step{Chunks: [][]byte{[]byte(first)}, Hang: true},
			status: 200, body: first + terminal("the provider did not answer in time: idle timeout", "upstream_timeout"), cause: "idle timeout"},
		{name: "an error chunk after the commit", step: mockprovider.Step{Chunks: sse(chunk("a"), echo, "[DONE]")},
			status: 200, body: first + "data: " + strings.Replace(echo, planted+"MOCK_KEY", "[redacted]", 1) + "\n\n", cause: "bad key [redacted]"},
		{name: "an event over max_response_bytes after the commit", step: mockprovider.Step{Chunks: sse(chunk("a"), big)},
			status: 200, body: first + terminal("the provider's stream failed", "upstream_error"), cause: "larger than max_response_bytes"},
		{name: "a chunk that is not JSON", step: mockprovider.Step{Chunks: sse(chunk("a"), "oops")},
			status: 200, body: first + terminal("the provider's stream failed", "upstream_error"), cause: "a chunk of the stream"},
		{name: "a relayed chunk echoing the credential", step: mockprovider.Step{Chunks: sse(chunk(planted+"MOCK_KEY"), "[DONE]")},
			status: 200, body: "data: " + chunk("[redacted]") + "\n\ndata: [DONE]\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.mock.Script(tc.step)
			before := len(s.mock.Calls())
			resp, got := s.post(t, streamBody("plain"))
			if resp.StatusCode != tc.status || resp.Header.Get(targetHeader) != "mock/up-2" || strings.Contains(got, planted) {
				t.Fatalf("status %d, headers %v: %q", resp.StatusCode, resp.Header, got)
			}
			if tc.status == 200 {
				if resp.Header.Get("Content-Type") != "text/event-stream" || got != tc.body {
					t.Errorf("body %q", got)
				}
			} else {
				var env struct{ Error domain.Error }
				if err := json.Unmarshal([]byte(got), &env); err != nil || resp.Header.Get("Content-Type") != "application/json" || env.Error.Code != tc.code ||
					!strings.Contains(env.Error.Message, tc.message) {
					t.Errorf("envelope %s", got)
				}
			}
			if len(s.mock.Calls()) != before+1 {
				t.Errorf("%d calls", len(s.mock.Calls())-before)
			}
			log := s.log.String()
			line := log[strings.LastIndex(log, `"msg":"attempt"`):]
			if strings.Contains(line, `"cause":"`) != (tc.cause != "") || !strings.Contains(line, tc.cause) || strings.Contains(log, planted) {
				t.Errorf("attempt line %s", line)
			}
		})
	}
	var attempts, requests int
	for line := range strings.SplitSeq(strings.TrimSpace(s.log.String()), "\n") {
		attempts += strings.Count(line, `"msg":"attempt"`)
		requests += strings.Count(line, `"msg":"request"`)
	}
	if calls := len(s.mock.Calls()); attempts != calls || requests != calls {
		t.Errorf("%d attempt lines and %d request lines for %d calls", attempts, requests, calls)
	}
}

// TestChatStreamStalledHeaders (RES-3): a provider that sends the first
// bytes of its answer, part of a status line or a 1xx, and then stalls is cut
// by the idle timer, which runs from the first byte.
func TestChatStreamStalledHeaders(t *testing.T) {
	s := newStreamFixture(t, streamConfig)
	for _, prefix := range []string{"HTTP/1.1 200 OK\r\n", "HTTP/1.1 103 Early Hints\r\n\r\n"} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		go func() {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			if _, err := http.ReadRequest(bufio.NewReader(conn)); err == nil {
				_, _ = io.WriteString(conn, prefix)
			}
			_, _ = io.Copy(io.Discard, conn) // until the proxy drops the connection
		}()
		s.apply(t, strings.Replace(streamConfig, "MOCK", "http://"+ln.Addr().String(), 1))

		req, err := http.NewRequest(http.MethodPost, s.url+"/v1/chat/completions", strings.NewReader(streamBody("plain")))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+s.token)
		resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("%q: %v", prefix, err)
		}
		got, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		var env struct{ Error domain.Error }
		if err := json.Unmarshal(got, &env); err != nil || resp.StatusCode != http.StatusGatewayTimeout || env.Error.Code != "upstream_timeout" ||
			!strings.Contains(env.Error.Message, "idle timeout") {
			t.Errorf("%q: %d %s", prefix, resp.StatusCode, got)
		}
	}
}

// TestChatStreamSlowClient: a client that stops reading for longer than
// stream_idle_ms while the provider keeps sending still gets the whole
// stream; the idle timer measures the provider's silence, not a blocked
// write, which the write deadline bounds.
func TestChatStreamSlowClient(t *testing.T) {
	s := newStreamFixture(t, streamConfig)
	// Enough bytes to fill the socket buffers between the proxy and the client.
	s.mock.Script(mockprovider.Step{Chunks: sse(append(slices.Repeat([]string{chunk(strings.Repeat("x", 3000))}, 4000), "[DONE]")...)})
	req, err := http.NewRequest(http.MethodPost, s.url+"/v1/chat/completions", strings.NewReader(streamBody("plain")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	time.Sleep(time.Second) // longer than stream_idle_ms, shorter than the write deadline
	got, err := io.ReadAll(resp.Body)
	if err != nil || !strings.HasSuffix(string(got), "data: [DONE]\n\n") {
		t.Errorf("%d bytes ending in %q: %v", len(got), got[max(0, len(got)-200):], err)
	}
	if log := s.log.String(); strings.Contains(log, `"cause":"`) {
		t.Errorf("log %s", log[strings.LastIndex(log, `"msg":"attempt"`):])
	}
}

// TestChatStreamClientGone (RES-5, the part of step C14): a client that goes
// away mid-stream ends the provider's request, so neither its body nor a
// goroutine is left, and finish and done each run once.
func TestChatStreamClientGone(t *testing.T) {
	released := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+chunk("a")+"\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		released <- struct{}{}
	}))
	t.Cleanup(upstream.Close)
	yaml := strings.Replace(streamConfig, "stream_idle_ms: 300", "stream_idle_ms: 60000", 1)
	s := newStreamFixture(t, yaml)
	s.apply(t, strings.Replace(yaml, "MOCK", upstream.URL, 1))

	req, err := http.NewRequest(http.MethodPost, s.url+"/v1/chat/completions", strings.NewReader(streamBody("plain")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "data: "+chunk("a")+"\n" {
		t.Fatalf("first line %q: %v", line, err)
	}
	gone := time.Now()
	_ = resp.Body.Close()
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("the provider's request did not end within 1 s of the client going away")
	}
	for !strings.Contains(s.log.String(), `"msg":"request"`) {
		if time.Since(gone) > 2*time.Second {
			t.Fatalf("no request line: %s", s.log.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	log := s.log.String()
	if strings.Count(log, `"msg":"attempt"`) != 1 || strings.Count(log, `"msg":"request"`) != 1 || !strings.Contains(log, `"cause":"client gone"`) {
		t.Errorf("log %s", log)
	}
}

// TestChatStreamAcrossApply is AC-05 and RES-7: a stream open while an apply
// moves its alias to another target and removes the first finishes on its
// snapshot; the next request goes to the new target.
func TestChatStreamAcrossApply(t *testing.T) {
	s := newStreamFixture(t, streamConfig)
	s.mock.Script(mockprovider.Step{Chunks: sse(chunk("a"), chunk("b"), chunk("c"), "[DONE]"), Gap: 100 * time.Millisecond})
	req, err := http.NewRequest(http.MethodPost, s.url+"/v1/chat/completions", strings.NewReader(streamBody("plain")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	r := bufio.NewReader(resp.Body)
	if line, err := r.ReadString('\n'); err != nil || line != "data: "+chunk("a")+"\n" {
		t.Fatalf("first line %q: %v", line, err)
	}
	moved := strings.Replace(strings.Replace(streamConfig, "  mock/up-2: { kind: chat }\n", "", 1), "plain: { targets: [mock/up-2] }", "plain: { targets: [mock/up-1] }", 1)
	s.apply(t, strings.Replace(moved, "MOCK", s.mockURL, 1))
	rest, err := io.ReadAll(r)
	if err != nil || resp.Header.Get(targetHeader) != "mock/up-2" ||
		string(rest) != "\ndata: "+chunk("b")+"\n\ndata: "+chunk("c")+"\n\ndata: [DONE]\n\n" {
		t.Errorf("target %q, rest %q: %v", resp.Header.Get(targetHeader), rest, err)
	}
	if resp, _ := s.post(t, streamBody("plain")); resp.Header.Get(targetHeader) != "mock/up-1" {
		t.Errorf("after the apply: target %q", resp.Header.Get(targetHeader))
	}
	if calls := s.mock.Calls(); len(calls) != 2 || !strings.Contains(string(calls[0].Body), `"model":"up-2"`) || !strings.Contains(string(calls[1].Body), `"model":"up-1"`) {
		t.Errorf("calls %+v", calls)
	}
}

// TestChatThink is AC-19 through the data plane, toward a target that
// declares inline_think_tags: the synchronous answer loses every block, an
// escaped one included, with every other byte the provider's; the stream
// fixture, whose tags are split across chunks and escaped, gives the same
// client bytes split at every byte offset and one byte per chunk; the removal
// is on the attempt line (stripped, visible bytes). A target that does not
// declare them gets the provider's bytes, tags included.
func TestChatThink(t *testing.T) {
	yaml := strings.Replace(strings.Replace(streamConfig, "  mock/up-2: { kind: chat }\n",
		"  mock/up-2: { kind: chat }\n  mock/up-3: { kind: chat, inline_think_tags: true }\n", 1),
		"  plain: { targets: [mock/up-2] }\n", "  plain: { targets: [mock/up-2] }\n  think: { targets: [mock/up-3] }\n", 1)
	s := newStreamFixture(t, strings.Replace(yaml, "agent: { models: [fast, plain] }", "agent: { models: [fast, plain, think] }", 1))
	reply := `{"id":"x","object":"chat.completion","created":0,"model":"up-3","choices":[{"index":0,"message":{"role":"assistant",` +
		`"content":"<think>plan</think>\n\nHi <think>more</think>there é"},"finish_reason":"stop"}],"usage":null}`
	attempt := func() string {
		log := s.log.String()
		return log[strings.LastIndex(log, `"msg":"attempt"`):]
	}
	s.mock.Script(mockprovider.Step{Body: []byte(reply)})
	if resp, got := s.post(t, chatBody("think", "")); resp.StatusCode != http.StatusOK ||
		got != strings.Replace(reply, `"<think>plan</think>\n\nHi <think>more</think>there é"`, `"\n\nHi there é"`, 1) ||
		!strings.Contains(attempt(), `"visible_bytes":13,"tool_calls":0,"stripped":true`) {
		t.Errorf("sync: %d %s; %s", resp.StatusCode, got, attempt())
	}
	s.mock.Script(mockprovider.Step{Body: []byte(reply)})
	if resp, got := s.post(t, chatBody("plain", "")); resp.StatusCode != http.StatusOK || got != reply ||
		!strings.Contains(attempt(), `"stripped":false`) {
		t.Errorf("sync without inline_think_tags: %d %s; %s", resp.StatusCode, got, attempt())
	}

	up, err := os.ReadFile("testdata/openai-think.up.sse")
	if err != nil {
		t.Fatal(err)
	}
	down, err := os.ReadFile("testdata/openai-think.down.sse")
	if err != nil {
		t.Fatal(err)
	}
	var bytewise [][]byte
	for i := range up {
		bytewise = append(bytewise, up[i:i+1])
	}
	splits := [][][]byte{bytewise}
	for i := 0; i <= len(up); i++ {
		splits = append(splits, [][]byte{up[:i], up[i:]})
	}
	for _, chunks := range splits {
		s.mock.Script(mockprovider.Step{Chunks: chunks})
		if resp, got := s.post(t, streamBody("think")); resp.StatusCode != http.StatusOK || got != string(down) ||
			!strings.Contains(attempt(), `"finish_reason":"stop","usage":{"input":9,"cache_read":0,"cache_write":0,"output":20},"visible_bytes":27,"tool_calls":0,"stripped":true`) {
			t.Fatalf("split %d: %d %q; %s", len(chunks[0]), resp.StatusCode, got, attempt())
		}
	}
	s.mock.Script(mockprovider.Step{Chunks: [][]byte{up}})
	if resp, got := s.post(t, streamBody("plain")); resp.StatusCode != http.StatusOK ||
		got != string(up) || !strings.Contains(attempt(), `"stripped":false`) {
		t.Errorf("stream without inline_think_tags: %d %q", resp.StatusCode, got)
	}
}
