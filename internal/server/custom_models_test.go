package server

import (
	"encoding/json/v2"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/d-kuro/kirocc/internal/auth"
	"github.com/d-kuro/kirocc/internal/customprov"
)

func TestGetModels_CustomProviderModels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mejituu.json5")
	if err := os.WriteFile(path, []byte(`{
		"providers": {
			"bailian": {
				"base_url": "http://provider.test",
				"auth_token": "t",
				"models": ["glm-5.3[1m]", "kimi-k3", {"id": "real-id", "alias": "pretty", "context_1m": true, "label": "Pretty · label"}]
			}
		}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := customprov.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	mgr := &mockAuthManager{creds: &auth.Credentials{AccessToken: "t", ProfileARN: "arn", Region: "us-east-1"}}
	s := New(mgr, "", nil, WithCustomProviders(customprov.NewProxy(reg, 5*time.Second)))
	srv := newTCP4TestServer(t, s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	var result struct {
		Data []struct {
			ID          string `json:"id"`
			OwnedBy     string `json:"owned_by"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.UnmarshalRead(resp.Body, &result); err != nil {
		t.Fatal(err)
	}

	counts := make(map[string]int)
	ownedBy := make(map[string]string)
	displayName := make(map[string]string)
	lastIndex := make(map[string]int)
	for i, m := range result.Data {
		counts[m.ID]++
		ownedBy[m.ID] = m.OwnedBy
		displayName[m.ID] = m.DisplayName
		lastIndex[m.ID] = i
	}
	for id, n := range counts {
		if n != 1 {
			t.Errorf("model %s listed %d times, want exactly once", id, n)
		}
	}
	if counts["claude-glm-5.3[1m]"] != 1 || counts["claude-kimi-k3"] != 1 {
		t.Errorf("custom models missing: counts = %v", counts)
	}
	if ownedBy["claude-glm-5.3[1m]"] != "bailian" || ownedBy["claude-kimi-k3"] != "bailian" {
		t.Errorf("custom models owned_by = %v / %v, want bailian",
			ownedBy["claude-glm-5.3[1m]"], ownedBy["claude-kimi-k3"])
	}
	if displayName["claude-glm-5.3[1m]"] != "glm-5.3[1m]" || displayName["claude-kimi-k3"] != "kimi-k3" {
		t.Errorf("custom display_name = %v / %v", displayName["claude-glm-5.3[1m]"], displayName["claude-kimi-k3"])
	}
	// A model with a label shows it verbatim; one without derives it from
	// the display name, and its context_1m adds the marker.
	if displayName["claude-pretty[1m]"] != "Pretty · label" {
		t.Errorf("labeled display_name = %q, want \"Pretty · label\"", displayName["claude-pretty[1m]"])
	}
	// A built-in entry must still be listed, and custom entries must come
	// after the built-in ones.
	if counts["claude-sonnet-4-6"] != 1 {
		t.Errorf("built-in model claude-sonnet-4-6 missing from the list")
	}
	if lastIndex["claude-glm-5.3[1m]"] <= lastIndex["claude-sonnet-4-6"] {
		t.Errorf("custom model listed before a built-in model")
	}
}
