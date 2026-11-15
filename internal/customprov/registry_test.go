package customprov

import "testing"

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
