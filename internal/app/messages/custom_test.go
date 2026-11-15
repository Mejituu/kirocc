package messages

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/d-kuro/kirocc/internal/auth"
	"github.com/d-kuro/kirocc/internal/customprov"
	"github.com/d-kuro/kirocc/internal/kiroclient"
	"github.com/d-kuro/kirocc/internal/kiroproto"
)

// countingAuth counts GetToken calls so tests can prove the custom path never
// asks for Kiro credentials.
type countingAuth struct {
	mu     sync.Mutex
	calls  int
	creds  *auth.Credentials
	failed bool
}

func (c *countingAuth) GetToken(ctx context.Context) (*auth.Credentials, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.failed {
		return nil, fmt.Errorf("countingAuth: GetToken must not be called on the custom path")
	}
	return c.creds, nil
}

func (c *countingAuth) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// tripwireClient fails the test if the Kiro path is reached.
type tripwireClient struct {
	t *testing.T
}

func (c *tripwireClient) GenerateAssistantResponse(ctx context.Context, token string, payload *kiroproto.Payload, region string) (*kiroclient.Response, error) {
	c.t.Errorf("Kiro client must not be called for a custom provider model")
	return nil, fmt.Errorf("unexpected Kiro call")
}

// recordingKiroClient records that the Kiro path was reached.
type recordingKiroClient struct {
	called *bool
}

func (c *recordingKiroClient) GenerateAssistantResponse(ctx context.Context, token string, payload *kiroproto.Payload, region string) (*kiroclient.Response, error) {
	*c.called = true
	return nil, fmt.Errorf("stop here")
}

// newCustomService builds a Service wired to an upstream provider and returns
// both. The provider declares glm-5.3 (count_tokens configurable) and, with
// localCount set to false, count_tokens forwarding.
func newCustomService(t *testing.T, upstreamURL string, countTokens bool) (*Service, *countingAuth) {
	t.Helper()
	t.Setenv("KIROCC_MODEL_MAPPINGS", "")
	path := filepath.Join(t.TempDir(), "mejituu.json")
	countField := ""
	if !countTokens {
		countField = `"count_tokens":false,`
	}
	content := fmt.Sprintf(`{"providers":[{"name":"bailian","base_url":%q,%s"auth_token":"secret-token","models":["glm-5.3[1m]"]}]}`, upstreamURL, countField)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := customprov.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	authMgr := &countingAuth{creds: &auth.Credentials{AccessToken: "kiro-token", ProfileARN: "arn:test", Region: "us-east-1"}, failed: true}
	s := New(authMgr, &tripwireClient{t: t}, WithCustomProviders(customprov.NewProxy(reg, 5*time.Second)))
	return s, authMgr
}

func TestHandleMessages_CustomModelForwarded(t *testing.T) {
	var gotBody string
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"glm-5.3","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn"}`))
	}))
	defer upstream.Close()

	s, authMgr := newCustomService(t, upstream.URL, true)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"claude-glm-5.3[1m]","max_tokens":16,"temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("X-Api-Key", "kirocc-key")
	w := httptest.NewRecorder()
	s.HandleMessages(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/messages" {
		t.Errorf("upstream path = %q, want /v1/messages", gotPath)
	}
	if !strings.Contains(gotBody, `"model":"glm-5.3"`) {
		t.Errorf("model was not rewritten to the base name: %s", gotBody)
	}
	// A field anthropic.Request does not model must survive the pass.
	if !strings.Contains(gotBody, `"temperature":0.7`) {
		t.Errorf("unknown field temperature was dropped: %s", gotBody)
	}
	if authMgr.count() != 0 {
		t.Errorf("GetToken called %d times on the custom path", authMgr.count())
	}
	if !strings.Contains(w.Body.String(), "hello") {
		t.Errorf("upstream response body was not passed through: %s", w.Body.String())
	}
}

func TestHandleMessages_NonCustomModelStillHitsKiroAuth(t *testing.T) {
	var upstreamCalled bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
	}))
	defer upstream.Close()

	// The tripwire client fails the test if reached, so build the service with
	// a client that records the call instead.
	path := filepath.Join(t.TempDir(), "mejituu.json")
	if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"providers":[{"name":"bailian","base_url":%q,"auth_token":"t","models":["glm-5.3[1m]"]}]}`, upstream.URL)), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := customprov.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	var kiroCalled bool
	client := &recordingKiroClient{called: &kiroCalled}
	authMgr := &countingAuth{creds: &auth.Credentials{AccessToken: "kiro-token", ProfileARN: "arn:test", Region: "us-east-1"}}
	s := New(authMgr, client, WithCustomProviders(customprov.NewProxy(reg, 5*time.Second)))

	// claude-unknown-model is claude-prefixed but not a custom model, and
	// models.Resolve passes it through to Kiro rather than refusing.
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"claude-unknown-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	s.HandleMessages(w, r)

	if upstreamCalled {
		t.Error("upstream provider must not be called for an unmatched model")
	}
	if !kiroCalled {
		t.Error("Kiro client should be reached for an unmatched model")
	}
	if authMgr.count() == 0 {
		t.Error("Kiro path should have requested credentials")
	}
}

func TestHandleMessages_CustomModelStreaming(t *testing.T) {
	// The upstream writes one SSE event, flushes, and waits for the client to
	// acknowledge before writing the second — proving chunked delivery rather
	// than full buffering.
	secondWrite := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"delta\":\"first\"}\n\n"))
		flusher.Flush()
		<-secondWrite
		_, _ = w.Write([]byte("event: message_stop\ndata: {}\n\n"))
	}))
	defer upstream.Close()

	s, _ := newCustomService(t, upstream.URL, true)
	srv := httptest.NewServer(http.HandlerFunc(s.HandleMessages))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(
		`{"model":"claude-glm-5.3","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	buf := make([]byte, 512)
	n, err := resp.Body.Read(buf)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	first := string(buf[:n])
	if !strings.Contains(first, "first") {
		t.Fatalf("first chunk missing first event: %q", first)
	}
	close(secondWrite)
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rest), "message_stop") {
		t.Fatalf("second event missing: %q", string(rest))
	}
}

func TestHandleMessages_CustomModelMaxBody(t *testing.T) {
	var upstreamCalled bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
	}))
	defer upstream.Close()

	s, _ := newCustomService(t, upstream.URL, true)
	s.maxRequestBody = 64
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body(4096)))
	r.Header.Set("X-Claude-Code-Session-Id", "sess")
	w := httptest.NewRecorder()
	s.HandleMessages(w, r)

	if upstreamCalled {
		t.Error("upstream must not be called for an over-sized body")
	}
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", w.Code)
	}
	if !strings.Contains(w.Body.String(), "request body exceeds the 64 byte limit") {
		t.Errorf("over-limit error should match the regular path's wording: %s", w.Body.String())
	}
}

func TestHandleMessages_CustomModelNoUsageRecorded(t *testing.T) {
	buf := captureSlog(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"type":"message"}`))
	}))
	defer upstream.Close()

	s, _ := newCustomService(t, upstream.URL, true)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"claude-glm-5.3","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	s.HandleMessages(w, r)

	logs := buf.String()
	if strings.Contains(logs, "session_credits") || strings.Contains(logs, "input_tokens") {
		t.Errorf("custom path must not record usage; logs:\n%s", logs)
	}
	if !strings.Contains(logs, "--> POST /v1/messages") || !strings.Contains(logs, "<-- POST /v1/messages") {
		t.Errorf("expected the request/response log pair; logs:\n%s", logs)
	}
	if strings.Contains(logs, "secret-token") {
		t.Errorf("auth token leaked into logs:\n%s", logs)
	}
}

func TestHandleCountTokens_MaxBodyStays400(t *testing.T) {
	var upstreamCalled bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
	}))
	defer upstream.Close()

	// With a custom provider configured: the raw read in maybeForwardCustom
	// still answers 400 for count_tokens, matching the regular path.
	s, _ := newCustomService(t, upstream.URL, true)
	s.maxRequestBody = 64
	r := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body(4096)))
	w := httptest.NewRecorder()
	s.HandleCountTokens(w, r)

	if upstreamCalled {
		t.Error("upstream must not be called for an over-sized body")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (count_tokens never returns 413)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "invalid request") || !strings.Contains(w.Body.String(), "request body too large") {
		t.Errorf("error body should report the over-limit cause as invalid request: %s", w.Body.String())
	}

	// Without a custom provider the request never takes this path at all:
	// parseAndValidateRequest produces the same 400 for the same cause. The
	// message text differs only by the "jsontext: read error:" inner prefix
	// the streaming decoder adds; status, error type and cause match.
	plain := New(nil, nil)
	plain.maxRequestBody = 64
	r2 := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body(4096)))
	w2 := httptest.NewRecorder()
	plain.HandleCountTokens(w2, r2)
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 without custom providers", w2.Code)
	}
	if !strings.Contains(w2.Body.String(), "request body too large") {
		t.Errorf("plain service error body should report the same cause: %s", w2.Body.String())
	}
}

func TestHandleCountTokens_ForwardedToProvider(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":42}`))
	}))
	defer upstream.Close()

	s, authMgr := newCustomService(t, upstream.URL, true)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(
		`{"model":"claude-glm-5.3","messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	s.HandleCountTokens(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if gotPath != "/v1/messages/count_tokens" {
		t.Errorf("upstream path = %q, want /v1/messages/count_tokens", gotPath)
	}
	if !strings.Contains(w.Body.String(), "42") {
		t.Errorf("provider count was not passed through: %s", w.Body.String())
	}
	if authMgr.count() != 0 {
		t.Errorf("GetToken called %d times on the custom count_tokens path", authMgr.count())
	}
}

func TestHandleCountTokens_LocalEstimate(t *testing.T) {
	var upstreamCalled bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
	}))
	defer upstream.Close()

	s, _ := newCustomService(t, upstream.URL, false)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(
		`{"model":"claude-glm-5.3","messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	s.HandleCountTokens(w, r)

	if upstreamCalled {
		t.Error("provider must not be called when count_tokens is disabled")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"input_tokens"`) {
		t.Errorf("local estimate missing input_tokens: %s", w.Body.String())
	}
}

func TestHandleCountTokens_UnmatchedModelStaysLocalKiro(t *testing.T) {
	var upstreamCalled bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
	}))
	defer upstream.Close()

	s, authMgr := newCustomService(t, upstream.URL, true)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(
		`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	s.HandleCountTokens(w, r)

	if upstreamCalled {
		t.Error("provider must not be called for an unmatched model")
	}
	if authMgr.count() == 0 {
		t.Error("Kiro path should have requested credentials for the token count")
	}
	if !strings.Contains(w.Body.String(), `"input_tokens"`) {
		t.Errorf("expected the local Kiro count response: %s", w.Body.String())
	}
}
