package customprov

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
)

// oaiRespMessage is the message of a non-streaming response choice as decoded
// here. It stays local because the shared oaiMessage carries only bailian's
// reasoning_content spelling, while some upstreams put reasoning there
// instead; the converter reads both and takes the first non-empty one.
type oaiRespMessage struct {
	Role             string        `json:"role"`
	Content          string        `json:"content,omitempty"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	Reasoning        string        `json:"reasoning,omitempty"`
	ToolCalls        []oaiToolCall `json:"tool_calls,omitempty"`
}

// oaiRespChoice is one choice of a non-streaming response, decoded against
// oaiRespMessage.
type oaiRespChoice struct {
	Index        int             `json:"index"`
	Message      *oaiRespMessage `json:"message,omitempty"`
	FinishReason string          `json:"finish_reason,omitempty"`
}

// oaiRespBody is the part of a non-streaming Chat Completions response the
// converter reads.
type oaiRespBody struct {
	ID      string          `json:"id,omitempty"`
	Model   string          `json:"model,omitempty"`
	Choices []oaiRespChoice `json:"choices,omitempty"`
	Usage   *oaiUsage       `json:"usage,omitempty"`
}

// anthropicTextBlock is a text content block of the converted response.
type anthropicTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// anthropicThinkingBlock is a thinking content block. The openai protocol
// carries no signature, so none is emitted.
type anthropicThinkingBlock struct {
	Type     string `json:"type"`
	Thinking string `json:"thinking"`
}

// anthropicToolUseBlock is a tool_use block converted from a tool call.
type anthropicToolUseBlock struct {
	Type  string         `json:"type"`
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

// anthropicRespUsage is the usage object of the converted response.
// input_tokens and output_tokens are always present; the cache fields appear
// only when non-zero, matching the Anthropic wire format.
type anthropicRespUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// omitempty would not drop a zero int under encoding/json/v2; omitzero does.
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitzero"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitzero"`
}

// anthropicResp is the converted Anthropic Messages response.
type anthropicResp struct {
	ID           string             `json:"id"`
	Type         string             `json:"type"`
	Role         string             `json:"role"`
	Content      []any              `json:"content"`
	Model        string             `json:"model"`
	StopReason   string             `json:"stop_reason"`
	StopSequence *string            `json:"stop_sequence"`
	Usage        anthropicRespUsage `json:"usage"`
}

// openAIToAnthropicResponse converts a non-streaming Chat Completions
// response body to an Anthropic Messages response body. Empty choices yield
// an empty end_turn message rather than an error. The returned usage is the
// provider's, unconverted, for the response log.
func openAIToAnthropicResponse(ctx context.Context, body []byte) ([]byte, *oaiUsage, error) {
	var oai oaiRespBody
	if err := json.Unmarshal(body, &oai); err != nil {
		return nil, nil, fmt.Errorf("invalid openai response body: %w", err)
	}

	resp := anthropicResp{
		ID:         oai.ID,
		Type:       "message",
		Role:       "assistant",
		Content:    []any{},
		Model:      oai.Model,
		StopReason: "end_turn",
	}
	if len(oai.Choices) > 0 {
		choice := oai.Choices[0]
		resp.StopReason = oaiStopReason(choice.FinishReason)
		if m := choice.Message; m != nil {
			// Reasoning leads the content array, as on the Anthropic wire.
			if reasoning := firstNonEmpty(m.ReasoningContent, m.Reasoning); reasoning != "" {
				resp.Content = append(resp.Content, anthropicThinkingBlock{Type: "thinking", Thinking: reasoning})
			}
			if m.Content != "" {
				resp.Content = append(resp.Content, anthropicTextBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				if tc.Function == nil {
					continue
				}
				resp.Content = append(resp.Content, anthropicToolUseBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: decodeToolArguments(ctx, tc.Function.Arguments),
				})
			}
		}
	}
	resp.Usage = anthropicUsageOf(oai.Usage)

	out, err := json.Marshal(resp)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal anthropic response: %w", err)
	}
	return out, oai.Usage, nil
}

// decodeToolArguments parses a tool call's arguments into the input object of
// a tool_use block. Empty or null arguments — the no-argument spellings —
// yield an empty object silently; anything else that fails to parse into an
// object also yields an empty object, with a warning carrying the error.
func decodeToolArguments(ctx context.Context, arguments string) map[string]any {
	if arguments == "" || arguments == "null" {
		return map[string]any{}
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(arguments), &input); err != nil || input == nil {
		if err != nil {
			slog.WarnContext(ctx, "custom provider tool arguments invalid", "err", err)
		}
		return map[string]any{}
	}
	return input
}

// anthropicUsageOf converts the provider's usage to the response's usage
// object; a missing usage decodes to all zeros.
func anthropicUsageOf(u *oaiUsage) anthropicRespUsage {
	n := u.numbers()
	return anthropicRespUsage{
		InputTokens:              n.Input,
		OutputTokens:             n.Output,
		CacheReadInputTokens:     n.CacheRead,
		CacheCreationInputTokens: n.CacheCreation,
	}
}

// firstNonEmpty returns the first argument that is not "".
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// openAIErrorMessage extracts the message of an upstream error body: the
// JSON error.message when present, else the first 512 bytes of the raw body.
func openAIErrorMessage(body []byte) string {
	var errBody struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &errBody); err == nil && errBody.Error != nil && errBody.Error.Message != "" {
		return errBody.Error.Message
	}
	if len(body) > 512 {
		body = body[:512]
	}
	return string(body)
}
