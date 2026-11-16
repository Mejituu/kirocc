package messages

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
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
)

// newFallbackService builds a service from a raw config document. The
// web_search fallback tests configure web_search_fallback members on the
// providers and the kiro section, which newMappingService's fixed template
// cannot express.
func newFallbackService(t *testing.T, config string, client kiroclient.Client) (*Service, *countingAuth) {
	t.Helper()
	t.Setenv("KIROCC_MODEL_MAPPINGS", "")
	path := filepath.Join(t.TempDir(), "mejituu.json5")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := customprov.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	authMgr := &countingAuth{creds: &auth.Credentials{AccessToken: "kiro-token", ProfileARN: "arn:test", Region: "us-east-1"}}
	s := New(authMgr, client, WithCustomProviders(customprov.NewProxy(reg, 5*time.Second)))
	return s, authMgr
}

// webSearchRequest builds a messages body declaring the web_search
// server-side tool for the given model.
func webSearchRequest(model string) string {
	return `{"model":"` + model + `","max_tokens":16,` +
		`"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":5}],` +
		`"messages":[{"role":"user","content":"search this"}]}`
}

// TestWebSearchFallback_CustomModel asserts the provider-level fallback: a
// request carrying the web_search tool for a custom model without native
// search is forwarded to the provider's fallback model. The session header is
// carried but unknown — the fallback does not depend on the session table.
func TestWebSearchFallback_CustomModel(t *testing.T) {
	upstream, upstreamRequests := mappingUpstream(t)
	buf := captureTint(t, slog.LevelInfo)
	config := fmt.Sprintf(`{"providers":{"bailian":{"base_url":%q,"auth_token":"secret-token",`+
		`"models":["glm-5.3[1m]",{"id":"qwen3.8-max","web_search":true}],`+
		`"web_search_fallback":{"model":"qwen3.8-max"}}}}`, upstream.URL)
	s, _ := newFallbackService(t, config, &tripwireClient{t: t})

	postWithSession(t, s, "sess-1", webSearchRequest("claude-glm-5.3"))

	requests := upstreamRequests()
	if len(requests) != 1 {
		t.Fatalf("upstream requests = %v, want 1", requests)
	}
	if requests[0][1] != "qwen3.8-max" {
		t.Errorf("upstream model = %q, want the fallback target qwen3.8-max, not glm-5.3", requests[0][1])
	}

	line := findLine(t, buf, "web search fallback")
	assertAttrOrder(t, line, "trace_id", "session_id", "from_provider", "from_model", "to_provider", "to_model")
	if !strings.Contains(line, "from_provider=bailian") || !strings.Contains(line, "from_model=glm-5.3") ||
		!strings.Contains(line, "to_provider=bailian") || !strings.Contains(line, "to_model=qwen3.8-max") {
		t.Errorf("web search fallback line: %s", line)
	}
}

// TestWebSearchFallback_KiroModel asserts the kiro-section fallback: a search
// request whose model is a Kiro model (custom lookup miss) is forwarded to the
// configured custom model instead of the Kiro path, and the session is not
// recorded as kiro — the follow-up request without the tool still routes
// through Kiro with the requested model.
func TestWebSearchFallback_KiroModel(t *testing.T) {
	upstream, upstreamRequests := mappingUpstream(t)
	buf := captureTint(t, slog.LevelInfo)
	client := &capturingKiroClient{}
	config := fmt.Sprintf(`{"providers":{"bailian":{"base_url":%q,"auth_token":"secret-token",`+
		`"models":["glm-5.3[1m]",{"id":"qwen3.8-max","web_search":true}],`+
		`"web_search_fallback":{"model":"qwen3.8-max"}}},`+
		`"kiro":{"web_search_fallback":{"provider":"bailian","model":"qwen3.8-max"}}}`, upstream.URL)
	s, authMgr := newFallbackService(t, config, client)

	// claude-sonnet-4-6 is no custom model; the kiro group's fallback sends
	// it to bailian's search model.
	postWithSession(t, s, "sess-k", webSearchRequest("claude-sonnet-4-6"))

	requests := upstreamRequests()
	if len(requests) != 1 {
		t.Fatalf("upstream requests = %v, want 1", requests)
	}
	if requests[0][1] != "qwen3.8-max" {
		t.Errorf("upstream model = %q, want the fallback target qwen3.8-max", requests[0][1])
	}
	if authMgr.count() != 0 {
		t.Errorf("GetToken called %d times; the fallback must not touch Kiro credentials", authMgr.count())
	}

	line := findLine(t, buf, "web search fallback")
	assertAttrOrder(t, line, "trace_id", "session_id", "from_provider", "from_model", "to_provider", "to_model")
	if !strings.Contains(line, "from_provider=kiro") || !strings.Contains(line, "to_provider=bailian") ||
		!strings.Contains(line, "to_model=qwen3.8-max") {
		t.Errorf("web search fallback line: %s", line)
	}

	// The fallback must not record the session: the follow-up request without
	// the tool still takes the regular Kiro path (claude-sonnet-4.6 is the
	// catalog's spelling) and stays off the provider.
	postWithSession(t, s, "sess-k", `{"model":"claude-sonnet-4-6","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if got := client.model(); got != "claude-sonnet-4.6" {
		t.Errorf("Kiro path received model %q, want claude-sonnet-4.6", got)
	}
	if got := upstreamRequests(); len(got) != 1 {
		t.Errorf("the follow-up request must stay off the provider; requests = %v", got)
	}
}

// TestWebSearchFallback_PriorityMappingOverFallback asserts the routing
// priority: a session-following mapping that matches the search request wins
// over the provider's web_search_fallback.
func TestWebSearchFallback_PriorityMappingOverFallback(t *testing.T) {
	upBailian, bailianRequests := mappingUpstream(t)
	upSecond, secondRequests := mappingUpstream(t)
	buf := captureTint(t, slog.LevelInfo)
	config := fmt.Sprintf(`{"providers":{`+
		`"bailian":{"base_url":%q,"auth_token":"secret-token",`+
		`"models":["glm-5.3[1m]",{"id":"qwen3.8-max","web_search":true}],`+
		`"web_search_fallback":{"model":"qwen3.8-max"},`+
		`"mappings":[{"model":"claude-glm-5.3","web_search":true,"to":{"provider":"second","model":"deepseek-v4.1-flash-1017"}}]},`+
		`"second":{"base_url":%q,"auth_token":"secret-token","models":[{"id":"deepseek-v4.1-flash-1017","web_search":true}]}}}`,
		upBailian.URL, upSecond.URL)
	s, _ := newFallbackService(t, config, &tripwireClient{t: t})

	// 1. A plain glm request records the session as bailian.
	postWithSession(t, s, "sess-1", `{"model":"claude-glm-5.3","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)

	// 2. The search request matches the mapping, so it goes to second's model
	// rather than bailian's fallback target.
	postWithSession(t, s, "sess-1", webSearchRequest("claude-glm-5.3"))

	if got := bailianRequests(); len(got) != 1 || got[0][1] != "glm-5.3" {
		t.Errorf("bailian upstream requests = %v, want only the initial glm-5.3", got)
	}
	second := secondRequests()
	if len(second) != 1 {
		t.Fatalf("second upstream requests = %v, want 1", second)
	}
	if second[0][1] != "deepseek-v4.1-flash-1017" {
		t.Errorf("second upstream model = %q, want the mapping target deepseek-v4.1-flash-1017, not the fallback qwen3.8-max", second[0][1])
	}

	mappedLine := findLine(t, buf, "model mapped")
	assertAttrOrder(t, mappedLine, "trace_id", "session_id", "session_provider", "from", "to_provider", "to_model")
	if !strings.Contains(mappedLine, "session_provider=bailian") || !strings.Contains(mappedLine, "from=claude-glm-5.3") ||
		!strings.Contains(mappedLine, "to_provider=second") || !strings.Contains(mappedLine, "to_model=deepseek-v4.1-flash-1017") {
		t.Errorf("model mapped line: %s", mappedLine)
	}
	if strings.Contains(buf.String(), "web search fallback") {
		t.Errorf("the mapping must win over the fallback; logs:\n%s", buf.String())
	}
}

// TestWebSearchFallback_NotApplicable asserts the fallback trigger's
// boundaries: a model with native search forwards as itself, a request without
// the tool never falls back, and count_tokens never takes the fallback.
func TestWebSearchFallback_NotApplicable(t *testing.T) {
	var mu sync.Mutex
	var requests [][2]string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var peek struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &peek)
		mu.Lock()
		requests = append(requests, [2]string{r.URL.Path, peek.Model})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/messages/count_tokens" {
			_, _ = w.Write([]byte(`{"input_tokens":42}`))
			return
		}
		_, _ = w.Write([]byte(`{"type":"message","usage":{"input_tokens":10,"output_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)
	buf := captureTint(t, slog.LevelInfo)
	config := fmt.Sprintf(`{"providers":{"bailian":{"base_url":%q,"auth_token":"secret-token",`+
		`"models":["glm-5.3[1m]",{"id":"qwen3.8-max","web_search":true}],`+
		`"web_search_fallback":{"model":"qwen3.8-max"}}}}`, upstream.URL)
	s, _ := newFallbackService(t, config, &tripwireClient{t: t})

	// A model with web_search true forwards natively as itself.
	postCustomMessages(t, s, webSearchRequest("claude-qwen3.8-max"), nil)
	// Without the tool the request never falls back.
	postCustomMessages(t, s, `{"model":"claude-glm-5.3","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, nil)
	// count_tokens is not the messages endpoint, so the declared tool never
	// reroutes it: the provider answers the count for glm-5.3 itself.
	r := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(
		`{"model":"claude-glm-5.3","tools":[{"type":"web_search_20250305","name":"web_search","max_uses":5}],`+
			`"messages":[{"role":"user","content":"count this"}]}`))
	w := httptest.NewRecorder()
	s.HandleCountTokens(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("count_tokens status = %d, body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "42") {
		t.Errorf("provider count was not passed through: %s", w.Body.String())
	}

	want := [][2]string{
		{"/v1/messages", "qwen3.8-max"},
		{"/v1/messages", "glm-5.3"},
		{"/v1/messages/count_tokens", "glm-5.3"},
	}
	mu.Lock()
	got := append([][2]string(nil), requests...)
	mu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("upstream requests = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("request %d = %v, want %v", i, got[i], want[i])
		}
	}
	if strings.Contains(buf.String(), "web search fallback") {
		t.Errorf("no request should take the fallback; logs:\n%s", buf.String())
	}
}

// TestWebSearchFallback_NoConfig asserts the behavior without any
// web_search_fallback: a search request for a search-less model still
// forwards to the model itself.
func TestWebSearchFallback_NoConfig(t *testing.T) {
	upstream, upstreamRequests := mappingUpstream(t)
	buf := captureTint(t, slog.LevelInfo)
	config := fmt.Sprintf(`{"providers":{"bailian":{"base_url":%q,"auth_token":"secret-token",`+
		`"models":["glm-5.3[1m]",{"id":"qwen3.8-max","web_search":true}]}}}`, upstream.URL)
	s, _ := newFallbackService(t, config, &tripwireClient{t: t})

	postCustomMessages(t, s, webSearchRequest("claude-glm-5.3"), nil)

	requests := upstreamRequests()
	if len(requests) != 1 {
		t.Fatalf("upstream requests = %v, want 1", requests)
	}
	if requests[0][1] != "glm-5.3" {
		t.Errorf("upstream model = %q, want glm-5.3 forwarded as itself", requests[0][1])
	}
	if strings.Contains(buf.String(), "web search fallback") {
		t.Errorf("no fallback is configured; logs:\n%s", buf.String())
	}
}
