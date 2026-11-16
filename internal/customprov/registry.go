package customprov

import (
	"strings"

	"github.com/d-kuro/kirocc/internal/models"
)

// Model is one model declared for a custom provider.
type Model struct {
	// ID is the model ID sent to the provider, verbatim — it may carry "/"
	// or upper case letters, and no kirocc rewriting applies.
	ID string
	// Display is the name requests match on and the gateway advertises
	// (before the claude- prefix): the alias when one is set, else the ID.
	Display string
	// Context1M reports whether the model advertises a 1M context window.
	Context1M bool
	// Label is the model picker's display name; empty derives it from
	// Display.
	Label string

	// AlwaysThinking marks an openai-protocol model that rejects
	// enable_thinking:false; its requests omit the field entirely so the
	// model keeps thinking.
	AlwaysThinking bool

	// WebSearch marks a model whose provider runs the web_search server-side
	// tool natively — the anthropic protocol forwards the declaration with a
	// fixed billing-header block, the openai protocol sets enable_search.
	WebSearch bool
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

	// Protocol is the wire protocol the provider speaks; a separate group
	// keeps the field alignment above stable.
	Protocol Protocol

	// WebSearchFallback names the model search requests fall back to when the
	// requested model of this provider does not search; nil = no fallback.
	WebSearchFallback *WebSearchFallbackTarget

	// authToken is unexported so it cannot leak into logs or JSON dumps.
	authToken string
}

// Hit is a successful model lookup.
type Hit struct {
	Provider  *Provider
	BaseModel string // model ID to send to the provider
	// Config1M reports whether the model advertises a 1M context window,
	// regardless of how the request spelled the ID.
	Config1M bool

	// AlwaysThinking reports an openai-protocol model that rejects
	// enable_thinking:false, so its requests omit the field.
	AlwaysThinking bool

	// WebSearch reports a model declared with web_search, so its requests
	// activate the provider's native search.
	WebSearch bool
}

// modelMatch is the registry entry a display name resolves to: the provider
// and the model declaration as configured.
type modelMatch struct {
	provider *Provider
	model    Model
}

// Registry indexes the configured models for request matching.
type Registry struct {
	// providers keeps the file's declaration order, which /v1/models and
	// the load log follow.
	providers []*Provider
	// byDisplay maps a model's display name to its match.
	byDisplay map[string]modelMatch
	// mappings holds the session-following rules, keyed by the provider the
	// session last used.
	mappings map[string][]Mapping
	// KiroWebSearchFallback routes search requests whose target is a Kiro
	// model to a custom model; nil = no fallback.
	KiroWebSearchFallback *WebSearchFallbackTarget
}

// Lookup maps a requested model ID to its provider. A model matches when the
// ID is "claude-" + display name, optionally with a trailing [1m] context
// marker: both claude-glm-5.3 and claude-glm-5.3[1m] hit glm-5.3. Anything
// else — including unknown claude- IDs — misses and stays on the Kiro path.
func (r *Registry) Lookup(model string) (Hit, bool) {
	if r == nil {
		return Hit{}, false
	}
	rest, ok := strings.CutPrefix(trimOneMSuffix(model), "claude-")
	if !ok {
		return Hit{}, false
	}
	m, ok := r.byDisplay[rest]
	if !ok {
		return Hit{}, false
	}
	return Hit{Provider: m.provider, BaseModel: m.model.ID, Config1M: m.model.Context1M, AlwaysThinking: m.model.AlwaysThinking, WebSearch: m.model.WebSearch}, true
}

// lookupModel resolves a mapping target: the model with exactly this id in
// the named provider. Aliases do not match — a mapping target names the id
// that goes upstream.
func (r *Registry) lookupModel(providerName, id string) (Hit, bool) {
	if r == nil {
		return Hit{}, false
	}
	p := r.providerByName(providerName)
	if p == nil {
		return Hit{}, false
	}
	for _, m := range p.Models {
		if m.ID == id {
			return Hit{Provider: p, BaseModel: m.ID, Config1M: m.Context1M, AlwaysThinking: m.AlwaysThinking, WebSearch: m.WebSearch}, true
		}
	}
	return Hit{}, false
}

// providerByName finds a declared provider by name.
func (r *Registry) providerByName(name string) *Provider {
	for _, p := range r.providers {
		if p.Name == name {
			return p
		}
	}
	return nil
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
	ID          string // exposed ID: "claude-" + display name (+ "[1m]")
	DisplayName string // label, or the display name (+ "[1m]")
	Provider    string
}

// ListModels returns one entry per configured model, in declaration order.
// A model with a 1M context window is advertised with the marker, so Claude
// Code's client-side 1M context detection picks it up.
func (r *Registry) ListModels() []ModelInfo {
	if r == nil {
		return nil
	}
	var result []ModelInfo
	for _, p := range r.providers {
		for _, m := range p.Models {
			suffix := ""
			if m.Context1M {
				suffix = models.ThinkingSuffix
			}
			displayName := m.Label
			if displayName == "" {
				displayName = m.Display + suffix
			}
			result = append(result, ModelInfo{
				ID:          "claude-" + m.Display + suffix,
				DisplayName: displayName,
				Provider:    p.Name,
			})
		}
	}
	return result
}
