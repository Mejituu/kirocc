package customprov

import (
	"github.com/d-kuro/kirocc/internal/anthropic"
)

// Mapping is one compiled session-following rule: a request from a session
// whose provider matches the group, and whose model, thinking and stream
// match the condition, is rerouted to Provider's ModelTo.
type Mapping struct {
	// Model is the matched request model with the [1m] marker stripped; the
	// comparison itself ignores the marker either way.
	Model string
	// Thinking and Stream narrow the condition; nil matches either value.
	Thinking *bool
	Stream   *bool
	// Provider is the target: "kiro" or a declared provider name.
	Provider string
	// ModelTo is the target model ID, verbatim — it may carry "/" or upper
	// case letters, and no kirocc rewriting applies.
	ModelTo string
}

// matches reports whether a peeked request satisfies the rule's condition.
func (m Mapping) matches(info RequestInfo) bool {
	if trimOneMSuffix(info.Model) != m.Model {
		return false
	}
	if m.Thinking != nil && *m.Thinking != thinkingRequested(info) {
		return false
	}
	if m.Stream != nil && *m.Stream != info.Stream {
		return false
	}
	return true
}

// sameCondition reports whether two rules in one group match the same
// requests, making the later one unreachable.
func (m Mapping) sameCondition(other Mapping) bool {
	return m.Model == other.Model &&
		boolPtrEqual(m.Thinking, other.Thinking) &&
		boolPtrEqual(m.Stream, other.Stream)
}

func boolPtrEqual(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// thinkingRequested reports whether the peeked request enables reasoning:
// only thinking.type counts — the [1m] marker is a context-window
// advertisement, not a thinking opt-in.
func thinkingRequested(info RequestInfo) bool {
	return info.Thinking != nil &&
		(info.Thinking.Type == anthropic.ThinkingTypeEnabled || info.Thinking.Type == anthropic.ThinkingTypeAdaptive)
}

// matchMapping returns the first rule of the session provider's group that
// matches the request, if any.
func (r *Registry) matchMapping(sessionProvider string, info RequestInfo) (Mapping, bool) {
	if r == nil {
		return Mapping{}, false
	}
	for _, m := range r.mappings[sessionProvider] {
		if m.matches(info) {
			return m, true
		}
	}
	return Mapping{}, false
}

// hasModelID reports whether the provider serves a model with exactly this
// id, for mapping-target validation.
func (p *Provider) hasModelID(id string) bool {
	for _, m := range p.Models {
		if m.ID == id {
			return true
		}
	}
	return false
}
