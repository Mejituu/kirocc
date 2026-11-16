package customprov

import (
	"encoding/json/jsontext"

	"github.com/d-kuro/kirocc/internal/httpx"
)

// Protocol selects the wire protocol a custom provider speaks.
type Protocol string

const (
	// ProtocolAnthropic forwards requests verbatim as Anthropic Messages.
	ProtocolAnthropic Protocol = "anthropic"
	// ProtocolOpenAI converts Anthropic Messages to OpenAI Chat
	// Completions and the response back.
	ProtocolOpenAI Protocol = "openai"
)

// oaiRequest is an OpenAI Chat Completions request body, as built by
// buildOpenAIRequest.
type oaiRequest struct {
	Model          string            `json:"model"`
	Messages       []oaiMessage      `json:"messages"`
	Tools          []oaiTool         `json:"tools,omitempty"`
	ToolChoice     any               `json:"tool_choice,omitempty"` // string or {"type":"function","function":{...}}
	MaxTokens      int               `json:"max_tokens,omitzero"`
	Temperature    *float64          `json:"temperature,omitempty"`
	TopP           *float64          `json:"top_p,omitempty"`
	Stop           []string          `json:"stop,omitempty"`
	Stream         bool              `json:"stream,omitzero"`
	StreamOptions  *oaiStreamOptions `json:"stream_options,omitempty"`
	EnableThinking *bool             `json:"enable_thinking,omitempty"` // nil omits the field
	ThinkingBudget int               `json:"thinking_budget,omitzero"`
	// EnableSearch turns on the provider's native web search for a
	// web_search model (Bailian's enable_search); SearchOptions pins
	// forced_search so the search actually runs.
	EnableSearch  bool              `json:"enable_search,omitzero"`
	SearchOptions *oaiSearchOptions `json:"search_options,omitempty"`
}

// oaiStreamOptions asks the provider for usage in the SSE stream.
type oaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// oaiSearchOptions configures the provider's native web search.
type oaiSearchOptions struct {
	ForcedSearch bool `json:"forced_search"`
}

// oaiMessage is one Chat Completions message. Content holds the request form
// (string or []oaiContentPart) and the response form (string); tool calls
// appear on assistant messages in both directions.
type oaiMessage struct {
	Role             string        `json:"role"`
	Content          any           `json:"content,omitempty"`
	ToolCalls        []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string        `json:"tool_call_id,omitempty"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
}

// oaiContentPart is one part of a multimodal message content array.
type oaiContentPart struct {
	Type     string       `json:"type"` // "text" | "image_url"
	Text     string       `json:"text,omitempty"`
	ImageURL *oaiImageURL `json:"image_url,omitempty"`
}

// oaiImageURL carries an image as a URL or data: URL.
type oaiImageURL struct {
	URL string `json:"url"`
}

// oaiToolCall is a function call on an assistant message.
type oaiToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function *oaiFunction `json:"function"`
}

// oaiFunction names a tool call and carries its arguments as a JSON string.
type oaiFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// oaiTool is one entry of the request's tools array.
type oaiTool struct {
	Type     string       `json:"type"` // "function"
	Function *oaiToolSpec `json:"function"`
}

// oaiToolSpec is the function part of a tools entry.
type oaiToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  jsontext.Value `json:"parameters,omitzero"` // JSON schema object
}

// oaiResponse is a non-streaming Chat Completions response.
type oaiResponse struct {
	ID      string      `json:"id"`
	Model   string      `json:"model"`
	Choices []oaiChoice `json:"choices"`
	Usage   *oaiUsage   `json:"usage,omitempty"`
}

// oaiChoice is one response choice.
type oaiChoice struct {
	Index        int         `json:"index"`
	Message      *oaiMessage `json:"message"`
	FinishReason string      `json:"finish_reason,omitempty"`
}

// oaiUsage mirrors the usage fields kirocc reads from an OpenAI-compatible
// provider, including the cache fields bailian reports.
type oaiUsage struct {
	PromptTokens        int                   `json:"prompt_tokens"`
	CompletionTokens    int                   `json:"completion_tokens"`
	PromptTokensDetails *oaiPromptTokenDetail `json:"prompt_tokens_details,omitempty"`
	PromptCacheHitToken int                   `json:"prompt_cache_hit_tokens,omitempty"`
}

// oaiPromptTokenDetail is the usage detail object.
type oaiPromptTokenDetail struct {
	CachedTokens     int `json:"cached_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

// oaiStreamChunk is one SSE data payload of a streaming response.
type oaiStreamChunk struct {
	ID      string            `json:"id,omitempty"`
	Model   string            `json:"model,omitempty"`
	Choices []oaiStreamChoice `json:"choices,omitempty"`
	Usage   *oaiUsage         `json:"usage,omitempty"`
}

// oaiStreamChoice is one choice of a stream chunk; its delta carries
// fragments that accumulate into the response.
type oaiStreamChoice struct {
	Index        int             `json:"index"`
	Delta        *oaiStreamDelta `json:"delta,omitempty"`
	FinishReason string          `json:"finish_reason,omitempty"`
}

// oaiStreamDelta is the incremental payload of one choice.
type oaiStreamDelta struct {
	Role             string              `json:"role,omitempty"`
	Content          string              `json:"content,omitempty"`
	Reasoning        string              `json:"reasoning,omitempty"`
	ReasoningContent string              `json:"reasoning_content,omitempty"`
	ToolCalls        []oaiStreamToolCall `json:"tool_calls,omitempty"`
}

// oaiStreamToolCall is one tool-call fragment; index identifies the call it
// belongs to and id/name may arrive only on the first fragment.
type oaiStreamToolCall struct {
	Index    int              `json:"index"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function *oaiStreamFnCall `json:"function,omitempty"`
}

// oaiStreamFnCall is the function part of a tool-call fragment.
type oaiStreamFnCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// anthropicUsageNumbers is the Anthropic-side usage triple computed from an
// OpenAI usage object, following the agreed rules: cached falls back from
// prompt_tokens_details.cached_tokens to prompt_cache_hit_tokens,
// cache_creation comes from prompt_tokens_details.cache_write_tokens, and
// input_tokens subtracts both from prompt_tokens (floored at 0).
type anthropicUsageNumbers struct {
	Input         int
	Output        int
	CacheRead     int
	CacheCreation int
}

// numbers converts the provider's usage to the Anthropic fields.
func (u *oaiUsage) numbers() anthropicUsageNumbers {
	if u == nil {
		return anthropicUsageNumbers{}
	}
	cached := u.PromptCacheHitToken
	creation := 0
	if u.PromptTokensDetails != nil {
		if u.PromptTokensDetails.CachedTokens != 0 {
			cached = u.PromptTokensDetails.CachedTokens
		}
		creation = u.PromptTokensDetails.CacheWriteTokens
	}
	input := u.PromptTokens - cached - creation
	if input < 0 {
		input = 0
	}
	return anthropicUsageNumbers{
		Input:         input,
		Output:        u.CompletionTokens,
		CacheRead:     cached,
		CacheCreation: creation,
	}
}

// oaiStopReason maps a Chat Completions finish_reason to the Anthropic
// stop_reason.
func oaiStopReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}

// oaiErrorType maps an upstream OpenAI status code to the Anthropic error
// type of the same meaning, so clients' type-driven handling (rate-limit
// backoff among them) survives the protocol hop.
func oaiErrorType(status int) string {
	switch status {
	case 400, 422:
		return httpx.ErrTypeInvalidRequest
	case 401:
		return httpx.ErrTypeAuthentication
	case 402:
		return httpx.ErrTypeBilling
	case 403:
		return httpx.ErrTypePermission
	case 404:
		return httpx.ErrTypeNotFound
	case 413:
		return httpx.ErrTypeRequestTooLarge
	case 429:
		return httpx.ErrTypeRateLimit
	case 529:
		return httpx.ErrTypeOverloaded
	default:
		return httpx.ErrTypeAPI
	}
}
