package reqconv

import (
	"log/slog"

	"github.com/d-kuro/kirocc/internal/anthropic"
)

// collectInlineToolDefinitions gathers tool definitions carried inside
// mid-conversation blocks rather than the request's top-level tool list:
// tool_addition blocks whose payload is a tool_definition, and tool_definition
// blocks nested in tool_result content. Every role is scanned, definitions are
// returned in wire order, and a name seen earlier wins over a later duplicate.
// Returns nil when no usable definition is found.
func collectInlineToolDefinitions(msgs []anthropic.Message) []anthropic.Tool {
	var (
		defs []anthropic.Tool
		seen map[string]struct{}
	)
	add := func(t *anthropic.Tool) {
		if t == nil || t.Name == "" {
			return
		}
		if seen == nil {
			seen = make(map[string]struct{})
		}
		if _, ok := seen[t.Name]; ok {
			return
		}
		seen[t.Name] = struct{}{}
		defs = append(defs, *t)
	}

	for i := range msgs {
		if msgs[i].Content.IsString() {
			continue
		}
		blocks := msgs[i].Content.Blocks
		for j := range blocks {
			switch blocks[j].Type {
			case anthropic.BlockTypeToolAddition:
				if blocks[j].Tool == nil || blocks[j].Tool.Type != anthropic.BlockTypeToolDefinition {
					continue
				}
				add(blocks[j].Tool.Definition)
			case anthropic.BlockTypeToolResult:
				if blocks[j].Content.IsString() {
					continue
				}
				for k := range blocks[j].Content.Blocks {
					inner := &blocks[j].Content.Blocks[k]
					if inner.Type == anthropic.BlockTypeToolDefinition {
						add(inner.Definition)
					}
				}
			}
		}
	}
	return defs
}

// stripInlineToolBlocks removes mid-conversation tool blocks so the Kiro path
// never renders them as placeholder text:
//
//   - tool_addition and tool_removal blocks are dropped in any role;
//   - tool_definition blocks nested in tool_result content are replaced in
//     place with an equivalent tool_reference, keeping the block's
//     cache_control; a definition without a name is dropped;
//   - a message whose content array is emptied by these deletions is dropped
//     as a whole, while a message that was already empty is kept.
//
// The input is never mutated. When no such block exists the original slice is
// returned without allocating.
func stripInlineToolBlocks(msgs []anthropic.Message) []anthropic.Message {
	var out []anthropic.Message
	for i := range msgs {
		msg := msgs[i]
		if msg.Content.IsString() {
			if out != nil {
				out = append(out, msg)
			}
			continue
		}
		blocks, changed := stripInlineFromBlocks(msg.Content.Blocks)
		if !changed {
			if out != nil {
				out = append(out, msg)
			}
			continue
		}
		if out == nil {
			out = make([]anthropic.Message, 0, len(msgs))
			out = append(out, msgs[:i]...)
		}
		if len(blocks) == 0 {
			continue
		}
		out = append(out, anthropic.Message{
			Role:    msg.Role,
			Content: anthropic.MessageContent{Blocks: blocks},
		})
	}
	if out == nil {
		return msgs
	}
	return out
}

// stripInlineFromBlocks removes the inline tool blocks from one message's
// content. Returns the original slice and false when nothing was removed.
func stripInlineFromBlocks(blocks []anthropic.ContentBlock) ([]anthropic.ContentBlock, bool) {
	var out []anthropic.ContentBlock
	changed := false
	for j := range blocks {
		b := blocks[j]
		switch b.Type {
		case anthropic.BlockTypeToolAddition:
			if b.Tool != nil && b.Tool.Type == anthropic.BlockTypeToolDefinition &&
				(b.Tool.Definition == nil || b.Tool.Definition.Name == "") {
				slog.Warn("dropping tool_addition with an unnamed tool definition")
			}
			changed = true
			if out == nil {
				out = make([]anthropic.ContentBlock, 0, len(blocks))
				out = append(out, blocks[:j]...)
			}
			continue
		case anthropic.BlockTypeToolRemoval:
			changed = true
			if out == nil {
				out = make([]anthropic.ContentBlock, 0, len(blocks))
				out = append(out, blocks[:j]...)
			}
			continue
		case anthropic.BlockTypeToolResult:
			if inner, innerChanged := stripInlineFromToolResult(b); innerChanged {
				changed = true
				if out == nil {
					out = make([]anthropic.ContentBlock, 0, len(blocks))
					out = append(out, blocks[:j]...)
				}
				out = append(out, inner)
				continue
			}
		}
		if out != nil {
			out = append(out, b)
		}
	}
	if !changed {
		return blocks, false
	}
	return out, true
}

// stripInlineFromToolResult replaces tool_definition blocks nested in a
// tool_result's content with tool_reference blocks. Returns the original block
// and false when it has no nested definition.
func stripInlineFromToolResult(b anthropic.ContentBlock) (anthropic.ContentBlock, bool) {
	if b.Content.IsString() {
		return b, false
	}
	var out []anthropic.ContentBlock
	changed := false
	for k := range b.Content.Blocks {
		inner := b.Content.Blocks[k]
		if inner.Type != anthropic.BlockTypeToolDefinition {
			if out != nil {
				out = append(out, inner)
			}
			continue
		}
		changed = true
		name := ""
		if inner.Definition != nil {
			name = inner.Definition.Name
		}
		if out == nil {
			out = make([]anthropic.ContentBlock, 0, len(b.Content.Blocks))
			out = append(out, b.Content.Blocks[:k]...)
		}
		if name == "" {
			slog.Warn("dropping an unnamed tool_definition inside tool_result")
			continue
		}
		out = append(out, anthropic.ContentBlock{
			Type:         anthropic.BlockTypeToolReference,
			ToolName:     name,
			CacheControl: inner.CacheControl,
		})
	}
	if !changed {
		return b, false
	}
	if out == nil {
		// Every nested block was an unnamed definition: keep the content an
		// (empty) array rather than collapsing it to string form.
		out = make([]anthropic.ContentBlock, 0)
	}
	b.Content = anthropic.MessageContent{Blocks: out}
	return b, true
}
