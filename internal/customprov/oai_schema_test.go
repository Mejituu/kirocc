package customprov

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
)

// oaiAssertJSONEq compares got against want. Both sides are canonicalized
// (sorted object keys) before comparison, mirroring what oaiInlineRefs
// itself does: json.Marshal emits map keys in randomized order, so a plain
// byte comparison of marshaled maps would flake.
func oaiAssertJSONEq(t *testing.T, want any, got []byte) {
	t.Helper()
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	wantVal := jsontext.Value(wantJSON)
	if err := wantVal.Canonicalize(); err != nil {
		t.Fatalf("canonicalize want: %v", err)
	}
	gotVal := jsontext.Value(got)
	if err := gotVal.Canonicalize(); err != nil {
		t.Fatalf("got is not valid JSON: %v", err)
	}
	if string(wantVal) != string(gotVal) {
		t.Fatalf("mismatch:\n want: %s\n  got: %s", wantVal, gotVal)
	}
}

func TestOAIInlineRefs_NoDefsReturnsInputUnchanged(t *testing.T) {
	inputs := []string{
		`{"type":"object",\n  "properties": {"x": {"type":"string"}}}`,
		`{"type":"object","properties":{"x":{"type":"string"}},  "description": "odd spacing"}`,
		`[1, 2, {"a": "b"}]`,
		`"just a string"`,
		`123`,
		`null`,
		`{"$defs":{}}`,
		`{"definitions":{}}`,
		`{"$defs":null}`,
	}
	for _, in := range inputs {
		in := strings.ReplaceAll(in, `\n`, "\n")
		got, err := oaiInlineRefs([]byte(in))
		if err != nil {
			t.Fatalf("input %q: unexpected error: %v", in, err)
		}
		if string(got) != in {
			t.Fatalf("input %q: output changed: %q", in, string(got))
		}
	}
}

func TestOAIInlineRefs_SingleLevelDefs(t *testing.T) {
	got, err := oaiInlineRefs([]byte(
		`{"type":"object","$defs":{"A":{"type":"string","description":"def"}},"properties":{"x":{"$ref":"#/$defs/A"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	oaiAssertJSONEq(t, map[string]any{
		"type":       "object",
		"properties": map[string]any{"x": map[string]any{"type": "string", "description": "def"}},
	}, got)
}

func TestOAIInlineRefs_SingleLevelDefinitions(t *testing.T) {
	got, err := oaiInlineRefs([]byte(
		`{"definitions":{"B":{"type":"number"}},"properties":{"y":{"$ref":"#/definitions/B"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	oaiAssertJSONEq(t, map[string]any{
		"properties": map[string]any{"y": map[string]any{"type": "number"}},
	}, got)
}

func TestOAIInlineRefs_NestedRefs(t *testing.T) {
	got, err := oaiInlineRefs([]byte(
		`{"$defs":{"A":{"type":"object","properties":{"b":{"$ref":"#/$defs/B"}}},"B":{"type":"string"}},"properties":{"p":{"$ref":"#/$defs/A"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	oaiAssertJSONEq(t, map[string]any{
		"properties": map[string]any{"p": map[string]any{
			"type":       "object",
			"properties": map[string]any{"b": map[string]any{"type": "string"}},
		}},
	}, got)
}

func TestOAIInlineRefs_CircularRefsKeepRef(t *testing.T) {
	got, err := oaiInlineRefs([]byte(
		`{"$defs":{"A":{"type":"object","properties":{"next":{"$ref":"#/$defs/B"}}},"B":{"type":"array","items":{"$ref":"#/$defs/A"}}},"properties":{"p":{"$ref":"#/$defs/A"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	oaiAssertJSONEq(t, map[string]any{
		"properties": map[string]any{"p": map[string]any{
			"type": "object",
			"properties": map[string]any{"next": map[string]any{
				"type":  "array",
				"items": map[string]any{"$ref": "#/$defs/A"},
			}},
		}},
	}, got)
}

func TestOAIInlineRefs_SelfRefKeepsRef(t *testing.T) {
	got, err := oaiInlineRefs([]byte(
		`{"$defs":{"A":{"type":"object","properties":{"child":{"$ref":"#/$defs/A"}}}},"properties":{"p":{"$ref":"#/$defs/A"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	oaiAssertJSONEq(t, map[string]any{
		"properties": map[string]any{"p": map[string]any{
			"type":       "object",
			"properties": map[string]any{"child": map[string]any{"$ref": "#/$defs/A"}},
		}},
	}, got)
}

func TestOAIInlineRefs_DepthCap(t *testing.T) {
	const chainLen = 150
	defs := map[string]any{}
	for i := 1; i <= chainLen; i++ {
		defs[fmt.Sprintf("D%d", i)] = map[string]any{"$ref": fmt.Sprintf("#/$defs/D%d", i+1)}
	}
	defs[fmt.Sprintf("D%d", chainLen+1)] = map[string]any{"type": "string"}
	input, err := json.Marshal(map[string]any{
		"$defs":      defs,
		"properties": map[string]any{"p": map[string]any{"$ref": "#/$defs/D1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := oaiInlineRefs(input)
	if err != nil {
		t.Fatal(err)
	}
	out := string(got)
	if !strings.Contains(out, `"$ref"`) {
		t.Fatalf("depth cap should leave a $ref in place, got: %s", out)
	}
	if strings.Contains(out, `"$defs"`) {
		t.Fatalf("top-level $defs should be removed, got: %s", out)
	}
	var back any
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
}

func TestOAIInlineRefs_SiblingFieldsMerge(t *testing.T) {
	got, err := oaiInlineRefs([]byte(
		`{"$defs":{"A":{"type":"string","description":"def-desc"}},"properties":{"x":{"$ref":"#/$defs/A","description":"sib-desc","title":"sib-title"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	oaiAssertJSONEq(t, map[string]any{
		"properties": map[string]any{"x": map[string]any{
			"type":        "string",
			"description": "def-desc", // definition wins the conflict
			"title":       "sib-title",
		}},
	}, got)
}

func TestOAIInlineRefs_NonLocalRefsKept(t *testing.T) {
	got, err := oaiInlineRefs([]byte(
		`{"$defs":{"A":{"type":"string"}},"properties":{
			"a":{"$ref":"#/$defs/A"},
			"b":{"$ref":"http://example.com/schema","description":"ext"},
			"c":{"$ref":"#/pointer/to/nowhere"},
			"d":{"$ref":"#/$defs/A/properties"}
		}}`))
	if err != nil {
		t.Fatal(err)
	}
	oaiAssertJSONEq(t, map[string]any{
		"properties": map[string]any{
			"a": map[string]any{"type": "string"},
			"b": map[string]any{"$ref": "http://example.com/schema", "description": "ext"},
			"c": map[string]any{"$ref": "#/pointer/to/nowhere"},
			"d": map[string]any{"$ref": "#/$defs/A/properties"},
		},
	}, got)
}

func TestOAIInlineRefs_MissingDefKept(t *testing.T) {
	got, err := oaiInlineRefs([]byte(
		`{"$defs":{"A":{"type":"string"}},"properties":{"x":{"$ref":"#/$defs/missing"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	oaiAssertJSONEq(t, map[string]any{
		"properties": map[string]any{"x": map[string]any{"$ref": "#/$defs/missing"}},
	}, got)
}

func TestOAIInlineRefs_TopLevelDefsRemoved(t *testing.T) {
	// Both tables present, both referenced, both keys must disappear.
	got, err := oaiInlineRefs([]byte(
		`{"$defs":{"A":{"type":"string"}},"definitions":{"B":{"type":"number"}},"properties":{"x":{"$ref":"#/$defs/A"},"y":{"$ref":"#/definitions/B"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatal(err)
	}
	if _, ok := obj["$defs"]; ok {
		t.Fatalf("$defs not removed: %s", got)
	}
	if _, ok := obj["definitions"]; ok {
		t.Fatalf("definitions not removed: %s", got)
	}
	oaiAssertJSONEq(t, map[string]any{
		"properties": map[string]any{
			"x": map[string]any{"type": "string"},
			"y": map[string]any{"type": "number"},
		},
	}, got)

	// Unreferenced defs are still dropped.
	got, err = oaiInlineRefs([]byte(`{"$defs":{"A":{"type":"string"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	oaiAssertJSONEq(t, map[string]any{}, got)
}

func TestOAIInlineRefs_InvalidJSON(t *testing.T) {
	for _, in := range []string{`{"type":`, `not json`, ``} {
		if _, err := oaiInlineRefs([]byte(in)); err == nil {
			t.Fatalf("input %q: expected error, got nil", in)
		}
	}
}

func TestOAIInlineRefs_DeepRefsInPropertiesAndItems(t *testing.T) {
	got, err := oaiInlineRefs([]byte(
		`{"$defs":{"A":{"type":"string"}},"type":"object","properties":{"list":{"type":"array","items":{"$ref":"#/$defs/A"}}},"oneOf":[{"$ref":"#/$defs/A"},{"type":"null"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	oaiAssertJSONEq(t, map[string]any{
		"type": "object",
		"properties": map[string]any{"list": map[string]any{
			"type":  "array",
			"items": map[string]any{"type": "string"},
		}},
		"oneOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "null"}},
	}, got)
}

func TestOAIInlineRefs_TopLevelRef(t *testing.T) {
	got, err := oaiInlineRefs([]byte(
		`{"$ref":"#/$defs/A","$defs":{"A":{"type":"object","properties":{"q":{"$ref":"#/$defs/B"}}},"B":{"type":"string"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	oaiAssertJSONEq(t, map[string]any{
		"type":       "object",
		"properties": map[string]any{"q": map[string]any{"type": "string"}},
	}, got)
}

func TestOAIInlineRefs_DeterministicOutput(t *testing.T) {
	input := []byte(
		`{"$defs":{"A":{"type":"object","properties":{"b":{"$ref":"#/$defs/B"},"c":{"$ref":"#/$defs/B"}}},"B":{"type":"string"},"C":{"$ref":"#/$defs/A"}},"properties":{"x":{"$ref":"#/$defs/C"},"y":{"$ref":"#/$defs/B"}}}`)
	first, err := oaiInlineRefs(input)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		again, err := oaiInlineRefs(input)
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatalf("non-deterministic output:\n first: %s\n again: %s", first, again)
		}
	}
}
