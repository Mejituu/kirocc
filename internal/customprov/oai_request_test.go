package customprov

import (
	"reflect"
	"strings"
	"testing"

	"encoding/json/jsontext"
	json "encoding/json/v2"
)

// buildForTest runs buildOpenAIRequest on raw with a fixed hit, failing the
// test on any conversion error. The second value counts the replaced media.
func buildForTest(t *testing.T, raw string) (*oaiRequest, mediaCounts) {
	t.Helper()
	return buildForTestWithHit(t, raw, Hit{BaseModel: "glm-5.3"})
}

// buildForTestWithHit is buildForTest with an explicit hit, for the
// text_only cases.
func buildForTestWithHit(t *testing.T, raw string, hit Hit) (*oaiRequest, mediaCounts) {
	t.Helper()
	req, counts, err := buildOpenAIRequest([]byte(raw), hit, RequestInfo{Model: "claude-" + hit.BaseModel})
	if err != nil {
		t.Fatalf("buildOpenAIRequest: %v", err)
	}
	return req, counts
}

// assertMessages compares converted messages against want, marshaling both
// sides for a readable diff on mismatch.
func assertMessages(t *testing.T, got, want []oaiMessage) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		gj, _ := json.Marshal(got)
		wj, _ := json.Marshal(want)
		t.Errorf("messages mismatch:\n got: %s\nwant: %s", gj, wj)
	}
}

func TestBuildOpenAIRequest_SystemString(t *testing.T) {
	req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,
		"system":"Be brief.","messages":[{"role":"user","content":"hi"}]}`)
	assertMessages(t, req.Messages, []oaiMessage{
		{Role: "system", Content: "Be brief."},
		{Role: "user", Content: "hi"},
	})
}

func TestBuildOpenAIRequest_SystemBlocks(t *testing.T) {
	req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"system":[
		{"type":"text","text":"Part one."},
		{"type":"text","text":"Part two."}
	],"messages":[]}`)
	assertMessages(t, req.Messages, []oaiMessage{
		{Role: "system", Content: "Part one.\nPart two."},
	})
}

func TestBuildOpenAIRequest_SystemSkipsBillingBlocks(t *testing.T) {
	req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"system":[
		{"type":"text","text":"x-anthropic-billing-header: {\"cch\":\"abc\"}"},
		{"type":"text","text":"  x-anthropic-billing-header: padded  "},
		{"type":"text","text":"x-anthropic-billing-header: first line\nreal instructions follow"},
		{"type":"text","text":"You are helpful."}
	],"messages":[]}`)
	// The single-line billing blocks drop out; the multi-line text that
	// merely starts with the prefix stays.
	assertMessages(t, req.Messages, []oaiMessage{
		{Role: "system", Content: "x-anthropic-billing-header: first line\nreal instructions follow\nYou are helpful."},
	})
}

func TestBuildOpenAIRequest_SystemEmptyAfterFilter(t *testing.T) {
	req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"system":[
		{"type":"text","text":"x-anthropic-billing-header: x"}
	],"messages":[]}`)
	if len(req.Messages) != 0 {
		t.Errorf("messages = %v, want none", req.Messages)
	}
}

func TestBuildOpenAIRequest_UserStringContent(t *testing.T) {
	req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,
		"messages":[{"role":"user","content":"hello"}]}`)
	assertMessages(t, req.Messages, []oaiMessage{
		{Role: "user", Content: "hello"},
	})
}

func TestBuildOpenAIRequest_UserTextBlocks(t *testing.T) {
	req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[
		{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}
	]}`)
	assertMessages(t, req.Messages, []oaiMessage{
		{Role: "user", Content: "a\nb"},
	})
}

func TestBuildOpenAIRequest_UserBase64Image(t *testing.T) {
	req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[
		{"role":"user","content":[
			{"type":"text","text":"look at this"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}
		]}
	]}`)
	assertMessages(t, req.Messages, []oaiMessage{
		{Role: "user", Content: []oaiContentPart{
			{Type: "text", Text: "look at this"},
			{Type: "image_url", ImageURL: &oaiImageURL{URL: "data:image/png;base64,aGk="}},
		}},
	})
}

func TestBuildOpenAIRequest_UserURLImage(t *testing.T) {
	req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[
		{"role":"user","content":[
			{"type":"image","source":{"type":"url","url":"https://example.com/cat.png"}}
		]}
	]}`)
	assertMessages(t, req.Messages, []oaiMessage{
		{Role: "user", Content: []oaiContentPart{
			{Type: "image_url", ImageURL: &oaiImageURL{URL: "https://example.com/cat.png"}},
		}},
	})
}

func TestBuildOpenAIRequest_ToolResultSingle(t *testing.T) {
	req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"tu_1","content":"42"}
		]}
	]}`)
	assertMessages(t, req.Messages, []oaiMessage{
		{Role: "tool", ToolCallID: "tu_1", Content: "42"},
	})
}

func TestBuildOpenAIRequest_ToolResultBlockContent(t *testing.T) {
	req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"tu_1","content":[
				{"type":"text","text":"line one"},
				{"type":"text","text":"line two"}
			]}
		]}
	]}`)
	assertMessages(t, req.Messages, []oaiMessage{
		{Role: "tool", ToolCallID: "tu_1", Content: "line one\nline two"},
	})
}

func TestBuildOpenAIRequest_ToolResultMultiple(t *testing.T) {
	req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"tu_1","content":"r1"},
			{"type":"tool_result","tool_use_id":"tu_2","content":"r2"}
		]}
	]}`)
	assertMessages(t, req.Messages, []oaiMessage{
		{Role: "tool", ToolCallID: "tu_1", Content: "r1"},
		{Role: "tool", ToolCallID: "tu_2", Content: "r2"},
	})
}

func TestBuildOpenAIRequest_ToolResultIsError(t *testing.T) {
	req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"tu_1","content":"boom","is_error":true}
		]}
	]}`)
	assertMessages(t, req.Messages, []oaiMessage{
		{Role: "tool", ToolCallID: "tu_1", Content: "Error: boom"},
	})
}

func TestBuildOpenAIRequest_ToolResultImages(t *testing.T) {
	// Images cannot ride on Chat tool messages; they move to a synthesized
	// user message right after the run of tool messages.
	req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"tu_1","content":[
				{"type":"text","text":"screenshot:"},
				{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"QUJD"}}
			]},
			{"type":"tool_result","tool_use_id":"tu_2","content":[
				{"type":"image","source":{"type":"url","url":"https://example.com/r2.png"}}
			]}
		]}
	]}`)
	assertMessages(t, req.Messages, []oaiMessage{
		{Role: "tool", ToolCallID: "tu_1", Content: "screenshot:"},
		{Role: "tool", ToolCallID: "tu_2", Content: ""},
		{Role: "user", Content: []oaiContentPart{
			{Type: "image_url", ImageURL: &oaiImageURL{URL: "data:image/jpeg;base64,QUJD"}},
			{Type: "image_url", ImageURL: &oaiImageURL{URL: "https://example.com/r2.png"}},
		}},
	})
}

func TestBuildOpenAIRequest_TextAndToolResultOrdering(t *testing.T) {
	req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[
		{"role":"user","content":[
			{"type":"text","text":"before"},
			{"type":"tool_result","tool_use_id":"tu_1","content":"r1"},
			{"type":"tool_result","tool_use_id":"tu_2","content":"r2"},
			{"type":"text","text":"after"}
		]}
	]}`)
	assertMessages(t, req.Messages, []oaiMessage{
		{Role: "user", Content: "before"},
		{Role: "tool", ToolCallID: "tu_1", Content: "r1"},
		{Role: "tool", ToolCallID: "tu_2", Content: "r2"},
		{Role: "user", Content: "after"},
	})
}

func TestBuildOpenAIRequest_AssistantTextAndToolUse(t *testing.T) {
	req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[
		{"role":"assistant","content":[
			{"type":"text","text":"let me check"},
			{"type":"tool_use","id":"tu_1","name":"get_weather","input":{"city":"Tokyo","unit":"c"}},
			{"type":"tool_use","id":"tu_2","name":"lookup","input":{"nested":{"z":1,"a":2},"b":true}}
		]}
	]}`)
	assertMessages(t, req.Messages, []oaiMessage{
		{Role: "assistant", Content: "let me check", ToolCalls: []oaiToolCall{
			{ID: "tu_1", Type: "function", Function: &oaiFunction{
				Name:      "get_weather",
				Arguments: `{"city":"Tokyo","unit":"c"}`,
			}},
			{ID: "tu_2", Type: "function", Function: &oaiFunction{
				Name:      "lookup",
				Arguments: `{"b":true,"nested":{"a":2,"z":1}}`,
			}},
		}},
	})
}

func TestBuildOpenAIRequest_AssistantToolUseUnsortedInput(t *testing.T) {
	// Arguments must be canonical regardless of input key order, so the
	// forwarded bytes stay stable for prompt-prefix caching.
	req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[
		{"role":"assistant","content":[
			{"type":"tool_use","id":"tu_1","name":"f","input":{"z":1,"a":2,"m":3}}
		]}
	]}`)
	want := `{"a":2,"m":3,"z":1}`
	got := req.Messages[0].ToolCalls[0].Function.Arguments
	if got != want {
		t.Errorf("arguments = %q, want %q", got, want)
	}
}

func TestBuildOpenAIRequest_AssistantToolUseMissingInput(t *testing.T) {
	req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[
		{"role":"assistant","content":[
			{"type":"tool_use","id":"tu_1","name":"f"}
		]}
	]}`)
	got := req.Messages[0].ToolCalls[0].Function.Arguments
	if got != "{}" {
		t.Errorf("arguments = %q, want {}", got)
	}
}

func TestBuildOpenAIRequest_AssistantThinking(t *testing.T) {
	req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[
		{"role":"assistant","content":[
			{"type":"thinking","thinking":"step one","signature":"sig"},
			{"type":"redacted_thinking","data":"opaque"},
			{"type":"thinking","thinking":"step two","signature":"sig"},
			{"type":"text","text":"the answer"}
		]}
	]}`)
	assertMessages(t, req.Messages, []oaiMessage{
		{Role: "assistant", Content: "the answer", ReasoningContent: "step one\nstep two"},
	})
}

func TestBuildOpenAIRequest_AssistantRedactedOnly(t *testing.T) {
	req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[
		{"role":"assistant","content":[{"type":"redacted_thinking","data":"opaque"}]}
	]}`)
	if len(req.Messages) != 0 {
		t.Errorf("messages = %v, want none", req.Messages)
	}
}

func TestBuildOpenAIRequest_Tools(t *testing.T) {
	req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[],"tools":[
		{"name":"no_schema","description":"no input_schema, skipped"},
		{"name":"search","description":"Search things","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}
	]}`)
	want := []oaiTool{
		{Type: "function", Function: &oaiToolSpec{
			Name:        "search",
			Description: "Search things",
			Parameters:  jsontext.Value(`{"type":"object","properties":{"q":{"type":"string"}}}`),
		}},
	}
	if !reflect.DeepEqual(req.Tools, want) {
		t.Errorf("tools = %+v, want %+v", req.Tools, want)
	}
}

// TestBuildOpenAIRequest_WebSearchToolSkipped pins the behavior the web_search
// stage relies on: Anthropic's server-side web_search tool definition
// ({"type":"web_search_20250305","name":"web_search","max_uses":5}) carries
// no input_schema, so the tool loop skips it like any other schema-less tool
// and only plain function tools are forwarded.
func TestBuildOpenAIRequest_WebSearchToolSkipped(t *testing.T) {
	t.Run("mixed with a function tool", func(t *testing.T) {
		req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[],"tools":[
			{"type":"web_search_20250305","name":"web_search","max_uses":5},
			{"name":"search","description":"Search things","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}
		]}`)
		want := []oaiTool{
			{Type: "function", Function: &oaiToolSpec{
				Name:        "search",
				Description: "Search things",
				Parameters:  jsontext.Value(`{"type":"object","properties":{"q":{"type":"string"}}}`),
			}},
		}
		if !reflect.DeepEqual(req.Tools, want) {
			t.Errorf("tools = %+v, want %+v", req.Tools, want)
		}
	})

	t.Run("web_search only", func(t *testing.T) {
		req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[],"tools":[
			{"type":"web_search_20250305","name":"web_search","max_uses":5}
		]}`)
		if len(req.Tools) != 0 {
			t.Errorf("tools = %+v, want none", req.Tools)
		}
	})
}

func TestBuildOpenAIRequest_ToolChoice(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want any
	}{
		{"auto string", `"auto"`, "auto"},
		{"any string", `"any"`, "required"},
		{"none string", `"none"`, "none"},
		{"auto object", `{"type":"auto"}`, "auto"},
		{"any object", `{"type":"any"}`, "required"},
		{"tool object", `{"type":"tool","name":"search"}`, oaiToolChoiceObject{
			Type:     "function",
			Function: &oaiToolChoiceFunction{Name: "search"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"model":"claude-x","max_tokens":16,"messages":[],"tool_choice":` + tc.raw + `}`
			req, _ := buildForTest(t, raw)
			if !reflect.DeepEqual(req.ToolChoice, tc.want) {
				t.Errorf("tool_choice = %#v, want %#v", req.ToolChoice, tc.want)
			}
		})
	}
}

func TestBuildOpenAIRequest_ToolChoiceUnknownSkipped(t *testing.T) {
	for _, tc := range []string{`"weird"`, `{"type":"weird"}`, `{"type":"tool"}`, `42`} {
		raw := `{"model":"claude-x","max_tokens":16,"messages":[],"tool_choice":` + tc + `}`
		req, _ := buildForTest(t, raw)
		if req.ToolChoice != nil {
			t.Errorf("tool_choice %s = %#v, want nil", tc, req.ToolChoice)
		}
	}
}

func TestBuildOpenAIRequest_SamplingParams(t *testing.T) {
	req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":100,
		"temperature":0.5,"top_p":0.9,"stop_sequences":["STOP","END"],
		"messages":[]}`)
	if req.MaxTokens != 100 {
		t.Errorf("max_tokens = %d, want 100", req.MaxTokens)
	}
	if req.Temperature == nil || *req.Temperature != 0.5 {
		t.Errorf("temperature = %v, want 0.5", req.Temperature)
	}
	if req.TopP == nil || *req.TopP != 0.9 {
		t.Errorf("top_p = %v, want 0.9", req.TopP)
	}
	if !reflect.DeepEqual(req.Stop, []string{"STOP", "END"}) {
		t.Errorf("stop = %v, want [STOP END]", req.Stop)
	}
}

func TestBuildOpenAIRequest_SamplingParamsAbsent(t *testing.T) {
	req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[]}`)
	if req.Temperature != nil || req.TopP != nil || req.Stop != nil {
		t.Errorf("sampling params = %v/%v/%v, want nil/nil/nil", req.Temperature, req.TopP, req.Stop)
	}
}

func TestBuildOpenAIRequest_Thinking(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		wantEnabled bool
		wantBudget  int
	}{
		{"enabled with budget", `{"type":"enabled","budget_tokens":2048}`, true, 2048},
		{"adaptive", `{"type":"adaptive","budget_tokens":512}`, true, 512},
		{"disabled", `{"type":"disabled","budget_tokens":2048}`, false, 2048},
		{"absent", `null`, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			thinking := ""
			if tc.raw != "null" {
				thinking = `,"thinking":` + tc.raw
			}
			raw := `{"model":"claude-x","max_tokens":16,"messages":[]` + thinking + `}`
			req, _ := buildForTest(t, raw)
			if req.EnableThinking == nil || *req.EnableThinking != tc.wantEnabled {
				t.Errorf("enable_thinking = %v, want %v", req.EnableThinking, tc.wantEnabled)
			}
			if req.ThinkingBudget != tc.wantBudget {
				t.Errorf("thinking_budget = %d, want %d", req.ThinkingBudget, tc.wantBudget)
			}
		})
	}
}

// TestBuildOpenAIRequest_AlwaysThinking asserts the three enable_thinking
// shapes: on with a budget as usual; off omitted for an always-thinking
// model (which rejects the field when false); off sent explicitly for a
// model that accepts the switch.
func TestBuildOpenAIRequest_AlwaysThinking(t *testing.T) {
	always := Hit{BaseModel: "glm-5.3", AlwaysThinking: true}
	switchable := Hit{BaseModel: "deepseek-v4.1-flash"}

	// Thinking on: enable_thinking true, budget forwarded.
	req, _, err := buildOpenAIRequest([]byte(`{"model":"claude-x","max_tokens":16,"messages":[],"thinking":{"type":"enabled","budget_tokens":512}}`), always, RequestInfo{})
	if err != nil {
		t.Fatalf("thinking on: %v", err)
	}
	if req.EnableThinking == nil || !*req.EnableThinking {
		t.Errorf("thinking on: enable_thinking = %v, want true", req.EnableThinking)
	}
	if req.ThinkingBudget != 512 {
		t.Errorf("thinking on: thinking_budget = %d, want 512", req.ThinkingBudget)
	}

	// Thinking off on an always-thinking model: the field is omitted.
	req, _, err = buildOpenAIRequest([]byte(`{"model":"claude-x","max_tokens":16,"messages":[],"thinking":{"type":"disabled","budget_tokens":512}}`), always, RequestInfo{})
	if err != nil {
		t.Fatalf("always off: %v", err)
	}
	if req.EnableThinking != nil {
		t.Errorf("always off: enable_thinking = %v, want omitted", *req.EnableThinking)
	}
	if req.ThinkingBudget != 0 {
		t.Errorf("always off: thinking_budget = %d, want 0", req.ThinkingBudget)
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "enable_thinking") {
		t.Errorf("always off: body carries enable_thinking: %s", body)
	}

	// Thinking off on a switchable model: the field is sent as false.
	req, _, err = buildOpenAIRequest([]byte(`{"model":"claude-x","max_tokens":16,"messages":[]}`), switchable, RequestInfo{})
	if err != nil {
		t.Fatalf("switchable off: %v", err)
	}
	if req.EnableThinking == nil || *req.EnableThinking {
		t.Errorf("switchable off: enable_thinking = %v, want false", req.EnableThinking)
	}
}

// TestBuildOpenAIRequest_MidConversationSystem covers Claude Code's
// mid-conversation role:"system" turns (session hooks): they convert in
// place, block arrays join like the top-level system, the billing block is
// filtered and an empty turn yields no message.
func TestBuildOpenAIRequest_MidConversationSystem(t *testing.T) {
	t.Run("string content keeps its position", func(t *testing.T) {
		req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[
			{"role":"user","content":"first"},
			{"role":"system","content":"hook context"},
			{"role":"user","content":"second"}
		]}`)
		var roles []string
		var contents []string
		for _, m := range req.Messages {
			roles = append(roles, m.Role)
			if s, ok := m.Content.(string); ok {
				contents = append(contents, s)
			}
		}
		wantRoles := []string{"user", "system", "user"}
		if !reflect.DeepEqual(roles, wantRoles) {
			t.Errorf("roles = %v, want %v", roles, wantRoles)
		}
		wantContents := []string{"first", "hook context", "second"}
		if !reflect.DeepEqual(contents, wantContents) {
			t.Errorf("contents = %v, want %v", contents, wantContents)
		}
	})

	t.Run("block array joins text and filters the billing block", func(t *testing.T) {
		req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[
			{"role":"user","content":"first"},
			{"role":"system","content":[
				{"type":"text","text":"x-anthropic-billing-header: cch=abc"},
				{"type":"text","text":"hook context line 1"},
				{"type":"text","text":"hook context line 2"}
			]}
		]}`)
		if len(req.Messages) != 2 {
			t.Fatalf("messages = %d, want 2: %+v", len(req.Messages), req.Messages)
		}
		if req.Messages[1].Role != "system" || req.Messages[1].Content != "hook context line 1\nhook context line 2" {
			t.Errorf("system message = %+v, want joined text without the billing block", req.Messages[1])
		}
	})

	t.Run("empty content is skipped", func(t *testing.T) {
		for _, raw := range []string{
			`{"model":"claude-x","max_tokens":16,"messages":[{"role":"user","content":"first"},{"role":"system","content":""}]}`,
			`{"model":"claude-x","max_tokens":16,"messages":[{"role":"user","content":"first"},{"role":"system","content":[{"type":"text","text":"x-anthropic-billing-header: cch=abc"}]}]}`,
		} {
			req, _ := buildForTest(t, raw)
			if len(req.Messages) != 1 {
				t.Errorf("%s: messages = %d, want 1 (the empty system turn drops): %+v", raw, len(req.Messages), req.Messages)
			}
		}
	})
}

// TestBuildOpenAIRequest_DocumentPlaceholder covers document blocks (PDFs
// and the like) in user messages and tool_result contents: each becomes
// the fixed placeholder text in place, a document-only turn still yields a
// non-empty message, and the replacement count adds up.
func TestBuildOpenAIRequest_DocumentPlaceholder(t *testing.T) {
	t.Run("user text and document interleaved keeps position", func(t *testing.T) {
		req, counts := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[
			{"role":"user","content":[
				{"type":"text","text":"before"},
				{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"aGVsbG8="}},
				{"type":"text","text":"after"}
			]}
		]}`)
		assertMessages(t, req.Messages, []oaiMessage{
			{Role: "user", Content: "before\n" + oaiDocumentPlaceholder + "\nafter"},
		})
		if counts.documents != 1 {
			t.Errorf("documents = %d, want 1", counts.documents)
		}
	})

	t.Run("document in tool_result content joins the text", func(t *testing.T) {
		req, counts := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"t1","content":[
					{"type":"text","text":"result text"},
					{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"aGVsbG8="}}
				]}
			]}
		]}`)
		assertMessages(t, req.Messages, []oaiMessage{
			{Role: "tool", ToolCallID: "t1", Content: "result text\n" + oaiDocumentPlaceholder},
		})
		if counts.documents != 1 {
			t.Errorf("documents = %d, want 1", counts.documents)
		}
	})

	t.Run("document-only user message stays non-empty", func(t *testing.T) {
		req, counts := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[
			{"role":"user","content":[
				{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"aGVsbG8="}}
			]}
		]}`)
		assertMessages(t, req.Messages, []oaiMessage{
			{Role: "user", Content: oaiDocumentPlaceholder},
		})
		if counts.documents != 1 {
			t.Errorf("documents = %d, want 1", counts.documents)
		}
	})

	t.Run("count adds up across messages and forms", func(t *testing.T) {
		_, counts := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[
			{"role":"user","content":[
				{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"aGVsbG8="}},
				{"type":"text","text":"run"},
				{"type":"document","source":{"type":"url","url":"https://example.com/doc.pdf"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"t1","content":[
					{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"aGVsbG8="}}
				]}
			]}
		]}`)
		if counts.documents != 3 {
			t.Errorf("documents = %d, want 3", counts.documents)
		}
	})
}

func TestBuildOpenAIRequest_TextOnlyImagePlaceholder(t *testing.T) {
	req, counts := buildForTestWithHit(t, `{"model":"claude-x","max_tokens":16,"messages":[
		{"role":"user","content":[
			{"type":"text","text":"look at this"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}
		]}
	]}`, Hit{BaseModel: "glm-5.3", TextOnly: true})
	assertMessages(t, req.Messages, []oaiMessage{
		{Role: "user", Content: "look at this\n" + imageOmittedMarker},
	})
	if counts.images != 1 || counts.documents != 0 {
		t.Errorf("counts = %+v, want 1 image and 0 documents", counts)
	}
}

func TestBuildOpenAIRequest_TextOnlyToolResultImageFoldsIntoText(t *testing.T) {
	req, counts := buildForTestWithHit(t, `{"model":"claude-x","max_tokens":16,"messages":[
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"tu_1","content":[
				{"type":"text","text":"screenshot:"},
				{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"QUJD"}}
			]}
		]}
	]}`, Hit{BaseModel: "glm-5.3", TextOnly: true})
	assertMessages(t, req.Messages, []oaiMessage{
		{Role: "tool", ToolCallID: "tu_1", Content: "screenshot:\n" + imageOmittedMarker},
	})
	if counts.images != 1 {
		t.Errorf("counts = %+v, want 1 image", counts)
	}
}

func TestBuildOpenAIRequest_TextOnlyImageOnlyToolResultStaysNonEmpty(t *testing.T) {
	req, counts := buildForTestWithHit(t, `{"model":"claude-x","max_tokens":16,"messages":[
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"tu_1","content":[
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}
			]}
		]}
	]}`, Hit{BaseModel: "glm-5.3", TextOnly: true})
	assertMessages(t, req.Messages, []oaiMessage{
		{Role: "tool", ToolCallID: "tu_1", Content: imageOmittedMarker},
	})
	if counts.images != 1 {
		t.Errorf("counts = %+v, want 1 image", counts)
	}
}

func TestBuildOpenAIRequest_Stream(t *testing.T) {
	req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[],"stream":true}`)
	if !req.Stream {
		t.Errorf("stream = false, want true")
	}
	if req.StreamOptions == nil || !req.StreamOptions.IncludeUsage {
		t.Errorf("stream_options = %+v, want include_usage", req.StreamOptions)
	}

	req, _ = buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[]}`)
	if req.Stream || req.StreamOptions != nil {
		t.Errorf("non-streaming request got stream=%v stream_options=%+v", req.Stream, req.StreamOptions)
	}
}

func TestBuildOpenAIRequest_UsesHitBaseModel(t *testing.T) {
	req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"messages":[]}`)
	if req.Model != "glm-5.3" {
		t.Errorf("model = %q, want glm-5.3 (hit.BaseModel)", req.Model)
	}
}

func TestBuildOpenAIRequest_MarshalForm(t *testing.T) {
	// The marshaled body shows the wire shape end to end: hit model,
	// system first, enable_thinking always present, include_usage on
	// stream.
	req, _ := buildForTest(t, `{"model":"claude-x","max_tokens":16,"stream":true,
		"system":"Be brief.","messages":[{"role":"user","content":"hi"}]}`)
	got, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// thinking_budget is absent: the field is tagged omitzero (json/v2
	// omitempty does not drop zero numbers), so an unset budget is not
	// sent.
	want := `{"model":"glm-5.3","messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"hi"}],"max_tokens":16,"stream":true,"stream_options":{"include_usage":true},"enable_thinking":false}`
	if string(got) != want {
		t.Errorf("marshaled request:\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildOpenAIRequest_InvalidInput(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"not json", `not json`},
		{"messages not array", `{"model":"claude-x","messages":"nope"}`},
		{"unsupported role", `{"model":"claude-x","messages":[{"role":"tool","content":"x"}]}`},
		{"message content number", `{"model":"claude-x","messages":[{"role":"user","content":42}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := buildOpenAIRequest([]byte(tc.raw), Hit{BaseModel: "glm-5.3"}, RequestInfo{}); err == nil {
				t.Errorf("buildOpenAIRequest(%q) succeeded, want error", tc.raw)
			}
		})
	}
}
