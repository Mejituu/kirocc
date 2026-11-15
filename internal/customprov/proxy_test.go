package customprov

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestProxy builds a proxy over a registry matching validConfig.
func newTestProxy(t *testing.T, upstream *httptest.Server) *Proxy {
	t.Helper()
	reg, err := loadTestRegistry(t, `{
		"providers": [{
			"name": "bailian",
			"base_url": "`+upstream.URL+`",
			"auth_token": "secret-token",
			"models": ["glm-5.3[1m]"]
		}]
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
	reg, err := loadTestRegistry(t, `{"providers":[{"name":"bailian","base_url":"`+url+`","auth_token":"t","models":["glm-5.3[1m]"]}]}`)
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
