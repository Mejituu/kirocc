package customprov

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/d-kuro/kirocc/internal/timing"
)

// oaiStreamEvent is one recorded Anthropic SSE event.
type oaiStreamEvent struct {
	event string
	data  string
}

// oaiStreamRecorder collects emitted events; failAt (1-based) makes that
// emit fail, simulating a client disconnect.
type oaiStreamRecorder struct {
	events []oaiStreamEvent
	failAt int
}

func (r *oaiStreamRecorder) emit(event string, data []byte) error {
	if r.failAt > 0 && len(r.events)+1 == r.failAt {
		return errors.New("client write failed")
	}
	r.events = append(r.events, oaiStreamEvent{event, string(data)})
	return nil
}

func (r *oaiStreamRecorder) names() []string {
	names := make([]string, len(r.events))
	for i, e := range r.events {
		names[i] = e.event
	}
	return names
}

func feedOAIStream(t *testing.T, rec *oaiStreamRecorder, timer *timing.Request, inputs []string) *oaiStreamState {
	t.Helper()
	s := newOAIStreamState(rec.emit, timer)
	for i, in := range inputs {
		if err := s.Feed([]byte(in)); err != nil {
			t.Fatalf("Feed(%d) = %v", i+1, err)
		}
	}
	return s
}

func oaiAssertEventNames(t *testing.T, rec *oaiStreamRecorder, want []string) {
	t.Helper()
	if got := rec.names(); !slices.Equal(got, want) {
		t.Errorf("event names = %v, want %v", got, want)
	}
}

func oaiDecodePayload[T any](t *testing.T, data string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		t.Fatalf("decode payload %s: %v", data, err)
	}
	return v
}

func oaiEventByType(t *testing.T, rec *oaiStreamRecorder, name string) string {
	t.Helper()
	for _, e := range rec.events {
		if e.event == name {
			return e.data
		}
	}
	t.Fatalf("no %s event in %v", name, rec.names())
	return ""
}

// hasTTFT reports whether LogArgs produced a ttft attribute, which is how
// a test can see that MarkFirst ran (the field itself is unexported).
func hasTTFT(args []any) bool {
	for i := 0; i+1 < len(args); i += 2 {
		if key, ok := args[i].(string); ok && key == "ttft" {
			return true
		}
	}
	return false
}

func TestOAIStreamSequences(t *testing.T) {
	const start = `{"id":"chatcmpl-1","model":"m-1","choices":[{"index":0,"delta":{"role":"assistant"}}]}`
	cases := []struct {
		name   string
		inputs []string
		finish bool
		want   []string
	}{
		{
			name: "text with usage-only tail",
			inputs: []string{
				start,
				`{"choices":[{"index":0,"delta":{"content":"Hello"}}]}`,
				`{"choices":[{"index":0,"delta":{"content":" world"}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":7}}`,
			},
			want: []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop"},
		},
		{
			name: "reasoning then text",
			inputs: []string{
				start,
				`{"choices":[{"index":0,"delta":{"reasoning_content":"think"}}]}`,
				`{"choices":[{"index":0,"delta":{"reasoning_content":" more"}}]}`,
				`{"choices":[{"index":0,"delta":{"content":"answer"}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			},
			finish: true,
			want:   []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"},
		},
		{
			name: "reasoning fallback field",
			inputs: []string{
				start,
				`{"choices":[{"index":0,"delta":{"reasoning":"via fallback"}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			},
			finish: true,
			want:   []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"},
		},
		{
			name: "empty reasoning and content skipped",
			inputs: []string{
				start,
				`{"choices":[{"index":0,"delta":{"content":"a"}}]}`,
				`{"choices":[{"index":0,"delta":{"reasoning_content":""}}]}`,
				`{"choices":[{"index":0,"delta":{"reasoning":""}}]}`,
				`{"choices":[{"index":0,"delta":{"content":""}}]}`,
				`{"choices":[{"index":0,"delta":{"content":"b"}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			},
			finish: true,
			want:   []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop"},
		},
		{
			name: "interleaved tool fragments route to their blocks",
			inputs: []string{
				start,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"get_weather","arguments":"{\"ci"}}]}}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"get_time","arguments":"{\"tz"}}]}}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"SF\"}"}}]}}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"\":\"UTC\"}"}}]}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			},
			finish: true,
			want:   []string{"message_start", "content_block_start", "content_block_delta", "content_block_start", "content_block_delta", "content_block_delta", "content_block_delta", "content_block_stop", "content_block_stop", "message_delta", "message_stop"},
		},
		{
			name: "late id and name flush buffered arguments",
			inputs: []string{
				start,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\""}}]}}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_late","function":{"name":"lookup"}}]}}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			},
			finish: true,
			want:   []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop"},
		},
		{
			name: "usage-only tail terminates after finish",
			inputs: []string{
				start,
				`{"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":5}}`,
			},
			want: []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"},
		},
		{
			name: "usage-only before finish is not terminal",
			inputs: []string{
				start,
				`{"choices":[{"index":0,"delta":{"content":"x"}}]}`,
				`{"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":2}}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			},
			finish: true,
			want:   []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"},
		},
		{
			name: "duplicate finish reason handled once",
			inputs: []string{
				start,
				`{"choices":[{"index":0,"delta":{"content":"x"}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			},
			finish: true,
			want:   []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"},
		},
		{
			name: "finish without usage",
			inputs: []string{
				start,
				`{"choices":[{"index":0,"delta":{"content":"x"}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			},
			finish: true,
			want:   []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"},
		},
		{
			name: "error payload mid-stream",
			inputs: []string{
				start,
				`{"choices":[{"index":0,"delta":{"content":"partial"}}]}`,
				`{"error":{"message":"rate limited"}}`,
			},
			finish: true, // no-op after the error event
			want:   []string{"message_start", "content_block_start", "content_block_delta", "error"},
		},
		{
			name: "end without finish reason",
			inputs: []string{
				start,
				`{"choices":[{"index":0,"delta":{"content":"x"}}]}`,
			},
			finish: true,
			want:   []string{"message_start", "content_block_start", "content_block_delta", "error"},
		},
		{
			name: "thinking text tool alternation",
			inputs: []string{
				start,
				`{"choices":[{"index":0,"delta":{"reasoning_content":"hmm"}}]}`,
				`{"choices":[{"index":0,"delta":{"content":"ans"}}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"act","arguments":"{}"}}]}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			},
			finish: true,
			want:   []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "content_block_start", "content_block_delta", "content_block_stop", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"},
		},
		{
			name: "unstarted tool synthesized at finish",
			inputs: []string{
				start,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":2,"function":{"arguments":"{\"x\":1}"}}]}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			},
			finish: true,
			want:   []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"},
		},
		{
			name: "chunks after termination are ignored",
			inputs: []string{
				start,
				`{"choices":[{"index":0,"delta":{"content":"x"}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
				`{"choices":[{"index":0,"delta":{"content":"late"}}]}`,
			},
			finish: true, // no-op: the usage-only chunk already terminated
			want:   []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &oaiStreamRecorder{}
			s := feedOAIStream(t, rec, nil, tc.inputs)
			if tc.finish {
				if err := s.Finish(); err != nil {
					t.Fatalf("Finish() = %v", err)
				}
			}
			oaiAssertEventNames(t, rec, tc.want)
		})
	}
}

func TestOAIStreamMessageStartPayload(t *testing.T) {
	rec := &oaiStreamRecorder{}
	feedOAIStream(t, rec, nil, []string{
		`{"id":"chatcmpl-1","model":"m-1","choices":[{"index":0,"delta":{"content":"hi"}}]}`,
	})
	data := oaiEventByType(t, rec, "message_start")
	for _, want := range []string{`"content":[]`, `"stop_reason":null`, `"stop_sequence":null`} {
		if !strings.Contains(data, want) {
			t.Errorf("message_start payload %s missing %s", data, want)
		}
	}
	msg := oaiDecodePayload[oaiSSEMessageStart](t, data).Message
	if msg.ID != "chatcmpl-1" || msg.Model != "m-1" || msg.Type != "message" || msg.Role != "assistant" {
		t.Errorf("message_start header = %+v", msg)
	}
	if msg.StopReason != nil || msg.StopSequence != nil {
		t.Errorf("message_start stop fields = %v/%v, want null", msg.StopReason, msg.StopSequence)
	}
	if msg.Usage != (oaiSSEUsage{}) {
		t.Errorf("message_start usage = %+v, want zero", msg.Usage)
	}
}

func TestOAIStreamMessageStartNotTriggeredByUsageOnly(t *testing.T) {
	rec := &oaiStreamRecorder{}
	s := feedOAIStream(t, rec, nil, []string{
		`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":0}}`,
	})
	if len(rec.events) != 0 {
		t.Errorf("usage-only first chunk emitted %v, want nothing", rec.names())
	}
	if _, ok := s.Usage(); !ok {
		t.Errorf("Usage() ok = false, want true")
	}
	feedOAIStream(t, rec, nil, []string{
		`{"id":"late-id","model":"late-model","choices":[{"index":0,"delta":{"content":"a"}}]}`,
	})
	msg := oaiDecodePayload[oaiSSEMessageStart](t, oaiEventByType(t, rec, "message_start")).Message
	if msg.ID != "late-id" || msg.Model != "late-model" {
		t.Errorf("message_start id/model = %q/%q", msg.ID, msg.Model)
	}
}

func TestOAIStreamBlockAndDeltaPayloads(t *testing.T) {
	rec := &oaiStreamRecorder{}
	feedOAIStream(t, rec, nil, []string{
		`{"id":"i","model":"m","choices":[{"index":0,"delta":{"content":"Hello"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":" world"}}]}`,
	})
	block := oaiDecodePayload[oaiSSEBlockStart](t, rec.events[1].data)
	if block.Index != 0 {
		t.Errorf("text block index = %d, want 0", block.Index)
	}
	if b, ok := block.ContentBlock.(map[string]any); !ok || b["type"] != "text" || b["text"] != "" {
		t.Errorf("text content_block = %#v", block.ContentBlock)
	}
	delta := oaiDecodePayload[oaiSSEBlockDelta](t, rec.events[2].data)
	if delta.Index != 0 {
		t.Errorf("text delta index = %d, want 0", delta.Index)
	}
	if d, ok := delta.Delta.(map[string]any); !ok || d["type"] != "text_delta" || d["text"] != "Hello" {
		t.Errorf("text delta = %#v", delta.Delta)
	}

	rec = &oaiStreamRecorder{}
	feedOAIStream(t, rec, nil, []string{
		`{"id":"i","model":"m","choices":[{"index":0,"delta":{"reasoning_content":"think"}}]}`,
	})
	block = oaiDecodePayload[oaiSSEBlockStart](t, rec.events[1].data)
	if b, ok := block.ContentBlock.(map[string]any); !ok || b["type"] != "thinking" || b["thinking"] != "" {
		t.Errorf("thinking content_block = %#v", block.ContentBlock)
	}
	delta = oaiDecodePayload[oaiSSEBlockDelta](t, rec.events[2].data)
	if d, ok := delta.Delta.(map[string]any); !ok || d["type"] != "thinking_delta" || d["thinking"] != "think" {
		t.Errorf("thinking delta = %#v", delta.Delta)
	}

	rec = &oaiStreamRecorder{}
	feedOAIStream(t, rec, nil, []string{
		`{"id":"i","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"act","arguments":"{\"x\":"}}]}}]}`,
	})
	if data := rec.events[1].data; !strings.Contains(data, `"input":{}`) {
		t.Errorf("tool_use content_block %s missing \"input\":{}", data)
	}
	block = oaiDecodePayload[oaiSSEBlockStart](t, rec.events[1].data)
	b, _ := block.ContentBlock.(map[string]any)
	if b["type"] != "tool_use" || b["id"] != "call_1" || b["name"] != "act" {
		t.Errorf("tool_use content_block = %#v", block.ContentBlock)
	}
	if in, _ := b["input"].(map[string]any); in == nil {
		t.Errorf("tool_use input = %#v, want {}", b["input"])
	}
	delta = oaiDecodePayload[oaiSSEBlockDelta](t, rec.events[2].data)
	if d, _ := delta.Delta.(map[string]any); d["type"] != "input_json_delta" || d["partial_json"] != `{"x":` {
		t.Errorf("input_json_delta = %#v", delta.Delta)
	}
}

func TestOAIStreamInterleavedToolRouting(t *testing.T) {
	rec := &oaiStreamRecorder{}
	feedOAIStream(t, rec, nil, []string{
		`{"id":"i","model":"m","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"fa","arguments":"A1"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"fb","arguments":"B1"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"A2"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"B2"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	})
	type blockEvent struct {
		name string
		// oaiSSEBlockStart and oaiSSEBlockStop share the index field's shape.
		payload oaiSSEBlockStop
	}
	var got []blockEvent
	for _, e := range rec.events {
		switch e.event {
		case "content_block_start", "content_block_stop":
			got = append(got, blockEvent{e.event, oaiDecodePayload[oaiSSEBlockStop](t, e.data)})
		case "content_block_delta":
			got = append(got, blockEvent{e.event, oaiDecodePayload[oaiSSEBlockStop](t, e.data)})
		}
	}
	// start(0) delta(0) start(1) delta(1) delta(0) delta(1) stop(0) stop(1)
	want := []blockEvent{
		{"content_block_start", oaiSSEBlockStop{Index: 0}},
		{"content_block_delta", oaiSSEBlockStop{Index: 0}},
		{"content_block_start", oaiSSEBlockStop{Index: 1}},
		{"content_block_delta", oaiSSEBlockStop{Index: 1}},
		{"content_block_delta", oaiSSEBlockStop{Index: 0}},
		{"content_block_delta", oaiSSEBlockStop{Index: 1}},
		{"content_block_stop", oaiSSEBlockStop{Index: 0}},
		{"content_block_stop", oaiSSEBlockStop{Index: 1}},
	}
	if !slices.EqualFunc(got, want, func(a, b blockEvent) bool {
		return a.name == b.name && a.payload.Index == b.payload.Index
	}) {
		t.Errorf("block events = %+v, want %+v", got, want)
	}
	// The fragments land in their own blocks in arrival order.
	var partials []string
	for _, e := range rec.events {
		if e.event != "content_block_delta" {
			continue
		}
		d := oaiDecodePayload[oaiSSEBlockDelta](t, e.data)
		p := d.Delta.(map[string]any)["partial_json"].(string)
		if d.Index == 0 {
			partials = append(partials, p)
		}
	}
	if !slices.Equal(partials, []string{"A1", "A2"}) {
		t.Errorf("tool 0 partials = %v, want [A1 A2]", partials)
	}
}

func TestOAIStreamLateIDPendingArgs(t *testing.T) {
	rec := &oaiStreamRecorder{}
	feedOAIStream(t, rec, nil, []string{
		`{"id":"i","model":"m","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_late","function":{"name":"lookup"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]}}]}`,
	})
	var partials []string
	for _, e := range rec.events {
		if e.event != "content_block_delta" {
			continue
		}
		d := oaiDecodePayload[oaiSSEBlockDelta](t, e.data)
		partials = append(partials, d.Delta.(map[string]any)["partial_json"].(string))
	}
	if !slices.Equal(partials, []string{`{"a"`, ":1}"}) {
		t.Errorf("partials = %v, want the buffered fragment then the live one", partials)
	}
	block := oaiDecodePayload[oaiSSEBlockStart](t, rec.events[1].data)
	b, _ := block.ContentBlock.(map[string]any)
	if b["id"] != "call_late" || b["name"] != "lookup" {
		t.Errorf("late tool block = %#v", block.ContentBlock)
	}
}

func TestOAIStreamSynthesizedToolBlock(t *testing.T) {
	rec := &oaiStreamRecorder{}
	s := feedOAIStream(t, rec, nil, []string{
		`{"id":"i","model":"m","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":2,"function":{"arguments":"{\"x\":1}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	})
	if err := s.Finish(); err != nil {
		t.Fatalf("Finish() = %v", err)
	}
	block := oaiDecodePayload[oaiSSEBlockStart](t, oaiEventByType(t, rec, "content_block_start"))
	b, _ := block.ContentBlock.(map[string]any)
	if b["id"] != "tool_call_2" || b["name"] != "unknown_tool" {
		t.Errorf("synthesized tool block = %#v, want tool_call_2/unknown_tool", block.ContentBlock)
	}
	d := oaiDecodePayload[oaiSSEBlockDelta](t, oaiEventByType(t, rec, "content_block_delta"))
	if p := d.Delta.(map[string]any)["partial_json"].(string); p != `{"x":1}` {
		t.Errorf("synthesized partial_json = %q", p)
	}
}

func TestOAIStreamMessageDeltaUsage(t *testing.T) {
	cases := []struct {
		name       string
		inputs     []string
		finish     bool
		wantStop   string
		wantOutput int
	}{
		{
			name: "usage from tail chunk",
			inputs: []string{
				`{"id":"i","model":"m","choices":[{"index":0,"delta":{"content":"x"}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":7}}`,
			},
			wantStop:   "end_turn",
			wantOutput: 7,
		},
		{
			name: "no usage zeroes output",
			inputs: []string{
				`{"id":"i","model":"m","choices":[{"index":0,"delta":{"content":"x"}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			},
			finish:     true,
			wantStop:   "end_turn",
			wantOutput: 0,
		},
		{
			name: "tool_calls maps to tool_use",
			inputs: []string{
				`{"id":"i","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":"{}"}}]}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			},
			finish:     true,
			wantStop:   "tool_use",
			wantOutput: 0,
		},
		{
			name: "length maps to max_tokens",
			inputs: []string{
				`{"id":"i","model":"m","choices":[{"index":0,"delta":{"content":"x"}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,
			},
			finish:     true,
			wantStop:   "max_tokens",
			wantOutput: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &oaiStreamRecorder{}
			s := feedOAIStream(t, rec, nil, tc.inputs)
			if tc.finish {
				if err := s.Finish(); err != nil {
					t.Fatalf("Finish() = %v", err)
				}
			}
			data := oaiEventByType(t, rec, "message_delta")
			if !strings.Contains(data, `"stop_sequence":null`) {
				t.Errorf("message_delta %s missing stop_sequence null", data)
			}
			md := oaiDecodePayload[oaiSSEMessageDelta](t, data)
			if md.Delta.StopReason != tc.wantStop {
				t.Errorf("stop_reason = %q, want %q", md.Delta.StopReason, tc.wantStop)
			}
			if md.Usage.OutputTokens != tc.wantOutput {
				t.Errorf("output_tokens = %d, want %d", md.Usage.OutputTokens, tc.wantOutput)
			}
		})
	}
}

// TestOAIStreamMessageDeltaFullUsage asserts the message_delta carries the
// full usage set, not output_tokens alone: a usage-only tail with cache
// traffic must reach the client as input = prompt − cached plus the cache
// fields, like the Kiro path's message_delta.
func TestOAIStreamMessageDeltaFullUsage(t *testing.T) {
	rec := &oaiStreamRecorder{}
	s := feedOAIStream(t, rec, nil, []string{
		`{"id":"i","model":"m","choices":[{"index":0,"delta":{"content":"x"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":400,"cache_write_tokens":50}}}`,
	})
	if err := s.Finish(); err != nil {
		t.Fatalf("Finish() = %v", err)
	}
	md := oaiDecodePayload[oaiSSEMessageDelta](t, oaiEventByType(t, rec, "message_delta"))
	want := oaiSSEDeltaUsage{InputTokens: 550, OutputTokens: 7, CacheReadInputTokens: 400, CacheCreationInputTokens: 50}
	if md.Usage != want {
		t.Errorf("usage = %+v, want %+v", md.Usage, want)
	}
}

func TestOAIStreamErrorPayload(t *testing.T) {
	cases := []struct {
		name        string
		payload     string
		wantMessage string
	}{
		{
			name:        "object message",
			payload:     `{"error":{"message":"rate limited"}}`,
			wantMessage: "rate limited",
		},
		{
			name:        "bare string",
			payload:     `{"error":"quota exceeded"}`,
			wantMessage: "quota exceeded",
		},
		{
			name:        "object without message falls back to raw",
			payload:     `{"error":{"code":500}}`,
			wantMessage: `{"code":500}`,
		},
		{
			name:        "error beside a broken chunk field",
			payload:     `{"id":42,"error":{"message":"bad id type"}}`,
			wantMessage: "bad id type",
		},
		{
			// Some providers carry "error": null on regular chunks; a null
			// error must not read as a failure.
			name:        "null error is not an error",
			payload:     `{"error":null,"choices":[]}`,
			wantMessage: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &oaiStreamRecorder{}
			s := newOAIStreamState(rec.emit, nil)
			if err := s.Feed([]byte(tc.payload)); err != nil {
				t.Fatalf("Feed() = %v", err)
			}
			if tc.wantMessage == "" {
				if len(rec.events) != 0 {
					t.Fatalf("events = %v, want none", rec.names())
				}
				return
			}
			data := oaiEventByType(t, rec, "error")
			ev := oaiDecodePayload[oaiSSEError](t, data)
			if ev.Type != "error" || ev.Error.Type != "api_error" || ev.Error.Message != tc.wantMessage {
				t.Errorf("error event = %+v, want message %q", ev, tc.wantMessage)
			}
			// After an error the stream is over: Finish and Feed are no-ops.
			if err := s.Finish(); err != nil {
				t.Errorf("Finish() = %v, want nil", err)
			}
			if err := s.Feed([]byte(`{"choices":[{"delta":{"content":"x"}}]}`)); err != nil {
				t.Errorf("Feed() = %v, want nil", err)
			}
			if got := rec.names(); !slices.Equal(got, []string{"error"}) {
				t.Errorf("events after error = %v", got)
			}
		})
	}
}

func TestOAIStreamUnexpectedEnd(t *testing.T) {
	rec := &oaiStreamRecorder{}
	s := feedOAIStream(t, rec, nil, []string{
		`{"id":"i","model":"m","choices":[{"index":0,"delta":{"content":"x"}}]}`,
	})
	if err := s.Finish(); err != nil {
		t.Fatalf("Finish() = %v", err)
	}
	ev := oaiDecodePayload[oaiSSEError](t, oaiEventByType(t, rec, "error"))
	if ev.Error.Type != "api_error" || ev.Error.Message != "upstream stream ended unexpectedly" {
		t.Errorf("error event = %+v", ev)
	}
	if err := s.Finish(); err != nil {
		t.Errorf("second Finish() = %v, want nil", err)
	}
	if got := rec.names(); !slices.Equal(got, []string{"message_start", "content_block_start", "content_block_delta", "error"}) {
		t.Errorf("events = %v", got)
	}
}

func TestOAIStreamTerminatedOnce(t *testing.T) {
	rec := &oaiStreamRecorder{}
	s := feedOAIStream(t, rec, nil, []string{
		`{"id":"i","model":"m","choices":[{"index":0,"delta":{"content":"x"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
	})
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	oaiAssertEventNames(t, rec, want)
	if err := s.Finish(); err != nil {
		t.Fatalf("Finish() after termination = %v", err)
	}
	oaiAssertEventNames(t, rec, want)
}

func TestOAIStreamMarkFirst(t *testing.T) {
	logArgs := func(tr *timing.Request) []any {
		return tr.LogArgs(10, time.Now(), true)
	}
	t.Run("text delta", func(t *testing.T) {
		_, tr := timing.Start(context.Background())
		feedOAIStream(t, &oaiStreamRecorder{}, tr, []string{
			`{"id":"i","model":"m","choices":[{"index":0,"delta":{"content":"x"}}]}`,
		})
		if !hasTTFT(logArgs(tr)) {
			t.Errorf("no ttft after a text delta")
		}
	})
	t.Run("tool arguments delta", func(t *testing.T) {
		_, tr := timing.Start(context.Background())
		feedOAIStream(t, &oaiStreamRecorder{}, tr, []string{
			`{"id":"i","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":"{}"}}]}}]}`,
		})
		if !hasTTFT(logArgs(tr)) {
			t.Errorf("no ttft after an input_json_delta")
		}
	})
	t.Run("no content", func(t *testing.T) {
		_, tr := timing.Start(context.Background())
		s := feedOAIStream(t, &oaiStreamRecorder{}, tr, []string{
			`{"id":"i","model":"m","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		})
		if err := s.Finish(); err != nil {
			t.Fatalf("Finish() = %v", err)
		}
		if hasTTFT(logArgs(tr)) {
			t.Errorf("ttft without a content delta")
		}
	})
}

func TestOAIStreamUsageAccessor(t *testing.T) {
	rec := &oaiStreamRecorder{}
	s := feedOAIStream(t, rec, nil, []string{
		`{"id":"i","model":"m","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
	})
	if u, ok := s.Usage(); ok {
		t.Errorf("Usage() before any usage = (%+v, true), want false", u)
	}
	s = feedOAIStream(t, &oaiStreamRecorder{}, nil, []string{
		`{"id":"i","model":"m","choices":[{"index":0,"delta":{"content":"x"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":4,"prompt_cache_hit_tokens":40}}`,
	})
	u, ok := s.Usage()
	if !ok || u.PromptTokens != 100 || u.CompletionTokens != 4 || u.PromptCacheHitToken != 40 {
		t.Errorf("Usage() = (%+v, %v)", u, ok)
	}
}

func TestOAIStreamEmitFailurePropagates(t *testing.T) {
	rec := &oaiStreamRecorder{failAt: 2}
	s := newOAIStreamState(rec.emit, nil)
	if err := s.Feed([]byte(`{"id":"i","model":"m","choices":[{"index":0,"delta":{"role":"assistant"}}]}`)); err != nil {
		t.Fatalf("Feed(start) = %v", err)
	}
	err := s.Feed([]byte(`{"choices":[{"index":0,"delta":{"content":"x"}}]}`))
	if err == nil || !strings.Contains(err.Error(), "client write failed") {
		t.Fatalf("Feed(content) = %v, want the emit error", err)
	}
	// The client is gone: nothing else is emitted, nothing else fails.
	if err := s.Feed([]byte(`{"choices":[{"index":0,"delta":{"content":"y"}}]}`)); err != nil {
		t.Errorf("Feed after write failure = %v, want nil", err)
	}
	if err := s.Finish(); err != nil {
		t.Errorf("Finish after write failure = %v, want nil", err)
	}
	if got := rec.names(); !slices.Equal(got, []string{"message_start"}) {
		t.Errorf("events = %v, want only message_start", got)
	}
}
