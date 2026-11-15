package customprov

import (
	"strings"
	"testing"
	"time"
)

// mappingConfig wraps a provider declaration with mappings JSON.
const mappingProviderConfig = `{"providers":[{"name":"bailian","base_url":"http://p","auth_token":"t","models":["glm-5.3[1m]","deepseek-v4.1-flash-1017"]}],"mappings":%s}`

func loadMappingRegistry(t *testing.T, mappings string) (*Registry, error) {
	t.Helper()
	return loadTestRegistry(t, strings.Replace(mappingProviderConfig, "%s", mappings, 1))
}

func TestBuildMappings_ValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		mapping string
		wantErr string
	}{
		{
			name:    "unknown group",
			mapping: `{"unknown":[{"model":"m","to":{"provider":"kiro","model":"x"}}]}`,
			wantErr: `mappings["unknown"]: unknown provider`,
		},
		{
			name:    "unknown to.provider",
			mapping: `{"kiro":[{"model":"m","to":{"provider":"nope","model":"x"}}]}`,
			wantErr: `mappings["kiro"][0].to.provider: unknown provider "nope"`,
		},
		{
			name:    "to.model not a provider model",
			mapping: `{"kiro":[{"model":"m","to":{"provider":"bailian","model":"missing-model"}}]}`,
			wantErr: `mappings["kiro"][0].to.model: "missing-model" is not a model of provider "bailian"`,
		},
		{
			name:    "empty rule model",
			mapping: `{"kiro":[{"model":"","to":{"provider":"kiro","model":"x"}}]}`,
			wantErr: `mappings["kiro"][0].model must not be empty`,
		},
		{
			name:    "empty to.model",
			mapping: `{"kiro":[{"model":"m","to":{"provider":"kiro","model":""}}]}`,
			wantErr: `mappings["kiro"][0].to.model must not be empty`,
		},
		{
			name:    "duplicate rules in a group",
			mapping: `{"kiro":[{"model":"m","thinking":false,"stream":false,"to":{"provider":"kiro","model":"x"}},{"model":"m","thinking":false,"stream":false,"to":{"provider":"kiro","model":"y"}}]}`,
			wantErr: "can never match",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadMappingRegistry(t, tt.mapping)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestBuildMappings_KiroTargetNotValidated(t *testing.T) {
	// A kiro target's model is checked per request, not at startup: the
	// catalog fills in after startup.
	reg, err := loadMappingRegistry(t, `{"kiro":[{"model":"m","to":{"provider":"kiro","model":"claude-not-in-any-table"}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if reg.mappingCount() != 1 {
		t.Errorf("mappingCount = %d, want 1", reg.mappingCount())
	}
}

func TestBuildMappings_NoMappingsSection(t *testing.T) {
	reg, err := loadTestRegistry(t, validConfig)
	if err != nil {
		t.Fatal(err)
	}
	if reg.mappingCount() != 0 {
		t.Errorf("mappingCount = %d, want 0", reg.mappingCount())
	}
}

func TestBuildMappings_ToModelWith1MMatchesBase(t *testing.T) {
	// The [1m] marker is ignored when matching a mapping target against the
	// provider's models.
	if _, err := loadMappingRegistry(t, `{"kiro":[{"model":"m","to":{"provider":"bailian","model":"deepseek-v4.1-flash-1017[1m]"}}]}`); err != nil {
		t.Fatal(err)
	}
}

func TestBuildMappings_ReservedProviderName(t *testing.T) {
	_, err := loadTestRegistry(t, `{"providers":[{"name":"kiro","base_url":"http://p","auth_token":"t","models":["m"]}]}`)
	if err == nil {
		t.Fatal("expected an error for a provider named kiro")
	}
	if !strings.Contains(err.Error(), "is reserved") {
		t.Errorf("error should explain the reserved name: %v", err)
	}
}

func TestMappingsLoadedLog(t *testing.T) {
	buf := captureSlog(t)
	reg, err := loadMappingRegistry(t, `{"kiro":[{"model":"m","to":{"provider":"kiro","model":"x"}},{"model":"n","to":{"provider":"bailian","model":"glm-5.3"}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	_ = reg
	if !strings.Contains(buf.String(), "model mappings loaded") || !strings.Contains(buf.String(), `"rules":2`) {
		t.Errorf("expected a mappings-loaded log with 2 rules, got:\n%s", buf.String())
	}
}

func TestLookup_ModelWithSlashAndCase(t *testing.T) {
	reg, err := loadTestRegistry(t, `{"providers":[{"name":"bailian","base_url":"http://p","auth_token":"t","models":["ZHIPU/GLM-5.3[1m]"]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"claude-ZHIPU/GLM-5.3", "claude-ZHIPU/GLM-5.3[1m]"} {
		hit, ok := reg.Lookup(id)
		if !ok {
			t.Fatalf("Lookup(%q) did not hit", id)
		}
		if hit.BaseModel != "ZHIPU/GLM-5.3" {
			t.Errorf("Lookup(%q).BaseModel = %q, want ZHIPU/GLM-5.3", id, hit.BaseModel)
		}
		if !hit.Config1M {
			t.Errorf("Lookup(%q).Config1M = false, want true (config carries [1m])", id)
		}
	}
}

func TestMatchMapping_OrderBySessionProviderAndFilters(t *testing.T) {
	reg, err := loadMappingRegistry(t, `{
		"kiro": [
			{"model":"claude-sonnet-5","stream":false,"to":{"provider":"kiro","model":"claude-haiku-4.5"}},
			{"model":"claude-sonnet-5","to":{"provider":"bailian","model":"glm-5.3"}}
		]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	p := NewProxy(reg, time.Second)
	p.RecordSession("s1", KiroProviderName)

	// The first rule wins for a non-streaming request.
	_, m, ok := p.MatchMapping("s1", RequestInfo{Model: "claude-sonnet-5", Stream: false})
	if !ok || m.Provider != KiroProviderName || m.ModelTo != "claude-haiku-4.5" {
		t.Fatalf("match = %+v, ok = %v; want the first rule (kiro/claude-haiku-4.5)", m, ok)
	}
	// Streaming skips the first rule and hits the second.
	_, m, ok = p.MatchMapping("s1", RequestInfo{Model: "claude-sonnet-5", Stream: true})
	if !ok || m.Provider != "bailian" || m.ModelTo != "glm-5.3" {
		t.Fatalf("match = %+v, ok = %v; want the second rule (bailian/glm-5.3)", m, ok)
	}
	// The [1m] marker on the request model is ignored, in either case.
	if _, _, ok := p.MatchMapping("s1", RequestInfo{Model: "claude-sonnet-5[1m]", Stream: false}); !ok {
		t.Error("[1m] request model should still match")
	}
	// The model comparison is case-sensitive after the marker strip.
	if _, _, ok := p.MatchMapping("s1", RequestInfo{Model: "claude-Sonnet-5", Stream: false}); ok {
		t.Error("model match should be case-sensitive")
	}
	// A model with no rules in the group does not map.
	if _, _, ok := p.MatchMapping("s1", RequestInfo{Model: "claude-opus-5", Stream: false}); ok {
		t.Error("unrelated model should not map")
	}
	// An unknown session does not map.
	if _, _, ok := p.MatchMapping("s-other", RequestInfo{Model: "claude-sonnet-5", Stream: false}); ok {
		t.Error("unknown session should not map")
	}
}

func TestMatchMapping_ThinkingFilter(t *testing.T) {
	reg, err := loadMappingRegistry(t, `{
		"kiro": [
			{"model":"claude-sonnet-5","thinking":false,"to":{"provider":"kiro","model":"a"}},
			{"model":"claude-sonnet-5","thinking":true,"to":{"provider":"kiro","model":"b"}}
		]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	p := NewProxy(reg, time.Second)
	p.RecordSession("s", KiroProviderName)

	enabled := RequestInfo{Model: "claude-sonnet-5", Thinking: &thinkingPeek{Type: "enabled"}}
	adaptive := RequestInfo{Model: "claude-sonnet-5", Thinking: &thinkingPeek{Type: "adaptive"}}
	off := RequestInfo{Model: "claude-sonnet-5", Thinking: &thinkingPeek{Type: "disabled"}}
	// The [1m] marker alone does not enable thinking.
	markerOnly := RequestInfo{Model: "claude-sonnet-5[1m]"}

	for name, info := range map[string]RequestInfo{"enabled": enabled, "adaptive": adaptive} {
		_, m, ok := p.MatchMapping("s", info)
		if !ok || m.ModelTo != "b" {
			t.Errorf("%s: match = %+v, ok = %v; want thinking=true rule", name, m, ok)
		}
	}
	for name, info := range map[string]RequestInfo{"disabled": off, "marker only": markerOnly} {
		_, m, ok := p.MatchMapping("s", info)
		if !ok || m.ModelTo != "a" {
			t.Errorf("%s: match = %+v, ok = %v; want thinking=false rule", name, m, ok)
		}
	}
}

func TestSessionTable_LRU(t *testing.T) {
	tbl := newSessionTable(2)
	tbl.put("a", "p1")
	tbl.put("b", "p2")
	tbl.get("a") // refresh a, so b becomes the least recently updated
	tbl.put("c", "p3")

	if _, ok := tbl.get("b"); ok {
		t.Error("b should have been evicted")
	}
	if p, ok := tbl.get("a"); !ok || p != "p1" {
		t.Errorf("a = %q, %v; want p1, true", p, ok)
	}
	if p, ok := tbl.get("c"); !ok || p != "p3" {
		t.Errorf("c = %q, %v; want p3, true", p, ok)
	}

	// Updating an existing session keeps the table at capacity.
	tbl.put("a", "p9")
	if p, _ := tbl.get("a"); p != "p9" {
		t.Errorf("a = %q, want p9 after update", p)
	}
	if got := len(tbl.items); got != 2 {
		t.Errorf("len = %d, want 2", got)
	}
}
