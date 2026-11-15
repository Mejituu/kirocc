// Package customprov routes requests for user-declared models to
// Anthropic-compatible custom providers (e.g. Alibaba Bailian's Anthropic
// endpoint), forwarding the Messages API request verbatim instead of
// converting it to the Kiro protocol.
package customprov

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"encoding/json/v2"

	"github.com/d-kuro/kirocc/internal/models"
)

// configFileName is the file looked up next to the executable by default.
const configFileName = "mejituu.json"

// providerConfig is one entry of the config file's "providers" array.
type providerConfig struct {
	Name        string   `json:"name"`
	BaseURL     string   `json:"base_url"`
	AuthToken   string   `json:"auth_token"`
	CountTokens *bool    `json:"count_tokens,omitempty"` // nil means true
	Models      []string `json:"models"`
}

// fileConfig is the top level of the config file.
type fileConfig struct {
	Providers []providerConfig `json:"providers"`
}

// Load reads the custom provider config. An empty path selects the default
// location, mejituu.json next to the real executable; a default-location file
// that does not exist disables custom providers, while an explicitly given
// path must exist. The returned registry is nil when custom providers are
// disabled.
func Load(path string) (*Registry, error) {
	if path != "" {
		return loadFile(path)
	}
	p, err := defaultPath()
	if err != nil {
		return nil, fmt.Errorf("locating default config: %w", err)
	}
	if _, err := os.Stat(p); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			slog.Debug("custom provider config not found at default location, custom providers disabled", "path", p)
			return nil, nil
		}
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return loadFile(p)
}

// defaultPath returns the config location next to the real executable:
// ~/.local/bin/kirocc is often a symlink to dist/kirocc, and the config is
// expected to live beside the real file.
func defaultPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return defaultPathFor(exe), nil
}

// defaultPathFor resolves symlinks and returns the config location beside the
// resulting executable.
func defaultPathFor(exe string) string {
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return filepath.Join(filepath.Dir(exe), configFileName)
}

// loadFile reads, validates and installs one config file. Validation errors
// name the file and the offending field so a bad entry is fixable without
// guessing.
func loadFile(path string) (*Registry, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		slog.Warn("custom provider config is readable by group or others; run chmod 600",
			"path", path, "mode", perm.String())
	}
	var fc fileConfig
	if err := json.Unmarshal(raw, &fc); err != nil {
		return nil, fmt.Errorf("%s: invalid JSON: %w", path, err)
	}
	registry, err := buildRegistry(path, fc)
	if err != nil {
		return nil, err
	}
	for _, p := range registry.providers {
		slog.Info("custom provider loaded",
			"name", p.Name,
			"models", len(p.Models),
			"count_tokens", p.CountTokens,
			"config", path,
		)
	}
	return registry, nil
}

// buildRegistry validates the parsed file and turns it into a lookup table.
// Errors are prefixed with the file path and locate the offending entry.
func buildRegistry(path string, fc fileConfig) (*Registry, error) {
	registry := &Registry{byBase: make(map[string]*Provider, countModels(fc))}
	seenNames := make(map[string]bool, len(fc.Providers))
	for i, pc := range fc.Providers {
		if pc.Name == "" {
			return nil, fmt.Errorf("%s: providers[%d].name must not be empty", path, i)
		}
		if seenNames[pc.Name] {
			return nil, fmt.Errorf("%s: providers[%d].name %q is declared twice", path, i, pc.Name)
		}
		seenNames[pc.Name] = true

		baseURL, err := normalizeBaseURL(pc.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("%s: providers[%d].base_url: %w", path, i, err)
		}
		if pc.AuthToken == "" {
			return nil, fmt.Errorf("%s: providers[%d].auth_token must not be empty", path, i)
		}
		if len(pc.Models) == 0 {
			return nil, fmt.Errorf("%s: providers[%d].models must not be empty", path, i)
		}

		p := &Provider{
			Name:        pc.Name,
			BaseURL:     baseURL,
			authToken:   pc.AuthToken,
			CountTokens: pc.CountTokens == nil || *pc.CountTokens,
		}
		for _, name := range pc.Models {
			m, err := newModel(name)
			if err != nil {
				return nil, fmt.Errorf("%s: providers[%d].models: %w", path, i, err)
			}
			if other, ok := registry.byBase[m.Base]; ok {
				return nil, fmt.Errorf("%s: model %q is declared by providers %q and %q", path, m.Base, other.Name, p.Name)
			}
			if err := checkBuiltinConflict(path, m); err != nil {
				return nil, err
			}
			p.Models = append(p.Models, m)
			registry.byBase[m.Base] = p
		}
		registry.providers = append(registry.providers, p)
	}
	return registry, nil
}

// countModels sums the declared models to size the lookup map.
func countModels(fc fileConfig) int {
	n := 0
	for _, pc := range fc.Providers {
		n += len(pc.Models)
	}
	return n
}

// normalizeBaseURL validates an http/https endpoint and strips trailing
// slashes so endpoint paths can be appended directly.
func normalizeBaseURL(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("must not be empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("must be an http or https URL, got scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return "", errors.New("must include a host")
	}
	return strings.TrimRight(raw, "/"), nil
}

// checkBuiltinConflict rejects a model whose exposed IDs collide with a
// built-in kirocc model: the claude- alias would silently shadow it, since
// custom providers are matched before Kiro resolution.
func checkBuiltinConflict(path string, m Model) error {
	// Both spellings are routable: claude-glm-5.3 and claude-glm-5.3[1m] hit
	// the same model, so either colliding with a built-in ID is a conflict.
	for _, exposed := range []string{"claude-" + m.Base, "claude-" + m.Base + "[1m]"} {
		if models.HasAnthropicID(exposed) {
			return fmt.Errorf("%s: model %q would expose ID %q, which collides with a built-in kirocc model; rename the model", path, m.Name, exposed)
		}
	}
	return nil
}
