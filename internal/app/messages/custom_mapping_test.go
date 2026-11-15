package messages

import (
	"context"
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
	"github.com/d-kuro/kirocc/internal/kiroproto"
)

// mappingUpstream is a recording bailian upstream: it notes each request's
// path and forwarded model, and answers a minimal usage-bearing message.
func mappingUpstream(t *testing.T) (*httptest.Server, func() [][2]string) {
	t.Helper()
	var mu sync.Mutex
	var requests [][2]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var peek struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &peek)
		mu.Lock()
		requests = append(requests, [2]string{r.URL.Path, peek.Model})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","usage":{"input_tokens":10,"output_tokens":2}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() [][2]string {
		mu.Lock()
		defer mu.Unlock()
		return append([][2]string(nil), requests...)
	}
}

// newMappingService builds a service whose bailian provider carries the
// given mappings JSON. client serves the Kiro path; when requests are
// expected to stay off it, pass &tripwireClient{t: t}.
func newMappingService(t *testing.T, upstreamURL, mappings string, client kiroclient.Client) (*Service, *countingAuth) {
	t.Helper()
	t.Setenv("KIROCC_MODEL_MAPPINGS", "")
	path := filepath.Join(t.TempDir(), "mejituu.json")
	content := fmt.Sprintf(`{"providers":[{"name":"bailian","base_url":%q,"auth_token":"secret-token","models":["glm-5.3[1m]","deepseek-v4.1-flash-1017"]}],"mappings":%s}`, upstreamURL, mappings)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
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

// postWithSession sends a request body with a session header.
func postWithSession(t *testing.T, s *Service, session, body string) *httptest.ResponseRecorder {
	t.Helper()
	return postCustomMessages(t, s, body, map[string]string{"X-Claude-Code-Session-Id": session})
}

// TestCustomMapping_SessionFollowsProvider asserts the full flow: a custom
// request records the session, the classifier request then follows it to the
// provider, and a request the rules do not match stays on the Kiro path.
func TestCustomMapping_SessionFollowsProvider(t *testing.T) {
	const mappings = `{"bailian":[{"model":"claude-sonnet-5","thinking":false,"stream":false,"to":{"provider":"bailian","model":"deepseek-v4.1-flash-1017"}}]}`

	upstream, upstreamRequests := mappingUpstream(t)
	buf := captureTint(t, slog.LevelInfo)
	var kiroCalled bool
	s, _ := newMappingService(t, upstream.URL, mappings, &recordingKiroClient{called: &kiroCalled})

	// 1. A normal custom request records the session as bailian.
	postWithSession(t, s, "sess-1", `{"model":"claude-glm-5.3","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)

	// 2. The classifier request follows the session to bailian and is
	// rewritten to the mapped model.
	postWithSession(t, s, "sess-1", `{"model":"claude-sonnet-5","max_tokens":16,"messages":[{"role":"user","content":"classify"}]}`)

	requests := upstreamRequests()
	if len(requests) != 2 {
		t.Fatalf("upstream requests = %v, want 2", requests)
	}
	if requests[0][1] != "glm-5.3" || requests[1][1] != "deepseek-v4.1-flash-1017" {
		t.Errorf("upstream models = %v, want glm-5.3 then deepseek-v4.1-flash-1017", requests)
	}

	// The mapping log and the request line show from/to models.
	mappedLine := findLine(t, buf, "model mapped")
	assertAttrOrder(t, mappedLine, "trace_id", "session_id", "session_provider", "from", "to_provider", "to_model")
	if !strings.Contains(mappedLine, "from=claude-sonnet-5") || !strings.Contains(mappedLine, "to_model=deepseek-v4.1-flash-1017") {
		t.Errorf("model mapped line: %s", mappedLine)
	}
	// The mapped request's own line: findLine returns the first, so scan for
	// the one carrying the mapped target.
	var reqLine string
	for line := range strings.SplitSeq(buf.String(), "\n") {
		if strings.Contains(line, "--> POST /v1/messages") && strings.Contains(line, "model=claude-sonnet-5") {
			reqLine = line
			break
		}
	}
	if reqLine == "" {
		t.Fatalf("no request line for the mapped model; logs:\n%s", buf.String())
	}
	if !strings.Contains(reqLine, "upstream_model=deepseek-v4.1-flash-1017") {
		t.Errorf("mapped request line should show the original and target model: %s", reqLine)
	}

	// 3. The same model with stream=true matches no rule and stays on Kiro.
	postWithSession(t, s, "sess-1", `{"model":"claude-sonnet-5","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"classify"}]}`)
	if got := upstreamRequests(); len(got) != 2 {
		t.Errorf("streaming request must not reach the provider; requests = %v", got)
	}
	if !kiroCalled {
		t.Error("streaming request should have reached the Kiro path")
	}
}

// TestCustomMapping_MappedRequestKeepsSession asserts a mapped request does
// not rewrite the session's provider: mapping to kiro leaves the session on
// bailian, so later bailian rules still apply.
func TestCustomMapping_MappedRequestKeepsSession(t *testing.T) {
	const mappings = `{
		"bailian": [
			{"model":"claude-opus-5-5","to":{"provider":"kiro","model":"claude-haiku-4.5"}},
			{"model":"claude-sonnet-5","thinking":false,"stream":false,"to":{"provider":"bailian","model":"deepseek-v4.1-flash-1017"}}
		]
	}`
	upstream, upstreamRequests := mappingUpstream(t)
	s, _ := newMappingService(t, upstream.URL, mappings, &recordingKiroClient{called: new(bool)})

	postWithSession(t, s, "sess-1", `{"model":"claude-glm-5.3","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	// Mapped to kiro: had this rewritten the session, the next request would
	// find no bailian rule.
	postWithSession(t, s, "sess-1", `{"model":"claude-opus-5-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	postWithSession(t, s, "sess-1", `{"model":"claude-sonnet-5","max_tokens":16,"messages":[{"role":"user","content":"classify"}]}`)

	requests := upstreamRequests()
	if len(requests) != 2 {
		t.Fatalf("upstream requests = %v, want 2 (the kiro-mapped request must not reach the provider)", requests)
	}
	if requests[1][1] != "deepseek-v4.1-flash-1017" {
		t.Errorf("session should still follow bailian; requests = %v", requests)
	}
}

// capturingKiroClient records the last payload's model for the Kiro-path
// assertion and answers an error to stop the flow.
type capturingKiroClient struct {
	mu      sync.Mutex
	modelID string
}

func (c *capturingKiroClient) GenerateAssistantResponse(ctx context.Context, token string, payload *kiroproto.Payload, region string) (*kiroclient.Response, error) {
	c.mu.Lock()
	c.modelID = payload.ConversationState.CurrentMessage.UserInputMessage.ModelID
	c.mu.Unlock()
	return nil, fmt.Errorf("stop here")
}

func (c *capturingKiroClient) model() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.modelID
}

// TestCustomMapping_KiroSessionMapsToKiro asserts a session that last used
// Kiro maps through the kiro group, and the Kiro path receives the rewritten
// model.
func TestCustomMapping_KiroSessionMapsToKiro(t *testing.T) {
	const mappings = `{"kiro":[{"model":"claude-sonnet-5","thinking":false,"stream":false,"to":{"provider":"kiro","model":"claude-haiku-4.5"}}]}`
	upstream, upstreamRequests := mappingUpstream(t)
	buf := captureTint(t, slog.LevelInfo)
	client := &capturingKiroClient{}
	s, _ := newMappingService(t, upstream.URL, mappings, client)

	// A non-custom request records the session as kiro (the Kiro call then
	// fails; the session was already noted before auth).
	postWithSession(t, s, "sess-k", `{"model":"claude-sonnet-4-6","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)

	// The classifier request maps through the kiro group.
	postWithSession(t, s, "sess-k", `{"model":"claude-sonnet-5","max_tokens":16,"messages":[{"role":"user","content":"classify"}]}`)

	if got := client.model(); got != "claude-haiku-4.5" {
		t.Errorf("Kiro path received model %q, want claude-haiku-4.5", got)
	}
	if got := upstreamRequests(); len(got) != 0 {
		t.Errorf("provider must not be reached; requests = %v", got)
	}
	mappedLine := findLine(t, buf, "model mapped")
	if !strings.Contains(mappedLine, "session_provider=kiro") || !strings.Contains(mappedLine, "to_model=claude-haiku-4.5") {
		t.Errorf("model mapped line: %s", mappedLine)
	}
}

// TestCustomMapping_UnknownKiroTarget asserts an unknown kiro target warns
// and leaves the request on normal routing without rewriting the session.
func TestCustomMapping_UnknownKiroTarget(t *testing.T) {
	const mappings = `{
		"bailian": [
			{"model":"claude-opus-5-5","to":{"provider":"kiro","model":"claude-nonexistent-9"}},
			{"model":"claude-sonnet-5","thinking":false,"stream":false,"to":{"provider":"bailian","model":"deepseek-v4.1-flash-1017"}}
		]
	}`
	upstream, upstreamRequests := mappingUpstream(t)
	buf := captureSlogAtLevel(t, slog.LevelInfo)
	s, _ := newMappingService(t, upstream.URL, mappings, &recordingKiroClient{called: new(bool)})

	postWithSession(t, s, "sess-1", `{"model":"claude-glm-5.3","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	// Target unknown: warn, no mapping, and the session is not rewritten.
	postWithSession(t, s, "sess-1", `{"model":"claude-opus-5-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	// The session is still bailian, so the second rule applies.
	postWithSession(t, s, "sess-1", `{"model":"claude-sonnet-5","max_tokens":16,"messages":[{"role":"user","content":"classify"}]}`)

	if !strings.Contains(buf.String(), "model mapping target unknown") {
		t.Errorf("expected the unknown-target warning; logs:\n%s", buf.String())
	}
	requests := upstreamRequests()
	if len(requests) != 2 {
		t.Fatalf("upstream requests = %v, want 2", requests)
	}
	if requests[1][1] != "deepseek-v4.1-flash-1017" {
		t.Errorf("session should still follow bailian; requests = %v", requests)
	}
}

// TestCustomMapping_NoMappingWithoutSession asserts the preconditions: no
// session header, an unknown session, and count_tokens never map.
func TestCustomMapping_NoMappingWithoutSession(t *testing.T) {
	const mappings = `{"bailian":[{"model":"claude-sonnet-5","thinking":false,"stream":false,"to":{"provider":"bailian","model":"deepseek-v4.1-flash-1017"}}]}`
	upstream, upstreamRequests := mappingUpstream(t)
	var kiroCalled bool
	s, _ := newMappingService(t, upstream.URL, mappings, &recordingKiroClient{called: &kiroCalled})

	// No session header: no session to follow.
	postCustomMessages(t, s, `{"model":"claude-sonnet-5","max_tokens":16,"messages":[{"role":"user","content":"classify"}]}`, nil)
	// Unknown session: nothing recorded yet.
	postWithSession(t, s, "sess-unknown", `{"model":"claude-sonnet-5","max_tokens":16,"messages":[{"role":"user","content":"classify"}]}`)
	if got := upstreamRequests(); len(got) != 0 {
		t.Errorf("no request should map without a known session; requests = %v", got)
	}
	if !kiroCalled {
		t.Error("unmapped requests should have reached the Kiro path")
	}

	// count_tokens neither reads nor writes the session table: with the
	// session recorded as kiro, a subsequent messages request still maps.
	postWithSession(t, s, "sess-2", `{"model":"claude-sonnet-4-6","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(
		`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("X-Claude-Code-Session-Id", "sess-2")
	w := httptest.NewRecorder()
	s.HandleCountTokens(w, r)
	if got := upstreamRequests(); len(got) != 0 {
		t.Errorf("count_tokens must not map or reach the provider; requests = %v", got)
	}
	// The session is still kiro, so the messages request does not map either.
	postWithSession(t, s, "sess-2", `{"model":"claude-sonnet-5","max_tokens":16,"messages":[{"role":"user","content":"classify"}]}`)
	if got := upstreamRequests(); len(got) != 0 {
		t.Errorf("session should still be kiro after count_tokens; requests = %v", got)
	}
}
