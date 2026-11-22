package customprov

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
)

// rewriteForwardNoMedia is RewriteForward with the media pass disabled: the
// behavior the pre-media cases below pin.
func rewriteForwardNoMedia(raw []byte, model string, webSearch bool) ([]byte, error) {
	out, _, err := RewriteForward(raw, model, webSearch, mediaPolicy{})
	return out, err
}

// forwardNoSearch is RewriteForward with web search off: the behavior the
// pre-web-search cases below pin.
func forwardNoSearch(raw []byte, model string) ([]byte, error) {
	return rewriteForwardNoMedia(raw, model, false)
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
			out, err := rewriteForwardNoMedia([]byte(in), "glm-5.3", true)
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
			out, err := rewriteForwardNoMedia([]byte(in), "glm-5.3", true)
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
			out, err := rewriteForwardNoMedia([]byte(tc.in), "glm-5.3", true)
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
		out, err := rewriteForwardNoMedia([]byte(in), "UP", true)
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
		out, err := rewriteForwardNoMedia([]byte(in), "UP", true)
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
			out, err := rewriteForwardNoMedia([]byte(in), "UP", true)
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

// TestRewriteForward_MediaPassLeavesNoMediaBodyByteIdentical pins the
// byte-for-byte guarantee for a body with no media: enabling the media pass
// must not change the output at all.
func TestRewriteForward_MediaPassLeavesNoMediaBodyByteIdentical(t *testing.T) {
	in := `{"model":"claude-glm-5.3","system":[{"type":"text","text":"x-anthropic-billing-header: cch=abc"}],"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"text","text":"ok"}]}],"stream":true}`
	base, baseCounts, err := RewriteForward([]byte(in), "glm-5.3", false, mediaPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if !baseCounts.empty() {
		t.Fatalf("disabled media pass counted %+v", baseCounts)
	}
	withMedia, counts, err := RewriteForward([]byte(in), "glm-5.3", false, mediaPolicy{replaceImages: true, replaceDocuments: true})
	if err != nil {
		t.Fatal(err)
	}
	if !counts.empty() {
		t.Fatalf("media pass counted %+v for a body with no media", counts)
	}
	if !bytes.Equal(base, withMedia) {
		t.Errorf("media pass changed a no-media body:\nbase: %s\nwith: %s", base, withMedia)
	}
}

func TestRewriteForward_ReplacesTopLevelImage(t *testing.T) {
	in := `{"model":"claude-glm-5.3","messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}]}]}`
	out, counts, err := RewriteForward([]byte(in), "glm-5.3", false, mediaPolicy{replaceImages: true})
	if err != nil {
		t.Fatal(err)
	}
	if counts.images != 1 || counts.documents != 0 {
		t.Fatalf("counts = %+v, want 1 image and 0 documents", counts)
	}
	s := string(out)
	if !strings.Contains(s, `{"type":"text","text":"[image omitted]"}`) {
		t.Errorf("marker missing: %s", s)
	}
	if strings.Contains(s, "aGk=") {
		t.Errorf("image payload survived: %s", s)
	}
	if !strings.Contains(s, `"text":"look"`) {
		t.Errorf("sibling text block lost: %s", s)
	}
}

func TestRewriteForward_ReplacesAssistantImage(t *testing.T) {
	in := `{"model":"claude-glm-5.3","messages":[{"role":"assistant","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}]}]}`
	out, counts, err := RewriteForward([]byte(in), "glm-5.3", false, mediaPolicy{replaceImages: true})
	if err != nil {
		t.Fatal(err)
	}
	if counts.images != 1 {
		t.Fatalf("counts = %+v, want 1 image", counts)
	}
	if !strings.Contains(string(out), "[image omitted]") {
		t.Errorf("assistant image not replaced: %s", out)
	}
}

func TestRewriteForward_ImageOnlyTurnStaysNonEmpty(t *testing.T) {
	in := `{"model":"claude-glm-5.3","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}]}]}`
	out, _, err := RewriteForward([]byte(in), "glm-5.3", false, mediaPolicy{replaceImages: true})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Messages []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("rewritten body is not valid JSON: %v\n%s", err, out)
	}
	if len(decoded.Messages) != 1 || len(decoded.Messages[0].Content) != 1 {
		t.Fatalf("content = %#v, want one replacement block", decoded.Messages)
	}
	block := decoded.Messages[0].Content[0]
	if block.Type != "text" || block.Text != "[image omitted]" {
		t.Errorf("block = %+v, want the text marker", block)
	}
}

func TestRewriteForward_ReplacesImageInsideToolResult(t *testing.T) {
	in := `{"model":"claude-glm-5.3","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"ok"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}]}]}]}`
	out, counts, err := RewriteForward([]byte(in), "glm-5.3", false, mediaPolicy{replaceImages: true})
	if err != nil {
		t.Fatal(err)
	}
	if counts.images != 1 {
		t.Fatalf("counts = %+v, want 1 image", counts)
	}
	s := string(out)
	for _, want := range []string{`"tool_use_id":"t1"`, `"text":"ok"`, `[image omitted]`} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q: %s", want, s)
		}
	}
	if strings.Contains(s, "aGk=") {
		t.Errorf("image payload survived: %s", s)
	}
}

func TestRewriteForward_DocumentFollowsKeepDocuments(t *testing.T) {
	in := `{"model":"claude-glm-5.3","messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"cGRm"}},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}]}]}`
	out, counts, err := RewriteForward([]byte(in), "glm-5.3", false, mediaPolicy{replaceImages: true, replaceDocuments: true})
	if err != nil {
		t.Fatal(err)
	}
	if counts.images != 1 || counts.documents != 1 {
		t.Fatalf("counts = %+v, want 1 image and 1 document", counts)
	}
	s := string(out)
	if !strings.Contains(s, "[image omitted]") || !strings.Contains(s, "[document omitted]") {
		t.Errorf("markers missing: %s", s)
	}
	if strings.Contains(s, "aGk=") || strings.Contains(s, "cGRm") {
		t.Errorf("media payload survived: %s", s)
	}

	kept, keepCounts, err := RewriteForward([]byte(in), "glm-5.3", false, mediaPolicy{replaceImages: true})
	if err != nil {
		t.Fatal(err)
	}
	if keepCounts.images != 1 || keepCounts.documents != 0 {
		t.Fatalf("keep_documents counts = %+v, want 1 image and 0 documents", keepCounts)
	}
	keptStr := string(kept)
	if !strings.Contains(keptStr, `"type":"document"`) || !strings.Contains(keptStr, "cGRm") {
		t.Errorf("document was not kept: %s", keptStr)
	}
	if strings.Contains(keptStr, "[document omitted]") {
		t.Errorf("document marker appeared despite keep_documents: %s", keptStr)
	}
}

func TestRewriteForward_CarriesCacheControlToMarker(t *testing.T) {
	in := `{"model":"claude-glm-5.3","messages":[{"role":"user","content":[{"type":"image","cache_control":{"type":"ephemeral"},"source":{"type":"base64","media_type":"image/png","data":"aGk="}}]}]}`
	out, _, err := RewriteForward([]byte(in), "glm-5.3", false, mediaPolicy{replaceImages: true})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"text","text":"[image omitted]","cache_control":{"type":"ephemeral"}}`
	if !strings.Contains(string(out), want) {
		t.Errorf("cache_control not carried to the marker: %s", out)
	}
}

func TestRewriteForward_LeavesUnaffectedMessagesByteIdentical(t *testing.T) {
	in := `{"model":"claude-glm-5.3","messages":[{"role":"assistant","content":[{"type":"text","text":"keep"}],"extra":123},{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}]}]}`
	out, _, err := RewriteForward([]byte(in), "glm-5.3", false, mediaPolicy{replaceImages: true})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"role":"assistant","content":[{"type":"text","text":"keep"}],"extra":123}`
	if !strings.Contains(string(out), want) {
		t.Errorf("unaffected message bytes changed: %s", out)
	}
}

func TestRewriteForward_WebSearchAndMediaTogether(t *testing.T) {
	in := `{"model":"claude-x","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}]}]}`
	out, counts, err := RewriteForward([]byte(in), "UP", true, mediaPolicy{replaceImages: true})
	if err != nil {
		t.Fatal(err)
	}
	if counts.images != 1 {
		t.Fatalf("counts = %+v, want 1 image", counts)
	}
	s := string(out)
	if !strings.Contains(s, webSearchHeaderBlock) {
		t.Errorf("web search block missing: %s", s)
	}
	if !strings.Contains(s, "[image omitted]") {
		t.Errorf("image marker missing: %s", s)
	}
}

func TestRewriteForward_DoesNotDescendIntoNestedToolResult(t *testing.T) {
	in := `{"model":"claude-glm-5.3","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"outer","content":[{"type":"tool_result","tool_use_id":"inner","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}]}]}]}]}`
	out, counts, err := RewriteForward([]byte(in), "glm-5.3", false, mediaPolicy{replaceImages: true})
	if err != nil {
		t.Fatal(err)
	}
	if !counts.empty() {
		t.Fatalf("counts = %+v, want no replacements below the first tool_result level", counts)
	}
	if strings.Contains(string(out), "[image omitted]") {
		t.Errorf("nested tool_result image was replaced: %s", out)
	}
}

func TestRewriteForward_DropsNullCacheControl(t *testing.T) {
	in := `{"model":"claude-glm-5.3","messages":[{"role":"user","content":[{"type":"image","cache_control":null,"source":{"type":"base64","media_type":"image/png","data":"aGk="}}]}]}`
	out, _, err := RewriteForward([]byte(in), "glm-5.3", false, mediaPolicy{replaceImages: true})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"text","text":"[image omitted]"}`
	if !strings.Contains(string(out), want) {
		t.Errorf("marker missing: %s", out)
	}
	if strings.Contains(string(out), "cache_control") {
		t.Errorf("null cache_control leaked into the marker: %s", out)
	}
}

func TestRewriteModel_DoesNotTouchMedia(t *testing.T) {
	in := `{"model":"old","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}]}]}`
	out, err := RewriteModel([]byte(in), "new")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `"type":"image"`) || !strings.Contains(s, "aGk=") {
		t.Errorf("RewriteModel changed media: %s", s)
	}
	if !strings.Contains(s, `"model":"new"`) {
		t.Errorf("model not rewritten: %s", s)
	}
}
