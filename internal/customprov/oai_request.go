package customprov

import (
	"bytes"
	"fmt"
	"strings"

	"encoding/json/jsontext"
	json "encoding/json/v2"

	"github.com/d-kuro/kirocc/internal/anthropic"
	"github.com/d-kuro/kirocc/internal/reqconv"
)

// buildOpenAIRequest converts an Anthropic Messages request body to an
// OpenAI Chat Completions request. raw is the forwarded request bytes, hit
// the resolved provider/model, info the peeked request summary. The second
// return value counts the image/document blocks replaced by placeholders,
// for the caller's logs.
//
// Only the fields the conversion needs are decoded; everything else in raw
// is ignored. The output is byte-stable for byte-identical input: tool_use
// arguments are canonicalized (sorted keys, no whitespace) so prompt-prefix
// caching upstream keeps hitting, and message order follows block order.
func buildOpenAIRequest(raw []byte, hit Hit, info RequestInfo) (*oaiRequest, mediaCounts, error) {
	var req anthRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, mediaCounts{}, fmt.Errorf("decode anthropic request: %w", err)
	}

	// Deferred-tool blocks (Claude Code mid-conversation tool additions and
	// ToolSearch definitions) carry tool definitions the Chat protocol has no
	// channel for; the blocks themselves are dropped by the per-role
	// conversion, so their definitions are collected here and appended to the
	// converted tools below. The byte gate keeps requests without such a
	// block from paying for the contents slice and the scan.
	var deferredRefs []deferredToolRef
	if bytes.Contains(raw, []byte(anthropic.BlockTypeToolDefinition)) {
		contents := make([]jsontext.Value, len(req.Messages))
		for i := range req.Messages {
			contents[i] = req.Messages[i].Content
		}
		var err error
		deferredRefs, err = collectDeferredToolRefs(contents)
		if err != nil {
			return nil, mediaCounts{}, fmt.Errorf("collect deferred tools: %w", err)
		}
	}

	out := &oaiRequest{
		Model:       hit.BaseModel,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.StopSequences,
	}

	if s := reqconv.ExtractSystemPrompt(req.System); s != "" {
		out.Messages = append(out.Messages, oaiMessage{Role: "system", Content: s})
	}

	var counts mediaCounts
	for i := range req.Messages {
		msgs, err := oaiConvertMessage(&req.Messages[i], &counts, hit.TextOnly)
		if err != nil {
			return nil, mediaCounts{}, fmt.Errorf("message %d: %w", i, err)
		}
		out.Messages = append(out.Messages, msgs...)
	}
	out.Messages = oaiRelocateSystemAfterToolResults(out.Messages)

	for i := range req.Tools {
		tool, ok, err := oaiConvertTool(&req.Tools[i])
		if err != nil {
			return nil, mediaCounts{}, fmt.Errorf("tool %d: %w", i, err)
		}
		if ok {
			out.Tools = append(out.Tools, tool)
		}
	}
	mergedTools, err := oaiMergeDeferredTools(req.Tools, out.Tools, deferredRefs)
	if err != nil {
		return nil, mediaCounts{}, err
	}
	out.Tools = mergedTools

	choice, ok, err := oaiConvertToolChoice(req.ToolChoice)
	if err != nil {
		return nil, mediaCounts{}, err
	}
	if ok {
		out.ToolChoice = choice
	}

	if req.Stream {
		out.Stream = true
		out.StreamOptions = &oaiStreamOptions{IncludeUsage: true}
	}

	// enable_thinking: true when reasoning is requested; false for models
	// that accept the switch; omitted for always-thinking models (hit),
	// which reject the field outright when it is false — together with the
	// budget.
	thinkingOn := req.Thinking != nil &&
		(req.Thinking.Type == anthropic.ThinkingTypeEnabled || req.Thinking.Type == anthropic.ThinkingTypeAdaptive)
	if thinkingOn || !hit.AlwaysThinking {
		out.EnableThinking = &thinkingOn
	}
	if req.Thinking != nil && req.Thinking.BudgetTokens > 0 && (thinkingOn || !hit.AlwaysThinking) {
		out.ThinkingBudget = req.Thinking.BudgetTokens
	}

	return out, counts, nil
}

// anthRequest is the subset of an Anthropic Messages request that the
// OpenAI conversion reads. Union-typed members that the Kiro types already
// model (system, thinking) decode into those; the ones Kiro never reads
// (tool_choice, tool input) are kept as raw JSON and decoded per case.
type anthRequest struct {
	Model         string                    `json:"model"`
	Messages      []anthMessage             `json:"messages"`
	System        anthropic.SystemPrompt    `json:"system,omitzero"`
	Tools         []anthTool                `json:"tools,omitempty"`
	ToolChoice    jsontext.Value            `json:"tool_choice,omitempty"`
	MaxTokens     int                       `json:"max_tokens,omitempty"`
	Temperature   *float64                  `json:"temperature,omitempty"`
	TopP          *float64                  `json:"top_p,omitempty"`
	StopSequences []string                  `json:"stop_sequences,omitempty"`
	Stream        bool                      `json:"stream,omitempty"`
	Thinking      *anthropic.ThinkingConfig `json:"thinking,omitempty"`
}

// anthMessage is one Messages request entry; content is string or block
// array, kept raw until the per-role conversion.
type anthMessage struct {
	Role    string         `json:"role"`
	Content jsontext.Value `json:"content,omitempty"`
}

// anthTool is one request tools entry.
type anthTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema jsontext.Value `json:"input_schema,omitempty"`
}

// anthBlock is one content block across the user/assistant shapes: text,
// image, tool_use, tool_result, thinking and redacted_thinking. Only the
// members relevant to the conversion are declared; the rest is ignored.
type anthBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// image block.
	Source *anthSource `json:"source,omitempty"`
	// tool_result block.
	ToolUseID string         `json:"tool_use_id,omitempty"`
	Content   jsontext.Value `json:"content,omitempty"` // string or block array
	IsError   bool           `json:"is_error,omitempty"`
	// tool_use block.
	ID    string         `json:"id,omitempty"`
	Name  string         `json:"name,omitempty"`
	Input jsontext.Value `json:"input,omitempty"` // object
	// thinking block (redacted_thinking carries no readable text).
	Thinking string `json:"thinking,omitempty"`
}

// anthSource is an image block's source.
type anthSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// oaiToolChoiceObject is the object form of the Chat Completions
// tool_choice field, used when the Anthropic request pins a specific tool.
type oaiToolChoiceObject struct {
	Type     string                 `json:"type"`
	Function *oaiToolChoiceFunction `json:"function"`
}

// oaiToolChoiceFunction names the pinned tool.
type oaiToolChoiceFunction struct {
	Name string `json:"name"`
}

// oaiConvertMessage converts one Anthropic message to zero or more Chat
// Completions messages; tool_result-heavy user turns expand into several.
// counts accumulates the media blocks replaced with placeholders. textOnly
// turns image blocks into text markers instead of image_url parts.
func oaiConvertMessage(m *anthMessage, counts *mediaCounts, textOnly bool) ([]oaiMessage, error) {
	if len(m.Content) == 0 || m.Content.Kind() == jsontext.KindNull {
		return nil, nil
	}
	switch m.Role {
	case "user", "assistant":
	case "system":
		// Claude Code injects mid-conversation system turns (session-start
		// hooks and the like). Chat Completions allows a system message in
		// any position, so it converts in place with the same block rules
		// as the top-level system (string as-is, text blocks joined,
		// billing block filtered); an empty turn produces no message.
		var sp anthropic.SystemPrompt
		if err := json.Unmarshal(m.Content, &sp); err != nil {
			return nil, fmt.Errorf("system message: %w", err)
		}
		if s := reqconv.ExtractSystemPrompt(sp); s != "" {
			return []oaiMessage{{Role: "system", Content: s}}, nil
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported role %q", m.Role)
	}
	if m.Content.Kind() == jsontext.KindString {
		var s string
		if err := json.Unmarshal(m.Content, &s); err != nil {
			return nil, fmt.Errorf("decode content: %w", err)
		}
		return []oaiMessage{{Role: m.Role, Content: s}}, nil
	}
	if m.Content.Kind() != jsontext.KindBeginArray {
		return nil, fmt.Errorf("content: unsupported JSON kind %s", m.Content.Kind())
	}
	var blocks []anthBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil, fmt.Errorf("decode content blocks: %w", err)
	}
	if m.Role == "user" {
		return oaiConvertUserBlocks(blocks, counts, textOnly)
	}
	return oaiConvertAssistantBlocks(blocks)
}

// oaiRelocateSystemAfterToolResults moves a run of system messages that sits
// between an assistant's tool_calls and the tool messages answering them to
// just after the last matching tool message (rule 7). Chat Completions pairs
// tool messages with the preceding assistant turn, and a system message in
// that gap makes upstream models misread the tool results. A run not
// followed by a matching tool message stays where it is, as does every
// system message outside such a gap. Synthetic user image messages
// (flushToolImages) stay after the moved run.
func oaiRelocateSystemAfterToolResults(msgs []oaiMessage) []oaiMessage {
	var held map[int]struct{}
	var insertAfter map[int][]oaiMessage
	for i := range msgs {
		msg := &msgs[i]
		if msg.Role != "assistant" || len(msg.ToolCalls) == 0 {
			continue
		}
		runEnd := i + 1
		for runEnd < len(msgs) && msgs[runEnd].Role == "system" {
			runEnd++
		}
		if runEnd == i+1 {
			continue
		}
		ids := make(map[string]struct{}, len(msg.ToolCalls))
		for _, call := range msg.ToolCalls {
			if call.ID != "" {
				ids[call.ID] = struct{}{}
			}
		}
		if len(ids) == 0 {
			continue
		}
		last := -1
		for j := runEnd; j < len(msgs) && msgs[j].Role == "tool"; j++ {
			if _, ok := ids[msgs[j].ToolCallID]; ok {
				last = j
			}
		}
		if last < 0 {
			continue
		}
		if held == nil {
			held = make(map[int]struct{})
			insertAfter = make(map[int][]oaiMessage)
		}
		for j := i + 1; j < runEnd; j++ {
			held[j] = struct{}{}
			insertAfter[last] = append(insertAfter[last], msgs[j])
		}
	}
	if len(held) == 0 {
		return msgs
	}
	out := make([]oaiMessage, 0, len(msgs))
	for i, msg := range msgs {
		if _, ok := held[i]; ok {
			continue
		}
		out = append(out, msg)
		if moved, ok := insertAfter[i]; ok {
			out = append(out, moved...)
		}
	}
	return out
}

// oaiDocumentPlaceholder replaces a document block (PDF and the like) in
// the conversion: the Chat Completions request kirocc builds carries no
// documents, so the model at least sees that one existed, in place, and a
// document-only turn stays non-empty.
const oaiDocumentPlaceholder = documentOmittedMarker

// oaiConvertUserBlocks converts a user message's content blocks, preserving
// block order: ordinary text/image blocks accumulate into user messages,
// document blocks become placeholder text (see oaiDocumentPlaceholder),
// tool_result blocks become tool messages, and images inside tool results
// (which Chat tool messages cannot carry) move into a synthesized user
// message right after the run of tool messages they came from. With textOnly,
// image blocks become text markers and tool-result images fold into the tool
// text instead, so no synthetic user message is produced.
func oaiConvertUserBlocks(blocks []anthBlock, counts *mediaCounts, textOnly bool) ([]oaiMessage, error) {
	var out []oaiMessage
	var parts []oaiContentPart // pending ordinary text/image parts, in block order
	var toolImages []oaiContentPart

	flushParts := func() {
		if len(parts) == 0 {
			return
		}
		msg := oaiMessage{Role: "user"}
		if !oaiHasImagePart(parts) {
			var texts []string
			for _, p := range parts {
				texts = append(texts, p.Text)
			}
			msg.Content = strings.Join(texts, "\n")
		} else {
			msg.Content = parts
		}
		out = append(out, msg)
		parts = nil
	}
	flushToolImages := func() {
		if len(toolImages) == 0 {
			return
		}
		out = append(out, oaiMessage{Role: "user", Content: toolImages})
		toolImages = nil
	}

	for _, b := range blocks {
		switch b.Type {
		case "text":
			flushToolImages()
			if b.Text != "" {
				parts = append(parts, oaiContentPart{Type: "text", Text: b.Text})
			}
		case "image":
			flushToolImages()
			if textOnly {
				counts.add(mediaCounts{images: 1})
				parts = append(parts, oaiContentPart{Type: "text", Text: imageOmittedMarker})
			} else if p, ok := oaiImagePart(b.Source); ok {
				parts = append(parts, p)
			}
		case "document":
			// Replaced, not skipped: silently vanishing content is worse
			// than a visible marker, and the request stays valid.
			flushToolImages()
			counts.add(mediaCounts{documents: 1})
			parts = append(parts, oaiContentPart{Type: "text", Text: oaiDocumentPlaceholder})
		case "tool_result":
			flushParts()
			text, images, err := oaiToolResultContent(b, counts, textOnly)
			if err != nil {
				return nil, fmt.Errorf("tool_result %s: %w", b.ToolUseID, err)
			}
			if b.IsError {
				text = "Error: " + text
			}
			out = append(out, oaiMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: text})
			toolImages = append(toolImages, images...)
		}
	}
	flushParts()
	flushToolImages()
	return out, nil
}

// oaiHasImagePart reports whether parts carries at least one image.
func oaiHasImagePart(parts []oaiContentPart) bool {
	for _, p := range parts {
		if p.Type == "image_url" {
			return true
		}
	}
	return false
}

// oaiToolResultContent splits a tool_result's content into its text (string
// form used as-is, block form joined from text blocks; document blocks
// become the placeholder) and image parts. With textOnly, images become text
// markers in the returned text instead of image parts.
func oaiToolResultContent(b anthBlock, counts *mediaCounts, textOnly bool) (string, []oaiContentPart, error) {
	if len(b.Content) == 0 || b.Content.Kind() == jsontext.KindNull {
		return "", nil, nil
	}
	switch b.Content.Kind() {
	case jsontext.KindString:
		var s string
		if err := json.Unmarshal(b.Content, &s); err != nil {
			return "", nil, fmt.Errorf("decode content: %w", err)
		}
		return s, nil, nil
	case jsontext.KindBeginArray:
		var blocks []anthBlock
		if err := json.Unmarshal(b.Content, &blocks); err != nil {
			return "", nil, fmt.Errorf("decode content blocks: %w", err)
		}
		var texts []string
		var images []oaiContentPart
		for _, c := range blocks {
			switch c.Type {
			case "text":
				texts = append(texts, c.Text)
			case "image":
				if textOnly {
					counts.add(mediaCounts{images: 1})
					texts = append(texts, imageOmittedMarker)
				} else if p, ok := oaiImagePart(c.Source); ok {
					images = append(images, p)
				}
			case "document":
				counts.add(mediaCounts{documents: 1})
				texts = append(texts, oaiDocumentPlaceholder)
			}
		}
		return strings.Join(texts, "\n"), images, nil
	default:
		return "", nil, fmt.Errorf("content: unsupported JSON kind %s", b.Content.Kind())
	}
}

// oaiImagePart converts an Anthropic image source to an image_url part.
// Base64 sources become data URLs; url sources pass the URL through.
func oaiImagePart(src *anthSource) (oaiContentPart, bool) {
	if src == nil {
		return oaiContentPart{}, false
	}
	var url string
	switch src.Type {
	case "base64":
		url = "data:" + src.MediaType + ";base64," + src.Data
	case "url":
		url = src.URL
	default:
		return oaiContentPart{}, false
	}
	if url == "" {
		return oaiContentPart{}, false
	}
	return oaiContentPart{Type: "image_url", ImageURL: &oaiImageURL{URL: url}}, true
}

// oaiConvertAssistantBlocks converts an assistant message's content blocks
// into a single assistant message: text blocks merge into the content
// string, tool_use blocks become tool_calls, and thinking blocks feed
// reasoning_content. redacted_thinking carries no readable text and is
// skipped. A block-less message (redacted thinking only) yields nothing.
func oaiConvertAssistantBlocks(blocks []anthBlock) ([]oaiMessage, error) {
	var texts, thinking []string
	var calls []oaiToolCall
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				texts = append(texts, b.Text)
			}
		case "thinking":
			if b.Thinking != "" {
				thinking = append(thinking, b.Thinking)
			}
		case "tool_use":
			args, err := oaiCanonicalArguments(b.Input)
			if err != nil {
				return nil, fmt.Errorf("tool_use %s: %w", b.ID, err)
			}
			calls = append(calls, oaiToolCall{
				ID:       b.ID,
				Type:     "function",
				Function: &oaiFunction{Name: b.Name, Arguments: args},
			})
		}
	}
	if len(texts) == 0 && len(calls) == 0 && len(thinking) == 0 {
		return nil, nil
	}
	msg := oaiMessage{Role: "assistant", Content: strings.Join(texts, "\n")}
	if len(thinking) > 0 {
		msg.ReasoningContent = strings.Join(thinking, "\n")
	}
	if len(calls) > 0 {
		msg.ToolCalls = calls
	}
	return []oaiMessage{msg}, nil
}

// oaiCanonicalArguments renders a tool_use input object as canonical JSON:
// object keys sorted, no insignificant whitespace. Byte-stable arguments
// keep the request prefix stable for upstream prompt caching.
func oaiCanonicalArguments(input jsontext.Value) (string, error) {
	if len(input) == 0 || input.Kind() == jsontext.KindNull {
		return "{}", nil
	}
	v := input
	if err := v.Canonicalize(); err != nil {
		return "", fmt.Errorf("canonicalize input: %w", err)
	}
	return string(v), nil
}

// oaiConvertTool converts one Anthropic tool to a Chat Completions function
// tool, resolving local $ref references in its input schema. Tools without
// an input schema are reported as skipped.
func oaiConvertTool(t *anthTool) (oaiTool, bool, error) {
	if len(t.InputSchema) == 0 || t.InputSchema.Kind() == jsontext.KindNull {
		return oaiTool{}, false, nil
	}
	params, err := oaiInlineRefs(t.InputSchema)
	if err != nil {
		return oaiTool{}, false, fmt.Errorf("inline schema: %w", err)
	}
	return oaiTool{
		Type: "function",
		Function: &oaiToolSpec{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  jsontext.Value(params),
		},
	}, true, nil
}

// oaiMergeDeferredTools appends the tool definitions collected from deferred
// tool blocks to the converted tools (rules 1 and 2). A name already present
// in the original request wins: existing tools stay first and are never
// overridden, and new tools follow converted order, then ref order, so the
// emitted tools stay byte-stable across requests. Definitions without an
// input schema are skipped like their top-level counterparts.
func oaiMergeDeferredTools(existing []anthTool, converted []oaiTool, refs []deferredToolRef) ([]oaiTool, error) {
	if len(refs) == 0 {
		return converted, nil
	}
	seen := make(map[string]struct{}, len(existing)+len(refs))
	for i := range existing {
		if existing[i].Name != "" {
			seen[existing[i].Name] = struct{}{}
		}
	}
	for _, ref := range refs {
		if ref.Name == "" {
			continue
		}
		if _, ok := seen[ref.Name]; ok {
			continue
		}
		var tool anthTool
		if err := json.Unmarshal(ref.Definition, &tool); err != nil {
			return nil, fmt.Errorf("tool %q from deferred blocks: %w", ref.Name, err)
		}
		convertedTool, ok, err := oaiConvertTool(&tool)
		if err != nil {
			return nil, fmt.Errorf("tool %q from deferred blocks: %w", ref.Name, err)
		}
		if !ok {
			continue
		}
		seen[ref.Name] = struct{}{}
		converted = append(converted, convertedTool)
	}
	return converted, nil
}

// oaiConvertToolChoice maps the Anthropic tool_choice to the Chat
// Completions form: auto/any/none map to the strings auto/required/none
// (both Anthropic spellings, bare string and {"type": ...} object), and
// {"type":"tool","name":X} pins the function. Unknown shapes are skipped.
func oaiConvertToolChoice(tc jsontext.Value) (any, bool, error) {
	if len(tc) == 0 || tc.Kind() == jsontext.KindNull {
		return nil, false, nil
	}
	var form struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	switch tc.Kind() {
	case jsontext.KindString:
		if err := json.Unmarshal(tc, &form.Type); err != nil {
			return nil, false, fmt.Errorf("decode tool_choice: %w", err)
		}
	case jsontext.KindBeginObject:
		if err := json.Unmarshal(tc, &form); err != nil {
			return nil, false, fmt.Errorf("decode tool_choice: %w", err)
		}
	default:
		return nil, false, nil
	}
	switch form.Type {
	case "auto":
		return "auto", true, nil
	case "any":
		return "required", true, nil
	case "none":
		return "none", true, nil
	case "tool":
		if form.Name == "" {
			return nil, false, nil
		}
		return oaiToolChoiceObject{
			Type:     "function",
			Function: &oaiToolChoiceFunction{Name: form.Name},
		}, true, nil
	default:
		return nil, false, nil
	}
}
