package messages

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lmittmann/tint"

	"github.com/d-kuro/kirocc/internal/logging"
)

// captureSlogAtLevel redirects slog.Default to a buffer at the given level.
func captureSlogAtLevel(t *testing.T, level slog.Level) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(logging.NewOTelHandler(&buf, level)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

// captureTint redirects slog.Default to an order-preserving tint handler, so
// tests can assert attribute order; the OTel handler sorts attributes by key.
// Colors are off so the raw lines stay greppable.
func captureTint(t *testing.T, level slog.Level) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(tint.NewHandler(&buf, &tint.Options{Level: level, NoColor: true})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

// findLine returns the first raw tint log line carrying the message. tint
// prints the message unquoted after the level marker.
func findLine(t *testing.T, buf *bytes.Buffer, msg string) string {
	t.Helper()
	for line := range strings.SplitSeq(buf.String(), "\n") {
		if strings.Contains(line, msg) {
			return line
		}
	}
	t.Fatalf("no log line with msg=%q in output:\n%s", msg, buf.String())
	return ""
}

// assertAttrOrder checks that the key=value attributes appear on the line in
// the given order.
func assertAttrOrder(t *testing.T, line string, keys ...string) {
	t.Helper()
	prev := -1
	for _, key := range keys {
		idx := strings.Index(line, key+"=")
		if idx < 0 {
			t.Fatalf("line missing attribute %q: %s", key, line)
		}
		if idx < prev {
			t.Fatalf("attribute %q out of order: %s", key, line)
		}
		prev = idx
	}
}

// postCustomMessages sends a request body through the service's messages
// handler and returns the recorded response.
func postCustomMessages(t *testing.T, s *Service, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.HandleMessages(w, r)
	return w
}

// TestHandleMessages_CustomRequestLog asserts the "--> POST /v1/messages"
// line: field presence and order, the three thinking labels, and the context
// window derived from the configured model name rather than the request.
func TestHandleMessages_CustomRequestLog(t *testing.T) {
	tests := []struct {
		name string
		body string
		// wantThinking is the tint rendering of the thinking attribute.
		wantThinking      string
		wantContextWindow string
	}{
		{
			name: "thinking enabled with effort and budget_tokens",
			body: `{"model":"claude-glm-5.3","stream":true,"max_tokens":16,` +
				`"thinking":{"type":"enabled","budget_tokens":2048},` +
				`"output_config":{"effort":"high"},` +
				`"messages":[{"role":"user","content":"hi"}]}`,
			wantThinking:      "thinking=high",
			wantContextWindow: "context_window=1M",
		},
		{
			name: "thinking enabled without effort",
			body: `{"model":"claude-glm-5.3","stream":false,"max_tokens":16,` +
				`"thinking":{"type":"enabled","budget_tokens":1024},` +
				`"messages":[{"role":"user","content":"hi"}]}`,
			wantThinking:      "thinking=enabled",
			wantContextWindow: "context_window=1M",
		},
		{
			name: "thinking absent",
			body: `{"model":"claude-glm-5.3","stream":false,"max_tokens":16,` +
				`"messages":[{"role":"user","content":"hi"}]}`,
			wantThinking:      "thinking=false",
			wantContextWindow: "context_window=1M",
		},
		{
			name: "configured model without 1m marker",
			body: `{"model":"claude-kimi-k3","stream":false,"max_tokens":16,` +
				`"messages":[{"role":"user","content":"hi"}]}`,
			wantThinking:      "thinking=false",
			wantContextWindow: "context_window=200k",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"type":"message"}`))
			}))
			defer upstream.Close()

			s, _ := newCustomService(t, upstream.URL, true)
			buf := captureTint(t, slog.LevelDebug)
			w := postCustomMessages(t, s, tt.body, map[string]string{
				"X-Claude-Code-Session-Id": "sess-1234567890abcdef",
			})
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
			}

			line := findLine(t, buf, "--> POST /v1/messages")
			assertAttrOrder(t, line,
				"trace_id", "session_id", "provider", "model",
				"upstream_model", "thinking", "stream", "context_window")
			if !strings.Contains(line, tt.wantThinking) {
				t.Errorf("thinking label: line %s missing %q", line, tt.wantThinking)
			}
			if !strings.Contains(line, tt.wantContextWindow) {
				t.Errorf("context window: line %s missing %q", line, tt.wantContextWindow)
			}
			if !strings.Contains(line, "session_id="+logging.ShortID("sess-1234567890abcdef")) {
				t.Errorf("session_id should be the shortened header value: %s", line)
			}
			// The request spelled the model without the [1m] marker while the
			// config carries it: the window is still 1M (asserted above), and
			// the provider still receives the base name.
			if !strings.Contains(line, "upstream_model=glm-5.3") && !strings.Contains(line, "upstream_model=kimi-k3") {
				t.Errorf("upstream_model should be the base name: %s", line)
			}
			if strings.Contains(buf.String(), "secret-token") {
				t.Errorf("auth token leaked into logs:\n%s", buf.String())
			}
		})
	}
}

// TestHandleMessages_CustomLogSessionOptional asserts session_id appears only
// when the request carried the session header.
func TestHandleMessages_CustomLogSessionOptional(t *testing.T) {
	tests := []struct {
		name        string
		sessionID   string
		wantSession bool
	}{
		{name: "with session header", sessionID: "sess-1234567890abcdef", wantSession: true},
		{name: "without session header", sessionID: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"type":"message"}`))
			}))
			defer upstream.Close()

			s, _ := newCustomService(t, upstream.URL, true)
			buf := captureSlogAtLevel(t, slog.LevelInfo)
			headers := map[string]string{}
			if tt.sessionID != "" {
				headers["X-Claude-Code-Session-Id"] = tt.sessionID
			}
			w := postCustomMessages(t, s, `{"model":"claude-glm-5.3","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, headers)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d", w.Code)
			}

			for _, body := range []string{"--> POST /v1/messages", "<-- POST /v1/messages"} {
				rec := findRecord(t, buf, body)
				attrs, ok := rec["attributes"].(map[string]any)
				if !ok {
					t.Fatalf("%s: attributes missing", body)
				}
				_, has := attrs["session_id"]
				if has != tt.wantSession {
					t.Errorf("%s: session_id present = %v, want %v", body, has, tt.wantSession)
				}
			}
		})
	}
}

// TestHandleMessages_CustomResponseLogAfterBodyRead asserts the "<--" line is
// logged only after the response body is fully read: the upstream delays its
// second chunk, and the logged duration covers that delay.
func TestHandleMessages_CustomResponseLogAfterBodyRead(t *testing.T) {
	buf := captureSlogAtLevel(t, slog.LevelInfo)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = w.Write([]byte("event: one\ndata: first\n\n"))
		flusher.Flush()
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte("event: two\ndata: second\n\n"))
	}))
	defer upstream.Close()

	s, _ := newCustomService(t, upstream.URL, true)
	srv := httptest.NewServer(http.HandlerFunc(s.HandleMessages))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-glm-5.3","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "first") || !strings.Contains(string(body), "second") {
		t.Fatalf("client should receive both chunks: %q", string(body))
	}

	logs := buf.String()
	if idx := strings.Index(logs, "--> POST /v1/messages"); idx < 0 || idx > strings.Index(logs, "<-- POST /v1/messages") {
		t.Errorf("the request line should precede the response line; logs:\n%s", logs)
	}
	rec := findRecord(t, buf, "<-- POST /v1/messages")
	attrs := rec["attributes"].(map[string]any)
	duration, err := time.ParseDuration(attrs["duration"].(string))
	if err != nil {
		t.Fatalf("duration %q: %v", attrs["duration"], err)
	}
	if duration < 250*time.Millisecond {
		t.Errorf("duration = %s, want it to cover the upstream delay (>= 250ms)", duration)
	}
	if _, has := attrs["status"]; !has {
		t.Errorf("response line missing status: %v", attrs)
	}
}

// TestHandleMessages_CustomUpstreamErrorLog asserts the passthrough of an
// upstream error: the client gets the full body, and a warning carries the
// status plus the first 2048 bytes of the body.
func TestHandleMessages_CustomUpstreamErrorLog(t *testing.T) {
	buf := captureSlogAtLevel(t, slog.LevelInfo)
	errorBody := strings.Repeat("x", 3000)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(errorBody))
	}))
	defer upstream.Close()

	s, _ := newCustomService(t, upstream.URL, true)
	w := postCustomMessages(t, s, `{"model":"claude-glm-5.3","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, nil)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 passed through", w.Code)
	}
	if w.Body.Len() != len(errorBody) {
		t.Errorf("client body length = %d, want %d", w.Body.Len(), len(errorBody))
	}

	logs := buf.String()
	rec := findRecord(t, buf, "custom provider error")
	attrs := rec["attributes"].(map[string]any)
	if attrs["status"] != float64(http.StatusBadRequest) {
		t.Errorf("warning status = %v, want 400", attrs["status"])
	}
	cached, _ := attrs["body"].(string)
	if len(cached) != 2048 {
		t.Errorf("cached body length = %d, want 2048 (truncated)", len(cached))
	}
	// The completion line precedes the warning.
	if strings.Index(logs, "<-- POST /v1/messages") > strings.Index(logs, "custom provider error") {
		t.Errorf("the response line should precede the error warning; logs:\n%s", logs)
	}
	if strings.Contains(logs, "secret-token") {
		t.Errorf("auth token leaked into logs:\n%s", logs)
	}
}

// TestHandleCountTokens_NoDefaultLevelLogs asserts count_tokens stays quiet at
// the default level — no request/response pair — while upstream errors still
// warn.
func TestHandleCountTokens_NoDefaultLevelLogs(t *testing.T) {
	t.Run("success has no log pair", func(t *testing.T) {
		buf := captureSlogAtLevel(t, slog.LevelInfo)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"input_tokens":42}`))
		}))
		defer upstream.Close()

		s, _ := newCustomService(t, upstream.URL, true)
		r := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(
			`{"model":"claude-glm-5.3","messages":[{"role":"user","content":"hi"}]}`))
		w := httptest.NewRecorder()
		s.HandleCountTokens(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d", w.Code)
		}
		if strings.Contains(buf.String(), "--> POST") || strings.Contains(buf.String(), "<-- POST") {
			t.Errorf("count_tokens must not log the request/response pair; logs:\n%s", buf.String())
		}
	})

	t.Run("upstream error still warns", func(t *testing.T) {
		buf := captureSlogAtLevel(t, slog.LevelInfo)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"type":"error"}`))
		}))
		defer upstream.Close()

		s, _ := newCustomService(t, upstream.URL, true)
		r := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(
			`{"model":"claude-glm-5.3","messages":[{"role":"user","content":"hi"}]}`))
		w := httptest.NewRecorder()
		s.HandleCountTokens(w, r)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 passed through", w.Code)
		}
		if !strings.Contains(buf.String(), "custom provider error") {
			t.Errorf("upstream error should warn; logs:\n%s", buf.String())
		}
		if strings.Contains(buf.String(), "--> POST") || strings.Contains(buf.String(), "<-- POST") {
			t.Errorf("count_tokens must not log the request/response pair; logs:\n%s", buf.String())
		}
	})
}

// TestHandleMessages_CustomDebugLogs asserts the debug trio mirrors the Kiro
// path, and nothing of it at the default level.
func TestHandleMessages_CustomDebugLogs(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message"}`))
	}))
	defer upstream.Close()

	t.Run("debug level logs body, request and response headers", func(t *testing.T) {
		buf := captureSlog(t) // debug level
		s, _ := newCustomService(t, upstream.URL, true)
		w := postCustomMessages(t, s,
			`{"model":"claude-glm-5.3","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
			map[string]string{"X-Claude-Code-Session-Id": "sess-1234567890abcdef"})
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d", w.Code)
		}

		logs := buf.String()
		for _, want := range []string{
			"client request body",
			"client request headers",
			"client response headers",
		} {
			if !strings.Contains(logs, want) {
				t.Errorf("debug logs missing %q; logs:\n%s", want, logs)
			}
		}
		// Order mirrors the Kiro path: body, request headers, response
		// headers.
		bodyIdx := strings.Index(logs, "client request body")
		reqHdrIdx := strings.Index(logs, "client request headers")
		respHdrIdx := strings.Index(logs, "client response headers")
		if !(bodyIdx < reqHdrIdx && reqHdrIdx < respHdrIdx) {
			t.Errorf("debug logs out of order; logs:\n%s", logs)
		}

		bodyRec := findRecord(t, buf, "client request body")
		attrs := bodyRec["attributes"].(map[string]any)
		// The OTel handler unmarshals jsontext.Value into any, so a JSON
		// object body arrives as a map.
		requestBody, ok := attrs["request_body"].(map[string]any)
		if !ok {
			t.Fatalf("request body attribute is not a JSON object: %v", attrs["request_body"])
		}
		if requestBody["model"] != "claude-glm-5.3" {
			t.Errorf("request body log missing the model; body: %v", requestBody)
		}
		reqHdrRec := findRecord(t, buf, "client request headers")
		attrs = reqHdrRec["attributes"].(map[string]any)
		if attrs["session_id"] != "sess-1234567890abcdef" {
			t.Errorf("request headers log session_id = %v, want the full header value", attrs["session_id"])
		}
		if _, ok := attrs["headers"].(map[string]any); !ok {
			t.Errorf("request headers log missing headers attribute: %v", attrs)
		}
		if strings.Contains(logs, "secret-token") {
			t.Errorf("auth token leaked into logs:\n%s", logs)
		}
	})

	t.Run("default level logs none of them", func(t *testing.T) {
		buf := captureSlogAtLevel(t, slog.LevelInfo)
		s, _ := newCustomService(t, upstream.URL, true)
		w := postCustomMessages(t, s,
			`{"model":"claude-glm-5.3","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d", w.Code)
		}
		logs := buf.String()
		for _, unwanted := range []string{"client request body", "client request headers", "client response headers"} {
			if strings.Contains(logs, unwanted) {
				t.Errorf("default level should not log %q; logs:\n%s", unwanted, logs)
			}
		}
	})

	t.Run("count_tokens logs only the request body", func(t *testing.T) {
		buf := captureSlog(t) // debug level
		s, _ := newCustomService(t, upstream.URL, true)
		r := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(
			`{"model":"claude-glm-5.3","messages":[{"role":"user","content":"hi"}]}`))
		w := httptest.NewRecorder()
		s.HandleCountTokens(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
		}
		logs := buf.String()
		if !strings.Contains(logs, "client request body") {
			t.Errorf("count_tokens should log the request body at debug; logs:\n%s", logs)
		}
		for _, unwanted := range []string{"client request headers", "client response headers"} {
			if strings.Contains(logs, unwanted) {
				t.Errorf("count_tokens should not log %q; logs:\n%s", unwanted, logs)
			}
		}
	})
}

// TestHandleMessages_PeekWithFullThinkingFields asserts a request whose
// thinking/output_config carry fields beyond the peeked strings still routes
// to the custom provider.
func TestHandleMessages_PeekWithFullThinkingFields(t *testing.T) {
	var gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		_, _ = w.Write([]byte(`{"type":"message"}`))
	}))
	defer upstream.Close()

	s, authMgr := newCustomService(t, upstream.URL, true)
	w := postCustomMessages(t, s,
		`{"model":"claude-glm-5.3","stream":true,"max_tokens":16,`+
			`"thinking":{"type":"enabled","budget_tokens":8192},`+
			`"output_config":{"effort":"max","unknown":true},`+
			`"messages":[{"role":"user","content":"hi"}]}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(gotBody, `"model":"glm-5.3"`) {
		t.Errorf("request should be forwarded with the base model name: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"budget_tokens":8192`) {
		t.Errorf("thinking subfields must survive the peek and forward: %s", gotBody)
	}
	if authMgr.count() != 0 {
		t.Errorf("GetToken called %d times on the custom path", authMgr.count())
	}
}
