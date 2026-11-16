package customprov

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"strings"
	"testing"
)

// convertOaiResponse runs the converter and decodes its output as a generic
// object, failing the test on any error.
func convertOaiResponse(t *testing.T, body string) (map[string]any, *oaiUsage) {
	t.Helper()
	out, usage, err := openAIToAnthropicResponse(context.Background(), []byte(body))
	if err != nil {
		t.Fatalf("openAIToAnthropicResponse: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	return m, usage
}

// jsonInt fetches a number out of a decoded JSON value.
func jsonInt(t *testing.T, v any) int {
	t.Helper()
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("value %v (%T) is not a number", v, v)
	}
	return int(f)
}

// contentBlocks decodes the response's content array into block objects.
func contentBlocks(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	arr, ok := m["content"].([]any)
	if !ok {
		t.Fatalf("content = %v (%T), want an array", m["content"], m["content"])
	}
	blocks := make([]map[string]any, 0, len(arr))
	for _, b := range arr {
		block, ok := b.(map[string]any)
		if !ok {
			t.Fatalf("content block %v (%T) is not an object", b, b)
		}
		blocks = append(blocks, block)
	}
	return blocks
}

// blockTypes returns the content blocks' type sequence.
func blockTypes(t *testing.T, m map[string]any) []string {
	t.Helper()
	blocks := contentBlocks(t, m)
	types := make([]string, 0, len(blocks))
	for _, b := range blocks {
		types = append(types, b["type"].(string))
	}
	return types
}

// assertUsage checks the response usage object: input and output are always
// present; the cache fields only when non-zero.
func assertUsage(t *testing.T, m map[string]any, wantInput, wantOutput, wantCacheRead, wantCacheCreation int) {
	t.Helper()
	u, ok := m["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage = %v (%T), want an object", m["usage"], m["usage"])
	}
	for _, field := range []string{"input_tokens", "output_tokens"} {
		if _, has := u[field]; !has {
			t.Errorf("usage missing %s: %v", field, u)
		}
	}
	if got := jsonInt(t, u["input_tokens"]); got != wantInput {
		t.Errorf("usage.input_tokens = %d, want %d", got, wantInput)
	}
	if got := jsonInt(t, u["output_tokens"]); got != wantOutput {
		t.Errorf("usage.output_tokens = %d, want %d", got, wantOutput)
	}
	for field, want := range map[string]int{
		"cache_read_input_tokens":     wantCacheRead,
		"cache_creation_input_tokens": wantCacheCreation,
	} {
		if want == 0 {
			if _, has := u[field]; has {
				t.Errorf("usage has %s = %v, want it omitted", field, u[field])
			}
			continue
		}
		if _, has := u[field]; !has {
			t.Errorf("usage missing %s: %v", field, u)
			continue
		}
		if got := jsonInt(t, u[field]); got != want {
			t.Errorf("usage.%s = %d, want %d", field, got, want)
		}
	}
}

// oaiLogRecords parses the captured slog buffer into one record per line.
func oaiLogRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var recs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unparsable log line %q: %v", line, err)
		}
		recs = append(recs, rec)
	}
	return recs
}

func TestOpenAIToAnthropicResponse_TextOnly(t *testing.T) {
	m, usage := convertOaiResponse(t, `{
		"id": "chatcmpl-42",
		"model": "glm-5.3",
		"choices": [{"index": 0, "message": {"role": "assistant", "content": "hello there"}, "finish_reason": "stop"}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 5}
	}`)
	if got := m["id"]; got != "chatcmpl-42" {
		t.Errorf("id = %v, want chatcmpl-42", got)
	}
	if got := m["type"]; got != "message" {
		t.Errorf("type = %v, want message", got)
	}
	if got := m["role"]; got != "assistant" {
		t.Errorf("role = %v, want assistant", got)
	}
	if got := m["model"]; got != "glm-5.3" {
		t.Errorf("model = %v, want glm-5.3", got)
	}
	if got := m["stop_reason"]; got != "end_turn" {
		t.Errorf("stop_reason = %v, want end_turn", got)
	}
	if _, has := m["stop_sequence"]; !has || m["stop_sequence"] != nil {
		t.Errorf("stop_sequence = %v (present=%v), want null", m["stop_sequence"], has)
	}
	blocks := contentBlocks(t, m)
	if len(blocks) != 1 {
		t.Fatalf("content = %v, want one block", blocks)
	}
	if blocks[0]["type"] != "text" || blocks[0]["text"] != "hello there" {
		t.Errorf("text block = %v, want {text: hello there}", blocks[0])
	}
	assertUsage(t, m, 10, 5, 0, 0)
	// The returned usage stays in provider terms for the response log.
	if usage == nil || usage.PromptTokens != 10 || usage.CompletionTokens != 5 {
		t.Errorf("returned usage = %+v, want prompt 10 completion 5", usage)
	}
}

func TestOpenAIToAnthropicResponse_ThinkingLeads(t *testing.T) {
	m, _ := convertOaiResponse(t, `{
		"id": "chatcmpl-1",
		"model": "m",
		"choices": [{"index": 0, "message": {
			"role": "assistant",
			"reasoning_content": "let me think",
			"content": "the answer",
			"tool_calls": [{"id": "call_9", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"Paris\"}"}}]
		}, "finish_reason": "tool_calls"}],
		"usage": {"prompt_tokens": 1, "completion_tokens": 2}
	}`)
	types := blockTypes(t, m)
	want := strings.Join([]string{"thinking", "text", "tool_use"}, ",")
	if got := strings.Join(types, ","); got != want {
		t.Fatalf("content order = %v, want %v", got, want)
	}
	blocks := contentBlocks(t, m)
	thinking := blocks[0]
	if thinking["thinking"] != "let me think" {
		t.Errorf("thinking = %v, want let me think", thinking)
	}
	if _, has := thinking["signature"]; has {
		t.Errorf("thinking block carries a signature: %v", thinking)
	}
	if got := m["stop_reason"]; got != "tool_use" {
		t.Errorf("stop_reason = %v, want tool_use", got)
	}
	toolUse := blocks[2]
	if toolUse["id"] != "call_9" || toolUse["name"] != "get_weather" {
		t.Errorf("tool_use header = %v, want call_9/get_weather", toolUse)
	}
	input, ok := toolUse["input"].(map[string]any)
	if !ok || input["city"] != "Paris" {
		t.Errorf("tool_use input = %v, want {city: Paris}", toolUse["input"])
	}
}

func TestOpenAIToAnthropicResponse_ReasoningFallback(t *testing.T) {
	m, _ := convertOaiResponse(t, `{
		"id": "chatcmpl-2",
		"model": "m",
		"choices": [{"index": 0, "message": {"role": "assistant", "reasoning": "plain reasoning", "content": "answer"}, "finish_reason": "stop"}]
	}`)
	blocks := contentBlocks(t, m)
	if len(blocks) != 2 || blocks[0]["type"] != "thinking" || blocks[0]["thinking"] != "plain reasoning" {
		t.Errorf("content = %v, want a thinking block first with plain reasoning", blocks)
	}
}

func TestOpenAIToAnthropicResponse_ReasoningContentWins(t *testing.T) {
	m, _ := convertOaiResponse(t, `{
		"id": "chatcmpl-3",
		"model": "m",
		"choices": [{"index": 0, "message": {"role": "assistant", "reasoning_content": "primary", "reason": "secondary", "content": "answer"}, "finish_reason": "stop"}]
	}`)
	blocks := contentBlocks(t, m)
	if len(blocks) != 2 || blocks[0]["thinking"] != "primary" {
		t.Errorf("content = %v, want the reasoning_content spelling to win", blocks)
	}
}

func TestOpenAIToAnthropicResponse_NoReasoning(t *testing.T) {
	m, _ := convertOaiResponse(t, `{
		"id": "chatcmpl-4",
		"model": "m",
		"choices": [{"index": 0, "message": {"role": "assistant", "content": "answer"}, "finish_reason": "stop"}]
	}`)
	if types := blockTypes(t, m); strings.Join(types, ",") != "text" {
		t.Errorf("content types = %v, want only text", types)
	}
}

func TestOpenAIToAnthropicResponse_ToolArgumentsInvalid(t *testing.T) {
	buf := captureSlog(t)
	m, _ := convertOaiResponse(t, `{
		"id": "chatcmpl-5",
		"model": "m",
		"choices": [{"index": 0, "message": {"role": "assistant", "content": "calling", "tool_calls": [
			{"id": "call_bad", "type": "function", "function": {"name": "boom", "arguments": "{oops"}}
		]}, "finish_reason": "tool_calls"}]
	}`)
	blocks := contentBlocks(t, m)
	toolUse := blocks[len(blocks)-1]
	input, ok := toolUse["input"].(map[string]any)
	if !ok || len(input) != 0 {
		t.Errorf("tool_use input = %v, want an empty object", toolUse["input"])
	}
	var warned bool
	for _, rec := range oaiLogRecords(t, buf) {
		if rec["severityText"] != "WARN" || rec["body"] != "custom provider tool arguments invalid" {
			continue
		}
		warned = true
		attrs, ok := rec["attributes"].(map[string]any)
		if !ok {
			t.Fatalf("warn record has no attributes: %v", rec)
		}
		if errText, _ := attrs["err"].(string); errText == "" {
			t.Errorf("warn record carries no parse error: %v", rec)
		}
	}
	if !warned {
		t.Errorf("no warning logged for invalid tool arguments, logs:\n%s", buf.String())
	}
}

func TestOpenAIToAnthropicResponse_ToolArgumentsEmptyString(t *testing.T) {
	// The empty spelling is a no-argument call, not a parse failure: no warning.
	buf := captureSlog(t)
	m, _ := convertOaiResponse(t, `{
		"id": "chatcmpl-6",
		"model": "m",
		"choices": [{"index": 0, "message": {"role": "assistant", "tool_calls": [
			{"id": "call_none", "type": "function", "function": {"name": "ping", "arguments": ""}},
			{"id": "call_null", "type": "function", "function": {"name": "ping2", "arguments": "null"}}
		]}, "finish_reason": "tool_calls"}]
	}`)
	for _, block := range contentBlocks(t, m) {
		input, ok := block["input"].(map[string]any)
		if !ok || len(input) != 0 {
			t.Errorf("tool_use input = %v, want an empty object", block["input"])
		}
	}
	for _, rec := range oaiLogRecords(t, buf) {
		if rec["severityText"] == "WARN" {
			t.Errorf("unexpected warning for no-argument tool calls: %v", rec)
		}
	}
}

func TestOpenAIToAnthropicResponse_StopReason(t *testing.T) {
	tests := []struct {
		finishReason string
		want         string
	}{
		{finishReason: "stop", want: "end_turn"},
		{finishReason: "length", want: "max_tokens"},
		{finishReason: "tool_calls", want: "tool_use"},
		{finishReason: "function_call", want: "tool_use"},
		{finishReason: "content_filter", want: "refusal"},
		{finishReason: "", want: "end_turn"},
		{finishReason: "bogus", want: "end_turn"},
	}
	for _, tt := range tests {
		t.Run(tt.finishReason, func(t *testing.T) {
			body := `{"id":"i","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"` + tt.finishReason + `"}]}`
			m, _ := convertOaiResponse(t, body)
			if got := m["stop_reason"]; got != tt.want {
				t.Errorf("stop_reason = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOpenAIToAnthropicResponse_Usage(t *testing.T) {
	tests := []struct {
		name           string
		usage          string
		wantUsageNil   bool
		wantInput      int
		wantOutput     int
		wantCacheRead  int
		wantCacheWrite int
	}{
		{
			name:           "full details",
			usage:          `{"prompt_tokens": 100, "completion_tokens": 7, "prompt_tokens_details": {"cached_tokens": 30, "cache_write_tokens": 10}}`,
			wantInput:      60,
			wantOutput:     7,
			wantCacheRead:  30,
			wantCacheWrite: 10,
		},
		{
			name:          "bailian fallback field",
			usage:         `{"prompt_tokens": 50, "completion_tokens": 5, "prompt_cache_hit_tokens": 20}`,
			wantInput:     30,
			wantOutput:    5,
			wantCacheRead: 20,
		},
		{
			name:          "details win over fallback field",
			usage:         `{"prompt_tokens": 50, "completion_tokens": 5, "prompt_cache_hit_tokens": 20, "prompt_tokens_details": {"cached_tokens": 15}}`,
			wantInput:     35,
			wantOutput:    5,
			wantCacheRead: 15,
		},
		{
			name:          "negative input clamped to zero",
			usage:         `{"prompt_tokens": 5, "completion_tokens": 3, "prompt_tokens_details": {"cached_tokens": 10}}`,
			wantInput:     0,
			wantOutput:    3,
			wantCacheRead: 10,
		},
		{
			name:       "no cache fields at all",
			usage:      `{"prompt_tokens": 12, "completion_tokens": 4}`,
			wantInput:  12,
			wantOutput: 4,
		},
		{
			name:         "usage absent",
			wantUsageNil: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := `{"id":"i","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}]`
			if tt.usage != "" {
				body += `,"usage":` + tt.usage
			}
			body += `}`
			m, usage := convertOaiResponse(t, body)
			assertUsage(t, m, tt.wantInput, tt.wantOutput, tt.wantCacheRead, tt.wantCacheWrite)
			if tt.wantUsageNil && usage != nil {
				t.Errorf("returned usage = %+v, want nil", usage)
			}
		})
	}
}

func TestOpenAIToAnthropicResponse_EmptyChoices(t *testing.T) {
	m, usage := convertOaiResponse(t, `{
		"id": "chatcmpl-empty",
		"model": "empty-model",
		"choices": [],
		"usage": {"prompt_tokens": 3, "completion_tokens": 1}
	}`)
	blocks := contentBlocks(t, m)
	if len(blocks) != 0 {
		t.Errorf("content = %v, want empty", blocks)
	}
	if got := m["stop_reason"]; got != "end_turn" {
		t.Errorf("stop_reason = %v, want end_turn", got)
	}
	if got := m["id"]; got != "chatcmpl-empty" {
		t.Errorf("id = %v, want chatcmpl-empty", got)
	}
	if got := m["model"]; got != "empty-model" {
		t.Errorf("model = %v, want empty-model", got)
	}
	assertUsage(t, m, 3, 1, 0, 0)
	if usage == nil || usage.PromptTokens != 3 {
		t.Errorf("returned usage = %+v, want prompt 3", usage)
	}
}

func TestOpenAIToAnthropicResponse_InvalidBody(t *testing.T) {
	out, usage, err := openAIToAnthropicResponse(context.Background(), []byte("{not json"))
	if err == nil {
		t.Fatal("expected an error for an invalid body")
	}
	if out != nil || usage != nil {
		t.Errorf("out = %s, usage = %v, want nil/nil", out, usage)
	}
}

func TestOpenAIErrorMessage(t *testing.T) {
	t.Run("json error message", func(t *testing.T) {
		body := `{"error":{"message":"quota exceeded","type":"insufficient_quota"}}`
		if got := openAIErrorMessage([]byte(body)); got != "quota exceeded" {
			t.Errorf("openAIErrorMessage = %q, want quota exceeded", got)
		}
	})
	t.Run("json without error message falls back to the body", func(t *testing.T) {
		body := `{"foo":"bar"}`
		if got := openAIErrorMessage([]byte(body)); got != body {
			t.Errorf("openAIErrorMessage = %q, want %q", got, body)
		}
	})
	t.Run("empty error message falls back to the body", func(t *testing.T) {
		body := `{"error":{"message":""}}`
		if got := openAIErrorMessage([]byte(body)); got != body {
			t.Errorf("openAIErrorMessage = %q, want %q", got, body)
		}
	})
	t.Run("non-json body truncated to 512 bytes", func(t *testing.T) {
		body := strings.Repeat("x", 600)
		if got := openAIErrorMessage([]byte(body)); len(got) != 512 {
			t.Errorf("openAIErrorMessage length = %d, want 512", len(got))
		}
	})
	t.Run("short non-json body returned verbatim", func(t *testing.T) {
		if got := openAIErrorMessage([]byte("gateway timeout")); got != "gateway timeout" {
			t.Errorf("openAIErrorMessage = %q, want gateway timeout", got)
		}
	})
	t.Run("empty body", func(t *testing.T) {
		if got := openAIErrorMessage(nil); got != "" {
			t.Errorf("openAIErrorMessage = %q, want empty", got)
		}
	})
}
