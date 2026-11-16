package messages

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/d-kuro/kirocc/internal/customprov"
)

// TestHandleCountTokens_OpenAIProtocolLocal asserts an openai-protocol
// provider answers count_tokens with the local estimate: the protocol has
// no count_tokens endpoint, so the provider is never called.
func TestHandleCountTokens_OpenAIProtocolLocal(t *testing.T) {
	var upstreamCalled bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
	}))
	defer upstream.Close()

	t.Setenv("KIROCC_MODEL_MAPPINGS", "")
	path := filepath.Join(t.TempDir(), "mejituu.json5")
	content := fmt.Sprintf(`{"providers":{"bailian":{"base_url":%q,"auth_token":"t","protocol":"openai","models":["ZHIPU/GLM-5.3"]}}}`, upstream.URL)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := customprov.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil, &tripwireClient{t: t}, WithCustomProviders(customprov.NewProxy(reg, 5*time.Second)))

	r := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(
		`{"model":"claude-ZHIPU/GLM-5.3","messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	s.HandleCountTokens(w, r)

	if upstreamCalled {
		t.Error("upstream must not be called for count_tokens on an openai provider")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"input_tokens"`) {
		t.Errorf("expected the local estimate; body: %s", w.Body.String())
	}
}
