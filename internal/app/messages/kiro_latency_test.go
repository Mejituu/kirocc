package messages

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/d-kuro/kirocc/internal/auth"
	"github.com/d-kuro/kirocc/internal/kiroclient"
	"github.com/d-kuro/kirocc/internal/kiroproto"
	tu "github.com/d-kuro/kirocc/internal/testutil"
)

// timedEvent is one upstream Kiro event, preceded by a delay the fake client
// sleeps before writing it.
type timedEvent struct {
	wait    time.Duration
	typ     string
	payload string
}

// timedKiroClient serves scripted event streams, one script per upstream
// call (the last script repeats). Events are written to a pipe with the
// configured delays, so tests control first-byte and completion timing.
type timedKiroClient struct {
	mu      sync.Mutex
	calls   int
	scripts [][]timedEvent
}

func (c *timedKiroClient) GenerateAssistantResponse(ctx context.Context, token string, payload *kiroproto.Payload, region string) (*kiroclient.Response, error) {
	c.mu.Lock()
	call := c.calls
	c.calls++
	script := c.scripts[min(call, len(c.scripts)-1)]
	c.mu.Unlock()

	reader, writer := io.Pipe()
	go func() {
		defer writer.Close()
		for _, ev := range script {
			if ev.wait > 0 {
				time.Sleep(ev.wait)
			}
			if _, err := writer.Write(tu.BuildFrame(ev.typ, []byte(ev.payload))); err != nil {
				return
			}
		}
	}()
	return &kiroclient.Response{
		StatusCode: http.StatusOK,
		Body:       reader,
		Header:     http.Header{"Content-Type": []string{"application/vnd.amazon.eventstream"}},
	}, nil
}

// newKiroService builds a plain Kiro-path service around a fake client.
func newKiroService(t *testing.T, client kiroclient.Client) *Service {
	t.Helper()
	t.Setenv("KIROCC_MODEL_MAPPINGS", "")
	return New(&countingAuth{creds: &auth.Credentials{AccessToken: "kiro-token", ProfileARN: "arn:test", Region: "us-east-1"}}, client)
}

// tintAttr extracts a key=value field from a raw tint log line.
func tintAttr(line, key string) string {
	for _, field := range strings.Fields(line) {
		if v, ok := strings.CutPrefix(field, key+"="); ok {
			return v
		}
	}
	return ""
}

// latencyOf parses a duration field off a tint line.
func latencyOf(t *testing.T, line, key string) time.Duration {
	t.Helper()
	raw := tintAttr(line, key)
	if raw == "" {
		t.Fatalf("line missing %s: %s", key, line)
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("%s=%q: %v", key, raw, err)
	}
	return d
}

// The one shared script: first content after 200ms, usage and EOF 300ms
// later. 120 output tokens over a ~300ms generation span is ~400 tps.
const latencyBody = `{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`

var latencyScript = [][]timedEvent{{
	{wait: 200 * time.Millisecond, typ: "assistantResponseEvent", payload: `{"content":"hello world"}`},
	{wait: 300 * time.Millisecond, typ: "meteringEvent", payload: `{"usage":0.1,"inputTokens":100,"outputTokens":120}`},
}}

func assertLatencyLine(t *testing.T, line string) {
	t.Helper()
	assertAttrOrder(t, line,
		"trace_id", "session_id", "status", "input_tokens", "output_tokens",
		"context_usage", "ttft", "tps", "duration")
	if ttft := latencyOf(t, line, "ttft"); ttft < 150*time.Millisecond {
		t.Errorf("ttft = %s, want >= 150ms (first event after 200ms)", ttft)
	}
	if tps, err := strconv.ParseFloat(tintAttr(line, "tps"), 64); err != nil || tps < 150 || tps > 600 {
		t.Errorf("tps = %q, want 120 tokens over ~300-800ms (150-600)", tintAttr(line, "tps"))
	}
	if d := latencyOf(t, line, "duration"); d < 450*time.Millisecond {
		t.Errorf("duration = %s, want >= 450ms (500ms of scripted delays)", d)
	}
}

func TestHandleMessages_KiroStreamingLatency(t *testing.T) {
	buf := captureTint(t, slog.LevelInfo)
	s := newKiroService(t, &timedKiroClient{scripts: latencyScript})

	body := strings.Replace(latencyBody, `"max_tokens":1024`, `"stream":true,"max_tokens":1024`, 1)
	w := postCustomMessages(t, s, body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}

	assertLatencyLine(t, findLine(t, buf, "<-- POST /v1/messages"))
}

func TestHandleMessages_KiroNonStreamingLatency(t *testing.T) {
	buf := captureTint(t, slog.LevelInfo)
	s := newKiroService(t, &timedKiroClient{scripts: latencyScript})

	w := postCustomMessages(t, s, latencyBody, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}

	assertLatencyLine(t, findLine(t, buf, "<-- POST /v1/messages"))
}

// TestHandleMessages_KiroRetryLatency asserts the retry semantics: the start
// stays at the first request, but the first byte comes from the attempt the
// client actually receives. Attempt 1 returns thinking-only content quickly
// (marking a first byte that must be discarded); attempt 2 answers 200ms in.
func TestHandleMessages_KiroRetryLatency(t *testing.T) {
	buf := captureTint(t, slog.LevelInfo)
	client := &timedKiroClient{scripts: [][]timedEvent{
		{{wait: 50 * time.Millisecond, typ: "reasoningContentEvent", payload: `{"text":"discarded thinking"}`}},
		{
			{wait: 200 * time.Millisecond, typ: "assistantResponseEvent", payload: `{"content":"retry answer"}`},
			{wait: 100 * time.Millisecond, typ: "meteringEvent", payload: `{"usage":0.1,"inputTokens":100,"outputTokens":60}`},
		},
	}}
	s := newKiroService(t, client)

	w := postCustomMessages(t, s, latencyBody, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}

	if !strings.Contains(buf.String(), "retrying upstream request") {
		t.Fatalf("the scripted empty response should have triggered a retry; logs:\n%s", buf.String())
	}
	line := findLine(t, buf, "<-- POST /v1/messages")
	// Without the per-attempt reset the ttft would be ~50ms (attempt 1's
	// thinking); with it, ttft covers attempt 1's whole lifetime plus
	// attempt 2's delay.
	if ttft := latencyOf(t, line, "ttft"); ttft < 200*time.Millisecond {
		t.Errorf("ttft = %s, want >= 200ms (the discarded attempt's first byte must not count)", ttft)
	}
}

// TestHandleMessages_ServerToolRetryLatency asserts the same reset for the
// server-tool orchestrator's outer retry: the first run's thinking-only
// response is discarded, so its first byte must not count either.
func TestHandleMessages_ServerToolRetryLatency(t *testing.T) {
	buf := captureTint(t, slog.LevelInfo)
	client := &timedKiroClient{scripts: [][]timedEvent{
		{{wait: 50 * time.Millisecond, typ: "reasoningContentEvent", payload: `{"text":"discarded thinking"}`}},
		{
			{wait: 200 * time.Millisecond, typ: "assistantResponseEvent", payload: `{"content":"retry answer"}`},
			{wait: 100 * time.Millisecond, typ: "meteringEvent", payload: `{"usage":0.1,"inputTokens":100,"outputTokens":60}`},
		},
	}}
	s := newKiroService(t, client)

	w := postCustomMessages(t, s,
		`{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[{"role":"user","content":"hi"}],`+
			`"tools":[{"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"}]}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}

	if !strings.Contains(buf.String(), "retrying server tool loop after empty visible end_turn") {
		t.Fatalf("expected the server tool outer retry; logs:\n%s", buf.String())
	}
	line := findLine(t, buf, "<-- POST /v1/messages")
	if ttft := latencyOf(t, line, "ttft"); ttft < 200*time.Millisecond {
		t.Errorf("ttft = %s, want >= 200ms (the discarded run's first byte must not count)", ttft)
	}
}
