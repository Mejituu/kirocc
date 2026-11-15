package customprov

import (
	"strings"
	"testing"
	"time"
)

// mappingProviderConfig is one provider carrying a mappings group.
const mappingProviderConfig = `{"providers":{"bailian":{"base_url":"http://p","auth_token":"t","models":["glm-5.3[1m]","deepseek-v4.1-flash-1017"],"mappings":%s}}}`

// mappingKiroConfig carries the mappings under kiro.
const mappingKiroConfig = `{"kiro":{"mappings":%s},"providers":{"bailian":{"base_url":"http://p","auth_token":"t","models":["glm-5.3[1m]","deepseek-v4.1-flash-1017"]}}}`

func TestBuildMappings_ValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		mapping string
		wantErr string
	}{
		{
			name:    "unknown to.provider",
			mapping: `[{"model":"m","to":{"provider":"nope","model":"x"}}]`,
			wantErr: `providers.bailian.mappings[0].to.provider: unknown provider "nope"`,
		},
		{
			name:    "to.model not a provider model",
			mapping: `[{"model":"m","to":{"provider":"bailian","model":"missing-model"}}]`,
			wantErr: `providers.bailian.mappings[0].to.model: "missing-model" is not a model id of provider "bailian"`,
		},
		{
			name:    "empty rule model",
			mapping: `[{"model":"","to":{"provider":"kiro","model":"x"}}]`,
			wantErr: `providers.bailian.mappings[0].model must not be empty`,
		},
		{
			name:    "empty to.model",
			mapping: `[{"model":"m","to":{"provider":"kiro","model":""}}]`,
			wantErr: `providers.bailian.mappings[0].to.model must not be empty`,
		},
		{
			name:    "duplicate rules in a group",
			mapping: `[{"model":"m","thinking":false,"stream":false,"to":{"provider":"kiro","model":"x"}},{"model":"m","thinking":false,"stream":false,"to":{"provider":"kiro","model":"y"}}]`,
			wantErr: "can never match",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadTestRegistry(t, strings.Replace(mappingProviderConfig, "%s", tt.mapping, 1))
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
	// catalog only fills in after startup.
	reg, err := loadTestRegistry(t, strings.Replace(mappingProviderConfig, "%s", `[{"model":"m","to":{"provider":"kiro","model":"claude-not-in-any-table"}}]`, 1))
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

// TestBuildMappings_DefaultProvider asserts an omitted to.provider defaults
// to the group: kiro for kiro.mappings, the provider itself for its own
// group.
func TestBuildMappings_DefaultProvider(t *testing.T) {
	t.Run("kiro group defaults to kiro", func(t *testing.T) {
		reg, err := loadTestRegistry(t, strings.Replace(mappingKiroConfig, "%s", `[{"model":"claude-m","to":{"model":"x"}}]`, 1))
		if err != nil {
			t.Fatal(err)
		}
		m, ok := reg.matchMapping(KiroProviderName, RequestInfo{Model: "claude-m"})
		if !ok || m.Provider != KiroProviderName || m.ModelTo != "x" {
			t.Errorf("match = %+v, ok = %v; want provider kiro", m, ok)
		}
	})

	t.Run("provider group defaults to the provider", func(t *testing.T) {
		reg, err := loadTestRegistry(t, strings.Replace(mappingProviderConfig, "%s", `[{"model":"claude-m","to":{"model":"deepseek-v4.1-flash-1017"}}]`, 1))
		if err != nil {
			t.Fatal(err)
		}
		m, ok := reg.matchMapping("bailian", RequestInfo{Model: "claude-m"})
		if !ok || m.Provider != "bailian" || m.ModelTo != "deepseek-v4.1-flash-1017" {
			t.Errorf("match = %+v, ok = %v; want provider bailian", m, ok)
		}
		hit, ok := reg.lookupModel("bailian", m.ModelTo)
		if !ok || hit.BaseModel != "deepseek-v4.1-flash-1017" {
			t.Errorf("lookupModel = %+v, %v", hit, ok)
		}
	})
}

// TestBuildMappings_TargetByAliasRejected asserts a mapping target must use
// the model id; the alias does not resolve (config-level error already
// covered, this locks the runtime lookup too).
func TestBuildMappings_TargetByAliasRejected(t *testing.T) {
	config := `{"providers":{"p":{"base_url":"http://p","auth_token":"t",` +
		`"models":[{"id":"real-id","alias":"pretty"}],` +
		`"mappings":[{"model":"m","to":{"model":"real-id"}}]}}}`
	reg, err := loadTestRegistry(t, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.lookupModel("p", "pretty"); ok {
		t.Error("lookupModel by alias must not hit")
	}
	hit, ok := reg.lookupModel("p", "real-id")
	if !ok || hit.BaseModel != "real-id" {
		t.Errorf("lookupModel by id = %+v, %v", hit, ok)
	}
	// The alias still routes normal requests.
	if hit, ok := reg.Lookup("claude-pretty"); !ok || hit.BaseModel != "real-id" {
		t.Errorf("Lookup(claude-pretty) = %+v, %v; want the aliased model", hit, ok)
	}
}

func TestLookup_ModelWithSlashAndCase(t *testing.T) {
	reg, err := loadTestRegistry(t, `{"providers":{"bailian":{"base_url":"http://p","auth_token":"t","models":["ZHIPU/GLM-5.3[1m]"]}}}`)
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
	mappings := `[
		{"model":"claude-sonnet-5","stream":false,"to":{"model":"claude-haiku-4.5"}},
		{"model":"claude-sonnet-5","to":{"provider":"bailian","model":"glm-5.3"}}
	]`
	reg, err := loadTestRegistry(t, strings.Replace(mappingKiroConfig, "%s", mappings, 1))
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
	mappings := `[
		{"model":"claude-sonnet-5","thinking":false,"to":{"model":"a"}},
		{"model":"claude-sonnet-5","thinking":true,"to":{"model":"b"}}
	]`
	reg, err := loadTestRegistry(t, strings.Replace(mappingKiroConfig, "%s", mappings, 1))
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

// TestBuildMappings_ForwardTarget asserts a rule may target a provider
// declared later in the file — mappings compile after every provider is
// registered.
func TestBuildMappings_ForwardTarget(t *testing.T) {
	config := `{"providers":{
		"a":{"base_url":"http://a","auth_token":"t","models":["m1"],"mappings":[{"model":"claude-x","to":{"provider":"b","model":"m2"}}]},
		"b":{"base_url":"http://b","auth_token":"t","models":["m2"]}
	}}`
	reg, err := loadTestRegistry(t, config)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := reg.matchMapping("a", RequestInfo{Model: "claude-x"})
	if !ok || m.Provider != "b" || m.ModelTo != "m2" {
		t.Errorf("match = %+v, ok = %v; want provider b, model m2", m, ok)
	}
}

// TestBuildMappings_KiroToCustomProvider asserts kiro.mappings may target a
// custom provider.
func TestBuildMappings_KiroToCustomProvider(t *testing.T) {
	config := `{"kiro":{"mappings":[{"model":"claude-x","to":{"provider":"bailian","model":"deepseek-v4.1-flash-1017"}}]},
		"providers":{"bailian":{"base_url":"http://p","auth_token":"t","models":["deepseek-v4.1-flash-1017"]}}}`
	reg, err := loadTestRegistry(t, config)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := reg.matchMapping(KiroProviderName, RequestInfo{Model: "claude-x"})
	if !ok || m.Provider != "bailian" || m.ModelTo != "deepseek-v4.1-flash-1017" {
		t.Errorf("match = %+v, ok = %v; want provider bailian", m, ok)
	}
	if _, ok := reg.lookupModel("bailian", m.ModelTo); !ok {
		t.Error("mapping target should resolve to the provider's model")
	}
}
