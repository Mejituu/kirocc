package timing

import (
	"context"
	"testing"
	"time"
)

// attrMap turns a LogArgs slice into a map for field lookups.
func attrMap(args []any) map[string]any {
	m := make(map[string]any, len(args)/2)
	for i := 0; i < len(args); i += 2 {
		m[args[i].(string)] = args[i+1]
	}
	return m
}

func TestMarkFirst_RecordsOnlyTheFirst(t *testing.T) {
	_, r := Start(context.Background())
	r.MarkFirst()
	first := r.first
	time.Sleep(5 * time.Millisecond)
	r.MarkFirst()
	if !r.first.Equal(first) {
		t.Errorf("second MarkFirst moved the first byte: %v -> %v", first, r.first)
	}
}

func TestResetFirst_AllowsRemarkingStartUnchanged(t *testing.T) {
	_, r := Start(context.Background())
	start := r.start
	r.MarkFirst()
	first := r.first
	r.ResetFirst()
	if !r.first.IsZero() {
		t.Fatalf("ResetFirst left a first byte: %v", r.first)
	}
	time.Sleep(5 * time.Millisecond)
	r.MarkFirst()
	if !r.first.After(first) {
		t.Errorf("re-marked first %v should be after the original %v", r.first, first)
	}
	if !r.start.Equal(start) {
		t.Errorf("ResetFirst moved the start: %v -> %v", start, r.start)
	}
}

func TestStart_ReusesContextRequest(t *testing.T) {
	ctx, r := Start(context.Background())
	ctx2, r2 := Start(ctx)
	if r2 != r {
		t.Error("Start must reuse the Request already in the context")
	}
	if From(ctx2) != r {
		t.Error("derived context lost the Request")
	}
	if From(context.Background()) != nil {
		t.Error("From must return nil without a Request")
	}
}

func TestLogArgs_StreamingFromFirstByte(t *testing.T) {
	_, r := Start(context.Background())
	r.start = time.Now().Add(-210 * time.Millisecond)
	r.first = r.start.Add(10 * time.Millisecond)
	now := r.start.Add(210 * time.Millisecond)

	args := r.LogArgs(100, now, true)
	m := attrMap(args)
	if got, want := m["ttft"].(time.Duration), 10*time.Millisecond; got != want {
		t.Errorf("ttft = %v, want %v", got, want)
	}
	if got, want := m["duration"].(time.Duration), 210*time.Millisecond; got != want {
		t.Errorf("duration = %v, want %v", got, want)
	}
	if got, want := m["tps"].(float64), 500.0; got != want {
		t.Errorf("tps = %v, want %v", got, want)
	}
	if _, has := m["tps_total"]; has {
		t.Errorf("streaming must not report tps_total: %v", m)
	}
}

func TestLogArgs_SingleOutputTokenHasNoTps(t *testing.T) {
	_, r := Start(context.Background())
	r.start = time.Now().Add(-210 * time.Millisecond)
	r.first = r.start.Add(10 * time.Millisecond)

	m := attrMap(r.LogArgs(1, r.start.Add(210*time.Millisecond), true))
	if _, has := m["tps"]; has {
		t.Errorf("output_tokens=1 must suppress tps: %v", m)
	}
	if _, has := m["ttft"]; !has {
		t.Errorf("ttft should still be reported: %v", m)
	}
	if _, has := m["duration"]; !has {
		t.Errorf("duration should still be reported: %v", m)
	}
}

func TestLogArgs_StreamingWithoutFirstByte(t *testing.T) {
	_, r := Start(context.Background())
	r.start = time.Now().Add(-210 * time.Millisecond)

	m := attrMap(r.LogArgs(100, r.start.Add(210*time.Millisecond), true))
	for _, name := range []string{"ttft", "tps", "tps_total"} {
		if _, has := m[name]; has {
			t.Errorf("streaming without a first byte must not report %s: %v", name, m)
		}
	}
	if _, has := m["duration"]; !has {
		t.Errorf("duration should still be reported: %v", m)
	}
}

func TestLogArgs_NonStreamingTpsTotal(t *testing.T) {
	_, r := Start(context.Background())
	r.start = time.Now().Add(-210 * time.Millisecond)

	args := r.LogArgs(100, r.start.Add(210*time.Millisecond), false)
	m := attrMap(args)
	if _, has := m["ttft"]; has {
		t.Errorf("non-streaming must not report ttft: %v", m)
	}
	if got, want := m["tps_total"].(float64), 476.2; got != want {
		t.Errorf("tps_total = %v, want %v", got, want)
	}
	if _, has := m["tps"]; has {
		t.Errorf("non-streaming must not report tps: %v", m)
	}
	if got, want := m["duration"].(time.Duration), 210*time.Millisecond; got != want {
		t.Errorf("duration = %v, want %v", got, want)
	}
	// DurationArgs carries the same rounding.
	d := attrMap(r.DurationArgs(r.start.Add(210 * time.Millisecond)))
	if got, want := d["duration"].(time.Duration), 210*time.Millisecond; got != want {
		t.Errorf("DurationArgs duration = %v, want %v", got, want)
	}
}

func TestLogArgs_SubMillisecondSpanHasNoSpeed(t *testing.T) {
	_, r := Start(context.Background())
	r.start = time.Now().Add(-210 * time.Millisecond)
	r.first = r.start.Add(210*time.Millisecond - 100*time.Microsecond)

	m := attrMap(r.LogArgs(100, r.start.Add(210*time.Millisecond), true))
	if _, has := m["tps"]; has {
		t.Errorf("sub-millisecond generation span must suppress tps: %v", m)
	}
}

func TestNilRequestIsSafe(t *testing.T) {
	var r *Request
	r.ResetFirst()
	r.MarkFirst()
	if got := r.LogArgs(100, time.Now(), true); got != nil {
		t.Errorf("LogArgs on nil = %v, want nil", got)
	}
	if got := r.DurationArgs(time.Now()); got != nil {
		t.Errorf("DurationArgs on nil = %v, want nil", got)
	}
}
