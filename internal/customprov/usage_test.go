package customprov

import "testing"

// TestScanUsage_SplitLines feeds an SSE stream whose message_start and
// message_delta lines are broken mid-line across separate reads, the way
// chunked response reads split them.
func TestScanUsage_SplitLines(t *testing.T) {
	s := &scanUsage{}
	s.feed([]byte("event: message_start\nda"))
	s.feed([]byte(`ta: {"type":"message_start","message":{"usage":{"input_tokens":10,`))
	s.feed([]byte(`"cache_read_input_tokens":900,"cache_creation_input_tokens":90,"output_tokens":3}}}`))
	s.feed([]byte("\nevent: message_delta\ndata: {\"type\":\"mess"))
	s.feed([]byte("age_delta\",\"usage\":{\"output_tokens\":42}}\n"))
	s.feed([]byte("event: message_stop\ndata: {}\n"))

	if !s.found {
		t.Fatal("usage should be found")
	}
	if s.usage.inputTokens != 1000 {
		t.Errorf("inputTokens = %d, want 1000 (input + cache_read + cache_creation)", s.usage.inputTokens)
	}
	if s.usage.outputTokens != 42 {
		t.Errorf("outputTokens = %d, want 42 (message_delta wins)", s.usage.outputTokens)
	}
}

// TestScanUsage_DeltaInputOverrides asserts a message_delta carrying
// cumulative input-class fields overrides the message_start snapshot.
func TestScanUsage_DeltaInputOverrides(t *testing.T) {
	s := &scanUsage{}
	s.feed([]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":5,"cache_read_input_tokens":7,"output_tokens":2}}}` + "\n"))
	s.feed([]byte(`data: {"type":"message_delta","usage":{"input_tokens":500,"output_tokens":30}}` + "\n"))

	if s.usage.inputTokens != 500 {
		t.Errorf("inputTokens = %d, want 500 (delta's cumulative input overrides)", s.usage.inputTokens)
	}
	if s.usage.outputTokens != 30 {
		t.Errorf("outputTokens = %d, want 30", s.usage.outputTokens)
	}
}

// TestScanUsage_IgnoresNonDataAndBadLines asserts event lines, comments and
// unparsable payloads are skipped without failing the scan.
func TestScanUsage_IgnoresNonDataAndBadLines(t *testing.T) {
	s := &scanUsage{}
	s.feed([]byte(": keep-alive\n"))
	s.feed([]byte("event: message_start\n"))
	s.feed([]byte("data: not json\n"))
	s.feed([]byte("data: {\"type\":\"message_stop\"}\n"))

	if s.found {
		t.Error("no usage-bearing event was sent, usage should stay unfound")
	}
}

// TestScanUsage_SpaceAfterColon asserts the optional space after data: is
// tolerated in both spellings.
func TestScanUsage_SpaceAfterColon(t *testing.T) {
	withSpace := &scanUsage{}
	withSpace.feed([]byte(`data: {"type":"message_delta","usage":{"output_tokens":9}}` + "\n"))
	noSpace := &scanUsage{}
	noSpace.feed([]byte(`data:{"type":"message_delta","usage":{"output_tokens":9}}` + "\n"))

	for name, s := range map[string]*scanUsage{"with space": withSpace, "no space": noSpace} {
		if !s.found || s.usage.outputTokens != 9 {
			t.Errorf("%s: usage = %+v, found = %v", name, s.usage, s.found)
		}
	}
}

// TestScanUsage_CRLF asserts carriage returns from CRLF line endings are
// stripped before the data: prefix match.
func TestScanUsage_CRLF(t *testing.T) {
	s := &scanUsage{}
	s.feed([]byte("data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":4}}\r\n"))

	if !s.found || s.usage.outputTokens != 4 {
		t.Errorf("usage = %+v, found = %v", s.usage, s.found)
	}
}

func TestJsonUsage(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantNil    bool
		wantInput  int
		wantOutput int
	}{
		{
			name:       "usage object with cache fields",
			body:       `{"id":"msg_1","usage":{"input_tokens":10,"cache_read_input_tokens":900,"cache_creation_input_tokens":90,"output_tokens":7}}`,
			wantInput:  1000,
			wantOutput: 7,
		},
		{name: "no usage", body: `{"type":"message"}`, wantNil: true},
		{name: "not json", body: `oops`, wantNil: true},
		{name: "empty body", body: ``, wantNil: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usage := jsonUsage([]byte(tt.body))
			if tt.wantNil {
				if usage != nil {
					t.Fatalf("usage = %+v, want nil", usage)
				}
				return
			}
			if usage == nil {
				t.Fatal("usage = nil")
			}
			if usage.inputTokens != tt.wantInput || usage.outputTokens != tt.wantOutput {
				t.Errorf("usage = %+v, want input %d output %d", usage, tt.wantInput, tt.wantOutput)
			}
		})
	}
}
