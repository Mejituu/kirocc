package customprov

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/d-kuro/kirocc/internal/logging"
)

// captureSlog redirects slog.Default to a buffer for the duration of the test.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(logging.NewOTelHandler(&buf, slog.LevelDebug)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

// writeConfig writes content to a temp config file and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mejituu.json5")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// loadTestRegistry pins KIROCC_MODEL_MAPPINGS so conflict checks test the
// built-in table, not whatever the environment carries.
func loadTestRegistry(t *testing.T, content string) (*Registry, error) {
	t.Helper()
	t.Setenv("KIROCC_MODEL_MAPPINGS", "")
	return loadFile(writeConfig(t, content))
}

// validConfig is the smallest well-formed new-style config.
const validConfig = `{
  "providers": {
    "bailian": {
      "base_url": "http://provider.test/",
      "auth_token": "secret-token",
      "models": ["glm-5.3[1m]", "kimi-k3"]
    }
  }
}`

// providerEntry builds one provider object's JSON.
func providerEntry(extra string) string {
	return `{"base_url":"http://p","auth_token":"t","models":["m"]` + extra + `}`
}

func TestLoad_ExplicitPathMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json5")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for a missing explicitly-configured file")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error should name the file, got: %v", err)
	}
}

func TestLoad_DefaultPathMissing(t *testing.T) {
	// The test binary's directory holds no mejituu.json5, so the default
	// location resolves to a missing file: custom providers are disabled.
	reg, err := Load("")
	if err != nil {
		t.Fatalf("missing default config should disable the feature, got: %v", err)
	}
	if reg != nil {
		t.Errorf("registry = %v, want nil", reg)
	}
}

func TestLoadFile_InvalidJSON(t *testing.T) {
	path := writeConfig(t, "{not json")
	_, err := loadFile(path)
	if err == nil {
		t.Fatal("expected an error for invalid JSON5")
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "invalid JSON5") {
		t.Errorf("error should name the file and the problem, got: %v", err)
	}
}

func TestLoadFile_ValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		wantErr string
	}{
		{
			name:    "empty provider name",
			config:  `{"providers": {"": ` + providerEntry("") + `}}`,
			wantErr: "providers: provider name must not be empty",
		},
		{
			name:    "duplicate provider name",
			config:  `{"providers": {"p": ` + providerEntry("") + `, "p": ` + providerEntry("") + `}}`,
			wantErr: `duplicate object member name "p"`,
		},
		{
			name:    "provider named kiro",
			config:  `{"providers": {"kiro": ` + providerEntry("") + `}}`,
			wantErr: `providers.kiro: "kiro" is reserved`,
		},
		{
			name:    "empty base_url",
			config:  `{"providers": {"p": {"base_url":"","auth_token":"t","models":["m"]}}}`,
			wantErr: "providers.p.base_url: must not be empty",
		},
		{
			name:    "non-http scheme",
			config:  `{"providers": {"p": {"base_url":"ftp://example.com","auth_token":"t","models":["m"]}}}`,
			wantErr: `must be an http or https URL`,
		},
		{
			name:    "missing host",
			config:  `{"providers": {"p": {"base_url":"http://","auth_token":"t","models":["m"]}}}`,
			wantErr: "must include a host",
		},
		{
			name:    "empty auth_token",
			config:  `{"providers": {"p": {"base_url":"http://p","auth_token":"","models":["m"]}}}`,
			wantErr: "providers.p.auth_token must not be empty",
		},
		{
			name:    "empty models",
			config:  `{"providers": {"p": {"base_url":"http://p","auth_token":"t","models":[]}}}`,
			wantErr: "providers.p.models must not be empty",
		},
		{
			name:    "empty model id in shorthand",
			config:  `{"providers": {"p": {"base_url":"http://p","auth_token":"t","models":[""]}}}`,
			wantErr: "providers.p.models[0].id must not be empty",
		},
		{
			name:    "empty model id in object",
			config:  `{"providers": {"p": {"base_url":"http://p","auth_token":"t","models":[{"alias":"m"}]}}}`,
			wantErr: "providers.p.models[0].id must not be empty",
		},
		{
			name:    "object id carrying the 1m marker",
			config:  `{"providers": {"p": {"base_url":"http://p","auth_token":"t","models":[{"id":"m[1m]"}]}}}`,
			wantErr: "providers.p.models[0].id must not carry the [1m] marker; use context_1m",
		},
		{
			name:    "object alias carrying the 1m marker",
			config:  `{"providers": {"p": {"base_url":"http://p","auth_token":"t","models":[{"id":"m","alias":"a[1m]"}]}}}`,
			wantErr: "providers.p.models[0].alias must not carry the [1m] marker; use context_1m",
		},
		{
			name: "duplicate model id within a provider",
			config: `{"providers": {"p": {"base_url":"http://p","auth_token":"t",` +
				`"models":[{"id":"glm-5.3"},{"id":"glm-5.3","alias":"other"}]}}}`,
			wantErr: `providers.p.models[1].id: "glm-5.3" is declared twice in this provider`,
		},
		{
			name: "same display name in two providers",
			config: `{"providers": {"p1": {"base_url":"http://p1","auth_token":"t","models":["glm-5.3"]},` +
				`"p2": {"base_url":"http://p2","auth_token":"t","models":["glm-5.3[1m]"]}}}`,
			wantErr: `providers.p2.models[0]: display name "glm-5.3" is already exposed by provider "p1"; use an alias`,
		},
		{
			name:    "collision with built-in anthropic id",
			config:  `{"providers": {"p": {"base_url":"http://p","auth_token":"t","models":["sonnet-4-6"]}}}`,
			wantErr: `providers.p.models[0] would expose ID "claude-sonnet-4-6"`,
		},
		{
			name:    "collision with built-in id via 1m suffix form",
			config:  `{"providers": {"p": {"base_url":"http://p","auth_token":"t","models":["zeta-9"]}}}`,
			wantErr: `providers.p.models[0] would expose ID "claude-zeta-9[1m]"`,
		},
		{
			name: "mapping with empty model",
			config: `{"providers": {"p": {"base_url":"http://p","auth_token":"t","models":["m"],` +
				`"mappings":[{"to":{"model":"x"}}]}}}`,
			wantErr: "providers.p.mappings[0].model must not be empty",
		},
		{
			name: "mapping with empty to.model",
			config: `{"providers": {"p": {"base_url":"http://p","auth_token":"t","models":["m"],` +
				`"mappings":[{"model":"m","to":{"provider":"kiro"}}]}}}`,
			wantErr: "providers.p.mappings[0].to.model must not be empty",
		},
		{
			name: "mapping with unknown to.provider",
			config: `{"providers": {"p": {"base_url":"http://p","auth_token":"t","models":["m"],` +
				`"mappings":[{"model":"m","to":{"provider":"nope","model":"x"}}]}}}`,
			wantErr: `providers.p.mappings[0].to.provider: unknown provider "nope"`,
		},
		{
			name: "mapping target given as alias instead of id",
			config: `{"providers": {"p": {"base_url":"http://p","auth_token":"t",` +
				`"models":[{"id":"real-id","alias":"pretty"}],` +
				`"mappings":[{"model":"m","to":{"model":"pretty"}}]}}}`,
			wantErr: `providers.p.mappings[0].to.model: "pretty" is not a model id of provider "p"; write the model's id, not its alias`,
		},
		{
			name: "duplicate rules in a provider group",
			config: `{"providers": {"p": {"base_url":"http://p","auth_token":"t","models":["m"],` +
				`"mappings":[{"model":"m","to":{"model":"m"}},{"model":"m","to":{"model":"m"}}]}}}`,
			wantErr: "providers.p.mappings[1] duplicates an earlier rule",
		},
		{
			name:    "kiro group mapping errors carry the kiro path",
			config:  `{"kiro": {"mappings":[{"to":{"model":"x"}}]},"providers": {"p": ` + providerEntry("") + `}}`,
			wantErr: "kiro.mappings[0].model must not be empty",
		},
		{
			name:    "duplicate rules in the kiro group",
			config:  `{"kiro":{"mappings":[{"model":"m","to":{"model":"x"}},{"model":"m","to":{"model":"y"}}]}}`,
			wantErr: "kiro.mappings[1] duplicates an earlier rule",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The suffix-form case needs an env mapping that only defines the
			// [1m] spelling; the other cases test the built-in table.
			env := ""
			if tt.name == "collision with built-in id via 1m suffix form" {
				env = `[{"anthropic":"claude-zeta-9[1m]","kiro":"zeta-9"}]`
			}
			t.Setenv("KIROCC_MODEL_MAPPINGS", env)
			_, err := loadFile(writeConfig(t, tt.config))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadFile_UnknownMembers(t *testing.T) {
	tests := []struct {
		name   string
		config string
		want   string
	}{
		{
			name:   "top level",
			config: `{"bogus": 1}`,
			want:   `unknown object member name "bogus"`,
		},
		{
			name:   "kiro section",
			config: `{"kiro": {"bogus": 1}}`,
			want:   `unknown object member name "bogus"`,
		},
		{
			name:   "provider object",
			config: `{"providers": {"p": {"base_url":"http://p","auth_token":"t","models":["m"],"bogus": 1}}}`,
			want:   `unknown object member name "bogus"`,
		},
		{
			name: "models object",
			config: `{"providers": {"p": {"base_url":"http://p","auth_token":"t",` +
				`"models":[{"id":"m","bogus": 1}]}}}`,
			want: `unknown object member name "bogus"`,
		},
		{
			name:   "mapping rule object",
			config: `{"kiro": {"mappings":[{"model":"m","to":{"model":"x"},"bogus": 1}]}}`,
			want:   `unknown object member name "bogus"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadFile(writeConfig(t, tt.config))
			if err == nil {
				t.Fatalf("expected an unknown-member error, got nil")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestLoadFile_JSON5Syntax(t *testing.T) {
	path := writeConfig(t, `{
		// Providers whose models are forwarded verbatim.
		providers: {
			bailian: {
				base_url: 'http://provider.test/',
				auth_token: 'secret-token',
				count_tokens: false,
				models: ['glm-5.3[1m]', 'kimi-k3'], /* trailing comma */
			},
		},
	}`)
	reg, err := loadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	p := reg.providers[0]
	if p.Name != "bailian" || p.BaseURL != "http://provider.test" || p.CountTokens {
		t.Errorf("provider fields wrong: %+v", p)
	}
	if len(p.Models) != 2 {
		t.Fatalf("models = %v, want 2", p.Models)
	}
	if hit, ok := reg.Lookup("claude-kimi-k3"); !ok || hit.BaseModel != "kimi-k3" {
		t.Errorf("Lookup(claude-kimi-k3) = %+v, %v", hit, ok)
	}
}

func TestLoadFile_DecodeErrorLocation(t *testing.T) {
	t.Run("syntax error after comments and strings", func(t *testing.T) {
		// The missing colon sits on line 7, after a comment and
		// single-quoted strings.
		src := "{\n  // provider list\n  providers: {\n    bailian: {\n      base_url: 'http://p',\n      auth_token: 't',\n      url 'x',\n    },\n  },\n}"
		_, err := loadFile(writeConfig(t, src))
		if err == nil {
			t.Fatal("expected an error for the missing colon")
		}
		if !strings.Contains(err.Error(), "invalid JSON5") || !strings.Contains(err.Error(), "line 7,") {
			t.Errorf("error should locate the original line 7, got: %v", err)
		}
	})

	t.Run("type error keeps the pointer and line", func(t *testing.T) {
		// The numeric models entry sits on line 7.
		src := "{\n  // provider\n  providers: {\n    bailian: {\n      base_url: 'http://p',\n      auth_token: 't',\n      models: [42],\n    },\n  },\n}"
		_, err := loadFile(writeConfig(t, src))
		if err == nil {
			t.Fatal("expected an error for the numeric models entry")
		}
		if !strings.Contains(err.Error(), "line 7,") {
			t.Errorf("error should locate the original line 7, got: %v", err)
		}
		if !strings.Contains(err.Error(), "/providers/bailian/models/0") {
			t.Errorf("error should keep the JSON pointer, got: %v", err)
		}
	})
}

// TestLoadFile_ProviderOrder asserts the file's declaration order survives:
// the load log and ListModels both follow it, with keys that sort in the
// opposite order.
func TestLoadFile_ProviderOrder(t *testing.T) {
	config := `{
		"providers": {
			"zulu": {"base_url":"http://z","auth_token":"t","models":["z-model"]},
			"mike": {"base_url":"http://m","auth_token":"t","models":["m-model"]},
			"alpha": {"base_url":"http://a","auth_token":"t","models":["a-model"]}
		}
	}`
	buf := captureSlog(t)
	reg, err := loadTestRegistry(t, config)
	if err != nil {
		t.Fatal(err)
	}

	var wantNames []string
	for _, want := range []string{"zulu", "mike", "alpha"} {
		wantNames = append(wantNames, want)
	}
	got := make([]string, 0, len(reg.providers))
	for _, p := range reg.providers {
		got = append(got, p.Name)
	}
	if strings.Join(got, ",") != strings.Join(wantNames, ",") {
		t.Errorf("provider order = %v, want %v", got, wantNames)
	}
	logs := buf.String()
	prev := -1
	for _, want := range wantNames {
		idx := strings.Index(logs, `"name":"`+want+`"`)
		if idx < 0 || idx < prev {
			t.Errorf("load log out of order for %q; logs:\n%s", want, logs)
		}
		prev = idx
	}
	var wantIDs []string
	for _, p := range reg.providers {
		for _, m := range p.Models {
			wantIDs = append(wantIDs, "claude-"+m.Display)
		}
	}
	models := reg.ListModels()
	gotIDs := make([]string, 0, len(models))
	for _, m := range models {
		gotIDs = append(gotIDs, m.ID)
	}
	if strings.Join(gotIDs, ",") != strings.Join(wantIDs, ",") {
		t.Errorf("ListModels order = %v, want %v", gotIDs, wantIDs)
	}
}

// TestLoadFile_ModelForms asserts both models spellings and their fields.
func TestLoadFile_ModelForms(t *testing.T) {
	config := `{
		"providers": {
			"p": {
				"base_url": "http://p",
				"auth_token": "t",
				"models": [
					"kimi-k3[1m]",
					"plain",
					{"id": "real-id", "alias": "pretty", "context_1m": true, "label": "Pretty · label"},
					{"id": "ZHIPU/GLM-5.3"}
				]
			}
		}
	}`
	reg, err := loadTestRegistry(t, config)
	if err != nil {
		t.Fatal(err)
	}
	models := reg.providers[0].Models
	if len(models) != 4 {
		t.Fatalf("models = %v, want 4", models)
	}
	want := []Model{
		{ID: "kimi-k3", Display: "kimi-k3", Context1M: true},
		{ID: "plain", Display: "plain"},
		{ID: "real-id", Display: "pretty", Context1M: true, Label: "Pretty · label"},
		{ID: "ZHIPU/GLM-5.3", Display: "ZHIPU/GLM-5.3"},
	}
	for i, w := range want {
		if models[i] != w {
			t.Errorf("models[%d] = %+v, want %+v", i, models[i], w)
		}
	}
}

func TestLoadFile_BaseURLTrailingSlashTrimmed(t *testing.T) {
	reg, err := loadTestRegistry(t, validConfig)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := reg.providers[0].BaseURL, "http://provider.test"; got != want {
		t.Errorf("BaseURL = %q, want %q", got, want)
	}
}

func TestLoadFile_CountTokensDefault(t *testing.T) {
	tests := []struct {
		name   string
		config string
		want   bool
	}{
		{name: "omitted defaults to true", config: validConfig, want: true},
		{
			name:   "explicit false",
			config: `{"providers": {"p": {"base_url":"http://p","auth_token":"t","count_tokens":false,"models":["m"]}}}`,
			want:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg, err := loadTestRegistry(t, tt.config)
			if err != nil {
				t.Fatal(err)
			}
			if got := reg.providers[0].CountTokens; got != tt.want {
				t.Errorf("CountTokens = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestLoadOpenAIProtocol covers the protocol field: the anthropic default,
// the openai conversion, and the count_tokens interplay each protocol implies.
func TestLoadOpenAIProtocol(t *testing.T) {
	tests := []struct {
		name      string
		provider  string
		wantErrs  []string
		wantProto Protocol
		wantCount bool
	}{
		{
			name:      "omitted protocol defaults to anthropic",
			provider:  providerEntry(""),
			wantProto: ProtocolAnthropic,
			wantCount: true,
		},
		{
			name:      "explicit anthropic",
			provider:  providerEntry(`,"protocol":"anthropic"`),
			wantProto: ProtocolAnthropic,
			wantCount: true,
		},
		{
			name:      "openai defaults count_tokens to false",
			provider:  providerEntry(`,"protocol":"openai"`),
			wantProto: ProtocolOpenAI,
			wantCount: false,
		},
		{
			name:      "openai with explicit count_tokens false",
			provider:  providerEntry(`,"protocol":"openai","count_tokens":false`),
			wantProto: ProtocolOpenAI,
			wantCount: false,
		},
		{
			name:      "anthropic with explicit count_tokens true",
			provider:  providerEntry(`,"protocol":"anthropic","count_tokens":true`),
			wantProto: ProtocolAnthropic,
			wantCount: true,
		},
		{
			name:     "unknown protocol",
			provider: providerEntry(`,"protocol":"grpc"`),
			wantErrs: []string{
				`providers.p.protocol`,
				`only "anthropic" and "openai" are supported, got "grpc"`,
			},
		},
		{
			name:     "openai with count_tokens true",
			provider: providerEntry(`,"protocol":"openai","count_tokens":true`),
			wantErrs: []string{
				`providers.p.count_tokens`,
				"no count_tokens endpoint",
				"kirocc estimates locally",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg, err := loadTestRegistry(t, `{"providers": {"p": `+tt.provider+`}}`)
			if len(tt.wantErrs) > 0 {
				if err == nil {
					t.Fatalf("expected an error, got nil")
				}
				for _, want := range tt.wantErrs {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error = %v, want it to contain %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			p := reg.providers[0]
			if p.Protocol != tt.wantProto {
				t.Errorf("Protocol = %q, want %q", p.Protocol, tt.wantProto)
			}
			if p.CountTokens != tt.wantCount {
				t.Errorf("CountTokens = %v, want %v", p.CountTokens, tt.wantCount)
			}
		})
	}
}

// TestLoadAlwaysThinking covers the always_thinking model flag: valid on
// openai-protocol models (and carried through lookup), rejected on
// anthropic providers, and a misspelling caught by the unknown-member
// rejection.
func TestLoadAlwaysThinking(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		wantErrs []string
	}{
		{
			name:     "anthropic protocol rejects always_thinking",
			provider: `{"base_url":"http://p","auth_token":"t","models":[{"id":"m","always_thinking":true}]}`,
			wantErrs: []string{
				`providers.p.models[0].always_thinking`,
				"only OpenAI-protocol models support it",
			},
		},
		{
			name:     "anthropic protocol accepts an explicit false",
			provider: `{"base_url":"http://p","auth_token":"t","models":[{"id":"m","always_thinking":false}]}`,
		},
		{
			name:     "misspelled field is an unknown member",
			provider: `{"base_url":"http://p","auth_token":"t","protocol":"openai","models":[{"id":"m","always_thinkin":true}]}`,
			wantErrs: []string{"always_thinkin"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadTestRegistry(t, `{"providers": {"p": `+tt.provider+`}}`)
			if len(tt.wantErrs) > 0 {
				if err == nil {
					t.Fatalf("expected an error, got nil")
				}
				for _, want := range tt.wantErrs {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error = %v, want it to contain %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestLookupModelCarriesAlwaysThinking asserts the mapping target lookup
// (the path session-following mappings take) hands the flag to the Hit.
func TestLookupModelCarriesAlwaysThinking(t *testing.T) {
	reg, err := loadTestRegistry(t, `{"providers": {"p": {
		"base_url": "http://p", "auth_token": "t", "protocol": "openai",
		"models": [{"id": "m", "always_thinking": true}, {"id": "n"}]
	}}}`)
	if err != nil {
		t.Fatal(err)
	}

	hit, ok := reg.lookupModel("p", "m")
	if !ok {
		t.Fatal("lookupModel missed")
	}
	if !hit.AlwaysThinking {
		t.Error("hit.AlwaysThinking = false, want true for the configured model")
	}
	hit, ok = reg.lookupModel("p", "n")
	if !ok {
		t.Fatal("lookupModel missed")
	}
	if hit.AlwaysThinking {
		t.Error("hit.AlwaysThinking = true, want false for a plain model")
	}
}

// TestLoadWebSearch covers the web_search model flag: the object form turns
// it on, the string shorthand and an omitted field stay off, and both
// protocols accept it (unlike always_thinking).
func TestLoadWebSearch(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		want     bool
	}{
		{
			name:     "anthropic protocol object form",
			provider: `{"base_url":"http://p","auth_token":"t","models":[{"id":"qwen3.8-max","web_search":true}]}`,
			want:     true,
		},
		{
			name:     "openai protocol object form",
			provider: `{"base_url":"http://p","auth_token":"t","protocol":"openai","models":[{"id":"qwen3.8-max","web_search":true}]}`,
			want:     true,
		},
		{
			name:     "object form without the field defaults to false",
			provider: `{"base_url":"http://p","auth_token":"t","models":[{"id":"qwen3.8-max"}]}`,
			want:     false,
		},
		{
			name:     "string shorthand stays false",
			provider: `{"base_url":"http://p","auth_token":"t","models":["qwen3.8-max"]}`,
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg, err := loadTestRegistry(t, `{"providers": {"p": `+tt.provider+`}}`)
			if err != nil {
				t.Fatal(err)
			}
			m := reg.providers[0].Models[0]
			if m.WebSearch != tt.want {
				t.Errorf("Models[0].WebSearch = %v, want %v", m.WebSearch, tt.want)
			}
		})
	}
}

// TestLookupCarriesWebSearch asserts both lookup paths hand the flag to the
// Hit: Lookup (the claude- display-name match) and lookupModel (the target
// of session-following mappings, exercised through a mapping rule).
func TestLookupCarriesWebSearch(t *testing.T) {
	reg, err := loadTestRegistry(t, `{"providers": {"p": {
		"base_url": "http://p", "auth_token": "t",
		"models": [{"id": "qwen3.8-max", "web_search": true}, {"id": "n"}],
		"mappings": [{"model": "other", "to": {"model": "qwen3.8-max"}}]
	}}}`)
	if err != nil {
		t.Fatal(err)
	}

	hit, ok := reg.Lookup("claude-qwen3.8-max")
	if !ok {
		t.Fatal("Lookup missed")
	}
	if !hit.WebSearch {
		t.Error("hit.WebSearch = false, want true for the configured model")
	}
	hit, ok = reg.Lookup("claude-n")
	if !ok {
		t.Fatal("Lookup missed")
	}
	if hit.WebSearch {
		t.Error("hit.WebSearch = true, want false for a plain model")
	}
	hit, ok = reg.lookupModel("p", "qwen3.8-max")
	if !ok {
		t.Fatal("lookupModel missed")
	}
	if !hit.WebSearch {
		t.Error("hit.WebSearch = false, want true for the mapping target")
	}
}

// TestLoadMappingWebSearch covers the web_search mapping condition: a
// web_search rule may leave model empty (it matches any model), a custom
// target must itself enable web_search, and web_search:false gains no
// privileges.
func TestLoadMappingWebSearch(t *testing.T) {
	tests := []struct {
		name      string
		config    string
		wantErrs  []string
		wantCount int
	}{
		{
			name:      "web_search rule without a model matches any model",
			config:    `{"kiro":{"mappings":[{"web_search":true,"to":{"provider":"kiro","model":"x"}}]}}`,
			wantCount: 1,
		},
		{
			name:      "web_search rule with a model",
			config:    `{"kiro":{"mappings":[{"model":"m","web_search":true,"to":{"provider":"kiro","model":"x"}}]}}`,
			wantCount: 1,
		},
		{
			name:     "omitted model without web_search still errors",
			config:   `{"providers":{"p":` + providerEntry(`,"mappings":[{"to":{"model":"m"}}]`) + `}}`,
			wantErrs: []string{"providers.p.mappings[0].model must not be empty"},
		},
		{
			name:     "web_search false does not unlock an omitted model",
			config:   `{"providers":{"p":` + providerEntry(`,"mappings":[{"web_search":false,"to":{"model":"m"}}]`) + `}}`,
			wantErrs: []string{"providers.p.mappings[0].model must not be empty"},
		},
		{
			name:   "web_search rule targeting a model without web_search",
			config: `{"providers":{"p":` + providerEntry(`,"mappings":[{"model":"m","web_search":true,"to":{"model":"m"}}]`) + `}}`,
			wantErrs: []string{
				`providers.p.mappings[0].to.model: "m" does not enable web_search`,
				"a web_search rule must target a model with web_search true",
			},
		},
		{
			name:      "web_search rule targeting a model with web_search",
			config:    `{"providers":{"p":{"base_url":"http://p","auth_token":"t","models":[{"id":"qwen3.8-max","web_search":true}],"mappings":[{"model":"m","web_search":true,"to":{"model":"qwen3.8-max"}}]}}}`,
			wantCount: 1,
		},
		{
			name:      "web_search rule targeting kiro skips the target check",
			config:    `{"providers":{"p":` + providerEntry(`,"mappings":[{"web_search":true,"to":{"provider":"kiro","model":"x"}}]`) + `}}`,
			wantCount: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg, err := loadTestRegistry(t, tt.config)
			if len(tt.wantErrs) > 0 {
				if err == nil {
					t.Fatalf("expected an error, got nil")
				}
				for _, want := range tt.wantErrs {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error = %v, want it to contain %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := reg.mappingCount(); got != tt.wantCount {
				t.Errorf("mappingCount = %d, want %d", got, tt.wantCount)
			}
		})
	}
}

// TestLoadMappingWebSearchCondition asserts the compiled rule carries the
// web_search condition through to the Mapping, true or false, and keeps an
// omitted model empty.
func TestLoadMappingWebSearchCondition(t *testing.T) {
	reg, err := loadTestRegistry(t, `{"kiro":{"mappings":[
		{"web_search": true, "to": {"provider": "kiro", "model": "x"}},
		{"model": "m", "web_search": false, "to": {"provider": "kiro", "model": "y"}}
	]}}`)
	if err != nil {
		t.Fatal(err)
	}
	ms := reg.mappings["kiro"]
	if len(ms) != 2 {
		t.Fatalf("mappings = %v, want 2 rules", ms)
	}
	if ms[0].Model != "" {
		t.Errorf("mappings[0].Model = %q, want empty for an any-model rule", ms[0].Model)
	}
	if ms[0].WebSearch == nil || !*ms[0].WebSearch {
		t.Error("mappings[0].WebSearch should carry a true condition")
	}
	if ms[1].WebSearch == nil || *ms[1].WebSearch {
		t.Error("mappings[1].WebSearch should carry a false condition")
	}
}

// TestLoadWebSearchFallback covers the web_search_fallback entry: an
// omitted provider targets the group's own provider, an explicit one may be
// declared later in the file, and the kiro section has no default at all.
// The target must be a declared provider, one of its model ids, and
// web_search-enabled — never kiro itself, which cannot search.
func TestLoadWebSearchFallback(t *testing.T) {
	tests := []struct {
		name     string
		config   string
		wantErrs []string
		onKiro   bool
		want     *WebSearchFallbackTarget
	}{
		{
			name: "omitted provider targets the provider itself",
			config: `{"providers":{"p":{
				"base_url":"http://p","auth_token":"t",
				"models":[{"id":"m"},{"id":"qwen3.8-max","web_search":true}],
				"web_search_fallback":{"model":"qwen3.8-max"}
			}}}`,
			want: &WebSearchFallbackTarget{Provider: "p", Model: "qwen3.8-max"},
		},
		{
			name: "explicit cross-provider target declared later in the file",
			config: `{"providers":{
				"a":{"base_url":"http://a","auth_token":"t","models":["m"],
					"web_search_fallback":{"provider":"b","model":"s"}},
				"b":{"base_url":"http://b","auth_token":"t","models":[{"id":"s","web_search":true}]}
			}}`,
			want: &WebSearchFallbackTarget{Provider: "b", Model: "s"},
		},
		{
			name: "target model without web_search",
			config: `{"providers":{"p":{
				"base_url":"http://p","auth_token":"t","models":["m"],
				"web_search_fallback":{"model":"m"}
			}}}`,
			wantErrs: []string{`providers.p.web_search_fallback.model: "m" does not enable web_search`},
		},
		{
			name: "unknown target provider",
			config: `{"providers":{"p":{
				"base_url":"http://p","auth_token":"t","models":["m"],
				"web_search_fallback":{"provider":"nope","model":"m"}
			}}}`,
			wantErrs: []string{`providers.p.web_search_fallback.provider: unknown provider "nope"`},
		},
		{
			name: "unknown target model",
			config: `{"providers":{"p":{
				"base_url":"http://p","auth_token":"t","models":["m"],
				"web_search_fallback":{"model":"x"}
			}}}`,
			wantErrs: []string{`providers.p.web_search_fallback.model: "x" is not a model id of provider "p"`},
		},
		{
			name:     "kiro section without a provider",
			config:   `{"kiro":{"web_search_fallback":{"model":"qwen3.8-max"}}}`,
			wantErrs: []string{"kiro.web_search_fallback.provider: kiro cannot search; the fallback must name a custom provider"},
		},
		{
			name:     "kiro section naming kiro",
			config:   `{"kiro":{"web_search_fallback":{"provider":"kiro","model":"x"}}}`,
			wantErrs: []string{"kiro.web_search_fallback.provider: kiro cannot search; the fallback must name a custom provider"},
		},
		{
			name: "provider section naming kiro",
			config: `{"providers":{"p":{
				"base_url":"http://p","auth_token":"t","models":["m"],
				"web_search_fallback":{"provider":"kiro","model":"x"}
			}}}`,
			wantErrs: []string{"providers.p.web_search_fallback.provider: kiro cannot search; the fallback must name a custom provider"},
		},
		{
			name: "kiro section with an explicit custom provider",
			config: `{"kiro":{"web_search_fallback":{"provider":"p","model":"qwen3.8-max"}},
				"providers":{"p":{"base_url":"http://p","auth_token":"t","models":[{"id":"qwen3.8-max","web_search":true}]}}}`,
			onKiro: true,
			want:   &WebSearchFallbackTarget{Provider: "p", Model: "qwen3.8-max"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg, err := loadTestRegistry(t, tt.config)
			if len(tt.wantErrs) > 0 {
				if err == nil {
					t.Fatalf("expected an error, got nil")
				}
				for _, want := range tt.wantErrs {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error = %v, want it to contain %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := reg.KiroWebSearchFallback
			if !tt.onKiro {
				got = reg.providers[0].WebSearchFallback
			}
			switch {
			case tt.want == nil && got != nil:
				t.Errorf("WebSearchFallback = %+v, want nil", got)
			case tt.want != nil && (got == nil || *got != *tt.want):
				t.Errorf("WebSearchFallback = %+v, want %+v", got, *tt.want)
			}
		})
	}

	t.Run("no fallback configured stays nil", func(t *testing.T) {
		reg, err := loadTestRegistry(t, validConfig)
		if err != nil {
			t.Fatal(err)
		}
		if reg.providers[0].WebSearchFallback != nil || reg.KiroWebSearchFallback != nil {
			t.Errorf("fallbacks = %+v/%+v, want nil/nil",
				reg.providers[0].WebSearchFallback, reg.KiroWebSearchFallback)
		}
	})
}

func TestLoadFile_PermissionWarning(t *testing.T) {
	buf := captureSlog(t)
	path := filepath.Join(t.TempDir(), "mejituu.json5")
	if err := os.WriteFile(path, []byte(validConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFile(path); err != nil {
		t.Fatal(err)
	}
	logs := buf.String()
	if !strings.Contains(logs, "chmod 600") {
		t.Errorf("expected a chmod 600 warning, logs:\n%s", logs)
	}
	// The token must not surface in any log line, warning or info.
	if strings.Contains(logs, "secret-token") {
		t.Errorf("auth_token leaked into logs:\n%s", logs)
	}
}

func TestLoadFile_ProviderLoadedLog(t *testing.T) {
	buf := captureSlog(t)
	if _, err := loadTestRegistry(t, validConfig); err != nil {
		t.Fatal(err)
	}
	logs := buf.String()
	for _, want := range []string{"custom provider loaded", `"name":"bailian"`, `"models":2`, `"count_tokens":true`} {
		if !strings.Contains(logs, want) {
			t.Errorf("log output missing %q, logs:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, "secret-token") {
		t.Errorf("auth_token leaked into logs:\n%s", logs)
	}
}

func TestDefaultPathFor_Symlink(t *testing.T) {
	dir := t.TempDir()
	realPath := filepath.Join(dir, "dist", "kirocc")
	if err := os.MkdirAll(filepath.Dir(realPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(realPath, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(dir, "bin", "kirocc")
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatal(err)
	}
	// EvalSymlinks so the comparison holds on macOS, where /var resolves
	// under /private.
	realDir, err := filepath.EvalSymlinks(filepath.Dir(realPath))
	if err != nil {
		t.Fatal(err)
	}
	got, want := defaultPathFor(linkPath), filepath.Join(realDir, "mejituu.json5")
	if got != want {
		t.Errorf("defaultPathFor(symlink) = %q, want %q", got, want)
	}
}
