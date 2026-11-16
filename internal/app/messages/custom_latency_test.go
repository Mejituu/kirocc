package messages

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestCustomLatency_Streaming asserts the custom path's streaming latency
// fields: the first content_block_delta marks ttft, and tps covers the span
// from it to the end of the stream.
func TestCustomLatency_Streaming(t *testing.T) {
	buf := captureTint(t, slog.LevelInfo)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = w.Write([]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":50,"output_tokens":0}}}` + "\n\n"))
		flusher.Flush()
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}` + "\n\n"))
		flusher.Flush()
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(`data: {"type":"message_delta","delta":{},"usage":{"output_tokens":120}}` + "\n\n"))
	}))
	defer upstream.Close()

	s, _ := newCustomService(t, upstream.URL, true)
	w := postCustomMessages(t, s,
		`{"model":"claude-glm-5.3","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"X-Claude-Code-Session-Id": "sess-1234567890abcdef"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}

	line := findLine(t, buf, "<-- POST /v1/messages")
	assertAttrOrder(t, line,
		"trace_id", "session_id", "provider", "status", "input_tokens",
		"output_tokens", "context_usage", "ttft", "tps", "duration")
	if ttft := latencyOf(t, line, "ttft"); ttft < 150*time.Millisecond {
		t.Errorf("ttft = %s, want >= 150ms (first delta after 200ms)", ttft)
	}
	if tps, err := strconv.ParseFloat(tintAttr(line, "tps"), 64); err != nil || tps < 300 || tps > 1200 {
		t.Errorf("tps = %q, want 120 tokens over ~200-400ms (300-1200)", tintAttr(line, "tps"))
	}
	if d := latencyOf(t, line, "duration"); d < 350*time.Millisecond {
		t.Errorf("duration = %s, want >= 350ms (400ms of scripted delays)", d)
	}
	if strings.Contains(line, "tps_total") {
		t.Errorf("a streaming response must not report tps_total: %s", line)
	}
}

// TestCustomLatency_NonStreaming asserts the custom path's non-streaming
// fields: no ttft, and tps_total measured over the whole request.
func TestCustomLatency_NonStreaming(t *testing.T) {
	buf := captureTint(t, slog.LevelInfo)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(`{"type":"message","usage":{"input_tokens":50,"output_tokens":120}}`))
	}))
	defer upstream.Close()

	s, _ := newCustomService(t, upstream.URL, true)
	w := postCustomMessages(t, s,
		`{"model":"claude-glm-5.3","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}

	line := findLine(t, buf, "<-- POST /v1/messages")
	assertAttrOrder(t, line,
		"trace_id", "provider", "status", "input_tokens",
		"output_tokens", "context_usage", "tps_total", "duration")
	if strings.Contains(line, "ttft=") || strings.Contains(line, "tps=") {
		t.Errorf("non-streaming must report neither ttft nor tps: %s", line)
	}
	if tps, err := strconv.ParseFloat(tintAttr(line, "tps_total"), 64); err != nil || tps < 200 || tps > 2000 {
		t.Errorf("tps_total = %q, want 120 tokens over ~100-600ms (200-2000)", tintAttr(line, "tps_total"))
	}
	if d := latencyOf(t, line, "duration"); d < 50*time.Millisecond {
		t.Errorf("duration = %s, want to cover the upstream delay", d)
	}
}

// TestCustomLatency_ErrorHasDurationOnly asserts an upstream error drops the
// ttft/tps fields while duration is still logged.
func TestCustomLatency_ErrorHasDurationOnly(t *testing.T) {
	buf := captureTint(t, slog.LevelInfo)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}` + "\n\n"))
		flusher.Flush()
	}))
	defer upstream.Close()

	s, _ := newCustomService(t, upstream.URL, true)
	postCustomMessages(t, s,
		`{"model":"claude-glm-5.3","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, nil)

	line := findLine(t, buf, "<-- POST /v1/messages")
	for _, unwanted := range []string{"ttft=", "tps=", "tps_total="} {
		if strings.Contains(line, unwanted) {
			t.Errorf("an error response must not report %q: %s", unwanted, line)
		}
	}
	if !strings.Contains(line, "duration=") {
		t.Errorf("duration should still be logged: %s", line)
	}
}
