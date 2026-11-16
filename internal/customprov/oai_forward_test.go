package customprov

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/d-kuro/kirocc/internal/httpx"
	"github.com/d-kuro/kirocc/internal/logging"
)

// oaiTestModel is the requested model ID; the configured model id it maps to
// is "ZHIPU/GLM-5.3".
const oaiTestModel = "claude-ZHIPU/GLM-5.3"

// newOpenAIProxy builds a proxy over a registry whose single provider speaks
// the openai protocol, pointing at the test upstream.
func newOpenAIProxy(t *testing.T, upstream *httptest.Server) *Proxy {
	t.Helper()
	reg, err := loadTestRegistry(t, `{
		"providers": {
			"zhipu": {
				"base_url": "`+upstream.URL+`",
				"auth_token": "secret-token",
				"protocol": "openai",
				"models": ["ZHIPU/GLM-5.3[1m]"]
			}
		}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	return NewProxy(reg, 5*time.Second)
}

// newOpenAIWebSearchProxy builds a proxy over a registry whose single
// openai-protocol model opts into web_search, pointing at the test upstream.
func newOpenAIWebSearchProxy(t *testing.T, upstream *httptest.Server) *Proxy {
	t.Helper()
	reg, err := loadTestRegistry(t, `{
		"providers": {
			"zhipu": {
				"base_url": "`+upstream.URL+`",
				"auth_token": "secret-token",
				"protocol": "openai",
				"models": [{"id": "ZHIPU/GLM-5.3", "web_search": true}]
			}
		}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	return NewProxy(reg, 5*time.Second)
}

// forwardOpenAISend sends raw through the proxy like forward in
// proxy_test.go, with the stream flag of RequestInfo set from stream.
func forwardOpenAISend(t *testing.T, p *Proxy, stream bool, clientHeaders http.Header, raw string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	hit, ok := p.Lookup(oaiTestModel)
	if !ok {
		t.Fatalf("Lookup(%q) did not hit", oaiTestModel)
	}
	r := httptest.NewRequest(http.MethodPost, "http://kirocc.local/v1/messages", strings.NewReader(raw))
	r.Header = clientHeaders
	p.Forward(context.Background(), w, r.Header, []byte(raw), hit, RequestInfo{Model: oaiTestModel, Stream: stream}, EndpointMessages)
	return w
}

// captureLogs redirects slog.Default to a buffer at the given level for the
// duration of the test, like captureSlog in the messages package.
func captureLogs(t *testing.T, level slog.Level) *strings.Builder {
	t.Helper()
	var buf strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(logging.NewOTelHandler(&buf, level)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

// findLogRecord returns the first JSON-Lines record whose body matches, like
// findRecord in the messages package.
func findLogRecord(t *testing.T, buf interface{ String() string }, body string) map[string]any {
	t.Helper()
	for line := range strings.SplitSeq(buf.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["body"] == body {
			return rec
		}
	}
	t.Fatalf("no log record with body=%q in output:\n%s", body, buf.String())
	return nil
}

// oaiAnthropicRequest is a request exercising the conversion: thinking on
// with a budget, one user turn and one function tool.
const oaiAnthropicRequest = `{
	"model": "claude-ZHIPU/GLM-5.3",
	"max_tokens": 1024,
	"thinking": {"type": "enabled", "budget_tokens": 512},
	"messages": [{"role": "user", "content": [{"type": "text", "text": "hello"}]}],
	"tools": [{
		"name": "get_weather",
		"description": "Get the weather of a city",
		"input_schema": {"type": "object", "properties": {"city": {"type": "string"}}}
	}]
}`

// oaiChatCompletion is the upstream's non-streaming answer: reasoning, text,
// a tool call and a usage object with cache traffic.
const oaiChatCompletion = `{
	"id": "chatcmpl-test",
	"model": "ZHIPU/GLM-5.3",
	"choices": [{
		"index": 0,
		"message": {
			"role": "assistant",
			"reasoning_content": "let me check the weather",
			"content": "It is sunny.",
			"tool_calls": [{
				"id": "call_1",
				"type": "function",
				"function": {"name": "get_weather", "arguments": "{\"city\":\"Beijing\"}"}
			}]
		},
		"finish_reason": "tool_calls"
	}],
	"usage": {"prompt_tokens": 1000, "completion_tokens": 50, "prompt_tokens_details": {"cached_tokens": 400}}
}`

func TestForwardOpenAI_NonStream(t *testing.T) {
	var gotPath string
	var gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(oaiChatCompletion))
	}))
	defer upstream.Close()

	w := forwardOpenAISend(t, newOpenAIProxy(t, upstream), false, http.Header{}, oaiAnthropicRequest)

	if gotPath != "/chat/completions" {
		t.Errorf("upstream path = %q, want /chat/completions", gotPath)
	}
	var req map[string]any
	if err := json.Unmarshal([]byte(gotBody), &req); err != nil {
		t.Fatalf("upstream body is not JSON: %s", gotBody)
	}
	if req["model"] != "ZHIPU/GLM-5.3" {
		t.Errorf("upstream model = %v, want ZHIPU/GLM-5.3; body: %s", req["model"], gotBody)
	}
	if req["enable_thinking"] != true {
		t.Errorf("upstream enable_thinking = %v, want true; body: %s", req["enable_thinking"], gotBody)
	}
	if req["thinking_budget"] != float64(512) {
		t.Errorf("upstream thinking_budget = %v, want 512; body: %s", req["thinking_budget"], gotBody)
	}
	if req["stream"] == true {
		t.Errorf("non-stream request must not set stream true; body: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"name":"get_weather"`) {
		t.Errorf("upstream body missing the converted tool; body: %s", gotBody)
	}
	// The tool schema must ride inline as a JSON object; a []byte field
	// would marshal as a base64 string instead.
	if !strings.Contains(gotBody, `"parameters":{"type":"object","properties":{"city":{"type":"string"}}}`) {
		t.Errorf("upstream parameters not inline JSON; body: %s", gotBody)
	}

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var resp struct {
		Type       string `json:"type"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type     string         `json:"type"`
			Thinking string         `json:"thinking,omitempty"`
			Text     string         `json:"text,omitempty"`
			ID       string         `json:"id,omitempty"`
			Name     string         `json:"name,omitempty"`
			Input    map[string]any `json:"input,omitempty"`
		} `json:"content"`
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("client body is not JSON: %s", w.Body.String())
	}
	if resp.Type != "message" || resp.StopReason != "tool_use" {
		t.Errorf("type/stop_reason = %q/%q, want message/tool_use", resp.Type, resp.StopReason)
	}
	if len(resp.Content) != 3 {
		t.Fatalf("content blocks = %d, want 3 (thinking, text, tool_use); body: %s", len(resp.Content), w.Body.String())
	}
	if resp.Content[0].Type != "thinking" || resp.Content[0].Thinking != "let me check the weather" {
		t.Errorf("first block = %+v, want the thinking block", resp.Content[0])
	}
	if resp.Content[1].Type != "text" || resp.Content[1].Text != "It is sunny." {
		t.Errorf("second block = %+v, want the text block", resp.Content[1])
	}
	if resp.Content[2].Type != "tool_use" || resp.Content[2].Name != "get_weather" || resp.Content[2].Input["city"] != "Beijing" {
		t.Errorf("third block = %+v, want the tool_use block", resp.Content[2])
	}
	// usage is the Anthropic view: input excludes the 400 cached tokens.
	if resp.Usage.InputTokens != 600 || resp.Usage.OutputTokens != 50 || resp.Usage.CacheReadInputTokens != 400 {
		t.Errorf("usage = %+v, want input 600, output 50, cache_read 400", resp.Usage)
	}
}

func TestForwardOpenAI_Stream(t *testing.T) {
	var gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []string{
			`{"id":"chatcmpl-1","model":"ZHIPU/GLM-5.3","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"thinking hard"}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"Hello"}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"ci"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"Beijing\"}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":800,"completion_tokens":42,"prompt_tokens_details":{"cached_tokens":100}}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", chunk)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstream.Close()

	w := forwardOpenAISend(t, newOpenAIProxy(t, upstream), true, http.Header{},
		strings.Replace(oaiAnthropicRequest, `"max_tokens": 1024,`, `"max_tokens": 1024,"stream": true,`, 1))

	var req map[string]any
	if err := json.Unmarshal([]byte(gotBody), &req); err != nil {
		t.Fatalf("upstream body is not JSON: %s", gotBody)
	}
	if req["stream"] != true {
		t.Errorf("upstream stream = %v, want true; body: %s", req["stream"], gotBody)
	}
	if so, ok := req["stream_options"].(map[string]any); !ok || so["include_usage"] != true {
		t.Errorf("upstream stream_options = %v, want include_usage true; body: %s", req["stream_options"], gotBody)
	}

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	body := w.Body.String()
	for _, want := range []string{
		"event: message_start",
		"event: message_stop",
		"thinking hard",
		"Hello",
		"get_weather",
		"Beijing",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("client stream missing %q; body:\n%s", want, body)
		}
	}
	// The event order follows the Anthropic stream shape: message_start
	// before the content events, message_stop last.
	if start, stop := strings.Index(body, "event: message_start"), strings.Index(body, "event: message_stop"); !(start < stop) {
		t.Errorf("message_start must precede message_stop; body:\n%s", body)
	}
}

func TestForwardOpenAI_UpstreamError(t *testing.T) {
	for _, tc := range []struct {
		status      int
		errType     string
		upstream    string
		wantMessage string
	}{
		{http.StatusBadRequest, "invalid_request_error", `{"error":{"message":"bad model"}}`, "bad model"},
		{http.StatusInternalServerError, "api_error", `{"error":{"message":"upstream exploded"}}`, "upstream exploded"},
	} {
		t.Run(fmt.Sprintf("status %d", tc.status), func(t *testing.T) {
			buf := captureLogs(t, slog.LevelInfo)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.upstream))
			}))
			defer upstream.Close()

			w := forwardOpenAISend(t, newOpenAIProxy(t, upstream), false, http.Header{}, oaiAnthropicRequest)

			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d (passed through); body: %s", w.Code, tc.status, w.Body.String())
			}
			var payload struct {
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
				t.Fatalf("client error body is not JSON: %s", w.Body.String())
			}
			if payload.Error.Type != tc.errType {
				t.Errorf("error.type = %q, want %q", payload.Error.Type, tc.errType)
			}
			if !strings.Contains(payload.Error.Message, tc.wantMessage) {
				t.Errorf("error.message = %q, want it to carry the upstream message %q", payload.Error.Message, tc.wantMessage)
			}
			rec := findLogRecord(t, buf, "custom provider error")
			attrs, _ := rec["attributes"].(map[string]any)
			if attrs["provider"] != "zhipu" {
				t.Errorf("warning provider = %v, want zhipu", attrs["provider"])
			}
			if !strings.Contains(fmt.Sprint(attrs["body"]), tc.wantMessage) {
				t.Errorf("warning body = %v, want the upstream body", attrs["body"])
			}
		})
	}
}

// TestOAIErrorType covers every row of the status-to-type mapping plus the
// default fallback, so a drifted mapping fails here rather than in the wild.
func TestOAIErrorType(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{400, httpx.ErrTypeInvalidRequest},
		{422, httpx.ErrTypeInvalidRequest},
		{401, httpx.ErrTypeAuthentication},
		{402, httpx.ErrTypeBilling},
		{403, httpx.ErrTypePermission},
		{404, httpx.ErrTypeNotFound},
		{413, httpx.ErrTypeRequestTooLarge},
		{429, httpx.ErrTypeRateLimit},
		{529, httpx.ErrTypeOverloaded},
		{500, httpx.ErrTypeAPI},
		{503, httpx.ErrTypeAPI},
	}
	for _, tc := range cases {
		if got := oaiErrorType(tc.status); got != tc.want {
			t.Errorf("oaiErrorType(%d) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

// TestForwardOpenAI_UpstreamErrorRetryAfter pins the Retry-After pass-through:
// a rate-limited upstream's backoff hint reaches the client verbatim next to
// the rate_limit_error type, and an upstream without the header produces a
// response without it.
func TestForwardOpenAI_UpstreamErrorRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name        string
		upstreamSet bool
		wantHeader  string
	}{
		{name: "retry-after forwarded", upstreamSet: true, wantHeader: "7"},
		{name: "no upstream header", upstreamSet: false, wantHeader: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if tc.upstreamSet {
					w.Header().Set("Retry-After", "7")
				}
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			}))
			defer upstream.Close()

			w := forwardOpenAISend(t, newOpenAIProxy(t, upstream), false, http.Header{}, oaiAnthropicRequest)

			if w.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429; body: %s", w.Code, w.Body.String())
			}
			if got := w.Header().Get("Retry-After"); got != tc.wantHeader {
				t.Errorf("Retry-After = %q, want %q", got, tc.wantHeader)
			}
			var payload struct {
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
				t.Fatalf("client error body is not JSON: %s", w.Body.String())
			}
			if payload.Error.Type != "rate_limit_error" {
				t.Errorf("error.type = %q, want rate_limit_error", payload.Error.Type)
			}
			if payload.Error.Message != "rate limited" {
				t.Errorf("error.message = %q, want the upstream message", payload.Error.Message)
			}
		})
	}
}

// TestForwardOpenAI_DocumentDroppedWarn pins the document-replacement
// warning: one WRN with provider and count, and none of the document's
// payload reaching any log.
func TestForwardOpenAI_DocumentDroppedWarn(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(oaiChatCompletion))
	}))
	defer upstream.Close()

	buf := captureLogs(t, slog.LevelInfo)
	// Recognizable if it leaks; the real thing is a PDF's base64.
	const docData = "aGVsbG8gZG9jdW1lbnQh"
	raw := `{
		"model": "claude-ZHIPU/GLM-5.3",
		"max_tokens": 1024,
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "read these"},
				{"type": "document", "source": {"type": "base64", "media_type": "application/pdf", "data": "` + docData + `"}},
				{"type": "document", "source": {"type": "url", "url": "https://example.com/doc.pdf"}}
			]}
		]
	}`
	w := forwardOpenAISend(t, newOpenAIProxy(t, upstream), false, http.Header{}, raw)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}

	rec := findLogRecord(t, buf, "custom provider document dropped")
	attrs, _ := rec["attributes"].(map[string]any)
	if attrs["provider"] != "zhipu" {
		t.Errorf("warning provider = %v, want zhipu", attrs["provider"])
	}
	if attrs["count"] != float64(2) {
		t.Errorf("warning count = %v, want 2", attrs["count"])
	}
	if strings.Contains(buf.String(), docData) {
		t.Errorf("document payload leaked into logs:\n%s", buf.String())
	}
}

func TestForwardOpenAI_ServerToolRejected(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called for a server-tool request")
	}))
	defer upstream.Close()

	raw := `{
		"model": "claude-ZHIPU/GLM-5.3",
		"max_tokens": 1024,
		"messages": [{"role": "user", "content": "search the web"}],
		"tools": [{"type": "web_search_20250305", "name": "web_search", "max_uses": 5}]
	}`
	w := forwardOpenAISend(t, newOpenAIProxy(t, upstream), false, http.Header{}, raw)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("error body is not JSON: %s", w.Body.String())
	}
	if payload.Error.Type != "invalid_request_error" {
		t.Errorf("error.type = %q, want invalid_request_error", payload.Error.Type)
	}
	for _, want := range []string{"server-side tools", "anthropic", "--disallowedTools"} {
		if !strings.Contains(payload.Error.Message, want) {
			t.Errorf("error.message = %q, want it to mention %q", payload.Error.Message, want)
		}
	}
}

// assertOpenAIServerToolRejected checks the 400 the openai path answers a
// server-tool request with: the invalid_request_error type and the hint to
// disallow the tool, unchanged from before web_search got its escape hatch.
func assertOpenAIServerToolRejected(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("error body is not JSON: %s", w.Body.String())
	}
	if payload.Error.Type != "invalid_request_error" {
		t.Errorf("error.type = %q, want invalid_request_error", payload.Error.Type)
	}
	for _, want := range []string{"server-side tools", "anthropic", "--disallowedTools"} {
		if !strings.Contains(payload.Error.Message, want) {
			t.Errorf("error.message = %q, want it to mention %q", payload.Error.Message, want)
		}
	}
}

// TestForwardOpenAI_WebSearchEnableSearch pins the openai path's web-search
// stage on the wire: a web_search tool on a model that opted in no longer
// rejects the request — the declaration becomes enable_search with a forced
// search instead of a tools entry, the function tools still convert, and the
// INF line records mode=enable_search. forwardPeeked decodes the RequestInfo
// from the body like maybeForwardCustom, so the tool detection runs as in
// production.
func TestForwardOpenAI_WebSearchEnableSearch(t *testing.T) {
	var gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(oaiChatCompletion))
	}))
	defer upstream.Close()

	buf := captureLogs(t, slog.LevelInfo)
	raw := `{
		"model": "claude-ZHIPU/GLM-5.3",
		"max_tokens": 1024,
		"messages": [{"role": "user", "content": "search the web"}],
		"tools": [` + webSearchToolJSON + `, {
			"name": "get_weather",
			"description": "Get the weather of a city",
			"input_schema": {"type": "object", "properties": {"city": {"type": "string"}}}
		}]
	}`
	w := forwardPeeked(t, newOpenAIWebSearchProxy(t, upstream), oaiTestModel, raw)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("client body is not JSON: %s", w.Body.String())
	}
	if resp.Type != "message" {
		t.Errorf("client type = %q, want message; body: %s", resp.Type, w.Body.String())
	}

	var req map[string]any
	if err := json.Unmarshal([]byte(gotBody), &req); err != nil {
		t.Fatalf("upstream body is not JSON: %s", gotBody)
	}
	if req["enable_search"] != true {
		t.Errorf("upstream enable_search = %v, want true; body: %s", req["enable_search"], gotBody)
	}
	so, ok := req["search_options"].(map[string]any)
	if !ok || so["forced_search"] != true {
		t.Errorf("upstream search_options = %v, want forced_search true; body: %s", req["search_options"], gotBody)
	}
	// web_search rides the switch above, not the tools array: only the
	// function tool converts (the server-side definition has no schema).
	tools, ok := req["tools"].([]any)
	if !ok {
		t.Fatalf("upstream tools is not an array; body: %s", gotBody)
	}
	if len(tools) != 1 {
		t.Fatalf("upstream tools = %d entries, want only the function tool; body: %s", len(tools), gotBody)
	}
	tool, _ := tools[0].(map[string]any)
	fn, _ := tool["function"].(map[string]any)
	if tool["type"] != "function" || fn["name"] != "get_weather" {
		t.Errorf("upstream tool = %v, want the get_weather function tool; body: %s", tools[0], gotBody)
	}

	rec := findLogRecord(t, buf, "web search enabled")
	attrs, _ := rec["attributes"].(map[string]any)
	if attrs["provider"] != "zhipu" {
		t.Errorf("log provider = %v, want zhipu", attrs["provider"])
	}
	if attrs["mode"] != "enable_search" {
		t.Errorf("log mode = %v, want enable_search", attrs["mode"])
	}
}

// TestForwardOpenAI_WebSearchNotRequested: a model that opted in but a request
// without the web_search tool converts plainly — no enable_search, no
// search_options, and no "web search enabled" line.
func TestForwardOpenAI_WebSearchNotRequested(t *testing.T) {
	var gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(oaiChatCompletion))
	}))
	defer upstream.Close()

	buf := captureLogs(t, slog.LevelInfo)
	w := forwardPeeked(t, newOpenAIWebSearchProxy(t, upstream), oaiTestModel, oaiAnthropicRequest)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	var req map[string]any
	if err := json.Unmarshal([]byte(gotBody), &req); err != nil {
		t.Fatalf("upstream body is not JSON: %s", gotBody)
	}
	if req["enable_search"] != nil {
		t.Errorf("upstream enable_search = %v, want absent; body: %s", req["enable_search"], gotBody)
	}
	if req["search_options"] != nil {
		t.Errorf("upstream search_options = %v, want absent; body: %s", req["search_options"], gotBody)
	}
	// The request line proves the capture works; the point is that no
	// "web search enabled" record joins it.
	findLogRecord(t, buf, "--> POST /v1/messages")
	if strings.Contains(buf.String(), `"web search enabled"`) {
		t.Errorf("unexpected web search enabled record:\n%s", buf.String())
	}
}

// TestForwardOpenAI_WebSearchModelDisabled: the web_search tool on a model
// that did not opt in (newOpenAIProxy's string shorthand) stays a plain
// server-tool rejection — the openai path cannot stand in for the Anthropic
// backend's search.
func TestForwardOpenAI_WebSearchModelDisabled(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called for a server-tool request")
	}))
	defer upstream.Close()

	raw := `{
		"model": "claude-ZHIPU/GLM-5.3",
		"max_tokens": 1024,
		"messages": [{"role": "user", "content": "search the web"}],
		"tools": [` + webSearchToolJSON + `]
	}`
	w := forwardPeeked(t, newOpenAIProxy(t, upstream), oaiTestModel, raw)
	assertOpenAIServerToolRejected(t, w)
}

// TestForwardOpenAI_WebSearchWithOtherServerTool: a tool_search definition
// next to the web_search tool still rejects the request — only web_search has
// a native escape hatch, and it does not open one for its server-side kin.
func TestForwardOpenAI_WebSearchWithOtherServerTool(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called for a server-tool request")
	}))
	defer upstream.Close()

	raw := `{
		"model": "claude-ZHIPU/GLM-5.3",
		"max_tokens": 1024,
		"messages": [{"role": "user", "content": "search the web"}],
		"tools": [` + webSearchToolJSON + `, {"type": "tool_search_tool_regex_20251119", "name": "tool_search_tool_regex"}]
	}`
	w := forwardPeeked(t, newOpenAIWebSearchProxy(t, upstream), oaiTestModel, raw)
	assertOpenAIServerToolRejected(t, w)
}

func TestForwardOpenAI_NoClientHeadersForwarded(t *testing.T) {
	var gotHeaders http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(oaiChatCompletion))
	}))
	defer upstream.Close()

	clientHeaders := http.Header{}
	clientHeaders.Set("Content-Type", "application/json")
	clientHeaders.Set("Accept", "text/event-stream")
	clientHeaders.Set("User-Agent", "claude-cli/2.0")
	clientHeaders.Set("X-Api-Key", "kirocc-key")
	clientHeaders.Set("Anthropic-Version", "2023-06-01")
	clientHeaders.Set("Anthropic-Beta", "context-1m-2025-10-22")
	clientHeaders.Set("Cookie", "session=1")

	w := forwardOpenAISend(t, newOpenAIProxy(t, upstream), false, clientHeaders, oaiAnthropicRequest)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}

	if got, want := gotHeaders.Get("Authorization"), "Bearer secret-token"; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
	if gotHeaders.Get("Accept") != "application/json" {
		t.Errorf("Accept = %q, want the openai path's own application/json", gotHeaders.Get("Accept"))
	}
	if gotHeaders.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotHeaders.Get("Content-Type"))
	}
	for name := range gotHeaders {
		switch name {
		// The transport adds these itself; anything else came from the
		// client request and must not have been forwarded.
		case "Authorization", "Accept", "Content-Type", "Accept-Encoding", "Content-Length", "Host", "User-Agent":
			continue
		default:
			t.Errorf("unexpected forwarded header %q: %v", name, gotHeaders.Get(name))
		}
	}
	if gotHeaders.Get("User-Agent") == "claude-cli/2.0" {
		t.Errorf("client User-Agent must not be forwarded, got %q", gotHeaders.Get("User-Agent"))
	}
	for _, name := range []string{"X-Api-Key", "Anthropic-Version", "Anthropic-Beta", "Cookie"} {
		if gotHeaders.Get(name) != "" {
			t.Errorf("client header %s must not be forwarded, got %q", name, gotHeaders.Get(name))
		}
	}
}

func TestForwardOpenAI_DebugRequestBodyLogged(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(oaiChatCompletion))
	}))
	defer upstream.Close()

	buf := captureLogs(t, slog.LevelDebug)
	w := forwardOpenAISend(t, newOpenAIProxy(t, upstream), false, http.Header{}, oaiAnthropicRequest)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}

	rec := findLogRecord(t, buf, "custom provider request body")
	attrs, ok := rec["attributes"].(map[string]any)
	if !ok {
		t.Fatalf("attributes missing; record: %v", rec)
	}
	requestBody, ok := attrs["request_body"].(map[string]any)
	if !ok {
		t.Fatalf("request_body attribute is not a JSON object: %v", attrs["request_body"])
	}
	if requestBody["model"] != "ZHIPU/GLM-5.3" {
		t.Errorf("request body log model = %v, want ZHIPU/GLM-5.3", requestBody["model"])
	}
	if requestBody["enable_thinking"] != true {
		t.Errorf("request body log enable_thinking = %v, want true", requestBody["enable_thinking"])
	}
	if strings.Contains(buf.String(), "secret-token") {
		t.Errorf("auth token leaked into logs:\n%s", buf.String())
	}
}
