package customprov

import (
	"bytes"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"strings"
	"testing"
)

type deferredTestMessage struct {
	Role    string         `json:"role"`
	Content jsontext.Value `json:"content"`
}

type deferredTestTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema jsontext.Value `json:"input_schema"`
}

type deferredTestBody struct {
	Model    string                `json:"model"`
	Messages []deferredTestMessage `json:"messages"`
	Tools    []deferredTestTool    `json:"tools"`
}

type deferredTestBlock struct {
	Type         string         `json:"type"`
	Text         string         `json:"text"`
	ToolUseID    string         `json:"tool_use_id"`
	ToolName     string         `json:"tool_name"`
	CacheControl jsontext.Value `json:"cache_control"`
	Content      jsontext.Value `json:"content"`
}

func rewriteDeferredTest(t *testing.T, raw string) ([]byte, deferredTestBody) {
	t.Helper()
	out, _, err := RewriteForward([]byte(raw), "claude-x", false, mediaPolicy{})
	if err != nil {
		t.Fatalf("RewriteForward: %v", err)
	}
	var body deferredTestBody
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("decode rewritten body: %v\n%s", err, out)
	}
	return out, body
}

func decodeBlocks(t *testing.T, content jsontext.Value) []deferredTestBlock {
	t.Helper()
	var blocks []deferredTestBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		t.Fatalf("decode blocks: %v\n%s", err, content)
	}
	return blocks
}

func TestRewriteForward_FlattensAdditionAndMovesSystemAfterResult(t *testing.T) {
	in := `{"model":"claude-x","max_tokens":16,"messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"probe","input":{}}]},` +
		`{"role":"system","content":[{"type":"text","text":"Reminder: UTC+8"},` +
		`{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"newtool","description":"d","input_schema":{"type":"object","properties":{}}}}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"GREEN-42"}]}],` +
		`"tools":[{"name":"old","description":"o","input_schema":{"type":"object"}}]}`

	out, body := rewriteDeferredTest(t, in)
	if strings.Contains(string(out), "tool_addition") {
		t.Fatalf("tool_addition still present: %s", out)
	}
	if len(body.Messages) != 4 {
		t.Fatalf("messages = %d, want 4: %s", len(body.Messages), out)
	}
	wantRoles := []string{"user", "assistant", "user", "system"}
	for i, want := range wantRoles {
		if body.Messages[i].Role != want {
			t.Fatalf("message %d role = %q, want %q", i, body.Messages[i].Role, want)
		}
	}
	resultBlocks := decodeBlocks(t, body.Messages[2].Content)
	if len(resultBlocks) != 1 || resultBlocks[0].Type != "tool_result" || resultBlocks[0].ToolUseID != "t1" {
		t.Fatalf("tool_result not preserved in place: %+v", resultBlocks)
	}
	systemBlocks := decodeBlocks(t, body.Messages[3].Content)
	if len(systemBlocks) != 1 || systemBlocks[0].Type != "text" || systemBlocks[0].Text != "Reminder: UTC+8" {
		t.Fatalf("system text not preserved: %+v", systemBlocks)
	}
	if len(body.Tools) != 2 || body.Tools[0].Name != "old" || body.Tools[1].Name != "newtool" {
		t.Fatalf("tools = %+v, want [old newtool]", body.Tools)
	}
}

func TestRewriteForward_ReplacesToolResultDefinitionAndCarriesCacheControl(t *testing.T) {
	in := `{"model":"claude-x","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[` +
		`{"type":"text","text":"loaded"},` +
		`{"type":"tool_definition","cache_control":{"type":"ephemeral"},"definition":{"name":"newtool","description":"d","input_schema":{"type":"object"}}},` +
		`{"type":"tool_reference","tool_name":"kept"}]}]}]}`

	out, body := rewriteDeferredTest(t, in)
	if strings.Contains(string(out), "tool_definition") {
		t.Fatalf("tool_definition still present: %s", out)
	}
	if len(body.Tools) != 1 || body.Tools[0].Name != "newtool" {
		t.Fatalf("tools = %+v, want [newtool]", body.Tools)
	}
	blocks := decodeBlocks(t, body.Messages[0].Content)
	if len(blocks) != 1 || blocks[0].Type != "tool_result" {
		t.Fatalf("tool_result missing: %+v", blocks)
	}
	var nested []deferredTestBlock
	if err := json.Unmarshal(blocks[0].Content, &nested); err != nil {
		t.Fatalf("decode nested content: %v", err)
	}
	if len(nested) != 3 {
		t.Fatalf("nested blocks = %d, want 3: %+v", len(nested), nested)
	}
	if nested[0].Type != "text" || nested[0].Text != "loaded" {
		t.Fatalf("text block changed: %+v", nested[0])
	}
	if nested[1].Type != "tool_reference" || nested[1].ToolName != "newtool" {
		t.Fatalf("tool_definition not replaced: %+v", nested[1])
	}
	if string(nested[1].CacheControl) != `{"type":"ephemeral"}` {
		t.Fatalf("cache_control not carried: %s", nested[1].CacheControl)
	}
	if nested[2].Type != "tool_reference" || nested[2].ToolName != "kept" {
		t.Fatalf("existing tool_reference changed: %+v", nested[2])
	}
}

func TestRewriteForward_ExistingDefinitionWins(t *testing.T) {
	in := `{"model":"claude-x","messages":[` +
		`{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"newtool","description":"new","input_schema":{"type":"object"}}}}]},` +
		`{"role":"user","content":"hi"}],` +
		`"tools":[{"name":"newtool","description":"old","input_schema":{"type":"object"}}]}`
	out, body := rewriteDeferredTest(t, in)
	if strings.Contains(string(out), "tool_addition") {
		t.Fatalf("tool_addition still present: %s", out)
	}
	if len(body.Tools) != 1 || body.Tools[0].Description != "old" {
		t.Fatalf("existing definition not preserved: %+v", body.Tools)
	}
	if len(body.Messages) != 1 || body.Messages[0].Role != "user" {
		t.Fatalf("empty system message not dropped: %+v", body.Messages)
	}
}

func TestRewriteForward_ToolRemovalKeepsTool(t *testing.T) {
	in := `{"model":"claude-x","messages":[` +
		`{"role":"system","content":[{"type":"text","text":"removed"},{"type":"tool_removal","tool":{"type":"tool_reference","tool_name":"old"}}]},` +
		`{"role":"user","content":"hi"}],` +
		`"tools":[{"name":"old","description":"o","input_schema":{"type":"object"}}]}`
	out, body := rewriteDeferredTest(t, in)
	if strings.Contains(string(out), "tool_removal") {
		t.Fatalf("tool_removal still present: %s", out)
	}
	if len(body.Tools) != 1 || body.Tools[0].Name != "old" {
		t.Fatalf("tool_removal removed a tool: %+v", body.Tools)
	}
	blocks := decodeBlocks(t, body.Messages[0].Content)
	if len(blocks) != 1 || blocks[0].Text != "removed" {
		t.Fatalf("sibling text lost: %+v", blocks)
	}
}

func TestRewriteForward_AdditionOnlySystemBetweenUsersDropsMessage(t *testing.T) {
	in := `{"model":"claude-x","messages":[` +
		`{"role":"user","content":"a"},` +
		`{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"newtool","description":"d","input_schema":{"type":"object"}}}}]},` +
		`{"role":"user","content":"b"}]}`
	_, body := rewriteDeferredTest(t, in)
	if len(body.Messages) != 2 {
		t.Fatalf("messages = %d, want 2: %+v", len(body.Messages), body.Messages)
	}
	if body.Messages[0].Role != "user" || body.Messages[1].Role != "user" {
		t.Fatalf("consecutive users changed: %+v", body.Messages)
	}
}

func TestRewriteForward_InsertsToolsAtRootEndWhenMissing(t *testing.T) {
	in := `{"model":"claude-x","messages":[{"role":"system","content":[` +
		`{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"newtool","description":"d","input_schema":{"type":"object"}}}}]}]}`
	out, body := rewriteDeferredTest(t, in)
	if len(body.Tools) != 1 || body.Tools[0].Name != "newtool" {
		t.Fatalf("tools = %+v, want [newtool]", body.Tools)
	}
	toolsAt := strings.LastIndex(string(out), `"tools"`)
	messagesAt := strings.LastIndex(string(out), `"messages"`)
	if toolsAt < 0 || messagesAt < 0 || toolsAt < messagesAt {
		t.Fatalf("tools member not appended after messages: %s", out)
	}
}

func TestRewriteForward_InsertedToolsPrecedeInsertedSystem(t *testing.T) {
	in := `{"model":"claude-x","messages":[{"role":"user","content":[` +
		`{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"newtool","description":"d","input_schema":{"type":"object"}}}}]}]}`
	out, _, err := RewriteForward([]byte(in), "claude-x", true, mediaPolicy{})
	if err != nil {
		t.Fatalf("RewriteForward: %v", err)
	}
	toolsAt := strings.Index(string(out), `"tools"`)
	systemAt := strings.Index(string(out), `"system"`)
	if toolsAt < 0 || systemAt < 0 {
		t.Fatalf("tools or system not inserted: %s", out)
	}
	if toolsAt > systemAt {
		t.Fatalf("tools inserted after system: %s", out)
	}
}

func TestRewriteForward_ToolsNullTreatedAsMissing(t *testing.T) {
	in := `{"model":"claude-x","tools":null,"messages":[{"role":"system","content":[` +
		`{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"newtool","description":"d","input_schema":{"type":"object"}}}}]}]}`
	_, body := rewriteDeferredTest(t, in)
	if len(body.Tools) != 1 || body.Tools[0].Name != "newtool" {
		t.Fatalf("tools = %+v, want [newtool]", body.Tools)
	}
}

func TestRewriteForward_NoDeferredBlocksIsByteIdentical(t *testing.T) {
	cases := []string{
		`{"model":"claude-x","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"old","description":"o","input_schema":{"type":"object"}}]}`,
		`{"model":"claude-x","system":[{"type":"text","text":"x-anthropic-billing-header: cc_entrypoint=cli;"}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
		`{"model":"claude-x","messages":[{"role":"user","content":[{"type":"tool_use","id":"t1","name":"x","input":{"a":1}}]},{"role":"assistant","content":"ok"}]}`,
		`{"model":"claude-x","messages":[{"role":"user","content":"hi"},{"role":"system","content":"note"},{"role":"assistant","content":"ok"}]}`,
	}
	for _, in := range cases {
		legacy, _, err := rewrite([]byte(in), "claude-x", true, false, mediaPolicy{}, deferredPolicy{}, deferredPlan{})
		if err != nil {
			t.Fatalf("legacy rewrite: %v", err)
		}
		got, _, err := RewriteForward([]byte(in), "claude-x", false, mediaPolicy{})
		if err != nil {
			t.Fatalf("RewriteForward: %v", err)
		}
		if !bytes.Equal(got, legacy) {
			t.Fatalf("no-block output changed:\n got %s\nwant %s", got, legacy)
		}
	}
}

func TestAnalyzeDeferred_FastPathMatchesFullPath(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantMove bool
	}{
		{
			name: "no_system",
			body: `{"model":"claude-x","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"ok"}]}`,
		},
		{
			name: "system_outside_gap",
			body: `{"model":"claude-x","messages":[{"role":"user","content":"hi"},{"role":"system","content":"note"},{"role":"assistant","content":"ok"}]}`,
		},
		{
			name:     "gap_move",
			body:     `{"model":"claude-x","messages":[{"role":"user","content":"go"},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"probe","input":{}}]},{"role":"system","content":"note"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"GREEN-42"}]}]}`,
			wantMove: true,
		},
		{
			name: "gap_assistant_without_tool_use",
			body: `{"model":"claude-x","messages":[{"role":"assistant","content":"thinking"},{"role":"system","content":"note"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"x"}]}]}`,
		},
		{
			name: "gap_user_not_answering",
			body: `{"model":"claude-x","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"probe","input":{}}]},{"role":"system","content":"note"},{"role":"user","content":"just text"}]}`,
		},
		{
			name:     "gap_partial_intersection",
			body:     `{"model":"claude-x","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"a","input":{}},{"type":"tool_use","id":"t2","name":"b","input":{}}]},{"role":"system","content":"note"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"x"}]}]}`,
			wantMove: true,
		},
		{
			name:     "gap_consecutive_systems",
			body:     `{"model":"claude-x","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"probe","input":{}}]},{"role":"system","content":"one"},{"role":"system","content":"two"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"x"}]}]}`,
			wantMove: true,
		},
		{
			name:     "multiple_gaps",
			body:     `{"model":"claude-x","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"probe","input":{}}]},{"role":"system","content":"one"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"x"}]},{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"probe","input":{}}]},{"role":"system","content":"two"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"y"}]}]}`,
			wantMove: true,
		},
		{
			name:     "gap_then_trailing_system",
			body:     `{"model":"claude-x","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"probe","input":{}}]},{"role":"system","content":"note"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"x"}]},{"role":"system","content":"tail"}]}`,
			wantMove: true,
		},
		{
			name: "system_at_start",
			body: `{"model":"claude-x","messages":[{"role":"system","content":"note"},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"probe","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"x"}]}]}`,
		},
		{
			name: "system_between_users",
			body: `{"model":"claude-x","messages":[{"role":"user","content":"a"},{"role":"system","content":"note"},{"role":"user","content":"b"}]}`,
		},
		{
			name:     "whitespace_body_gap_move",
			body:     "{\n  \"model\": \"claude-x\",\n  \"messages\": [\n    {\"role\": \"assistant\", \"content\": [{\"type\": \"tool_use\", \"id\": \"t1\", \"name\": \"probe\", \"input\": {}}]},\n    {\"role\": \"system\", \"content\": \"note\"},\n    {\"role\": \"user\", \"content\": [{\"type\": \"tool_result\", \"tool_use_id\": \"t1\", \"content\": \"x\"}]}\n  ]\n}",
			wantMove: true,
		},
		{
			name:     "tools_before_messages_gap_move",
			body:     `{"tools":[{"name":"probe","input_schema":{"type":"object"}}],"model":"claude-x","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"probe","input":{}}]},{"role":"system","content":"note"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"x"}]}]}`,
			wantMove: true,
		},
		{
			name:     "tools_null_gap_move",
			body:     `{"model":"claude-x","tools":null,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"probe","input":{}}]},{"role":"system","content":"note"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"x"}]}]}`,
			wantMove: true,
		},
		{
			name: "empty_messages",
			body: `{"model":"claude-x","messages":[]}`,
		},
		{
			name: "assistant_string_no_move",
			body: `{"model":"claude-x","messages":[{"role":"assistant","content":"done"},{"role":"system","content":"note"},{"role":"user","content":"hi"}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(tc.body)
			fastPlan := analyzeDeferred(raw)
			if fastPlan.messagesChanged != tc.wantMove {
				t.Fatalf("fast plan messagesChanged = %v, want %v", fastPlan.messagesChanged, tc.wantMove)
			}
			fast, _, err := RewriteForward(raw, "claude-x", false, mediaPolicy{})
			if err != nil {
				t.Fatalf("fast rewrite: %v", err)
			}
			body, ok := scanDeferredBody(raw, true)
			if !ok {
				t.Fatal("scanDeferredBody failed")
			}
			fullPlan := analyzeDeferredBlocks(raw, body)
			full, _, err := rewrite(raw, "claude-x", true, false, mediaPolicy{}, deferredPolicy{enabled: true}, fullPlan)
			if err != nil {
				t.Fatalf("full rewrite: %v", err)
			}
			if !bytes.Equal(fast, full) {
				t.Fatalf("fast and full output differ:\n fast %s\n full %s", fast, full)
			}
		})
	}
}

func TestRewriteForward_UnknownBlockKept(t *testing.T) {
	in := `{"model":"claude-x","messages":[{"role":"user","content":[` +
		`{"type":"future_block","x":1},` +
		`{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"newtool","description":"d","input_schema":{"type":"object"}}}}]}]}`
	out, _ := rewriteDeferredTest(t, in)
	if !strings.Contains(string(out), `"future_block"`) || !strings.Contains(string(out), `"x":1`) {
		t.Fatalf("unknown block dropped: %s", out)
	}
}

func TestRewriteForward_ConsecutiveSystemsMoveTogether(t *testing.T) {
	in := `{"model":"claude-x","messages":[` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"probe","input":{}}]},` +
		`{"role":"system","content":"one"},` +
		`{"role":"system","content":"two"},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}]}`
	_, body := rewriteDeferredTest(t, in)
	if len(body.Messages) != 4 {
		t.Fatalf("messages = %d, want 4: %+v", len(body.Messages), body.Messages)
	}
	if body.Messages[1].Role != "user" || body.Messages[2].Role != "system" || body.Messages[3].Role != "system" {
		t.Fatalf("system run not moved: %+v", body.Messages)
	}
	if string(body.Messages[2].Content) != `"one"` || string(body.Messages[3].Content) != `"two"` {
		t.Fatalf("system order changed: %s / %s", body.Messages[2].Content, body.Messages[3].Content)
	}
}

func TestRewriteForward_SystemOutsideGapStays(t *testing.T) {
	in := `{"model":"claude-x","messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"system","content":"note"},` +
		`{"role":"assistant","content":"ok"}]}`
	_, body := rewriteDeferredTest(t, in)
	if len(body.Messages) != 3 || body.Messages[1].Role != "system" {
		t.Fatalf("system outside gap moved: %+v", body.Messages)
	}
}

func TestRewriteForward_MissingDefinitionNameDropsBlock(t *testing.T) {
	in := `{"model":"claude-x","messages":[{"role":"system","content":[` +
		`{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"description":"d","input_schema":{"type":"object"}}}}]}]}`
	out, body := rewriteDeferredTest(t, in)
	if strings.Contains(string(out), "tool_addition") {
		t.Fatalf("nameless tool_addition kept: %s", out)
	}
	if len(body.Tools) != 0 {
		t.Fatalf("nameless definition appended: %+v", body.Tools)
	}
	if len(body.Messages) != 0 {
		t.Fatalf("emptied message kept: %+v", body.Messages)
	}
}

func TestRewriteForward_ToolsBeforeMessagesCrossKey(t *testing.T) {
	in := `{"model":"claude-x","tools":[{"name":"old","description":"o","input_schema":{"type":"object"}}],"messages":[` +
		`{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"newtool","description":"d","input_schema":{"type":"object"}}}}]},` +
		`{"role":"user","content":"hi"}]}`
	_, body := rewriteDeferredTest(t, in)
	if len(body.Tools) != 2 || body.Tools[0].Name != "old" || body.Tools[1].Name != "newtool" {
		t.Fatalf("tools = %+v, want [old newtool]", body.Tools)
	}
	if len(body.Messages) != 1 || body.Messages[0].Role != "user" {
		t.Fatalf("messages = %+v, want one user", body.Messages)
	}
}

func TestRewriteForward_DuplicateDefinitionKeepsFirst(t *testing.T) {
	in := `{"model":"claude-x","messages":[` +
		`{"role":"system","content":[` +
		`{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"dup","description":"first","input_schema":{"type":"object"}}}},` +
		`{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"dup","description":"second","input_schema":{"type":"object"}}}}]},` +
		`{"role":"user","content":"hi"}]}`
	_, body := rewriteDeferredTest(t, in)
	if len(body.Tools) != 1 || body.Tools[0].Name != "dup" || body.Tools[0].Description != "first" {
		t.Fatalf("tools = %+v, want the first dup definition", body.Tools)
	}
}

func TestRewriteForward_OldNameOnlyAdditionDropsBlock(t *testing.T) {
	in := `{"model":"claude-x","messages":[` +
		`{"role":"system","content":[{"type":"text","text":"keep"},` +
		`{"type":"tool_addition","tool":{"type":"tool_reference","tool_name":"old"}}]},` +
		`{"role":"user","content":"hi"}],` +
		`"tools":[{"name":"old","description":"o","input_schema":{"type":"object"}}]}`
	out, body := rewriteDeferredTest(t, in)
	if strings.Contains(string(out), "tool_addition") {
		t.Fatalf("name-only tool_addition kept: %s", out)
	}
	if len(body.Tools) != 1 || body.Tools[0].Name != "old" {
		t.Fatalf("tools changed: %+v", body.Tools)
	}
	blocks := decodeBlocks(t, body.Messages[0].Content)
	if len(blocks) != 1 || blocks[0].Text != "keep" {
		t.Fatalf("sibling text lost: %+v", blocks)
	}
}
