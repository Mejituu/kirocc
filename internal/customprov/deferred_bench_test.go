package customprov

import (
	"fmt"
	"strings"
	"testing"
)

// benchmarkRawRequest builds a ~1MB Messages request shaped like a real long
// Claude Code conversation: alternating user/assistant text turns and a large
// tools array. withSystem inserts one role:system message outside any
// tool_use/tool_result gap, the shape that forces the full deferred scan.
func benchmarkRawRequest(withSystem bool) []byte {
	var b strings.Builder
	b.WriteString(`{"model":"claude-x","max_tokens":4096,"messages":[`)
	text := strings.Repeat("x", 8000)
	const turns = 60
	for i := 0; i < turns; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"role":"user","content":"%s"},{"role":"assistant","content":"%s"}`, text, text)
		if withSystem && i == turns/2 {
			b.WriteString(`,{"role":"system","content":"Reminder: the user's timezone is UTC+8."}`)
		}
	}
	b.WriteString(`],"tools":[`)
	for i := 0; i < 30; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"name":"tool_%d","description":"%s","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}`, i, strings.Repeat("d", 1000))
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

func BenchmarkRewriteForward_NoDeferred(b *testing.B) {
	raw := benchmarkRawRequest(false)
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := RewriteForward(raw, "claude-x", false, mediaPolicy{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRewriteLegacyPath_NoDeferred(b *testing.B) {
	raw := benchmarkRawRequest(false)
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := rewrite(raw, "claude-x", true, false, mediaPolicy{}, deferredPolicy{}, deferredPlan{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRewriteForward_NoDeferredWithSystem(b *testing.B) {
	raw := benchmarkRawRequest(true)
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := RewriteForward(raw, "claude-x", false, mediaPolicy{}); err != nil {
			b.Fatal(err)
		}
	}
}

// benchmarkGapMoveRequest builds a ~1MB request whose only change is rule 7: a
// system message between an assistant's tool_use and the user's matching
// tool_result. It carries no deferred-tool block, so the fast path plans the
// move from the role sequence and the raw byte ranges.
func benchmarkGapMoveRequest() []byte {
	text := strings.Repeat("x", 8000)
	var b strings.Builder
	b.WriteString(`{"model":"claude-x","max_tokens":4096,"messages":[`)
	fmt.Fprintf(&b, `{"role":"user","content":"%s"},`, text)
	fmt.Fprintf(&b, `{"role":"assistant","content":[{"type":"text","text":"%s"},{"type":"tool_use","id":"t1","name":"probe","input":{}}]},`, text)
	b.WriteString(`{"role":"system","content":"Reminder: the user's timezone is UTC+8."},`)
	fmt.Fprintf(&b, `{"role":"user","content":[{"type":"text","text":"%s"},{"type":"tool_result","tool_use_id":"t1","content":"%s"}]}`, text, text)
	const turns = 58
	for i := 0; i < turns; i++ {
		fmt.Fprintf(&b, `,{"role":"user","content":"%s"},{"role":"assistant","content":"%s"}`, text, text)
	}
	b.WriteString(`],"tools":[`)
	for i := 0; i < 30; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"name":"tool_%d","description":"%s","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}`, i, strings.Repeat("d", 1000))
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

func BenchmarkRewriteForward_NoDeferredWithGapMove(b *testing.B) {
	raw := benchmarkGapMoveRequest()
	if plan := analyzeDeferred(raw); !plan.messagesChanged {
		b.Fatal("benchmark request must require a rule 7 move")
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := RewriteForward(raw, "claude-x", false, mediaPolicy{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAnalyzeDeferred_NoDeferred(b *testing.B) {
	raw := benchmarkRawRequest(false)
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if plan := analyzeDeferred(raw); !plan.empty() {
			b.Fatal("unexpected plan for a body without deferred blocks")
		}
	}
}

func BenchmarkAnalyzeDeferred_NoDeferredWithSystem(b *testing.B) {
	raw := benchmarkRawRequest(true)
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if plan := analyzeDeferred(raw); !plan.empty() {
			b.Fatal("unexpected plan for a body without deferred blocks")
		}
	}
}
