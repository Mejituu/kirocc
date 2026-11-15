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

const validConfig = `{
  "providers": [{
    "name": "bailian",
    "base_url": "http://provider.test/",
    "auth_token": "secret-token",
    "models": ["glm-5.3[1m]", "kimi-k3"]
  }]
}`

func TestLoad_ExplicitPathMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
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
			config:  `{"providers":[{"name":"","base_url":"http://p","auth_token":"t","models":["m"]}]}`,
			wantErr: "providers[0].name must not be empty",
		},
		{
			name: "duplicate provider name",
			config: `{"providers":[
				{"name":"p","base_url":"http://p","auth_token":"t","models":["m1"]},
				{"name":"p","base_url":"http://q","auth_token":"t","models":["m2"]}]}`,
			wantErr: `is declared twice`,
		},
		{
			name:    "empty base_url",
			config:  `{"providers":[{"name":"p","base_url":"","auth_token":"t","models":["m"]}]}`,
			wantErr: "providers[0].base_url: must not be empty",
		},
		{
			name:    "non-http scheme",
			config:  `{"providers":[{"name":"p","base_url":"ftp://example.com","auth_token":"t","models":["m"]}]}`,
			wantErr: `must be an http or https URL`,
		},
		{
			name:    "missing host",
			config:  `{"providers":[{"name":"p","base_url":"http://","auth_token":"t","models":["m"]}]}`,
			wantErr: "must include a host",
		},
		{
			name:    "empty auth_token",
			config:  `{"providers":[{"name":"p","base_url":"http://p","auth_token":"","models":["m"]}]}`,
			wantErr: "providers[0].auth_token must not be empty",
		},
		{
			name:    "empty models",
			config:  `{"providers":[{"name":"p","base_url":"http://p","auth_token":"t","models":[]}]}`,
			wantErr: "providers[0].models must not be empty",
		},
		{
			name:    "empty model name",
			config:  `{"providers":[{"name":"p","base_url":"http://p","auth_token":"t","models":[""]}]}`,
			wantErr: "model name must not be empty",
		},
		{
			name: "same model in two providers",
			config: `{"providers":[
				{"name":"p1","base_url":"http://p1","auth_token":"t","models":["glm-5.3"]},
				{"name":"p2","base_url":"http://p2","auth_token":"t","models":["glm-5.3[1m]"]}]}`,
			wantErr: `model "glm-5.3" is declared by providers "p1" and "p2"`,
		},
		{
			name:    "collision with built-in anthropic id",
			config:  `{"providers":[{"name":"p","base_url":"http://p","auth_token":"t","models":["sonnet-4-6"]}]}`,
			wantErr: `expose ID "claude-sonnet-4-6"`,
		},
		{
			name:    "collision with built-in id via 1m suffix form",
			config:  `{"providers":[{"name":"p","base_url":"http://p","auth_token":"t","models":["zeta-9"]}]}`,
			wantErr: `expose ID "claude-zeta-9[1m]"`,
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

func TestLoadFile_JSON5Syntax(t *testing.T) {
	path := writeConfig(t, `{
		// Providers whose models are forwarded verbatim.
		providers: [{
			name: 'bailian',
			base_url: 'http://provider.test/',
			auth_token: 'secret-token',
			count_tokens: false,
			models: ['glm-5.3[1m]', 'kimi-k3'], /* trailing comma */
		}],
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
		// The missing colon sits on line 5, after a comment and a
		// single-quoted string.
		src := "{\n  // provider list\n  providers: [{\n    name: 'bailian',\n    url 'x',\n  }],\n}"
		_, err := loadFile(writeConfig(t, src))
		if err == nil {
			t.Fatal("expected an error for the missing colon")
		}
		if !strings.Contains(err.Error(), "invalid JSON5") || !strings.Contains(err.Error(), "line 5,") {
			t.Errorf("error should locate the original line 5, got: %v", err)
		}
	})

	t.Run("type error keeps the pointer and line", func(t *testing.T) {
		// count_tokens: 'yes' sits on line 7.
		src := "{\n  // provider\n  providers: [{\n    name: 'bailian',\n    base_url: 'http://p',\n    auth_token: 't',\n    count_tokens: 'yes',\n    models: ['m'],\n  }],\n}"
		_, err := loadFile(writeConfig(t, src))
		if err == nil {
			t.Fatal("expected an error for the string-typed count_tokens")
		}
		if !strings.Contains(err.Error(), "line 7,") {
			t.Errorf("error should locate the original line 7, got: %v", err)
		}
		if !strings.Contains(err.Error(), "/providers/0/count_tokens") {
			t.Errorf("error should keep the JSON pointer, got: %v", err)
		}
	})
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
			config: `{"providers":[{"name":"p","base_url":"http://p","auth_token":"t","count_tokens":false,"models":["m"]}]}`,
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
