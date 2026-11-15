package customprov

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
)

func TestRewriteModel_ReplacesTopLevelModel(t *testing.T) {
	in := `{"model":"claude-glm-5.3","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	out, err := RewriteModel([]byte(in), "glm-5.3")
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("rewritten body is not valid JSON: %v\n%s", err, out)
	}
	if decoded["model"] != "glm-5.3" {
		t.Errorf("model = %v, want glm-5.3", decoded["model"])
	}
	if decoded["max_tokens"] != float64(16) {
		t.Errorf("max_tokens = %v, want 16", decoded["max_tokens"])
	}
}

func TestRewriteModel_PreservesUnknownFieldsOrderAndNumbers(t *testing.T) {
	in := `{"model":"claude-glm-5.3","temperature":0.5,"metadata":{"user_id":"u1"},"service_tier":"auto","big":12345678901234567890,"mcp_servers":[{"type":"url"}]}`
	out, err := RewriteModel([]byte(in), "glm-5.3")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	// Key order must survive: model still first, temperature second.
	if !strings.HasPrefix(s, `{"model":"glm-5.3","temperature":0.5,`) {
		t.Errorf("key order changed: %s", s)
	}
	// Unknown fields and the exact number spelling must survive.
	for _, want := range []string{`"service_tier":"auto"`, `"big":12345678901234567890`, `"mcp_servers":[{"type":"url"}]`} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q: %s", want, s)
		}
	}
}

func TestRewriteModel_OnlyTopLevel(t *testing.T) {
	in := `{"model":"claude-glm-5.3","messages":[{"role":"user","content":[{"type":"text","text":"x"}]}],"nested":{"model":"claude-glm-5.3"}}`
	out, err := RewriteModel([]byte(in), "glm-5.3")
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Messages []struct {
			Content jsontext.Value `json:"content"`
		} `json:"messages"`
		Nested struct {
			Model string `json:"model"`
		} `json:"nested"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Nested.Model != "claude-glm-5.3" {
		t.Errorf("nested model was rewritten: %s", out)
	}
}

func TestRewriteModel_ModelValueNotString(t *testing.T) {
	_, err := RewriteModel([]byte(`{"model":42}`), "glm-5.3")
	if err == nil {
		t.Fatal("expected an error when model is not a string")
	}
}

func TestRewriteModel_RootNotObject(t *testing.T) {
	if _, err := RewriteModel([]byte(`["model"]`), "glm-5.3"); err == nil {
		t.Fatal("expected an error for a non-object root")
	}
}
