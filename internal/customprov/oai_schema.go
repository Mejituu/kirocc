package customprov

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
)

// oaiSchemaMaxDepth caps how deep oaiInlineRefs recurses while inlining.
// Realistic tool schemas stay far below it; anything deeper is treated as
// cyclic and keeps its $ref, which also bounds worst-case work on
// hostile inputs.
const oaiSchemaMaxDepth = 100

// oaiInlineRefs resolves local $ref references in a JSON schema so the
// forwarded tool parameters carry no $defs. Only "#/$defs/X" and
// "#/definitions/X" are inlined; other refs stay as-is. Circular references
// keep their $ref, recursion is depth-capped, sibling fields of an inlined
// $ref merge into the definition (conflicts resolved in the definition's
// favour), and the top-level $defs/definitions are removed afterwards. A
// schema without defs is returned unchanged. The rewritten schema is
// canonicalized (sorted object keys, no whitespace) so equal inputs always
// produce identical bytes; json.Marshal alone would not, since it emits map
// keys in Go's randomized iteration order.
func oaiInlineRefs(schema []byte) ([]byte, error) {
	var root any
	if err := json.Unmarshal(schema, &root); err != nil {
		return nil, err
	}
	obj, ok := root.(map[string]any)
	if !ok {
		// Not an object: no top-level $defs/definitions to strip.
		return schema, nil
	}

	defs := map[string]any{}
	for _, key := range []string{"$defs", "definitions"} {
		if d, ok := obj[key].(map[string]any); ok {
			for name, def := range d {
				defs[name] = def
			}
		}
	}
	if len(defs) == 0 {
		// Nothing to inline; return the input bytes untouched.
		return schema, nil
	}

	// Drop the def tables before resolving so a top-level $ref cannot merge
	// them back in as siblings.
	delete(obj, "$defs")
	delete(obj, "definitions")

	resolved := oaiSchemaResolve(obj, 0, map[string]bool{}, defs)
	out, err := json.Marshal(resolved)
	if err != nil {
		return nil, err
	}
	val := jsontext.Value(out)
	if err := val.Canonicalize(); err != nil {
		return nil, err
	}
	return []byte(val), nil
}

// oaiSchemaResolve inlines local $refs inside node and returns the
// rewritten value. active holds the def names currently being expanded, so
// a ref back to an active name is circular and keeps its $ref. depth caps
// the recursion; past the cap the node is returned as-is, which leaves any
// $ref in place.
func oaiSchemaResolve(node any, depth int, active map[string]bool, defs map[string]any) any {
	if depth > oaiSchemaMaxDepth {
		return node
	}
	switch v := node.(type) {
	case map[string]any:
		ref, hasRef := v["$ref"].(string)
		if !hasRef {
			for k, val := range v {
				v[k] = oaiSchemaResolve(val, depth+1, active, defs)
			}
			return v
		}
		name, ok := oaiSchemaRefName(ref)
		if !ok {
			// External or non-def pointer: leave the whole object as-is.
			return v
		}
		if active[name] {
			// Circular reference: keep the $ref instead of expanding.
			return v
		}
		def, found := defs[name]
		if !found {
			return v
		}
		active[name] = true
		resolved := oaiSchemaResolve(oaiSchemaDeepCopy(def), depth+1, active, defs)
		delete(active, name)
		if rm, ok := resolved.(map[string]any); ok {
			// Sibling fields next to $ref (e.g. description, title) merge
			// into the inlined definition; the definition wins collisions.
			for k, val := range v {
				if k == "$ref" {
					continue
				}
				if _, exists := rm[k]; !exists {
					rm[k] = val
				}
			}
			return rm
		}
		return resolved
	case []any:
		for i, val := range v {
			v[i] = oaiSchemaResolve(val, depth+1, active, defs)
		}
		return v
	}
	return node
}

// oaiSchemaRefName returns the def name for refs shaped "#/$defs/<name>" or
// "#/definitions/<name>". ok is false for any other shape, including deeper
// JSON pointers such as "#/$defs/A/properties" and empty names.
func oaiSchemaRefName(ref string) (name string, ok bool) {
	for _, prefix := range [...]string{"#/$defs/", "#/definitions/"} {
		if strings.HasPrefix(ref, prefix) {
			name = ref[len(prefix):]
			if name == "" || strings.Contains(name, "/") {
				return "", false
			}
			return name, true
		}
	}
	return "", false
}

// oaiSchemaDeepCopy copies a decoded JSON value so each inlined $ref site
// gets an independent def: sibling merges and nested resolution mutate the
// copy, not the shared definition.
func oaiSchemaDeepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[k] = oaiSchemaDeepCopy(val)
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i, val := range t {
			s[i] = oaiSchemaDeepCopy(val)
		}
		return s
	default:
		return v
	}
}
