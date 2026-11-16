package customprov

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"sort"

	"github.com/d-kuro/kirocc/internal/timing"
)

// oaiEmitFunc writes one Anthropic SSE event: the event name and the JSON
// payload of its data line. Returning an error aborts the stream (the
// caller treats it as a client disconnect).
type oaiEmitFunc func(event string, data []byte) error

// oaiToolBlockState follows one upstream tool call across the fragments
// that carry it: the Anthropic block index assigned when the call first
// appears, the id and name once they arrive, and the argument fragments
// buffered while either is still missing.
type oaiToolBlockState struct {
	callIndex  int // upstream tool_calls index, for synthesized ids
	blockIndex int // Anthropic content block index
	id         string
	name       string
	started    bool // content_block_start emitted
	open       bool // started and not yet stopped
	pending    string
}

// oaiStreamState converts an OpenAI Chat Completions SSE stream to
// Anthropic Messages SSE events chunk by chunk.
//
// The non-tool cursor and the tool blocks are independent: a tool-call
// fragment closes the running text/thinking block, but text arriving while
// a tool block is open simply opens the next non-tool block; open tool
// blocks are stopped when the finish_reason arrives (or, for fragments
// that arrive after it, at termination).
type oaiStreamState struct {
	emit  oaiEmitFunc
	timer *timing.Request

	id      string // first non-empty chunk id, for message_start
	model   string // first non-empty chunk model
	started bool   // message_start emitted

	nextIndex int    // next Anthropic content block index
	curKind   string // kind of the open non-tool block: "", "thinking" or "text"
	curIndex  int    // its Anthropic block index

	tools map[int]*oaiToolBlockState // keyed by the upstream tool_calls index

	seenFinish bool
	stopReason string

	usage      *oaiUsage // latest usage seen on any chunk
	terminated bool      // message_stop or an error event was emitted
	marked     bool      // timer.MarkFirst was called
}

// newOAIStreamState builds the converter. emit receives every Anthropic
// event; timer notes the first content delta for the response log's ttft.
func newOAIStreamState(emit oaiEmitFunc, timer *timing.Request) *oaiStreamState {
	return &oaiStreamState{
		emit:  emit,
		timer: timer,
		tools: make(map[int]*oaiToolBlockState),
	}
}

// Feed consumes one SSE data payload (the bytes after "data:", [DONE]
// excluded — the caller handles the terminator). The returned error
// signals a write failure.
func (s *oaiStreamState) Feed(data []byte) error {
	if s.terminated {
		return nil
	}

	// Providers report mid-stream failures as a plain {"error": ...}
	// object instead of a chunk, so probe for one before trusting the
	// chunk decode: a payload carrying an error never carries choices.
	var chunk oaiStreamChunk
	chunkErr := json.Unmarshal(data, &chunk)
	var probe struct {
		Error jsontext.Value `json:"error"`
	}
	probeErr := json.Unmarshal(data, &probe)
	if probeErr == nil && len(probe.Error) > 0 && probe.Error.Kind() != jsontext.KindNull && len(chunk.Choices) == 0 {
		return s.emitUpstreamError(probe.Error)
	}
	if chunkErr != nil {
		// Not a chunk and not a recognized error; skip it.
		return nil
	}

	if s.id == "" && chunk.ID != "" {
		s.id = chunk.ID
	}
	if s.model == "" && chunk.Model != "" {
		s.model = chunk.Model
	}
	if chunk.Usage != nil {
		s.usage = chunk.Usage
	}

	if len(chunk.Choices) == 0 {
		// A usage-only tail chunk ends a message whose finish_reason was
		// already seen, so the final message_delta carries full usage.
		if s.seenFinish && chunk.Usage != nil {
			return s.terminate()
		}
		return nil
	}

	choice := &chunk.Choices[0]
	if !s.started {
		if err := s.emitMessageStart(); err != nil {
			return err
		}
	}
	if choice.Delta != nil {
		if err := s.feedDelta(choice.Delta); err != nil {
			return err
		}
	}
	if choice.FinishReason != "" && !s.seenFinish {
		if err := s.handleFinish(choice.FinishReason); err != nil {
			return err
		}
	}
	return nil
}

// Finish ends the stream: it is called once at EOF, [DONE] or a read
// error. A stream that ended without a finish_reason (and without an
// upstream error event) ends with an error event instead of a normal
// termination; a stream terminated by an error event stays silent.
func (s *oaiStreamState) Finish() error {
	if s.terminated {
		return nil
	}
	if !s.seenFinish {
		s.terminated = true
		return s.emitEvent("error", oaiSSEError{
			Type:  "error",
			Error: oaiSSEErrorBody{Type: "api_error", Message: "upstream stream ended unexpectedly"},
		})
	}
	return s.terminate()
}

// Usage reports the stream's accumulated usage, if any chunk carried one.
func (s *oaiStreamState) Usage() (u oaiUsage, ok bool) {
	if s.usage == nil {
		return oaiUsage{}, false
	}
	return *s.usage, true
}

// feedDelta routes one choice delta: reasoning (reasoning_content with a
// reasoning fallback) and content feed the text/thinking cursor, tool
// fragments feed the per-call blocks. Empty fragments are skipped.
func (s *oaiStreamState) feedDelta(d *oaiStreamDelta) error {
	reasoning := d.ReasoningContent
	if reasoning == "" {
		reasoning = d.Reasoning
	}
	if reasoning != "" {
		if err := s.nonToolDelta("thinking", reasoning); err != nil {
			return err
		}
	}
	if d.Content != "" {
		if err := s.nonToolDelta("text", d.Content); err != nil {
			return err
		}
	}
	if len(d.ToolCalls) > 0 {
		if err := s.toolDeltas(d.ToolCalls); err != nil {
			return err
		}
	}
	return nil
}

// nonToolDelta emits the block switch (stop the old block, start the new
// one at the next index) and the delta for a text or thinking fragment.
// Tool blocks being open do not interfere: they keep receiving their
// fragments and are stopped at the finish_reason or termination.
func (s *oaiStreamState) nonToolDelta(kind, text string) error {
	if s.curKind != kind {
		if s.curKind != "" {
			if err := s.emitBlockStop(s.curIndex); err != nil {
				return err
			}
			s.curKind = ""
		}
		index := s.nextIndex
		s.nextIndex++
		var block any
		if kind == "thinking" {
			block = oaiSSEThinkingBlock{Type: "thinking"}
		} else {
			block = oaiSSETextBlock{Type: "text"}
		}
		if err := s.emitEvent("content_block_start", oaiSSEBlockStart{
			Type:         "content_block_start",
			Index:        index,
			ContentBlock: block,
		}); err != nil {
			return err
		}
		s.curKind, s.curIndex = kind, index
	}
	var delta any
	if kind == "thinking" {
		delta = oaiSSEThinkingDelta{Type: "thinking_delta", Thinking: text}
	} else {
		delta = oaiSSETextDelta{Type: "text_delta", Text: text}
	}
	if err := s.emitEvent("content_block_delta", oaiSSEBlockDelta{
		Type:  "content_block_delta",
		Index: s.curIndex,
		Delta: delta,
	}); err != nil {
		return err
	}
	s.markFirst()
	return nil
}

// toolDeltas routes tool-call fragments. A tool call closes the running
// text/thinking block (the non-tool cursor and the tool blocks are
// otherwise independent). Fragments for a call whose id and name have not
// both arrived buffer their arguments; once they have, the block starts
// and the buffered fragments flush.
func (s *oaiStreamState) toolDeltas(calls []oaiStreamToolCall) error {
	if s.curKind != "" {
		if err := s.emitBlockStop(s.curIndex); err != nil {
			return err
		}
		s.curKind = ""
	}
	for _, tc := range calls {
		st := s.tools[tc.Index]
		if st == nil {
			st = &oaiToolBlockState{callIndex: tc.Index, blockIndex: s.nextIndex}
			s.nextIndex++
			s.tools[tc.Index] = st
		}
		if tc.ID != "" {
			st.id = tc.ID
		}
		if tc.Function != nil && tc.Function.Name != "" {
			st.name = tc.Function.Name
		}
		start := !st.started && st.id != "" && st.name != ""
		if start {
			st.started, st.open = true, true
			if err := s.emitToolStart(st.blockIndex, st.id, st.name); err != nil {
				return err
			}
		}
		if start && st.pending != "" {
			args := st.pending
			st.pending = ""
			if err := s.emitArgsDelta(st.blockIndex, args); err != nil {
				return err
			}
		}
		if tc.Function != nil && tc.Function.Arguments != "" {
			if st.started {
				if err := s.emitArgsDelta(st.blockIndex, tc.Function.Arguments); err != nil {
					return err
				}
			} else {
				st.pending += tc.Function.Arguments
			}
		}
	}
	return nil
}

// handleFinish processes the first finish_reason: it fixes the stop
// reason, closes the running non-tool block, starts the tool blocks whose
// id and name never completed (synthesizing them), and stops every open
// block in index order. The message_delta itself waits for termination so
// usage can still arrive; repeated finish_reasons are ignored.
func (s *oaiStreamState) handleFinish(reason string) error {
	s.seenFinish = true
	s.stopReason = oaiStopReason(reason)

	if s.curKind != "" {
		if err := s.emitBlockStop(s.curIndex); err != nil {
			return err
		}
		s.curKind = ""
	}

	// Late starts for calls that only ever sent argument fragments.
	var late []*oaiToolBlockState
	for _, st := range s.tools {
		if st.started || (st.id == "" && st.name == "" && st.pending == "") {
			continue
		}
		if st.id == "" {
			st.id = fmt.Sprintf("tool_call_%d", st.callIndex)
		}
		if st.name == "" {
			st.name = "unknown_tool"
		}
		st.started, st.open = true, true
		late = append(late, st)
	}
	sort.Slice(late, func(i, j int) bool { return late[i].blockIndex < late[j].blockIndex })
	for _, st := range late {
		if err := s.emitToolStart(st.blockIndex, st.id, st.name); err != nil {
			return err
		}
		if st.pending != "" {
			args := st.pending
			st.pending = ""
			if err := s.emitArgsDelta(st.blockIndex, args); err != nil {
				return err
			}
		}
	}

	return s.closeOpenBlocks()
}

// closeOpenBlocks stops every open block — the non-tool cursor and the
// started tool blocks — in block-index order, and marks them closed. It
// upholds the Anthropic pairing invariant at the finish_reason normally,
// and at termination for blocks a post-finish delta reopened.
func (s *oaiStreamState) closeOpenBlocks() error {
	indices := make([]int, 0, 1+len(s.tools))
	if s.curKind != "" {
		indices = append(indices, s.curIndex)
		s.curKind = ""
	}
	for _, st := range s.tools {
		if st.open {
			indices = append(indices, st.blockIndex)
			st.open = false
		}
	}
	sort.Ints(indices)
	for _, index := range indices {
		if err := s.emitBlockStop(index); err != nil {
			return err
		}
	}
	return nil
}

// terminate emits the closing events once: any block a post-finish delta
// reopened, then the message_delta with the stop reason and the latest
// usage, then message_stop. message_start's usage is zero (usage arrives at
// the tail), so the delta carries the full usage set — the four fields are
// always present, zero when none arrived — for the SDK accumulator to
// correct, like the Kiro path's message_delta.
func (s *oaiStreamState) terminate() error {
	if s.terminated {
		return nil
	}
	s.terminated = true
	if err := s.closeOpenBlocks(); err != nil {
		return err
	}
	var n anthropicUsageNumbers
	if s.usage != nil {
		n = s.usage.numbers()
	}
	if err := s.emitEvent("message_delta", oaiSSEMessageDelta{
		Type:  "message_delta",
		Delta: oaiSSEMessageDeltaBody{StopReason: s.stopReason, StopSequence: nil},
		Usage: oaiSSEDeltaUsage{
			InputTokens:              n.Input,
			OutputTokens:             n.Output,
			CacheReadInputTokens:     n.CacheRead,
			CacheCreationInputTokens: n.CacheCreation,
		},
	}); err != nil {
		return err
	}
	return s.emitEvent("message_stop", oaiSSEMessageStop{Type: "message_stop"})
}

// emitMessageStart emits message_start for the first chunk with choices:
// id and model from the stream, zero usage, and the fields the official
// SDKs require present (empty content, null stop_reason and
// stop_sequence).
func (s *oaiStreamState) emitMessageStart() error {
	s.started = true
	return s.emitEvent("message_start", oaiSSEMessageStart{
		Type: "message_start",
		Message: oaiSSEMessageHeader{
			ID:           s.id,
			Type:         "message",
			Role:         "assistant",
			Model:        s.model,
			Content:      []any{},
			StopReason:   nil,
			StopSequence: nil,
			Usage:        oaiSSEUsage{},
		},
	})
}

// emitUpstreamError reports a {"error": ...} payload as an Anthropic
// error event and ends the stream: later chunks and Finish stay silent.
func (s *oaiStreamState) emitUpstreamError(raw jsontext.Value) error {
	message := ""
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err == nil {
		switch body := v.(type) {
		case string:
			message = body
		case map[string]any:
			if m, ok := body["message"].(string); ok {
				message = m
			}
		}
	}
	if message == "" {
		message = raw.String()
	}
	s.terminated = true
	return s.emitEvent("error", oaiSSEError{
		Type:  "error",
		Error: oaiSSEErrorBody{Type: "api_error", Message: message},
	})
}

// emitEvent marshals the payload and hands it to the emitter; a write
// failure terminates the conversion (the client is gone).
func (s *oaiStreamState) emitEvent(event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := s.emit(event, data); err != nil {
		s.terminated = true
		return err
	}
	return nil
}

func (s *oaiStreamState) emitBlockStop(index int) error {
	return s.emitEvent("content_block_stop", oaiSSEBlockStop{Type: "content_block_stop", Index: index})
}

func (s *oaiStreamState) emitToolStart(index int, id, name string) error {
	return s.emitEvent("content_block_start", oaiSSEBlockStart{
		Type:         "content_block_start",
		Index:        index,
		ContentBlock: oaiSSEToolUseBlock{Type: "tool_use", ID: id, Name: name, Input: map[string]any{}},
	})
}

func (s *oaiStreamState) emitArgsDelta(index int, args string) error {
	if err := s.emitEvent("content_block_delta", oaiSSEBlockDelta{
		Type:  "content_block_delta",
		Index: index,
		Delta: oaiSSEInputJSONDelta{Type: "input_json_delta", PartialJSON: args},
	}); err != nil {
		return err
	}
	s.markFirst()
	return nil
}

func (s *oaiStreamState) markFirst() {
	if !s.marked {
		s.marked = true
		s.timer.MarkFirst()
	}
}

// The Anthropic-side SSE payloads the converter emits. The message_start
// header carries content, stop_reason and stop_sequence even though they
// are empty/null: the official SDKs require the fields to be present.

type oaiSSEMessageStart struct {
	Type    string              `json:"type"`
	Message oaiSSEMessageHeader `json:"message"`
}

type oaiSSEMessageHeader struct {
	ID           string      `json:"id"`
	Type         string      `json:"type"`
	Role         string      `json:"role"`
	Model        string      `json:"model"`
	Content      []any       `json:"content"`
	StopReason   *string     `json:"stop_reason"`
	StopSequence *string     `json:"stop_sequence"`
	Usage        oaiSSEUsage `json:"usage"`
}

type oaiSSEUsage struct {
	Input  int `json:"input_tokens"`
	Output int `json:"output_tokens"`
}

type oaiSSEBlockStart struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock any    `json:"content_block"`
}

type oaiSSEThinkingBlock struct {
	Type     string `json:"type"`
	Thinking string `json:"thinking"`
}

type oaiSSETextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type oaiSSEToolUseBlock struct {
	Type  string         `json:"type"`
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

type oaiSSEBlockDelta struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	Delta any    `json:"delta"`
}

type oaiSSEThinkingDelta struct {
	Type     string `json:"type"`
	Thinking string `json:"thinking"`
}

type oaiSSETextDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type oaiSSEInputJSONDelta struct {
	Type        string `json:"type"`
	PartialJSON string `json:"partial_json"`
}

type oaiSSEBlockStop struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type oaiSSEMessageDelta struct {
	Type  string                 `json:"type"`
	Delta oaiSSEMessageDeltaBody `json:"delta"`
	Usage oaiSSEDeltaUsage       `json:"usage"`
}

type oaiSSEMessageDeltaBody struct {
	StopReason   string  `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
}

// oaiSSEDeltaUsage is the message_delta usage: the full four-field set, so
// clients can correct message_start's zeros with the final numbers.
type oaiSSEDeltaUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

type oaiSSEMessageStop struct {
	Type string `json:"type"`
}

type oaiSSEError struct {
	Type  string          `json:"type"`
	Error oaiSSEErrorBody `json:"error"`
}

type oaiSSEErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}
