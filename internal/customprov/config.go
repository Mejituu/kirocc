// Package customprov routes requests for user-declared models to
// Anthropic-compatible custom providers (e.g. Alibaba Bailian's Anthropic
// endpoint), forwarding the Messages API request verbatim instead of
// converting it to the Kiro protocol.
package customprov

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/d-kuro/kirocc/internal/models"
)

// configFileName is the file looked up next to the executable by default.
const configFileName = "mejituu.json5"

// fileConfig is the top level of the config file.
type fileConfig struct {
	Kiro *kiroSection `json:"kiro,omitempty"`
	// Providers is the provider map; the custom unmarshaling preserves the
	// file's declaration order.
	Providers orderedProviders `json:"providers,omitempty"`
}

// kiroSection holds the rules applied to sessions that last used Kiro.
type kiroSection struct {
	Mappings []mappingRule `json:"mappings,omitempty"`
}

// orderedProviders is the providers object keyed by provider name. It keeps
// the declaration order, which /v1/models and the load log follow, so a
// plain map decode would not do.
type orderedProviders struct {
	names   []string
	entries map[string]providerConfig
}

// UnmarshalJSONFrom reads the object member by member: jsontext rejects
// duplicate names and the shared decoder carries the unmarshal options
// (unknown-member rejection) into every nested decode.
func (p *orderedProviders) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	tok, err := dec.ReadToken()
	if err != nil {
		return err
	}
	if tok.Kind() != '{' {
		return fmt.Errorf("providers must be an object, got %v", tok.Kind())
	}
	for dec.PeekKind() == '"' {
		name, err := dec.ReadToken()
		if err != nil {
			return err
		}
		// Extract the key before the next read: jsontext voids a token once
		// the decoder advances past it.
		key := name.String()
		var pc providerConfig
		if err := json.UnmarshalDecode(dec, &pc); err != nil {
			return err
		}
		if p.entries == nil {
			p.entries = make(map[string]providerConfig)
		}
		p.names = append(p.names, key)
		p.entries[key] = pc
	}
	if _, err := dec.ReadToken(); err != nil {
		return err
	}
	return nil
}

// providerConfig is one entry of the providers object.
type providerConfig struct {
	BaseURL     string       `json:"base_url"`
	AuthToken   string       `json:"auth_token"`
	CountTokens *bool        `json:"count_tokens,omitempty"` // nil means true
	Models      []modelEntry `json:"models"`
	Protocol    string       `json:"protocol,omitempty"` // empty means anthropic

	// Mappings are the session-following rules for this provider's
	// sessions; a separate group keeps the field alignment stable.
	Mappings []mappingRule `json:"mappings,omitempty"`
}

// modelEntry is one entry of a provider's models array: the string
// shorthand ("kimi-k3[1m]") or the explicit object form. Exactly one of the
// two is set.
type modelEntry struct {
	String string
	Spec   *modelSpec
}

// UnmarshalJSONFrom dispatches on the entry's kind: a string is the
// shorthand, an object the explicit form; any other kind falls through to
// the struct decode, which reports it with an offset.
func (m *modelEntry) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	if dec.PeekKind() == '"' {
		return json.UnmarshalDecode(dec, &m.String)
	}
	m.Spec = &modelSpec{}
	return json.UnmarshalDecode(dec, m.Spec)
}

// modelSpec is the object form of a models entry.
type modelSpec struct {
	ID        string `json:"id"`
	Alias     string `json:"alias,omitempty"`
	Context1M bool   `json:"context_1m,omitempty"`
	Label     string `json:"label,omitempty"`

	// AlwaysThinking is only meaningful for openai-protocol providers; the
	// buildRegistry loop rejects it on anthropic providers.
	AlwaysThinking bool `json:"always_thinking,omitempty"`

	// WebSearch turns on the provider's native web search for this model;
	// allowed on both protocols.
	WebSearch bool `json:"web_search,omitempty"`
}

// mappingRule is one entry of a mappings group.
type mappingRule struct {
	Model     string        `json:"model"`
	Thinking  *bool         `json:"thinking,omitempty"`   // nil = either
	Stream    *bool         `json:"stream,omitempty"`     // nil = either
	WebSearch *bool         `json:"web_search,omitempty"` // nil = either
	To        mappingTarget `json:"to"`
}

// mappingTarget is the rule's destination. An empty Provider takes the
// group's default: kiro for kiro.mappings, the group's provider otherwise.
type mappingTarget struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model"`
}

// Load reads the custom provider config. An empty path selects the default
// location, mejituu.json5 next to the real executable; a default-location
// file that does not exist disables custom providers, while an explicitly
// given path must exist. The returned registry is nil when custom providers
// are disabled.
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
	doc, err := convertJSON5(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: invalid JSON5: %w", path, err)
	}
	var fc fileConfig
	// Unknown members are rejected at every level; duplicate names jsontext
	// rejects by default. Both errors carry offsets that locateJSON5Error
	// maps back to the original file.
	if err := json.Unmarshal(doc.json, &fc, json.RejectUnknownMembers(true)); err != nil {
		// Decoding errors carry offsets into the converted text; the offset
		// table locates them in the original file.
		return nil, fmt.Errorf("%s: invalid JSON5: %s", path, locateJSON5Error(doc, err))
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
	if n := registry.mappingCount(); n > 0 {
		slog.Info("model mappings loaded", "rules", n)
	}
	return registry, nil
}

// buildRegistry validates the parsed file and turns it into a lookup table.
// Errors are prefixed with the file path and locate the offending entry in
// the new structure's terms (providers.<name>.models[i].id, …).
func buildRegistry(path string, fc fileConfig) (*Registry, error) {
	registry := &Registry{byDisplay: make(map[string]modelMatch)}
	// Display names must be unique across providers, so a match is
	// unambiguous.
	seenDisplays := make(map[string]string)
	for _, name := range fc.Providers.names {
		pc := fc.Providers.entries[name]
		if name == "" {
			return nil, fmt.Errorf("%s: providers: provider name must not be empty", path)
		}
		if name == KiroProviderName {
			return nil, fmt.Errorf("%s: providers.kiro: %q is reserved for the Kiro path; rename the provider", path, name)
		}

		baseURL, err := normalizeBaseURL(pc.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("%s: providers.%s.base_url: %w", path, name, err)
		}
		if pc.AuthToken == "" {
			return nil, fmt.Errorf("%s: providers.%s.auth_token must not be empty", path, name)
		}
		if len(pc.Models) == 0 {
			return nil, fmt.Errorf("%s: providers.%s.models must not be empty", path, name)
		}

		protocol := Protocol(pc.Protocol)
		switch protocol {
		case "":
			protocol = ProtocolAnthropic
		case ProtocolAnthropic, ProtocolOpenAI:
		default:
			return nil, fmt.Errorf("%s: providers.%s.protocol: only %q and %q are supported, got %q", path, name, ProtocolAnthropic, ProtocolOpenAI, pc.Protocol)
		}
		countTokens := pc.CountTokens == nil || *pc.CountTokens
		if protocol == ProtocolOpenAI {
			if pc.CountTokens != nil && *pc.CountTokens {
				return nil, fmt.Errorf("%s: providers.%s.count_tokens: OpenAI protocol providers have no count_tokens endpoint; kirocc estimates locally, so count_tokens cannot be true", path, name)
			}
			countTokens = false
		}

		p := &Provider{
			Name:        name,
			BaseURL:     baseURL,
			authToken:   pc.AuthToken,
			CountTokens: countTokens,
			Protocol:    protocol,
		}
		seenIDs := make(map[string]bool, len(pc.Models))
		for i, entry := range pc.Models {
			m, err := buildModel(path, name, i, entry)
			if err != nil {
				return nil, err
			}
			if m.AlwaysThinking && protocol != ProtocolOpenAI {
				return nil, fmt.Errorf("%s: providers.%s.models[%d].always_thinking: only OpenAI-protocol models support it (some reject enable_thinking:false)", path, name, i)
			}
			if seenIDs[m.ID] {
				return nil, fmt.Errorf("%s: providers.%s.models[%d].id: %q is declared twice in this provider", path, name, i, m.ID)
			}
			seenIDs[m.ID] = true
			if other, ok := seenDisplays[m.Display]; ok {
				return nil, fmt.Errorf("%s: providers.%s.models[%d]: display name %q is already exposed by provider %q; use an alias", path, name, i, m.Display, other)
			}
			seenDisplays[m.Display] = name
			if err := checkBuiltinConflict(path, fmt.Sprintf("providers.%s.models[%d]", name, i), m.Display); err != nil {
				return nil, err
			}
			p.Models = append(p.Models, m)
			registry.byDisplay[m.Display] = modelMatch{provider: p, model: m}
		}
		registry.providers = append(registry.providers, p)
	}

	// Mappings compile only after every provider is registered: a rule may
	// target a provider declared later in the file, or a custom provider
	// from kiro.mappings. Provider groups compile in declaration order,
	// then kiro.
	var mappingGroups []struct {
		key       string
		loc       string
		rules     []mappingRule
		defaultTo string
	}
	for _, name := range fc.Providers.names {
		mappingGroups = append(mappingGroups, struct {
			key       string
			loc       string
			rules     []mappingRule
			defaultTo string
		}{key: name, loc: "providers." + name, rules: fc.Providers.entries[name].Mappings, defaultTo: name})
	}
	if fc.Kiro != nil {
		mappingGroups = append(mappingGroups, struct {
			key       string
			loc       string
			rules     []mappingRule
			defaultTo string
		}{key: KiroProviderName, loc: "kiro", rules: fc.Kiro.Mappings, defaultTo: KiroProviderName})
	}
	for _, group := range mappingGroups {
		mappings, err := compileMappings(path, group.loc, group.rules, group.defaultTo, registry)
		if err != nil {
			return nil, err
		}
		if len(mappings) > 0 {
			if registry.mappings == nil {
				registry.mappings = make(map[string][]Mapping)
			}
			registry.mappings[group.key] = mappings
		}
	}
	return registry, nil
}

// buildModel compiles one models entry. The string shorthand maps a
// trailing [1m] marker to Context1M; the object form forbids the marker on
// id and alias and takes context_1m instead.
func buildModel(path, providerName string, i int, entry modelEntry) (Model, error) {
	if entry.Spec != nil {
		s := entry.Spec
		if s.ID == "" {
			return Model{}, fmt.Errorf("%s: providers.%s.models[%d].id must not be empty", path, providerName, i)
		}
		if trimOneMSuffix(s.ID) != s.ID {
			return Model{}, fmt.Errorf("%s: providers.%s.models[%d].id must not carry the [1m] marker; use context_1m", path, providerName, i)
		}
		if trimOneMSuffix(s.Alias) != s.Alias {
			return Model{}, fmt.Errorf("%s: providers.%s.models[%d].alias must not carry the [1m] marker; use context_1m", path, providerName, i)
		}
		display := s.Alias
		if display == "" {
			display = s.ID
		}
		return Model{ID: s.ID, Display: display, Context1M: s.Context1M, Label: s.Label, AlwaysThinking: s.AlwaysThinking, WebSearch: s.WebSearch}, nil
	}
	if entry.String == "" {
		return Model{}, fmt.Errorf("%s: providers.%s.models[%d].id must not be empty", path, providerName, i)
	}
	id := trimOneMSuffix(entry.String)
	return Model{ID: id, Display: id, Context1M: id != entry.String}, nil
}

// compileMappings validates and compiles one mappings group. groupLoc is
// the error-path prefix ("providers.bailian" or "kiro"); defaultProvider is
// the target when a rule leaves to.provider empty. Errors locate the
// offending rule.
func compileMappings(path, groupLoc string, rules []mappingRule, defaultProvider string, registry *Registry) ([]Mapping, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	compiled := make([]Mapping, 0, len(rules))
	for i, rule := range rules {
		// A web_search rule matches any model, so it may leave model empty;
		// every other rule still names one.
		webSearch := rule.WebSearch != nil && *rule.WebSearch
		if rule.Model == "" && !webSearch {
			return nil, fmt.Errorf("%s: %s.mappings[%d].model must not be empty", path, groupLoc, i)
		}
		if rule.To.Model == "" {
			return nil, fmt.Errorf("%s: %s.mappings[%d].to.model must not be empty", path, groupLoc, i)
		}
		target := rule.To.Provider
		if target == "" {
			target = defaultProvider
		}
		if target != KiroProviderName {
			p := registry.providerByName(target)
			if p == nil {
				return nil, fmt.Errorf("%s: %s.mappings[%d].to.provider: unknown provider %q", path, groupLoc, i, target)
			}
			// A kiro target is checked per request, not at startup: the
			// catalog only fills in after startup. A custom target must name
			// the model by id, not by alias.
			if !p.hasModelID(rule.To.Model) {
				return nil, fmt.Errorf("%s: %s.mappings[%d].to.model: %q is not a model id of provider %q; write the model's id, not its alias", path, groupLoc, i, rule.To.Model, target)
			}
			// A web_search rule reroutes requests to the target's native web
			// search, so the target model must itself enable it. hasModelID
			// has already vouched for the id, so the nil arm is a guard only.
			if webSearch {
				if tm := p.modelByID(rule.To.Model); tm == nil || !tm.WebSearch {
					return nil, fmt.Errorf("%s: %s.mappings[%d].to.model: %q does not enable web_search; a web_search rule must target a model with web_search true", path, groupLoc, i, rule.To.Model)
				}
			}
		}
		m := Mapping{
			Model:     trimOneMSuffix(rule.Model),
			Thinking:  rule.Thinking,
			Stream:    rule.Stream,
			WebSearch: rule.WebSearch,
			Provider:  target,
			ModelTo:   rule.To.Model,
		}
		for _, prev := range compiled {
			if prev.sameCondition(m) {
				return nil, fmt.Errorf("%s: %s.mappings[%d] duplicates an earlier rule with the same model, thinking, stream and web_search, so it can never match", path, groupLoc, i)
			}
		}
		compiled = append(compiled, m)
	}
	return compiled, nil
}

// modelByID returns the model declaration with exactly this id — the
// web_search mapping-target check reads the declaration's flag. Aliases do
// not match, same as hasModelID.
func (p *Provider) modelByID(id string) *Model {
	for i := range p.Models {
		if p.Models[i].ID == id {
			return &p.Models[i]
		}
	}
	return nil
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
func checkBuiltinConflict(path, entryLoc, display string) error {
	// Both spellings are routable: claude-glm-5.3 and claude-glm-5.3[1m] hit
	// the same model, so either colliding with a built-in ID is a conflict.
	for _, exposed := range []string{"claude-" + display, "claude-" + display + "[1m]"} {
		if models.HasAnthropicID(exposed) {
			return fmt.Errorf("%s: %s would expose ID %q, which collides with a built-in kirocc model; rename the model", path, entryLoc, exposed)
		}
	}
	return nil
}
