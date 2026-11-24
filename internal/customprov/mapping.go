package customprov

import (
	"path"
	"strings"

	"github.com/d-kuro/kirocc/internal/anthropic"
)

// Mapping is one compiled session-following rule: a request from a session
// whose provider matches the group, and whose model, thinking, stream and
// web_search match the condition, is rerouted to Provider's ModelTo.
type Mapping struct {
	// Model is the rule's model pattern in canonical form (glob patterns
	// dash-escaped), kept for duplicate detection; matching uses the
	// compiled model below. Empty matches any model, for rules narrowed by
	// web_search alone.
	Model string
	// model is Model compiled once at config load; the zero value never
	// matches, which guards an uninitialized Mapping.
	model modelPattern
	// Thinking and Stream narrow the condition; nil matches either value.
	Thinking *bool
	Stream   *bool
	// WebSearch narrows the condition to requests that carry (or do not
	// carry) the web_search tool; nil matches either.
	WebSearch *bool
	// Provider is the target: "kiro" or a declared provider name.
	Provider string
	// ModelTo is the target model ID, verbatim — it may carry "/" or upper
	// case letters, and no kirocc rewriting applies.
	ModelTo string
}

// matches reports whether a peeked request satisfies the rule's condition.
func (m Mapping) matches(info RequestInfo) bool {
	if m.Model != "" && !m.model.matches(trimOneMSuffix(info.Model)) {
		return false
	}
	if m.Thinking != nil && *m.Thinking != thinkingRequested(info) {
		return false
	}
	if m.Stream != nil && *m.Stream != info.Stream {
		return false
	}
	if m.WebSearch != nil && *m.WebSearch != info.HasWebSearch() {
		return false
	}
	return true
}

// wildcardChars are the glob metacharacters a model pattern understands. A
// pattern containing none of them is a plain literal compared with ==.
const wildcardChars = "*?["

// hasWildcard reports whether a model pattern uses glob metacharacters.
func hasWildcard(pattern string) bool {
	return strings.ContainsAny(pattern, wildcardChars)
}

// matcherKind selects how a modelPattern matches. The zero value never
// matches.
type matcherKind uint8

const (
	kindNever    matcherKind = iota // uninitialized or malformed
	kindLiteral                     // plain == (pattern without metacharacters)
	kindGlob                        // compiled starless glob: prefix, byte sets, suffix, exact length
	kindStar                        // compiled glob with stars: chunks stitched by the star walk
	kindFallback                    // non-ASCII pattern: path.Match per call (rune-level ? and classes)
)

// modelPattern is a model condition compiled once at config load. Starless
// patterns compile to one chunk with an exact byte length, so matching is a
// length check, a prefix compare, one bit test per character class and a
// suffix compare. Starred patterns keep the same chunks and walk them the
// way path.Match does. Patterns holding non-ASCII bytes fall back to
// path.Match: its ? and classes work on runes, and byte-level sets could
// disagree with rune-level consumption there.
type modelPattern struct {
	kind matcherKind
	lit  string // kindLiteral: compare with ==
	pat  string // other kinds: the dash-escaped pattern, for path.Match fallback
	// kindStar bookkeeping: the pattern starts/ends with a star.
	leading  bool
	trailing bool
	// chunks are the star-separated runs: exactly one for kindGlob.
	chunks []chunk
}

// matches reports whether a request model satisfies the pattern. A compiled
// pattern defers to path.Match when a byte set meets a non-ASCII byte (its
// byte-level answer could disagree with rune-level semantics); that path is
// cold — model IDs are ASCII.
func (p modelPattern) matches(model string) bool {
	switch p.kind {
	case kindLiteral:
		return p.lit == model
	case kindGlob:
		c := &p.chunks[0]
		// An ASCII model matching must have exactly the chunk's byte
		// length; a non-ASCII one may not ('?' consumes a whole rune, two
		// or three bytes), so only a mismatched ASCII model is rejected
		// here and the rest defers through matchAt.
		if len(model) != c.length && isASCII(model) {
			return false
		}
		rest, ok, unsure := c.matchAt(model)
		if !unsure {
			return ok && len(rest) == 0
		}
	case kindStar:
		ok, unsure := p.matchStarred(model)
		if !unsure {
			return ok
		}
	case kindFallback:
		ok, _ := path.Match(p.pat, model)
		return ok
	default:
		return false
	}
	// Unsure: a byte set met a non-ASCII byte. path.Match decides.
	ok, _ := path.Match(p.pat, model)
	return ok
}

// text returns the pattern's canonical (dash-escaped) text — the string
// duplicate detection compares, where equivalent spellings compile equal.
func (p modelPattern) text() string {
	if p.kind == kindLiteral {
		return p.lit
	}
	return p.pat
}

// compileModelPattern compiles a rule's raw model pattern once, at config load. A pattern without
// glob metacharacters is a plain literal. A glob pattern is dash-escaped
// (escapeClassDashes) and compiled to the byte-level program; one holding
// non-ASCII bytes keeps path.Match but is still validated, so a malformed
// pattern fails at load time either way (kindNever).
func compileModelPattern(pattern string) modelPattern {
	if !hasWildcard(pattern) {
		return modelPattern{kind: kindLiteral, lit: pattern}
	}
	escaped := escapeClassDashes(pattern)
	if !isASCII(escaped) {
		if _, err := path.Match(escaped, ""); err != nil {
			return modelPattern{kind: kindNever}
		}
		return modelPattern{kind: kindFallback, pat: escaped}
	}
	p, err := parseGlob(escaped)
	if err != nil {
		return modelPattern{kind: kindNever}
	}
	return p
}

// isASCII reports whether s is all bytes below 0x80.
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// chunk is one star-free run of a glob pattern: a literal prefix and suffix
// hoisted out of segs, which hold the byte sets and the literal runs between
// them. length is the exact byte count the chunk consumes.
type chunk struct {
	prefix string
	segs   []seg
	suffix string
	length int
}

// seg matches one position of a chunk: either a literal run, or a 256-bit
// byte set of the characters a '?' or [...] accepts there.
type seg struct {
	lit string   // non-empty: match these literal bytes
	set [32]byte // lit == "": the next byte must have its bit set
}

func (s *seg) testByte(c byte) bool { return s.set[c>>3]&(1<<(c&7)) != 0 }

// matchAt tries the chunk at the start of s and returns the unconsumed tail.
// unsure reports a byte set meeting a non-ASCII byte: byte-level sets can
// disagree with path.Match's rune-level '?' and classes there, so the caller
// defers to path.Match for the final answer.
func (c *chunk) matchAt(s string) (rest string, ok, unsure bool) {
	if len(s) < c.length {
		return "", false, false
	}
	if c.prefix != "" && !strings.HasPrefix(s, c.prefix) {
		return "", false, false
	}
	s = s[len(c.prefix):]
	for i := range c.segs {
		if lit := c.segs[i].lit; lit != "" {
			if !strings.HasPrefix(s, lit) {
				return "", false, false
			}
			s = s[len(lit):]
			continue
		}
		b := s[0]
		if b >= 0x80 {
			return "", false, true
		}
		if !c.segs[i].testByte(b) {
			return "", false, false
		}
		s = s[1:]
	}
	if c.suffix != "" && !strings.HasPrefix(s, c.suffix) {
		return "", false, false
	}
	return s[len(c.suffix):], true, false
}

// matchStarred mirrors path.Match's chunk walk: each chunk is tried at the
// current position, and a chunk preceded by '*' is retried at every later
// offset that does not cross '/'. A trailing star consumes the rest unless
// it holds a '/'.
func (p modelPattern) matchStarred(name string) (matched, unsure bool) {
	pos := 0
	last := len(p.chunks) - 1
Pattern:
	for ci := range p.chunks {
		star := ci > 0 || p.leading
		rest, ok, unsure := p.chunks[ci].matchAt(name[pos:])
		if unsure {
			return false, true
		}
		// Advance when the chunk consumed the model, or more pattern
		// remains to take up the slack.
		if ok && (len(rest) == 0 || ci < last || p.trailing) {
			pos = len(name) - len(rest)
			continue
		}
		if star {
			for i := pos; i < len(name) && name[i] != '/'; i++ {
				rest, ok, unsure := p.chunks[ci].matchAt(name[i+1:])
				if unsure {
					return false, true
				}
				if ok {
					// The last chunk with nothing after must consume to
					// the end; otherwise keep scanning.
					if ci == last && !p.trailing && len(rest) > 0 {
						continue
					}
					pos = len(name) - len(rest)
					continue Pattern
				}
			}
		}
		return false, false
	}
	if p.trailing {
		return !strings.Contains(name[pos:], "/"), false
	}
	return pos == len(name), false
}

// parseGlob compiles a dash-escaped ASCII glob pattern. The single pass
// splits star-separated chunks the way path.Match's scanChunk does — '\'
// skips the next byte, '['…']' is a class, and only a '*' outside a class
// ends a chunk — so the accepted grammar and the malformed patterns match
// path.Match exactly.
func parseGlob(p string) (modelPattern, error) {
	mp := modelPattern{pat: p}
	var (
		segs    []seg
		cur     []byte // pending literal run
		started bool   // any token since the last chunk boundary
		starred bool
	)
	flushChunk := func() {
		if len(cur) > 0 {
			segs = append(segs, seg{lit: string(cur)})
			cur = cur[:0]
		}
		var c chunk
		if len(segs) > 0 && segs[0].lit != "" {
			c.prefix = segs[0].lit
			segs = segs[1:]
		}
		if len(segs) > 0 && segs[len(segs)-1].lit != "" {
			c.suffix = segs[len(segs)-1].lit
			segs = segs[:len(segs)-1]
		}
		c.segs = segs
		c.length = len(c.prefix) + len(c.suffix)
		for _, s := range segs {
			if s.lit != "" {
				c.length += len(s.lit)
			} else {
				c.length++
			}
		}
		mp.chunks = append(mp.chunks, c)
		segs = nil
		started = false
	}
	i := 0
	for i < len(p) {
		switch p[i] {
		case '\\':
			if i+1 >= len(p) {
				return modelPattern{}, path.ErrBadPattern
			}
			cur = append(cur, p[i+1])
			i += 2
			started = true
		case '[':
			set, negated, next, err := parseClass(p, i)
			if err != nil {
				return modelPattern{}, err
			}
			if negated {
				set = complement(set)
			}
			if len(cur) > 0 {
				segs = append(segs, seg{lit: string(cur)})
				cur = cur[:0]
			}
			segs = append(segs, seg{set: set})
			i = next
			started = true
		case '?':
			var set [32]byte
			for b := 0; b < 256; b++ {
				if b != '/' {
					set[b>>3] |= 1 << (byte(b) & 7)
				}
			}
			if len(cur) > 0 {
				segs = append(segs, seg{lit: string(cur)})
				cur = cur[:0]
			}
			segs = append(segs, seg{set: set})
			i++
			started = true
		case '*':
			starred = true
			if !started {
				if len(mp.chunks) == 0 {
					mp.leading = true
				}
				i++
				continue // collapse consecutive stars
			}
			flushChunk()
			i++
		default:
			cur = append(cur, p[i])
			i++
			started = true
		}
	}
	if started {
		flushChunk()
	} else if starred {
		mp.trailing = true
	}
	if !starred {
		mp.kind = kindGlob
	} else {
		mp.kind = kindStar
	}
	return mp, nil
}

// parseClass parses a [...] character class starting at p[i] == '[' and
// returns the byte set, whether it was negated, and the index just past the
// closing ']'. It mirrors path.Match's parser: '^' negates, '\' escapes, a
// ']' before any item or a trailing '\' is malformed, and a-z is a range.
// The pattern is known ASCII here, so byte-wise items equal rune-wise ones.
func parseClass(p string, i int) (set [32]byte, negated bool, next int, err error) {
	i++ // past '['
	if i < len(p) && p[i] == '^' {
		negated = true
		i++
	}
	nrange := 0
	for {
		if i < len(p) && p[i] == ']' && nrange > 0 {
			return set, negated, i + 1, nil
		}
		lo, ni, err := getEsc(p, i)
		if err != nil {
			return set, false, 0, err
		}
		i = ni
		hi := lo
		if i < len(p) && p[i] == '-' {
			hi, i, err = getEsc(p, i+1)
			if err != nil {
				return set, false, 0, err
			}
		}
		if lo <= hi {
			for b := int(lo); b <= int(hi); b++ {
				set[b>>3] |= 1 << (byte(b) & 7)
			}
		}
		nrange++
	}
}

// getEsc reads one class item, mirroring path.Match: '\' escapes the next
// byte, and a leading '-' or ']' is malformed.
func getEsc(p string, i int) (byte, int, error) {
	if i >= len(p) || p[i] == '-' || p[i] == ']' {
		return 0, 0, path.ErrBadPattern
	}
	if p[i] == '\\' {
		i++
		if i >= len(p) {
			return 0, 0, path.ErrBadPattern
		}
	}
	return p[i], i + 1, nil
}

// complement flips a byte set, for negated classes.
func complement(set [32]byte) [32]byte {
	for i := range set {
		set[i] = ^set[i]
	}
	return set
}

// escapeClassDashes makes a '-' that sits at a character-class boundary a
// literal member, mirroring POSIX fnmatch: Go's path.Match parses every
// in-class dash as a range operator and rejects [.-] as malformed. A dash
// directly after '[' or '[^', or directly before ']', becomes '\-'; range
// dashes ([a-z]) and already-escaped dashes are untouched. Dashes outside a
// class are ordinary literals in glob and pass through.
func escapeClassDashes(p string) string {
	out := make([]byte, 0, len(p)+2)
	inClass := false
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == '\\' && i+1 < len(p) {
			// Copy an escape pair verbatim; the dash it may shield is already escaped.
			out = append(out, c, p[i+1])
			i++
			continue
		}
		switch {
		case c == '[':
			inClass = true
		case inClass && c == ']':
			inClass = false
		case inClass && c == '-':
			trailing := i+1 >= len(p) || p[i+1] == ']'
			leading := i >= 1 && (p[i-1] == '[' || (p[i-1] == '^' && i >= 2 && p[i-2] == '['))
			if trailing || leading {
				out = append(out, '\\')
			}
		}
		out = append(out, c)
	}
	return string(out)
}

// sameCondition reports whether two rules in one group match the same
// requests, making the later one unreachable.
func (m Mapping) sameCondition(other Mapping) bool {
	return m.Model == other.Model &&
		boolPtrEqual(m.Thinking, other.Thinking) &&
		boolPtrEqual(m.Stream, other.Stream) &&
		boolPtrEqual(m.WebSearch, other.WebSearch)
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
