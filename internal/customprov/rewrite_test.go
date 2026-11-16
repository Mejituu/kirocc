package customprov

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
)

// forwardNoSearch is RewriteForward with web search off: the behavior the
// pre-web-search cases below pin.
func forwardNoSearch(raw []byte, model string) ([]byte, error) {
	return RewriteForward(raw, model, false)
}

// webSearchHeaderBlock is the fixed block RewriteForward plants for web
// search, stated literally so the tests pin its exact bytes instead of
// echoing the implementation's constant.
const webSearchHeaderBlock = `{"type":"text","text":"x-anthropic-billing-header: cc_entrypoint=cli;"}`

func TestRewriteModel_ReplacesTopLevelModel(t *testing.T) {
	in := `{"model":"claude-glm-5.3","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	out, err := RewriteModel([]byte(in), "glm-5.3")
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("rewritten body is not valid JSON: %v\n%s", err, out)
	}
	if decoded["model"] != "glm-5.3" {
		t.Errorf("model = %v, want glm-5.3", decoded["model"])
	}
	if decoded["max_tokens"] != float64(16) {
		t.Errorf("max_tokens = %v, want 16", decoded["max_tokens"])
	}
}

func TestRewriteModel_PreservesUnknownFieldsOrderAndNumbers(t *testing.T) {
	in := `{"model":"claude-glm-5.3","temperature":0.5,"metadata":{"user_id":"u1"},"service_tier":"auto","big":12345678901234567890,"mcp_servers":[{"type":"url"}]}`
	out, err := RewriteModel([]byte(in), "glm-5.3")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	// Key order must survive: model still first, temperature second.
	if !strings.HasPrefix(s, `{"model":"glm-5.3","temperature":0.5,`) {
		t.Errorf("key order changed: %s", s)
	}
	// Unknown fields and the exact number spelling must survive.
	for _, want := range []string{`"service_tier":"auto"`, `"big":12345678901234567890`, `"mcp_servers":[{"type":"url"}]`} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q: %s", want, s)
		}
	}
}

func TestRewriteModel_OnlyTopLevel(t *testing.T) {
	in := `{"model":"claude-glm-5.3","messages":[{"role":"user","content":[{"type":"text","text":"x"}]}],"nested":{"model":"claude-glm-5.3"}}`
	out, err := RewriteModel([]byte(in), "glm-5.3")
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Messages []struct {
			Content jsontext.Value `json:"content"`
		} `json:"messages"`
		Nested struct {
			Model string `json:"model"`
		} `json:"nested"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Nested.Model != "claude-glm-5.3" {
		t.Errorf("nested model was rewritten: %s", out)
	}
}

func TestRewriteModel_ModelValueNotString(t *testing.T) {
	_, err := RewriteModel([]byte(`{"model":42}`), "glm-5.3")
	if err == nil {
		t.Fatal("expected an error when model is not a string")
	}
}

func TestRewriteModel_RootNotObject(t *testing.T) {
	if _, err := RewriteModel([]byte(`["model"]`), "glm-5.3"); err == nil {
		t.Fatal("expected an error for a non-object root")
	}
}

// TestRewriteModel_LeavesSystemAlone pins the mapping path's variant: it
// rewrites only the model, billing blocks included.
func TestRewriteModel_LeavesSystemAlone(t *testing.T) {
	in := `{"model":"claude-glm-5.3","system":[{"type":"text","text":"x-anthropic-billing-header: cch=abc"}],"messages":[]}`
	out, err := RewriteModel([]byte(in), "glm-5.3")
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("rewritten body is not valid JSON: %v\n%s", err, out)
	}
	sys, ok := decoded["system"].([]any)
	if !ok || len(sys) != 1 {
		t.Fatalf("system = %v, want the billing block kept: %s", decoded["system"], out)
	}
	if decoded["model"] != "glm-5.3" {
		t.Errorf("model = %v, want glm-5.3", decoded["model"])
	}
}

// TestRewriteForward_BillingHeaderPositions covers the system-array shapes:
// a billing block first, in the middle, or as the only element (the member
// drops entirely), with a cache_control block kept verbatim.
func TestRewriteForward_BillingHeaderPositions(t *testing.T) {
	const billing = `{"type":"text","text":"x-anthropic-billing-header: cch=abc"}`
	const sysText = `{"type":"text","text":"You are helpful."}`
	const sysCached = `{"type":"text","text":"Keep me.","cache_control":{"type":"ephemeral"}}`
	cases := []struct {
		name      string
		system    string
		wantSys   string // "" means the member is gone
		wantCount int
	}{
		{name: "billing first", system: "[" + billing + "," + sysText + "]", wantSys: `[` + sysText + `]`},
		{name: "billing in the middle", system: "[" + sysText + "," + billing + "," + sysCached + `]`, wantSys: `[` + sysText + `,` + sysCached + `]`},
		{name: "billing only drops the member", system: "[" + billing + `]`, wantSys: ""},
		{name: "cache_control neighbour kept", system: "[" + billing + "," + sysCached + `]`, wantSys: `[` + sysCached + `]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := `{"model":"claude-glm-5.3","system":` + tc.system + `,"max_tokens":16,"messages":[]}`
			out, err := forwardNoSearch([]byte(in), "glm-5.3")
			if err != nil {
				t.Fatal(err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(out, &decoded); err != nil {
				t.Fatalf("rewritten body is not valid JSON: %v\n%s", err, out)
			}
			if decoded["model"] != "glm-5.3" {
				t.Errorf("model = %v, want glm-5.3", decoded["model"])
			}
			if tc.wantSys == "" {
				if _, present := decoded["system"]; present {
					t.Errorf("system member must be dropped: %s", out)
				}
			} else if got := sysMember(t, out); got != tc.wantSys {
				t.Errorf("system = %s, want %s", got, tc.wantSys)
			}
			if strings.Contains(string(out), "x-anthropic-billing-header") {
				t.Errorf("billing header survived: %s", out)
			}
		})
	}
}

// sysMember extracts the raw bytes of the top-level system value.
func sysMember(t *testing.T, body []byte) string {
	t.Helper()
	dec := jsontext.NewDecoder(bytes.NewReader(body))
	if _, err := dec.ReadToken(); err != nil { // '{'
		t.Fatal(err)
	}
	for {
		tok, err := dec.ReadToken()
		if err != nil {
			t.Fatal(err)
		}
		if tok.Kind() == '}' {
			break
		}
		if tok.Kind() != '"' {
			t.Fatalf("expected a key token, got %v: %s", tok.Kind(), body)
		}
		key := tok.String() // before ReadValue voids the token
		v, err := dec.ReadValue()
		if err != nil {
			t.Fatal(err)
		}
		if key == "system" {
			return string(v)
		}
	}
	t.Fatalf("no system member in %s", body)
	return ""
}

// TestRewriteForward_SystemStringAndAbsent keeps the non-array forms
// untouched.
func TestRewriteForward_SystemStringAndAbsent(t *testing.T) {
	t.Run("string system unchanged", func(t *testing.T) {
		in := `{"model":"claude-glm-5.3","system":"x-anthropic-billing-header: cch=abc and more","messages":[]}`
		out, err := forwardNoSearch([]byte(in), "glm-5.3")
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(out, &decoded); err != nil {
			t.Fatalf("rewritten body is not valid JSON: %v\n%s", err, out)
		}
		if decoded["system"] != "x-anthropic-billing-header: cch=abc and more" {
			t.Errorf("string system must pass through verbatim: %s", out)
		}
	})
	t.Run("no system member", func(t *testing.T) {
		in := `{"model":"claude-glm-5.3","max_tokens":16,"messages":[]}`
		out, err := forwardNoSearch([]byte(in), "glm-5.3")
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(out, &decoded); err != nil {
			t.Fatalf("rewritten body is not valid JSON: %v\n%s", err, out)
		}
		if _, present := decoded["system"]; present {
			t.Errorf("system member must not appear: %s", out)
		}
		if decoded["model"] != "glm-5.3" {
			t.Errorf("model = %v, want glm-5.3", decoded["model"])
		}
	})
}

// TestRewriteForward_MultilineBillingPrefixNotDropped pins the Kiro-path
// rule: text starting with the prefix but carrying a newline is real
// content and must survive. (The JSON literal carries the two-character \n
// escape; the decoded text holds a real newline.)
func TestRewriteForward_MultilineBillingPrefixNotDropped(t *testing.T) {
	const multi = `{"type":"text","text":"x-anthropic-billing-header: cch=abc\nsecond line of instructions"}`
	in := `{"model":"claude-glm-5.3","system":[` + multi + `],"messages":[]}`
	out, err := forwardNoSearch([]byte(in), "glm-5.3")
	if err != nil {
		t.Fatal(err)
	}
	if got := sysMember(t, out); got != `[`+multi+`]` {
		t.Errorf("system = %s, want the block verbatim", got)
	}
}

// TestRewriteForward_VerbatimAroundBilling asserts byte-preservation: apart
// from the removed element the output is the input with only the model
// value swapped.
// TestRewriteForward_SystemBeforeModel covers a body where system precedes
// model: the parity tracking must not misread the following keys as values,
// so the model is still rewritten (with the system filtered, or dropped, or
// a string system passed through).
func TestRewriteForward_SystemBeforeModel(t *testing.T) {
	const billing = `{"type":"text","text":"x-anthropic-billing-header: cch=abc"}`
	const sysText = `{"type":"text","text":"You are helpful."}`
	t.Run("array system before model", func(t *testing.T) {
		in := `{"system":[` + billing + `,` + sysText + `],"model":"claude-x","max_tokens":5}`
		out, err := forwardNoSearch([]byte(in), "UP")
		if err != nil {
			t.Fatal(err)
		}
		want := `{"system":[` + sysText + `],"model":"UP","max_tokens":5}` + "\n"
		if string(out) != want {
			t.Errorf("output:\n got: %s\nwant: %s", out, want)
		}
	})
	t.Run("dropped system before model", func(t *testing.T) {
		in := `{"system":[` + billing + `],"model":"claude-x","max_tokens":5}`
		out, err := forwardNoSearch([]byte(in), "UP")
		if err != nil {
			t.Fatal(err)
		}
		want := `{"model":"UP","max_tokens":5}` + "\n"
		if string(out) != want {
			t.Errorf("output:\n got: %s\nwant: %s", out, want)
		}
	})
	t.Run("string system before model", func(t *testing.T) {
		in := `{"system":"plain","model":"claude-x","max_tokens":5}`
		out, err := forwardNoSearch([]byte(in), "UP")
		if err != nil {
			t.Fatal(err)
		}
		want := `{"system":"plain","model":"UP","max_tokens":5}` + "\n"
		if string(out) != want {
			t.Errorf("output:\n got: %s\nwant: %s", out, want)
		}
	})
}

func TestRewriteForward_VerbatimAroundBilling(t *testing.T) {
	in := `{"model":"claude-glm-5.3","temperature":0.5,"system":[{"type":"text","text":"x-anthropic-billing-header: cch=z","cache_control":{"type":"ephemeral"}},{"type":"text","text":"Keep.","cache_control":{"type":"ephemeral"}}],"big":12345678901234567890,"messages":[]}`
	out, err := forwardNoSearch([]byte(in), "glm-5.3")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"glm-5.3","temperature":0.5,"system":[` +
		`{"type":"text","text":"Keep.","cache_control":{"type":"ephemeral"}}]` +
		`,"big":12345678901234567890,"messages":[]}` + "\n" // the Encoder's top-level newline, same as RewriteModel
	if string(out) != want {
		t.Errorf("output is not the expected verbatim rewrite:\n got: %s\nwant: %s", out, want)
	}
}

// TestRewriteForward_WebSearchArray covers the array form with web search
// on: billing blocks are still stripped, the fixed block lands first, the
// survivors keep their bytes, and an array the strip pass empties keeps
// the member with the fixed block alone (unlike the web-search-off drop).
// An original element that looks exactly like the fixed block is stripped
// with the other billing blocks, so the block ends up in the output once.
func TestRewriteForward_WebSearchArray(t *testing.T) {
	const billing = `{"type":"text","text":"x-anthropic-billing-header: cch=abc"}`
	const sysText = `{"type":"text","text":"You are helpful."}`
	const sysCached = `{"type":"text","text":"Keep me.","cache_control":{"type":"ephemeral"}}`
	cases := []struct {
		name   string
		system string
		want   string
	}{
		{name: "billing first", system: "[" + billing + "," + sysText + "]", want: "[" + webSearchHeaderBlock + "," + sysText + "]"},
		{name: "billing in the middle", system: "[" + sysText + "," + billing + "," + sysCached + "]", want: "[" + webSearchHeaderBlock + "," + sysText + "," + sysCached + "]"},
		{name: "billing only keeps the member", system: "[" + billing + "]", want: "[" + webSearchHeaderBlock + "]"},
		{name: "lookalike stripped not duplicated", system: "[" + webSearchHeaderBlock + "," + sysText + "]", want: "[" + webSearchHeaderBlock + "," + sysText + "]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := `{"model":"claude-glm-5.3","system":` + tc.system + `,"max_tokens":16,"messages":[]}`
			out, err := RewriteForward([]byte(in), "glm-5.3", true)
			if err != nil {
				t.Fatal(err)
			}
			want := `{"model":"glm-5.3","system":` + tc.want + `,"max_tokens":16,"messages":[]}` + "\n"
			if string(out) != want {
				t.Errorf("output:\n got: %s\nwant: %s", out, want)
			}
			if got := strings.Count(string(out), webSearchHeaderBlock); got != 1 {
				t.Errorf("fixed block appears %d times, want exactly 1: %s", got, out)
			}
		})
	}
}

// TestRewriteForward_WebSearchString covers the string form with web search
// on: the string is promoted to the array form around the fixed block,
// with its raw JSON bytes (escape spellings included) verbatim in the text
// field of the promoted block.
func TestRewriteForward_WebSearchString(t *testing.T) {
	cases := []struct {
		name   string
		system string // raw JSON string token, quotes included
		want   string
	}{
		{name: "plain", system: `"You are helpful."`, want: `[` + webSearchHeaderBlock + `,{"type":"text","text":"You are helpful."}]`},
		// The JSON literal carries the two-character \n and \t escapes; the
		// promoted text block must keep those bytes, not re-encode them.
		{name: "escapes kept", system: `"line1\nline2\tend"`, want: `[` + webSearchHeaderBlock + `,{"type":"text","text":"line1\nline2\tend"}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := `{"model":"claude-glm-5.3","system":` + tc.system + `,"messages":[]}`
			out, err := RewriteForward([]byte(in), "glm-5.3", true)
			if err != nil {
				t.Fatal(err)
			}
			want := `{"model":"glm-5.3","system":` + tc.want + `,"messages":[]}` + "\n"
			if string(out) != want {
				t.Errorf("output:\n got: %s\nwant: %s", out, want)
			}
		})
	}
}

// TestRewriteForward_WebSearchNoSystem covers bodies without a system
// member: the fixed-block member is appended last (the Encoder supplies
// the separator), everything else keeps its bytes with the model rewrite
// intact, and an empty root ends up with the member alone.
func TestRewriteForward_WebSearchNoSystem(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "appended last", in: `{"model":"claude-glm-5.3","max_tokens":16,"messages":[]}`, want: `{"model":"glm-5.3","max_tokens":16,"messages":[],"system":[` + webSearchHeaderBlock + `]}` + "\n"},
		{name: "empty root", in: `{}`, want: `{"system":[` + webSearchHeaderBlock + `]}` + "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := RewriteForward([]byte(tc.in), "glm-5.3", true)
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != tc.want {
				t.Errorf("output:\n got: %s\nwant: %s", out, tc.want)
			}
		})
	}
}

// TestRewriteForward_WebSearchSystemBeforeModel mirrors
// TestRewriteForward_SystemBeforeModel with web search on: after
// rewriteSystem consumes the system value the next token is again a key,
// so the model rewrite must still fire with either member order (the
// model-first order is covered by TestRewriteForward_WebSearchArray).
func TestRewriteForward_WebSearchSystemBeforeModel(t *testing.T) {
	const billing = `{"type":"text","text":"x-anthropic-billing-header: cch=abc"}`
	const sysText = `{"type":"text","text":"You are helpful."}`
	t.Run("array system before model", func(t *testing.T) {
		in := `{"system":[` + billing + `,` + sysText + `],"model":"claude-x","max_tokens":5}`
		out, err := RewriteForward([]byte(in), "UP", true)
		if err != nil {
			t.Fatal(err)
		}
		want := `{"system":[` + webSearchHeaderBlock + `,` + sysText + `],"model":"UP","max_tokens":5}` + "\n"
		if string(out) != want {
			t.Errorf("output:\n got: %s\nwant: %s", out, want)
		}
	})
	t.Run("string system before model", func(t *testing.T) {
		in := `{"system":"plain","model":"claude-x","max_tokens":5}`
		out, err := RewriteForward([]byte(in), "UP", true)
		if err != nil {
			t.Fatal(err)
		}
		want := `{"system":[` + webSearchHeaderBlock + `,{"type":"text","text":"plain"}],"model":"UP","max_tokens":5}` + "\n"
		if string(out) != want {
			t.Errorf("output:\n got: %s\nwant: %s", out, want)
		}
	})
}

// TestRewriteForward_WebSearchOtherSystemShapes pins the forms that are
// neither array nor string: an object or null system is invalid for the
// upstream anyway, and — like with web search off — it is copied through
// unchanged rather than rewritten around a fixed block.
func TestRewriteForward_WebSearchOtherSystemShapes(t *testing.T) {
	cases := []struct {
		name   string
		system string
	}{
		{name: "object system", system: `{"type":"text","text":"hi"}`},
		{name: "null system", system: `null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := `{"system":` + tc.system + `,"model":"claude-x"}`
			out, err := RewriteForward([]byte(in), "UP", true)
			if err != nil {
				t.Fatal(err)
			}
			want := `{"system":` + tc.system + `,"model":"UP"}` + "\n"
			if string(out) != want {
				t.Errorf("output:\n got: %s\nwant: %s", out, want)
			}
		})
	}
}
