package customprov

import (
	"bytes"
	"encoding/json/v2"

	"github.com/d-kuro/kirocc/internal/timing"
)

// usagePeek mirrors the usage object of an Anthropic response. Only the
// fields kirocc logs are declared, so other members (server_tool_use, …)
// decode fine; a usage-bearing event whose shape is off is skipped rather
// than failed.
type usagePeek struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// messagePeek mirrors the message object of a message_start event.
type messagePeek struct {
	Usage *usagePeek `json:"usage"`
}

// sseEventPeek is a usage-bearing SSE event: {type, message:{usage}, usage}.
type sseEventPeek struct {
	Type    string       `json:"type"`
	Message *messagePeek `json:"message"`
	Usage   *usagePeek   `json:"usage"`
}

// jsonResponsePeek mirrors a non-streaming Anthropic response for its usage.
type jsonResponsePeek struct {
	Usage *usagePeek `json:"usage"`
}

// observedUsage is the usage seen in a provider response, with the input
// side already in kirocc's context-occupancy terms: Anthropic's
// input_tokens excludes cache traffic, so cache reads and creations are
// added in, matching what the Kiro path reports.
type observedUsage struct {
	inputTokens  int
	outputTokens int
}

// contextOccupancy converts an Anthropic usage object to the context
// occupancy kirocc reports: input plus cached reads and writes.
func contextOccupancy(u *usagePeek) int {
	return u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
}

// scanUsage incrementally extracts usage from an SSE stream as the response
// bytes stream past. It only reads what it is fed — the forwarded bytes,
// timing and flushes are untouched. Lines split across reads are buffered
// until their newline arrives.
type scanUsage struct {
	usage   observedUsage
	found   bool
	pending []byte

	// timer notes the first content_block_delta for the response log's ttft;
	// a nil timer is inert.
	timer *timing.Request
}

// feed consumes one forwarded chunk.
func (s *scanUsage) feed(chunk []byte) {
	s.pending = append(s.pending, chunk...)
	for {
		idx := bytes.IndexByte(s.pending, '\n')
		if idx < 0 {
			return
		}
		line := s.pending[:idx]
		s.pending = s.pending[idx+1:]
		s.line(line)
	}
}

// line handles one complete SSE line. Only data: lines carry payloads; the
// one optional space after the colon is part of the wire format.
func (s *scanUsage) line(line []byte) {
	line = bytes.TrimSuffix(line, []byte("\r"))
	data, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return
	}
	if len(data) > 0 && data[0] == ' ' {
		data = data[1:]
	}
	var ev sseEventPeek
	if err := json.Unmarshal(data, &ev); err != nil {
		return
	}
	switch ev.Type {
	case "message_start":
		if ev.Message != nil && ev.Message.Usage != nil {
			s.usage = observedUsage{inputTokens: contextOccupancy(ev.Message.Usage), outputTokens: ev.Message.Usage.OutputTokens}
			s.found = true
		}
	case "content_block_delta":
		// The first delta of any kind — text, thinking or tool-call input —
		// is the response's first byte.
		s.timer.MarkFirst()
	case "message_delta":
		if ev.Usage != nil {
			s.found = true
			// message_delta's output_tokens is authoritative. Its
			// input-class fields appear only in newer API revisions and
			// carry the cumulative input, so they override the start
			// snapshot when present.
			s.usage.outputTokens = ev.Usage.OutputTokens
			if ev.Usage.InputTokens != 0 || ev.Usage.CacheReadInputTokens != 0 || ev.Usage.CacheCreationInputTokens != 0 {
				s.usage.inputTokens = contextOccupancy(ev.Usage)
			}
		}
	}
}

// jsonUsage extracts the usage from a complete non-streaming response body.
// An unparsable or usage-less body yields nil.
func jsonUsage(body []byte) *observedUsage {
	var resp jsonResponsePeek
	if err := json.Unmarshal(body, &resp); err != nil || resp.Usage == nil {
		return nil
	}
	return &observedUsage{inputTokens: contextOccupancy(resp.Usage), outputTokens: resp.Usage.OutputTokens}
}
