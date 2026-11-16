package customprov

import (
	"testing"
	"time"
)

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	reg, err := loadTestRegistry(t, validConfig)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestLookup(t *testing.T) {
	reg := testRegistry(t)
	tests := []struct {
		model    string
		wantHit  bool
		wantBase string
		wantName string
	}{
		{model: "claude-glm-5.3", wantHit: true, wantBase: "glm-5.3", wantName: "bailian"},
		{model: "claude-glm-5.3[1m]", wantHit: true, wantBase: "glm-5.3", wantName: "bailian"},
		{model: "claude-glm-5.3[1M]", wantHit: true, wantBase: "glm-5.3", wantName: "bailian"},
		{model: "claude-kimi-k3", wantHit: true, wantBase: "kimi-k3", wantName: "bailian"},
		{model: "claude-kimi-k3[1m]", wantHit: true, wantBase: "kimi-k3", wantName: "bailian"},
		{model: "claude-unknown-model", wantHit: false},
		{model: "glm-5.3", wantHit: false},
		{model: "claude-sonnet-4-6", wantHit: false},
		{model: "", wantHit: false},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			hit, ok := reg.Lookup(tt.model)
			if ok != tt.wantHit {
				t.Fatalf("Lookup(%q) hit = %v, want %v", tt.model, ok, tt.wantHit)
			}
			if !tt.wantHit {
				return
			}
			if hit.BaseModel != tt.wantBase {
				t.Errorf("BaseModel = %q, want %q", hit.BaseModel, tt.wantBase)
			}
			if hit.Provider.Name != tt.wantName {
				t.Errorf("Provider.Name = %q, want %q", hit.Provider.Name, tt.wantName)
			}
		})
	}
}

func TestLookup_NilRegistry(t *testing.T) {
	var reg *Registry
	if _, ok := reg.Lookup("claude-glm-5.3"); ok {
		t.Error("nil registry must not match anything")
	}
}

func TestListModels(t *testing.T) {
	reg := testRegistry(t)
	models := reg.ListModels()
	if len(models) != 2 {
		t.Fatalf("len(ListModels()) = %d, want 2", len(models))
	}
	first := models[0]
	if first.ID != "claude-glm-5.3[1m]" {
		t.Errorf("models[0].ID = %q, want claude-glm-5.3[1m]", first.ID)
	}
	if first.DisplayName != "glm-5.3[1m]" {
		t.Errorf("models[0].DisplayName = %q, want glm-5.3[1m]", first.DisplayName)
	}
	if first.Provider != "bailian" {
		t.Errorf("models[0].Provider = %q, want bailian", first.Provider)
	}
	if models[1].ID != "claude-kimi-k3" {
		t.Errorf("models[1].ID = %q, want claude-kimi-k3", models[1].ID)
	}
}

// fallbackTestRegistry builds the registry by hand: the config-level fallback
// wiring lands with the config work, and these tests need no more than the
// fields themselves. bailian's models do not search, so bailian declares a
// fallback to searchy's web-search model.
func fallbackTestRegistry() *Registry {
	return &Registry{
		providers: []*Provider{
			{
				Name:     "bailian",
				BaseURL:  "https://bailian.example",
				Protocol: ProtocolAnthropic,
				Models:   []Model{{ID: "glm-5.3", Display: "glm-5.3"}},
				WebSearchFallback: &WebSearchFallbackTarget{
					Provider: "searchy",
					Model:    "web-model",
				},
			},
			{
				Name:     "searchy",
				BaseURL:  "https://searchy.example",
				Protocol: ProtocolOpenAI,
				Models:   []Model{{ID: "web-model", Display: "web-model", WebSearch: true}},
			},
		},
	}
}

func TestWebSearchFallbackHit(t *testing.T) {
	p := NewProxy(fallbackTestRegistry(), time.Second)

	hit, ok := p.WebSearchFallbackHit("bailian")
	if !ok {
		t.Fatal("WebSearchFallbackHit(bailian) missed, want hit")
	}
	if !hit.WebSearch {
		t.Error("hit.WebSearch = false, want true")
	}
	if hit.BaseModel != "web-model" {
		t.Errorf("BaseModel = %q, want web-model", hit.BaseModel)
	}
	if hit.Provider.Name != "searchy" {
		t.Errorf("Provider.Name = %q, want searchy", hit.Provider.Name)
	}

	// A provider without a fallback, and an unknown provider, both miss.
	if _, ok := p.WebSearchFallbackHit("searchy"); ok {
		t.Error("WebSearchFallbackHit(searchy) hit, want miss (no fallback)")
	}
	if _, ok := p.WebSearchFallbackHit("nope"); ok {
		t.Error("WebSearchFallbackHit(nope) hit, want miss (unknown provider)")
	}
}

// TestWebSearchFallbackHit_DanglingTarget: startup validation keeps fallback
// targets real, but a query must still miss quietly — not panic — when one
// slips through.
func TestWebSearchFallbackHit_DanglingTarget(t *testing.T) {
	reg := fallbackTestRegistry()
	reg.providers[0].WebSearchFallback = &WebSearchFallbackTarget{Provider: "searchy", Model: "missing"}
	p := NewProxy(reg, time.Second)
	if _, ok := p.WebSearchFallbackHit("bailian"); ok {
		t.Error("WebSearchFallbackHit with a dangling target hit, want miss")
	}
}

func TestKiroWebSearchFallbackHit(t *testing.T) {
	reg := fallbackTestRegistry()
	reg.KiroWebSearchFallback = &WebSearchFallbackTarget{Provider: "searchy", Model: "web-model"}
	p := NewProxy(reg, time.Second)

	hit, ok := p.KiroWebSearchFallbackHit()
	if !ok {
		t.Fatal("KiroWebSearchFallbackHit() missed, want hit")
	}
	if !hit.WebSearch {
		t.Error("hit.WebSearch = false, want true")
	}
	if hit.BaseModel != "web-model" {
		t.Errorf("BaseModel = %q, want web-model", hit.BaseModel)
	}
	if hit.Provider.Name != "searchy" {
		t.Errorf("Provider.Name = %q, want searchy", hit.Provider.Name)
	}

	// A registry without the kiro-section fallback misses.
	if _, ok := NewProxy(fallbackTestRegistry(), time.Second).KiroWebSearchFallbackHit(); ok {
		t.Error("KiroWebSearchFallbackHit without a kiro fallback hit, want miss")
	}
}

// TestWebSearchFallbackHit_NilGuards: a nil proxy or a nil registry (custom
// providers disabled) misses instead of panicking.
func TestWebSearchFallbackHit_NilGuards(t *testing.T) {
	var p *Proxy
	if _, ok := p.WebSearchFallbackHit("bailian"); ok {
		t.Error("nil proxy WebSearchFallbackHit hit, want miss")
	}
	if _, ok := p.KiroWebSearchFallbackHit(); ok {
		t.Error("nil proxy KiroWebSearchFallbackHit hit, want miss")
	}
	p = NewProxy(nil, time.Second)
	if _, ok := p.WebSearchFallbackHit("bailian"); ok {
		t.Error("nil registry WebSearchFallbackHit hit, want miss")
	}
	if _, ok := p.KiroWebSearchFallbackHit(); ok {
		t.Error("nil registry KiroWebSearchFallbackHit hit, want miss")
	}
}
