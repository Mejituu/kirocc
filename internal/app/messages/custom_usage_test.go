package messages

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// sseUpstream streams the given events with flushes between them and records
// every byte it writes, so the client's copy can be compared for equality.
// The record is mutex-guarded: the upstream handler writes from its own
// goroutine, and the test reads only after the bytes reached the client —
// ordering the race detector cannot see across the network hops.
func sseUpstream(t *testing.T, events ...string) (*httptest.Server, *logBuffer) {
	t.Helper()
	written := &logBuffer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, ev := range events {
			_, _ = written.Write([]byte(ev))
			_, _ = w.Write([]byte(ev))
			flusher.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv, written
}

// sseEvent renders one SSE event frame.
func sseEvent(payload string) string {
	return "event: message\ndata: " + payload + "\n\n"
}

// postStreamMessages issues a streaming request through a real server and
// returns the response body the client received.
func postStreamMessages(t *testing.T, s *Service, model string) ([]byte, *http.Response) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(s.HandleMessages))
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(
		`{"model":`+strconv.Quote(model)+`,"stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Claude-Code-Session-Id", "sess-1234567890abcdef")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return body, resp
}

// completionRecord returns the "<-- POST /v1/messages" log record's
// attributes.
func completionRecord(t *testing.T, buf interface{ String() string }) map[string]any {
	t.Helper()
	rec := findRecord(t, buf, "<-- POST /v1/messages")
	attrs, ok := rec["attributes"].(map[string]any)
	if !ok {
		t.Fatalf("completion line has no attributes: %v", rec)
	}
	return attrs
}

// TestHandleMessages_SSEUsage asserts a streamed SSE response's usage lands
// in the completion line and the client receives the exact upstream bytes.
func TestHandleMessages_SSEUsage(t *testing.T) {
	upstream, written := sseUpstream(t,
		sseEvent(`{"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":900,"cache_creation_input_tokens":90,"output_tokens":3}}}`),
		sseEvent(`{"type":"content_block_delta","delta":{"text":"hi"}}`),
		sseEvent(`{"type":"message_delta","usage":{"output_tokens":42}}`),
		sseEvent(`{"type":"message_stop"}`),
	)

	buf := captureTint(t, slog.LevelInfo)
	s, _ := newCustomService(t, upstream.URL, true)
	clientBody, resp := postStreamMessages(t, s, "claude-glm-5.3")

	if !bytes.Equal(clientBody, written.Bytes()) {
		t.Errorf("client body differs from upstream bytes:\nclient: %q\nupstream: %q", clientBody, written.Bytes())
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("client Content-Type = %q", ct)
	}

	line := findLine(t, buf, "<-- POST /v1/messages")
	assertAttrOrder(t, line,
		"trace_id", "session_id", "provider", "status",
		"input_tokens", "output_tokens", "context_usage", "duration")
	if !strings.Contains(line, "input_tokens=1000") {
		t.Errorf("input_tokens should be 1000 (input + cache_read + cache_creation): %s", line)
	}
	if !strings.Contains(line, "output_tokens=42") {
		t.Errorf("output_tokens should be 42 (message_delta): %s", line)
	}
	// 1000 tokens against the 1M window of glm-5.3[1m]: 0.1%.
	if !strings.Contains(line, "context_usage=1.0k(0.1%)") {
		t.Errorf("context_usage should render as 1.0k(0.1%%): %s", line)
	}
}

// TestHandleMessages_JSONUsage asserts a non-streaming response's usage and a
// byte-identical body.
func TestHandleMessages_JSONUsage(t *testing.T) {
	body := `{"id":"msg_1","type":"message","usage":{"input_tokens":10,"cache_read_input_tokens":900,"cache_creation_input_tokens":90,"output_tokens":7}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer upstream.Close()

	buf := captureSlogAtLevel(t, slog.LevelInfo)
	s, _ := newCustomService(t, upstream.URL, true)
	w := postCustomMessages(t, s,
		`{"model":"claude-glm-5.3","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if w.Body.String() != body {
		t.Errorf("client body differs: %q", w.Body.String())
	}

	attrs := completionRecord(t, buf)
	if attrs["input_tokens"] != float64(1000) {
		t.Errorf("input_tokens = %v, want 1000", attrs["input_tokens"])
	}
	if attrs["output_tokens"] != float64(7) {
		t.Errorf("output_tokens = %v, want 7", attrs["output_tokens"])
	}
	if attrs["context_usage"] != "1.0k(0.1%)" {
		t.Errorf("context_usage = %v, want 1.0k(0.1%%)", attrs["context_usage"])
	}
}

// TestHandleMessages_NoUsageOmitsFields asserts the three usage fields are
// absent when the response carries no usage, in both SSE and JSON shapes and
// for error statuses.
func TestHandleMessages_NoUsageOmitsFields(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "sse without usage events",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("event: message\ndata: {\"type\":\"message_stop\"}\n\n"))
			},
		},
		{
			name: "json without usage",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"type":"message"}`))
			},
		},
		{
			name: "json error status",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"type":"error"}`))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(tt.handler)
			defer upstream.Close()

			buf := captureTint(t, slog.LevelInfo)
			s, _ := newCustomService(t, upstream.URL, true)
			postStreamMessages(t, s, "claude-glm-5.3")

			line := findLine(t, buf, "<-- POST /v1/messages")
			for _, field := range []string{"input_tokens", "output_tokens", "context_usage"} {
				if strings.Contains(line, field+"=") {
					t.Errorf("completion line should omit %s when no usage was seen: %s", field, line)
				}
			}
			if !strings.Contains(line, "status=") || !strings.Contains(line, "duration=") {
				t.Errorf("completion line should keep status and duration: %s", line)
			}
		})
	}
}

// TestHandleMessages_ContextLimitWarning asserts the warning fires only when
// the usage fills the model's advertised window: 200k of input warns for
// kimi-k3 (200k) but not for glm-5.3[1m] (1M).
func TestHandleMessages_ContextLimitWarning(t *testing.T) {
	sseBody := sseEvent(`{"type":"message_start","message":{"usage":{"input_tokens":200000,"output_tokens":5}}}`)

	t.Run("200k model warns", func(t *testing.T) {
		upstream, _ := sseUpstream(t, sseBody)
		buf := captureSlogAtLevel(t, slog.LevelInfo)
		s, _ := newCustomService(t, upstream.URL, true)
		postStreamMessages(t, s, "claude-kimi-k3")

		logs := buf.String()
		rec := findRecord(t, buf, "context limit exceeded")
		attrs := rec["attributes"].(map[string]any)
		if attrs["context_usage"] != "200.0k(100.0%)" {
			t.Errorf("context_usage = %v, want 200.0k(100.0%%)", attrs["context_usage"])
		}
		if strings.Index(logs, "<-- POST /v1/messages") > strings.Index(logs, "context limit exceeded") {
			t.Errorf("warning should follow the completion line; logs:\n%s", logs)
		}
	})

	t.Run("1M model stays quiet", func(t *testing.T) {
		upstream, _ := sseUpstream(t, sseBody)
		buf := captureSlogAtLevel(t, slog.LevelInfo)
		s, _ := newCustomService(t, upstream.URL, true)
		postStreamMessages(t, s, "claude-glm-5.3")

		if strings.Contains(buf.String(), "context limit exceeded") {
			t.Errorf("200k of input against a 1M window should not warn; logs:\n%s", buf.String())
		}
		rec := findRecord(t, buf, "<-- POST /v1/messages")
		attrs := rec["attributes"].(map[string]any)
		if attrs["input_tokens"] != float64(200000) {
			t.Errorf("input_tokens = %v, want 200000", attrs["input_tokens"])
		}
	})
}
