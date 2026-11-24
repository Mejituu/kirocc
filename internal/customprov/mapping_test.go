package customprov

import (
	"math/rand"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/d-kuro/kirocc/internal/anthropic"
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
		{
			name:    "duplicate rules by model and web_search",
			mapping: `[{"model":"m","web_search":true,"to":{"provider":"kiro","model":"x"}},{"model":"m","web_search":true,"to":{"provider":"kiro","model":"y"}}]`,
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

// webSearchTools is the request's web_search server-side tool declaration,
// as Claude Code's WebSearch sends it.
func webSearchTools() []anthropic.Tool {
	return []anthropic.Tool{{Type: "web_search_20250305", Name: "web_search"}}
}

// TestMatchMapping_WebSearchFilter asserts the web_search condition: true
// matches only requests carrying the tool, false only those without it, and
// an omitted filter matches either.
func TestMatchMapping_WebSearchFilter(t *testing.T) {
	mappings := `[
		{"model":"claude-sonnet-5","web_search":true,"to":{"model":"search-native"}},
		{"model":"claude-sonnet-5","web_search":false,"to":{"model":"search-off"}},
		{"model":"claude-haiku-4.5","to":{"model":"either"}}
	]`
	reg, err := loadTestRegistry(t, strings.Replace(mappingKiroConfig, "%s", mappings, 1))
	if err != nil {
		t.Fatal(err)
	}
	p := NewProxy(reg, time.Second)
	p.RecordSession("s", KiroProviderName)

	if _, m, ok := p.MatchMapping("s", RequestInfo{Model: "claude-sonnet-5", Tools: webSearchTools()}); !ok || m.ModelTo != "search-native" {
		t.Errorf("search request: match = %+v, ok = %v; want the web_search=true rule", m, ok)
	}
	if _, m, ok := p.MatchMapping("s", RequestInfo{Model: "claude-sonnet-5"}); !ok || m.ModelTo != "search-off" {
		t.Errorf("plain request: match = %+v, ok = %v; want the web_search=false rule", m, ok)
	}
	// A rule without the filter matches either kind.
	for name, info := range map[string]RequestInfo{
		"search": {Model: "claude-haiku-4.5", Tools: webSearchTools()},
		"plain":  {Model: "claude-haiku-4.5"},
	} {
		if _, m, ok := p.MatchMapping("s", info); !ok || m.ModelTo != "either" {
			t.Errorf("%s: match = %+v, ok = %v; want the rule without a web_search filter", name, m, ok)
		}
	}
}

// TestMatchMapping_WebSearchAnyModel asserts a rule may leave model empty —
// the web_search condition alone narrows it, so requests of any model hit,
// and only those carrying the tool.
func TestMatchMapping_WebSearchAnyModel(t *testing.T) {
	mappings := `[{"web_search":true,"to":{"model":"glm-5.3"}}]`
	reg, err := loadTestRegistry(t, strings.Replace(mappingKiroConfig, "%s", mappings, 1))
	if err != nil {
		t.Fatal(err)
	}
	p := NewProxy(reg, time.Second)
	p.RecordSession("s", KiroProviderName)

	for _, model := range []string{"claude-x", "glm-5.3", "claude-x[1m]"} {
		_, m, ok := p.MatchMapping("s", RequestInfo{Model: model, Tools: webSearchTools()})
		if !ok || m.Provider != KiroProviderName || m.ModelTo != "glm-5.3" {
			t.Errorf("model %q with search: match = %+v, ok = %v; want kiro/glm-5.3", model, m, ok)
		}
		if _, _, ok := p.MatchMapping("s", RequestInfo{Model: model}); ok {
			t.Errorf("model %q without search should not match", model)
		}
	}
}

// TestMatchMapping_WebSearchWithThinkingAndStream crosses the web_search
// filter with thinking and stream: each combination is queried once, and the
// combinations no rule covers must not map.
func TestMatchMapping_WebSearchWithThinkingAndStream(t *testing.T) {
	mappings := `[
		{"model":"claude-sonnet-5","web_search":true,"thinking":true,"to":{"model":"a"}},
		{"model":"claude-sonnet-5","web_search":true,"stream":false,"to":{"model":"b"}},
		{"model":"claude-sonnet-5","web_search":false,"stream":true,"to":{"model":"c"}}
	]`
	reg, err := loadTestRegistry(t, strings.Replace(mappingKiroConfig, "%s", mappings, 1))
	if err != nil {
		t.Fatal(err)
	}
	p := NewProxy(reg, time.Second)
	p.RecordSession("s", KiroProviderName)

	tools := webSearchTools()
	enabled := &thinkingPeek{Type: "enabled"}
	tests := []struct {
		name      string
		info      RequestInfo
		wantModel string // "" = no rule matches
	}{
		{"search, thinking, streaming", RequestInfo{Model: "claude-sonnet-5", Tools: tools, Thinking: enabled, Stream: true}, "a"},
		{"search, thinking, non-streaming", RequestInfo{Model: "claude-sonnet-5", Tools: tools, Thinking: enabled, Stream: false}, "a"},
		{"search, no thinking, streaming", RequestInfo{Model: "claude-sonnet-5", Tools: tools, Stream: true}, ""},
		{"search, disabled thinking, non-streaming", RequestInfo{Model: "claude-sonnet-5", Tools: tools, Thinking: &thinkingPeek{Type: "disabled"}, Stream: false}, "b"},
		{"no search, thinking, streaming", RequestInfo{Model: "claude-sonnet-5", Thinking: enabled, Stream: true}, "c"},
		{"no search, thinking, non-streaming", RequestInfo{Model: "claude-sonnet-5", Thinking: enabled, Stream: false}, ""},
		{"no search, no thinking, streaming", RequestInfo{Model: "claude-sonnet-5", Stream: true}, "c"},
		{"no search, no thinking, non-streaming", RequestInfo{Model: "claude-sonnet-5"}, ""},
	}
	for _, tt := range tests {
		_, m, ok := p.MatchMapping("s", tt.info)
		if tt.wantModel == "" {
			if ok {
				t.Errorf("%s: match = %+v; want no rule to match", tt.name, m)
			}
			continue
		}
		if !ok || m.ModelTo != tt.wantModel {
			t.Errorf("%s: match = %+v, ok = %v; want model %q", tt.name, m, ok, tt.wantModel)
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

// TestMatchMapping_GlobModel covers mapping rules whose model uses glob
// metacharacters: * (any run), ? (exactly one char) and [set]. The match is
// whole-string; literal patterns keep their historical == behaviour.
func TestMatchMapping_GlobModel(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		model   string
		want    bool
	}{
		// [.-]: the two separator spellings of one model version.
		{"class matches hyphen", "claude-haiku-5[.-]5", "claude-haiku-5-5", true},
		{"class matches dot", "claude-haiku-5[.-]5", "claude-haiku-5.5", true},
		{"class rejects missing separator", "claude-haiku-5[.-]5", "claude-haiku-55", false},
		{"class rejects other model", "claude-haiku-5[.-]5", "claude-haiku-4-5", false},
		// *: any run, including none.
		{"star matches hyphen", "claude-haiku-5*5", "claude-haiku-5-5", true},
		{"star matches dot", "claude-haiku-5*5", "claude-haiku-5.5", true},
		{"star matches empty run", "claude-haiku-5*5", "claude-haiku-55", true},
		{"star matches longer run", "claude-haiku-5*5", "claude-haiku-5.1.25", true},
		{"star rejects different prefix", "claude-haiku-5*5", "claude-haiku-4-5", false},
		{"star whole-string, no prefix sneak", "claude-haiku-5*5", "my-claude-haiku-5-5", false},
		{"star whole-string, no suffix sneak", "claude-haiku-5*5", "claude-haiku-5-5-preview", false},
		// ?: exactly one character.
		{"question matches one char", "claude-haiku-5?5", "claude-haiku-5-5", true},
		{"question rejects two chars", "claude-haiku-5?5", "claude-haiku-5.15", false},
		{"question rejects zero chars", "claude-haiku-5?5", "claude-haiku-55", false},
		// Literal patterns are unchanged.
		{"literal exact", "claude-haiku-5-5", "claude-haiku-5-5", true},
		{"literal rejects the other spelling", "claude-haiku-5-5", "claude-haiku-5.5", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := compileModelPattern(tt.pattern).matches(trimOneMSuffix(tt.model)); got != tt.want {
				t.Fatalf("compileModelPattern(%q).matches(%q) = %v, want %v", tt.pattern, tt.model, got, tt.want)
			}
		})
	}
}

// TestMatchMapping_GlobRuleEndToEnd drives a glob rule through the same
// session-mapping path a real request takes, including the [1m] strip.
func TestMatchMapping_GlobRuleEndToEnd(t *testing.T) {
	mappings := `[{"model":"claude-haiku-5[.-]5","to":{"model":"deepseek-v4.1-flash-1017"}}]`
	reg, err := loadTestRegistry(t, strings.Replace(mappingProviderConfig, "%s", mappings, 1))
	if err != nil {
		t.Fatal(err)
	}
	p := NewProxy(reg, time.Second)
	p.RecordSession("s1", "bailian")

	for _, model := range []string{"claude-haiku-5-5", "claude-haiku-5.5", "claude-haiku-5-5[1m]"} {
		if _, m, ok := p.MatchMapping("s1", RequestInfo{Model: model, Stream: true}); !ok || m.ModelTo != "deepseek-v4.1-flash-1017" {
			t.Errorf("model %q should match the glob rule; got %+v, ok=%v", model, m, ok)
		}
	}
	if _, _, ok := p.MatchMapping("s1", RequestInfo{Model: "claude-haiku-55", Stream: true}); ok {
		t.Error("claude-haiku-55 should not match the [.-] class")
	}
}

// TestMatchMapping_GlobAfterLiteral asserts rule order still decides: a
// literal rule first, a glob rule second, the literal wins for its exact
// model and the glob catches the rest.
func TestMatchMapping_GlobAfterLiteral(t *testing.T) {
	mappings := `[
		{"model":"claude-haiku-5-5","to":{"model":"deepseek-v4.1-flash-1017"}},
		{"model":"claude-haiku-5[.-]5","to":{"model":"glm-5.3"}}
	]`
	reg, err := loadTestRegistry(t, strings.Replace(mappingProviderConfig, "%s", mappings, 1))
	if err != nil {
		t.Fatal(err)
	}
	p := NewProxy(reg, time.Second)
	p.RecordSession("s1", "bailian")

	_, m, ok := p.MatchMapping("s1", RequestInfo{Model: "claude-haiku-5-5"})
	if !ok || m.ModelTo != "deepseek-v4.1-flash-1017" {
		t.Fatalf("literal rule should win; got %+v, ok=%v", m, ok)
	}
	_, m, ok = p.MatchMapping("s1", RequestInfo{Model: "claude-haiku-5.5"})
	if !ok || m.ModelTo != "glm-5.3" {
		t.Fatalf("glob rule should catch the other spelling; got %+v, ok=%v", m, ok)
	}
}

// TestBuildMappings_GlobValidation covers load-time rejection of malformed
// glob patterns and the fact that to.model stays a literal id.
func TestBuildMappings_GlobValidation(t *testing.T) {
	t.Run("malformed class rejected", func(t *testing.T) {
		mapping := `[{"model":"claude-haiku-5[5","to":{"model":"glm-5.3"}}]`
		_, err := loadTestRegistry(t, strings.Replace(mappingProviderConfig, "%s", mapping, 1))
		if err == nil || !strings.Contains(err.Error(), "malformed glob pattern") {
			t.Fatalf("expected malformed glob pattern error, got %v", err)
		}
	})
	t.Run("valid pattern accepted", func(t *testing.T) {
		mapping := `[{"model":"claude-haiku-5[.-]5","to":{"model":"glm-5.3"}}]`
		if _, err := loadTestRegistry(t, strings.Replace(mappingProviderConfig, "%s", mapping, 1)); err != nil {
			t.Fatalf("valid glob pattern should load: %v", err)
		}
	})
	t.Run("to.model with wildcard rejected as unknown id", func(t *testing.T) {
		mapping := `[{"model":"claude-haiku-5-5","to":{"model":"glm-5*3"}}]`
		_, err := loadTestRegistry(t, strings.Replace(mappingProviderConfig, "%s", mapping, 1))
		if err == nil || !strings.Contains(err.Error(), `is not a model id of provider "bailian"`) {
			t.Fatalf("to.model wildcard should fail id validation, got %v", err)
		}
	})
}

// TestMatchMapping_ClassDashForms covers the dash handling inside [set]:
// a boundary dash is literal (POSIX style) with or without an explicit
// backslash, and a mid-class dash remains a range.
func TestMatchMapping_ClassDashForms(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		model   string
		want    bool
	}{
		// Boundary dash, no escaping: the form users write.
		{"trailing dash literal", "a[.-]b", "a-b", true},
		{"trailing dash literal dot", "a[.-]b", "a.b", true},
		{"leading dash literal", "a[-.]b", "a-b", true},
		{"boundary dash rejects others", "a[.-]b", "axb", false},
		// The same patterns with an explicit backslash still work.
		{"explicitly escaped dash", "a[.\\-]b", "a-b", true},
		{"explicitly escaped dot", "a[.\\-]b", "a.b", true},
		// A dash between characters is a range.
		{"mid-class dash is a range", "a[x-z]b", "ayb", true},
		{"range rejects outside", "a[x-z]b", "awb", false},
		{"digit range", "model[0-9]x", "model5x", true},
		// Dashes outside a class are literals either way.
		{"outside-class dash untouched", "a-b", "a-b", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := compileModelPattern(tt.pattern).matches(tt.model); got != tt.want {
				t.Fatalf("compileModelPattern(%q).matches(%q) = %v, want %v", tt.pattern, tt.model, got, tt.want)
			}
		})
	}
}

// oracleMatch computes the answer path.Match would give: the historical
// matchModel behaviour, against which the compiled matcher is differenced.
func oracleMatch(pattern, model string) (want, ok bool) {
	if !hasWildcard(pattern) {
		return pattern == model, true
	}
	v, err := path.Match(escapeClassDashes(pattern), model)
	return v, err == nil
}

// TestModelPattern_DifferentialVsPathMatch pins the compiled matcher to
// path.Match's answers across a fixed corpus of patterns and models,
// including malformed patterns (must compile to kindNever) and non-ASCII
// models (must defer to path.Match's rune semantics).
func TestModelPattern_DifferentialVsPathMatch(t *testing.T) {
	patterns := []string{
		"", "a", "claude-sonnet-5", "claude-haiku-5[.-]5", "claude-haiku-5*5",
		"claude-haiku-5?5", "claude-haiku-5[.-]5*", "*claude-haiku-5", "*",
		"**", "*a", "a*", "*a*", "a**b", "a*b*c", "a*b*", "[abc]", "[^abc]",
		"[a-z]", "[a-z0-9]", "[.-]", "[-]", "[^-]", "a[.-]b", "a[x-z]b",
		"a[-z]b", "a[z-]b", `a\*b`, `a\?b`, `a\[b`, `a\\b`, "a[b", "a[",
		"[", "[a-", "[]]", "a[b", `a\`, "[/]", "[^/]", "?", "??", "a?b",
		"a[.-]b[.-]c", "[5-9]", "[!a]", "ZHIPU/*", "a*/b", "*/*", "[*]",
		"[a*]", "model[0-9]*x", "a[0-5][5-9]b", "[.]", "[.\\-]", "é",
		"aéb", "[é]", "x[y-z]w",
		// Edge shapes from the review pass: ? beside sets and stars,
		// escaped ']' and '\' inside classes, negated ranges, multi-star
		// chains, and reversed ranges.
		"a?", "*?", "?*", "a?z", "x?", "*?*", `[\]]`, `[a\-b]`, "[--]",
		"a--b", "a*b?", "?a*", "a[.-]", "[.-]*", "*[.-]", `a\?b`, `\?b`,
		`\*b`, "*a?b*", "[^a-c]", "a[b-]c", "a[-b]c", "[b-a]", "z[-]z",
		"*-*", "a?*", "*?a", "?[.-]", "[.-]?", `[\]`, `[a\]`, "a[/]b",
		"[^/]b", "**a**", "a*b*c*", "?*?*?",
	}
	models := []string{
		"", "a", "b", "claude-sonnet-5", "claude-haiku-5-5",
		"claude-haiku-5.5", "claude-haiku-55", "claude-haiku-5.1.25",
		"my-claude-haiku-5-5", "claude-haiku-5-5-preview", "ZHIPU/GLM-5.3",
		"x/y", "a/b/c", "/", "-", ".", "x-y", "x.y", "5", "9", "!", "é",
		"xéy", "aéb", "aa", "ab", "abc", "a*b", "a?b", "a[b", "a\\b", "]",
		"?", "*", "0", "z", "yw",
		"aé", "éa", "aéz", "xé", "a-", "-a", "a--b", "a]b", `a\b`, "a5b",
		"a/z", "é-", "-é", "a-b-c", "a.b.c", "ac", "za", "az", "a5", "9b",
	}
	for _, pat := range patterns {
		for _, model := range models {
			want, valid := oracleMatch(pat, model)
			compiled := compileModelPattern(pat)
			if !valid {
				if compiled.kind != kindNever {
					t.Errorf("pattern %q is malformed but compiled to kind %v", pat, compiled.kind)
				}
				continue
			}
			if got := compiled.matches(model); got != want {
				t.Errorf("compileModelPattern(%q).matches(%q) = %v, path.Match says %v", pat, model, got, want)
			}
		}
	}
}

// TestModelPattern_FuzzDifferential builds random patterns from glob tokens
// and random models from a mixed alphabet, with a fixed seed, and differences
// every combination against path.Match.
func TestModelPattern_FuzzDifferential(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	tokens := []string{
		"a", "b", "5", ".", "-", "x", "*", "?", "[a-z]", "[.-]", "[^ab]",
		`[\a]`, `\`, "/", "[0-9]", "[a", "]",
	}
	modelAlphabet := []string{"a", "b", "5", ".", "-", "x", "/", "é", "ab", ""}
	for n := 0; n < 300; n++ {
		pat := ""
		for k := rng.Intn(5) + 1; k > 0; k-- {
			pat += tokens[rng.Intn(len(tokens))]
		}
		for m := 0; m < 20; m++ {
			model := ""
			for k := rng.Intn(8); k > 0; k-- {
				model += modelAlphabet[rng.Intn(len(modelAlphabet))]
			}
			want, valid := oracleMatch(pat, model)
			compiled := compileModelPattern(pat)
			if !valid {
				if compiled.kind != kindNever {
					t.Fatalf("pattern %q is malformed but compiled to kind %v", pat, compiled.kind)
				}
				continue
			}
			if got := compiled.matches(model); got != want {
				t.Fatalf("compileModelPattern(%q).matches(%q) = %v, path.Match says %v", pat, model, got, want)
			}
		}
	}
}

// TestModelPattern_Kinds sanity-checks the compiled shapes the fast paths
// rely on.
func TestModelPattern_Kinds(t *testing.T) {
	tests := []struct {
		pattern string
		kind    matcherKind
	}{
		{"claude-sonnet-5", kindLiteral},
		{"claude-haiku-5[.-]5", kindGlob},
		{"claude-haiku-5?5", kindGlob},
		{"claude-haiku-5*5", kindStar},
		{"*claude", kindStar},
		{"claude*", kindStar},
		{"[a-", kindNever},
		{"a?\\", kindNever},
		// A lone backslash without metacharacters is a plain literal.
		{"a\\", kindLiteral},
		{"aéb", kindLiteral},
		{"aé*", kindFallback},
	}
	for _, tt := range tests {
		if got := compileModelPattern(tt.pattern).kind; got != tt.kind {
			t.Errorf("compileModelPattern(%q).kind = %v, want %v", tt.pattern, got, tt.kind)
		}
	}
	// The deterministic glob hoists prefix and suffix around one byte set.
	c := compileModelPattern("claude-haiku-5[.-]5")
	if len(c.chunks) != 1 {
		t.Fatalf("chunks = %d, want 1", len(c.chunks))
	}
	ch := c.chunks[0]
	if ch.prefix != "claude-haiku-5" || ch.suffix != "5" || len(ch.segs) != 1 || ch.length != 16 {
		t.Errorf("chunk = %+v, want prefix claude-haiku-5, suffix 5, one seg, length 17", ch)
	}
}

// BenchmarkModelPattern measures the request-path cost per rule. The
// path.Match rows are the pre-optimization baseline.
func BenchmarkModelPattern(b *testing.B) {
	models := []string{"claude-haiku-5-5", "claude-haiku-5.5", "claude-sonnet-5"}
	b.Run("Literal", func(b *testing.B) {
		p := compileModelPattern("claude-sonnet-5")
		benchMatches(b, p, models[2:])
	})
	b.Run("Class-deterministic", func(b *testing.B) {
		p := compileModelPattern("claude-haiku-5[.-]5")
		benchMatches(b, p, models[:2])
	})
	b.Run("Star", func(b *testing.B) {
		p := compileModelPattern("claude-haiku-5*5")
		benchMatches(b, p, models[:2])
	})
	b.Run("Baseline-pathMatch-class", func(b *testing.B) {
		pat := escapeClassDashes("claude-haiku-5[.-]5")
		benchmarkMatchesOracle(b, pat, models[:2])
	})
	b.Run("Baseline-pathMatch-star", func(b *testing.B) {
		benchmarkMatchesOracle(b, "claude-haiku-5*5", models[:2])
	})
}

func benchMatches(b *testing.B, p modelPattern, models []string) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.matches(models[i%len(models)])
	}
}

func benchmarkMatchesOracle(b *testing.B, pattern string, models []string) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ok, _ := path.Match(pattern, models[i%len(models)])
		_ = ok
	}
}
