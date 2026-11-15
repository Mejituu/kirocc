package customprov

import (
	"fmt"
	"strings"

	"github.com/d-kuro/kirocc/internal/models"
)

// Model is one model declared for a custom provider.
type Model struct {
	// Name is the configured spelling, e.g. "glm-5.3" or "glm-5.3[1m]".
	Name string
	// Base is Name without the trailing [1m] marker: the model ID sent to
	// the provider, which takes neither the claude- prefix nor the marker.
	Base string
}

// newModel splits a configured model name into its base ID, rejecting empty
// names.
func newModel(name string) (Model, error) {
	if name == "" {
		return Model{}, fmt.Errorf("model name must not be empty")
	}
	return Model{Name: name, Base: trimOneMSuffix(name)}, nil
}

// trimOneMSuffix strips a trailing [1m] context marker, case-insensitively —
// Claude Code emits both [1m] and [1M].
func trimOneMSuffix(model string) string {
	if len(model) >= len(models.ThinkingSuffix) &&
		strings.EqualFold(model[len(model)-len(models.ThinkingSuffix):], models.ThinkingSuffix) {
		return model[:len(model)-len(models.ThinkingSuffix)]
	}
	return model
}

// Provider is one configured Anthropic-compatible provider.
type Provider struct {
	Name        string
	BaseURL     string // http(s) endpoint without trailing slash
	Models      []Model
	CountTokens bool

	// authToken is unexported so it cannot leak into logs or JSON dumps.
	authToken string
}

// Hit is a successful model lookup.
type Hit struct {
	Provider  *Provider
	BaseModel string // model ID to send to the provider
	// Config1M reports whether the model's configured name carries the [1m]
	// marker: the context window advertised in /v1/models, regardless of how
	// the request spelled the ID.
	Config1M bool
}

// modelMatch is the registry entry a base name resolves to: the provider and
// the model declaration as configured.
type modelMatch struct {
	provider *Provider
	model    Model
}

// Registry indexes the configured models for request matching.
type Registry struct {
	providers []*Provider
	// byBase maps a bare model base name ("glm-5.3") to its match.
	byBase map[string]modelMatch
	// mappings holds the session-following rules, keyed by the provider the
	// session last used.
	mappings map[string][]Mapping
}

// Lookup maps a requested model ID to its provider. A model matches when the
// ID is "claude-" + base, optionally with a trailing [1m] context marker:
// both claude-glm-5.3 and claude-glm-5.3[1m] hit glm-5.3. Anything else —
// including unknown claude- IDs — misses and stays on the Kiro path.
func (r *Registry) Lookup(model string) (Hit, bool) {
	if r == nil {
		return Hit{}, false
	}
	rest, ok := strings.CutPrefix(trimOneMSuffix(model), "claude-")
	if !ok {
		return Hit{}, false
	}
	m, ok := r.byBase[rest]
	if !ok {
		return Hit{}, false
	}
	return Hit{Provider: m.provider, BaseModel: rest, Config1M: m.model.Name != m.model.Base}, true
}

// lookupBase resolves a configured model by its base name — the mapping
// target spelling — without the claude- prefix.
func (r *Registry) lookupBase(base string) (Hit, bool) {
	if r == nil {
		return Hit{}, false
	}
	m, ok := r.byBase[base]
	if !ok {
		return Hit{}, false
	}
	return Hit{Provider: m.provider, BaseModel: m.model.Base, Config1M: m.model.Name != m.model.Base}, true
}

// mappingCount totals the compiled session-following rules.
func (r *Registry) mappingCount() int {
	n := 0
	for _, rules := range r.mappings {
		n += len(rules)
	}
	return n
}

// ModelInfo is one custom-provider entry for /v1/models.
type ModelInfo struct {
	ID          string // exposed ID: "claude-" + configured name
	DisplayName string // configured model name
	Provider    string
}

// ListModels returns one entry per configured model, in configuration order.
// A model written with [1m] in the config is advertised with the marker, so
// Claude Code's client-side 1M context detection picks it up.
func (r *Registry) ListModels() []ModelInfo {
	if r == nil {
		return nil
	}
	var result []ModelInfo
	for _, p := range r.providers {
		for _, m := range p.Models {
			result = append(result, ModelInfo{
				ID:          "claude-" + m.Name,
				DisplayName: m.Name,
				Provider:    p.Name,
			})
		}
	}
	return result
}
