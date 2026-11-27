package reqconv

import (
	"slices"
	"testing"

	"github.com/d-kuro/kirocc/internal/anthropic"
)

func inlineAddition(name string) anthropic.ContentBlock {
	return anthropic.ContentBlock{
		Type: anthropic.BlockTypeToolAddition,
		Tool: &anthropic.ToolAddition{
			Type:       anthropic.BlockTypeToolDefinition,
			Definition: &anthropic.Tool{Name: name, Description: "inline " + name},
		},
	}
}

func inlineDefinition(name string) anthropic.ContentBlock {
	return anthropic.ContentBlock{
		Type:       anthropic.BlockTypeToolDefinition,
		Definition: &anthropic.Tool{Name: name, Description: "nested " + name},
	}
}

func inlineToolNames(tools []anthropic.Tool) []string {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	return names
}

func TestCollectInlineToolDefinitions(t *testing.T) {
	t.Run("system additions in wire order, first name wins", func(t *testing.T) {
		msgs := []anthropic.Message{
			{
				Role: "system",
				Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
					{Type: anthropic.BlockTypeText, Text: "tools loaded"},
					inlineAddition("A"),
					inlineAddition("B"),
				}},
			},
			{
				Role: "user",
				Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
					inlineAddition("A"),
					inlineAddition("C"),
				}},
			},
		}
		got := collectInlineToolDefinitions(msgs)
		want := []string{"A", "B", "C"}
		if !slices.Equal(inlineToolNames(got), want) {
			t.Fatalf("names = %v, want %v", inlineToolNames(got), want)
		}
		if got[0].Description != "inline A" {
			t.Fatalf("first occurrence should win, got description %q", got[0].Description)
		}
	})

	t.Run("definitions nested in tool_result content", func(t *testing.T) {
		msgs := []anthropic.Message{
			{
				Role: "user",
				Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
					{
						Type:      anthropic.BlockTypeToolResult,
						ToolUseID: "t1",
						Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
							{Type: anthropic.BlockTypeText, Text: "found"},
							inlineDefinition("Nested"),
						}},
					},
				}},
			},
		}
		got := collectInlineToolDefinitions(msgs)
		if !slices.Equal(inlineToolNames(got), []string{"Nested"}) {
			t.Fatalf("names = %v, want [Nested]", inlineToolNames(got))
		}
	})

	t.Run("ignores non-definitions and unnamed definitions", func(t *testing.T) {
		msgs := []anthropic.Message{
			{
				Role: "user",
				Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
					{Type: anthropic.BlockTypeToolAddition, Tool: &anthropic.ToolAddition{Type: anthropic.BlockTypeToolReference}},
					{Type: anthropic.BlockTypeToolAddition},
					{Type: anthropic.BlockTypeToolAddition, Tool: &anthropic.ToolAddition{Type: anthropic.BlockTypeToolDefinition}},
					{Type: anthropic.BlockTypeToolAddition, Tool: &anthropic.ToolAddition{
						Type:       anthropic.BlockTypeToolDefinition,
						Definition: &anthropic.Tool{Description: "no name"},
					}},
					inlineDefinition(""),
					{Type: anthropic.BlockTypeToolResult, ToolUseID: "t1", Content: anthropic.MessageContent{Text: "plain"}},
				}},
			},
		}
		if got := collectInlineToolDefinitions(msgs); got != nil {
			t.Fatalf("got %v, want nil", inlineToolNames(got))
		}
	})

	t.Run("no hits returns nil", func(t *testing.T) {
		msgs := []anthropic.Message{
			{Role: "user", Content: anthropic.MessageContent{Text: "hello"}},
			{Role: "assistant", Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
				{Type: anthropic.BlockTypeToolUse, ID: "t1", Name: "Bash"},
			}}},
		}
		if got := collectInlineToolDefinitions(msgs); got != nil {
			t.Fatalf("got %v, want nil", inlineToolNames(got))
		}
	})
}

func TestStripInlineToolBlocks(t *testing.T) {
	cacheControl := &anthropic.CacheControl{Type: "ephemeral"}
	newMessages := func() []anthropic.Message {
		return []anthropic.Message{
			{
				Role: "user",
				Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
					{Type: anthropic.BlockTypeText, Text: "hello"},
					inlineAddition("A"),
					{Type: anthropic.BlockTypeToolRemoval},
				}},
			},
			{
				Role:    "system",
				Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{inlineAddition("B")}},
			},
			{
				Role: "user",
				Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
					{
						Type:      anthropic.BlockTypeToolResult,
						ToolUseID: "t1",
						Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
							{
								Type:         anthropic.BlockTypeToolDefinition,
								Definition:   &anthropic.Tool{Name: "C", Description: "nested C"},
								CacheControl: cacheControl,
							},
							{Type: anthropic.BlockTypeText, Text: "ok"},
						}},
					},
				}},
			},
		}
	}

	t.Run("drops additions and removals, rewrites nested definition", func(t *testing.T) {
		msgs := newMessages()
		got := stripInlineToolBlocks(msgs)
		if len(got) != 2 {
			t.Fatalf("got %d messages, want 2: %+v", len(got), got)
		}
		if got[0].Role != "user" || len(got[0].Content.Blocks) != 1 || got[0].Content.Blocks[0].Text != "hello" {
			t.Fatalf("got[0] = %+v, want just the text block", got[0])
		}
		inner := got[1].Content.Blocks[0].Content
		if len(inner.Blocks) != 2 {
			t.Fatalf("nested blocks = %+v, want 2", inner.Blocks)
		}
		ref := inner.Blocks[0]
		if ref.Type != anthropic.BlockTypeToolReference || ref.ToolName != "C" {
			t.Fatalf("nested definition should become tool_reference, got %+v", ref)
		}
		if ref.CacheControl != cacheControl {
			t.Fatalf("cache_control not carried over: %+v", ref.CacheControl)
		}
		if inner.Blocks[1].Type != anthropic.BlockTypeText || inner.Blocks[1].Text != "ok" {
			t.Fatalf("following text changed: %+v", inner.Blocks[1])
		}
	})

	t.Run("does not mutate the input", func(t *testing.T) {
		msgs := newMessages()
		stripInlineToolBlocks(msgs)
		if len(msgs) != 3 {
			t.Fatalf("input message count changed to %d", len(msgs))
		}
		if len(msgs[0].Content.Blocks) != 3 || msgs[0].Content.Blocks[1].Type != anthropic.BlockTypeToolAddition {
			t.Fatalf("input message 0 mutated: %+v", msgs[0].Content.Blocks)
		}
		if len(msgs[1].Content.Blocks) != 1 {
			t.Fatalf("input message 1 mutated: %+v", msgs[1].Content.Blocks)
		}
		if got := msgs[2].Content.Blocks[0].Content.Blocks[0]; got.Type != anthropic.BlockTypeToolDefinition {
			t.Fatalf("nested definition mutated: %+v", got)
		}
	})

	t.Run("emptied message is dropped, originally empty message is kept", func(t *testing.T) {
		msgs := []anthropic.Message{
			{Role: "user", Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{}}},
			{Role: "user", Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{inlineAddition("A")}}},
		}
		got := stripInlineToolBlocks(msgs)
		if len(got) != 1 {
			t.Fatalf("got %d messages, want 1", len(got))
		}
		if got[0].Content.IsString() || len(got[0].Content.Blocks) != 0 {
			t.Fatalf("originally empty message should be kept as an empty array, got %+v", got[0])
		}
	})

	t.Run("nameless nested definitions are dropped", func(t *testing.T) {
		msgs := []anthropic.Message{
			{
				Role: "user",
				Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
					{
						Type:      anthropic.BlockTypeToolResult,
						ToolUseID: "t1",
						Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
							{Type: anthropic.BlockTypeToolDefinition, Definition: &anthropic.Tool{}},
							{Type: anthropic.BlockTypeText, Text: "keep"},
						}},
					},
				}},
			},
		}
		got := stripInlineToolBlocks(msgs)
		inner := got[0].Content.Blocks[0].Content
		if len(inner.Blocks) != 1 || inner.Blocks[0].Text != "keep" {
			t.Fatalf("nested blocks = %+v, want only the text block", inner.Blocks)
		}

		// A result whose only content is a nameless definition keeps an empty
		// array (not string) content.
		msgs = []anthropic.Message{
			{
				Role: "user",
				Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
					{
						Type:      anthropic.BlockTypeToolResult,
						ToolUseID: "t1",
						Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
							{Type: anthropic.BlockTypeToolDefinition},
						}},
					},
				}},
			},
		}
		got = stripInlineToolBlocks(msgs)
		inner = got[0].Content.Blocks[0].Content
		if inner.IsString() {
			t.Fatal("empty result content must stay an array")
		}
		if len(inner.Blocks) != 0 {
			t.Fatalf("nested blocks = %+v, want none", inner.Blocks)
		}
	})

	t.Run("no inline blocks returns the input slice", func(t *testing.T) {
		msgs := []anthropic.Message{
			{Role: "user", Content: anthropic.MessageContent{Text: "hello"}},
			{Role: "assistant", Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
				{Type: anthropic.BlockTypeToolUse, ID: "t1", Name: "Bash"},
			}}},
		}
		got := stripInlineToolBlocks(msgs)
		if &got[0] != &msgs[0] {
			t.Fatal("no-hit path must return the original slice without cloning")
		}
	})
}

func TestInlineToolDefinitionDuplicates(t *testing.T) {
	msgs := []anthropic.Message{
		{
			Role: "user",
			Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
				{
					Type:      anthropic.BlockTypeToolResult,
					ToolUseID: "t1",
					Content:   anthropic.MessageContent{Blocks: []anthropic.ContentBlock{inlineDefinition("Dup")}},
				},
				{
					Type:      anthropic.BlockTypeToolResult,
					ToolUseID: "t2",
					Content:   anthropic.MessageContent{Blocks: []anthropic.ContentBlock{inlineDefinition("Dup")}},
				},
				{
					Type: "tool_addition",
					Tool: &anthropic.ToolAddition{
						Type:       anthropic.BlockTypeToolDefinition,
						Definition: &anthropic.Tool{Name: "Dup"},
					},
				},
			}},
		},
	}
	if defs := collectInlineToolDefinitions(msgs); len(defs) != 1 {
		t.Fatalf("definitions = %d, want 1 (duplicates deduped)", len(defs))
	}

	stripped := stripInlineToolBlocks(msgs)
	blocks := stripped[0].Content.Blocks
	if len(blocks) != 2 {
		t.Fatalf("tool results = %d, want 2 (addition dropped)", len(blocks))
	}
	for i, b := range blocks {
		inner := b.Content.Blocks
		if len(inner) != 1 || inner[0].Type != anthropic.BlockTypeToolReference || inner[0].ToolName != "Dup" {
			t.Fatalf("tool_result %d inner = %+v, want one tool_reference", i, inner)
		}
	}
}
