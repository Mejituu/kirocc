package customprov

import (
	"context"
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/d-kuro/kirocc/internal/anthropic"
)

// newTestProxy builds a proxy over a registry matching validConfig.
func newTestProxy(t *testing.T, upstream *httptest.Server) *Proxy {
	t.Helper()
	reg, err := loadTestRegistry(t, `{
		"providers": {
			"bailian": {
				"base_url": "`+upstream.URL+`",
				"auth_token": "secret-token",
				"models": ["glm-5.3[1m]"]
			}
		}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	return NewProxy(reg, 5*time.Second)
}

// forward sends a minimal request through the proxy and returns the recorded
// client response.
func forward(t *testing.T, p *Proxy, upstream *httptest.Server, model string, clientHeaders http.Header, raw string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	hit, ok := p.Lookup(model)
	if !ok {
		t.Fatalf("Lookup(%q) did not hit", model)
	}
	r := httptest.NewRequest(http.MethodPost, "http://kirocc.local/v1/messages", strings.NewReader(raw))
	r.Header = clientHeaders
	p.Forward(context.Background(), w, r.Header, []byte(raw), hit, RequestInfo{Model: model, Stream: true}, EndpointMessages)
	return w
}

func TestForward_TextOnlyReplacesMedia(t *testing.T) {
	var gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	reg, err := loadTestRegistry(t, `{"providers": {"p": {
		"base_url": "`+upstream.URL+`", "auth_token": "t",
		"models": [{"id": "glm-5.3", "text_only": true}]
	}}}`)
	if err != nil {
		t.Fatal(err)
	}
	p := NewProxy(reg, 5*time.Second)
	hit, ok := p.Lookup("claude-glm-5.3")
	if !ok {
		t.Fatal("Lookup missed")
	}
	raw := `{"model":"claude-glm-5.3","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}]}]}`
	w := httptest.NewRecorder()
	p.Forward(context.Background(), w, http.Header{}, []byte(raw), hit, RequestInfo{Model: "claude-glm-5.3"}, EndpointMessages)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(gotBody, "[image omitted]") || strings.Contains(gotBody, "aGk=") {
		t.Errorf("upstream body = %s, want the image replaced", gotBody)
	}
}

func TestForward_HeaderWhitelistAndAuth(t *testing.T) {
	var gotHeaders http.Header
	var gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	clientHeaders := http.Header{}
	clientHeaders.Set("Content-Type", "application/json")
	clientHeaders.Set("Accept", "text/event-stream")
	clientHeaders.Set("User-Agent", "claude-cli/2.0")
	clientHeaders.Set("Authorization", "Bearer kirocc-key")
	clientHeaders.Set("X-Api-Key", "kirocc-key")
	clientHeaders.Set("Cookie", "session=1")
	clientHeaders.Set("Anthropic-Version", "2023-06-01")
	clientHeaders.Set("Anthropic-Beta", "context-1m-2025-10-22")

	w := forward(t, newTestProxy(t, upstream), upstream, "claude-glm-5.3",
		clientHeaders, `{"model":"claude-glm-5.3","stream":true,"messages":[]}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if got, want := gotHeaders.Get("Authorization"), "Bearer secret-token"; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
	if gotHeaders.Get("X-Api-Key") != "" {
		t.Errorf("client X-Api-Key must not be forwarded, got %q", gotHeaders.Get("X-Api-Key"))
	}
	if gotHeaders.Get("Cookie") != "" {
		t.Errorf("client Cookie must not be forwarded")
	}
	for _, name := range []string{"Content-Type", "Accept", "User-Agent", "Anthropic-Version", "Anthropic-Beta"} {
		if gotHeaders.Get(name) == "" {
			t.Errorf("header %s was not forwarded", name)
		}
	}
	if !strings.Contains(gotBody, `"model":"glm-5.3"`) {
		t.Errorf("upstream body model was not rewritten: %s", gotBody)
	}
}

func TestForward_UpstreamErrorPassedThrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad model"}}`))
	}))
	defer upstream.Close()

	w := forward(t, newTestProxy(t, upstream), upstream, "claude-glm-5.3", http.Header{}, `{"model":"claude-glm-5.3","messages":[]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (passed through)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "bad model") {
		t.Errorf("upstream error body was not passed through: %s", w.Body.String())
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "application/json") {
		t.Errorf("upstream Content-Type was not passed through")
	}
}

func TestForward_SetCookieStripped(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom", "keep-me")
		w.Header().Add("Set-Cookie", "session=secret; Path=/")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	w := forward(t, newTestProxy(t, upstream), upstream, "claude-glm-5.3", http.Header{}, `{"model":"claude-glm-5.3","messages":[]}`)
	if w.Header().Get("X-Custom") != "keep-me" {
		t.Errorf("X-Custom header was not passed through")
	}
	if w.Header().Get("Set-Cookie") != "" {
		t.Errorf("Set-Cookie must not be passed through")
	}
}

func TestForward_ConnectionFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := upstream.URL
	upstream.Close() // refuse connections

	p := newTestProxy(t, httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))
	// Point the registry at the closed server.
	reg, err := loadTestRegistry(t, `{"providers":{"bailian":{"base_url":"`+url+`","auth_token":"t","models":["glm-5.3[1m]"]}}}`)
	if err != nil {
		t.Fatal(err)
	}
	p = NewProxy(reg, 5*time.Second)

	w := forward(t, p, nil, "claude-glm-5.3", http.Header{}, `{"model":"claude-glm-5.3","messages":[]}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "bailian") {
		t.Errorf("error should name the provider: %s", body)
	}
	if strings.Contains(body, "secret-token") {
		t.Errorf("error leaked the auth token: %s", body)
	}
}

func TestForward_ModelNotString(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	w := forward(t, newTestProxy(t, upstream), upstream, "claude-glm-5.3", http.Header{}, `{"model":42,"messages":[]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(w.Body.String())), &payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload.Error.Message, "model") {
		t.Errorf("error should explain the model problem: %s", payload.Error.Message)
	}
}

// TestForward_StripsBillingHeaderFromSystem asserts the anthropic-path
// rewrite on the wire: the upstream body carries no billing block, and
// everything else — key order, the cache_control block, number spellings —
// arrives exactly as the client sent it (modulo the model swap).
func TestForward_StripsBillingHeaderFromSystem(t *testing.T) {
	var gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	raw := `{"model":"claude-glm-5.3","system":[{"type":"text","text":"x-anthropic-billing-header: cch=abc"},{"type":"text","text":"You are helpful.","cache_control":{"type":"ephemeral"}}],"temperature":0.5,"big":12345678901234567890,"messages":[]}`
	w := forward(t, newTestProxy(t, upstream), upstream, "claude-glm-5.3", http.Header{}, raw)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	want := `{"model":"glm-5.3","system":[{"type":"text","text":"You are helpful.","cache_control":{"type":"ephemeral"}}],"temperature":0.5,"big":12345678901234567890,"messages":[]}` + "\n"
	if gotBody != want {
		t.Errorf("upstream body:\n got: %s\nwant: %s", gotBody, want)
	}
}

// webSearchToolJSON is the web_search server-side tool definition as Claude
// Code sends it.
const webSearchToolJSON = `{"type":"web_search_20250305","name":"web_search","max_uses":5}`

// fixedBillingBlock is the stable billing-header block RewriteForward
// inserts in place of the request's per-request one when web search runs
// natively on the provider.
const fixedBillingBlock = `{"type":"text","text":"x-anthropic-billing-header: cc_entrypoint=cli;"}`

// newWebSearchProxy builds a proxy over a registry whose single model opts
// into web_search (the object models form), pointing at the test upstream.
func newWebSearchProxy(t *testing.T, upstream *httptest.Server) *Proxy {
	t.Helper()
	reg, err := loadTestRegistry(t, `{
		"providers": {
			"bailian": {
				"base_url": "`+upstream.URL+`",
				"auth_token": "secret-token",
				"models": [{"id": "glm-5.3", "web_search": true}]
			}
		}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	return NewProxy(reg, 5*time.Second)
}

// forwardPeeked forwards raw with the RequestInfo decoded from the body —
// the same peek maybeForwardCustom runs before Forward — so the tools
// detection runs as in production.
func forwardPeeked(t *testing.T, p *Proxy, model, raw string) *httptest.ResponseRecorder {
	t.Helper()
	var info RequestInfo
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		t.Fatalf("peek decode failed: %v", err)
	}
	w := httptest.NewRecorder()
	hit, ok := p.Lookup(model)
	if !ok {
		t.Fatalf("Lookup(%q) did not hit", model)
	}
	r := httptest.NewRequest(http.MethodPost, "http://kirocc.local/v1/messages", strings.NewReader(raw))
	p.Forward(context.Background(), w, r.Header, []byte(raw), hit, info, EndpointMessages)
	return w
}

// TestForward_WebSearchSwapsBillingBlock pins the A1 trigger on the wire: a
// web_search tool on a model that opted in makes the rewrite replace the
// request's per-request billing block with the stable fixed one, while every
// other byte — key order, the cache_control block, number spellings, the
// tools array — arrives exactly as the client sent it (modulo the model
// swap).
func TestForward_WebSearchSwapsBillingBlock(t *testing.T) {
	var gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	raw := `{"model":"claude-glm-5.3","tools":[` + webSearchToolJSON + `],"system":[{"type":"text","text":"x-anthropic-billing-header: cch=abc"},{"type":"text","text":"You are helpful.","cache_control":{"type":"ephemeral"}}],"temperature":0.5,"big":12345678901234567890,"messages":[]}`
	w := forwardPeeked(t, newWebSearchProxy(t, upstream), "claude-glm-5.3", raw)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	want := `{"model":"glm-5.3","tools":[` + webSearchToolJSON + `],"system":[` + fixedBillingBlock + `,{"type":"text","text":"You are helpful.","cache_control":{"type":"ephemeral"}}],"temperature":0.5,"big":12345678901234567890,"messages":[]}` + "\n"
	if gotBody != want {
		t.Errorf("upstream body:\n got: %s\nwant: %s", gotBody, want)
	}
}

// TestForward_WebSearchAppendsSystem asserts the no-system case: the fixed
// block lands as a trailing "system" member instead of nowhere.
func TestForward_WebSearchAppendsSystem(t *testing.T) {
	var gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	raw := `{"model":"claude-glm-5.3","tools":[` + webSearchToolJSON + `],"messages":[]}`
	w := forwardPeeked(t, newWebSearchProxy(t, upstream), "claude-glm-5.3", raw)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	want := `{"model":"glm-5.3","tools":[` + webSearchToolJSON + `],"messages":[],"system":[` + fixedBillingBlock + `]}` + "\n"
	if gotBody != want {
		t.Errorf("upstream body:\n got: %s\nwant: %s", gotBody, want)
	}
}

// TestForward_WebSearchNotRequested: a request without the web_search tool
// passes through with no fixed block — byte-identical to the plain rewrite —
// even though the model opted in.
func TestForward_WebSearchNotRequested(t *testing.T) {
	var gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	raw := `{"model":"claude-glm-5.3","tools":[{"name":"get_weather","input_schema":{"type":"object"}}],"system":[{"type":"text","text":"You are helpful."}],"messages":[]}`
	w := forwardPeeked(t, newWebSearchProxy(t, upstream), "claude-glm-5.3", raw)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	want := `{"model":"glm-5.3","tools":[{"name":"get_weather","input_schema":{"type":"object"}}],"system":[{"type":"text","text":"You are helpful."}],"messages":[]}` + "\n"
	if gotBody != want {
		t.Errorf("upstream body:\n got: %s\nwant: %s", gotBody, want)
	}
}

// TestForward_WebSearchDisabledForModel: a model declared without web_search
// (newTestProxy's string shorthand) forwards the tool definition verbatim;
// the fixed block is not inserted.
func TestForward_WebSearchDisabledForModel(t *testing.T) {
	var gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	raw := `{"model":"claude-glm-5.3","tools":[` + webSearchToolJSON + `],"system":[{"type":"text","text":"You are helpful."}],"messages":[]}`
	w := forwardPeeked(t, newTestProxy(t, upstream), "claude-glm-5.3", raw)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	want := `{"model":"glm-5.3","tools":[` + webSearchToolJSON + `],"system":[{"type":"text","text":"You are helpful."}],"messages":[]}` + "\n"
	if gotBody != want {
		t.Errorf("upstream body:\n got: %s\nwant: %s", gotBody, want)
	}
}

// TestForward_WebSearchLog pins the A1 INF line: one "web search enabled"
// record with provider and mode=native when the trigger hits, and none when
// the request carries no web_search tool.
func TestForward_WebSearchLog(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	t.Run("enabled", func(t *testing.T) {
		buf := captureLogs(t, slog.LevelInfo)
		raw := `{"model":"claude-glm-5.3","tools":[` + webSearchToolJSON + `],"messages":[]}`
		if w := forwardPeeked(t, newWebSearchProxy(t, upstream), "claude-glm-5.3", raw); w.Code != http.StatusOK {
			t.Fatalf("status = %d", w.Code)
		}
		rec := findLogRecord(t, buf, "web search enabled")
		attrs, _ := rec["attributes"].(map[string]any)
		if attrs["provider"] != "bailian" {
			t.Errorf("log provider = %v, want bailian", attrs["provider"])
		}
		if attrs["mode"] != "native" {
			t.Errorf("log mode = %v, want native", attrs["mode"])
		}
	})

	t.Run("not requested", func(t *testing.T) {
		buf := captureLogs(t, slog.LevelInfo)
		raw := `{"model":"claude-glm-5.3","tools":[{"name":"get_weather","input_schema":{"type":"object"}}],"messages":[]}`
		if w := forwardPeeked(t, newWebSearchProxy(t, upstream), "claude-glm-5.3", raw); w.Code != http.StatusOK {
			t.Fatalf("status = %d", w.Code)
		}
		// The request line proves the capture works; the point is that no
		// "web search enabled" record joins it.
		findLogRecord(t, buf, "--> POST /v1/messages")
		if strings.Contains(buf.String(), `"web search enabled"`) {
			t.Errorf("unexpected web search enabled record:\n%s", buf.String())
		}
	})
}

// TestRequestInfo_HasWebSearch covers the trigger's request side: a
// web_search definition (peeked or built directly) reports true; plain
// function tools or no tools report false.
func TestRequestInfo_HasWebSearch(t *testing.T) {
	def := RequestInfo{Tools: []anthropic.Tool{{
		Type: anthropic.ToolTypeWebSearch, Name: anthropic.WebSearchToolName,
	}}}
	if !def.HasWebSearch() {
		t.Error("HasWebSearch with a web_search definition = false, want true")
	}
	callable := RequestInfo{Tools: []anthropic.Tool{{
		Name:        "get_weather",
		InputSchema: map[string]any{"type": "object"},
	}}}
	if callable.HasWebSearch() {
		t.Error("HasWebSearch with a function tool only = true, want false")
	}
	if (RequestInfo{}).HasWebSearch() {
		t.Error("HasWebSearch without tools = true, want false")
	}

	// The peek decode carries the tools, like maybeForwardCustom's.
	var peeked RequestInfo
	if err := json.Unmarshal([]byte(`{"model":"claude-glm-5.3","tools":[`+webSearchToolJSON+`]}`), &peeked); err != nil {
		t.Fatal(err)
	}
	if !peeked.HasWebSearch() {
		t.Error("peeked HasWebSearch = false, want true")
	}
}
