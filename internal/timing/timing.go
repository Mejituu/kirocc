// Package timing tracks per-request latency for the response log: the start
// is when kirocc received the request, the first byte is when the upstream
// first produced content. The Kiro path and the custom provider path share
// one Request, carried in the request context.
package timing

import (
	"context"
	"math"
	"sync"
	"time"
)

type contextKey struct{}

// Request measures one proxied request. A nil *Request is valid: every
// method is a no-op and LogArgs reports nothing.
type Request struct {
	start time.Time

	mu    sync.Mutex
	first time.Time
}

// Start returns a context carrying a Request started now, reusing the one
// already present. The returned Request is never nil.
func Start(ctx context.Context) (context.Context, *Request) {
	if r, ok := ctx.Value(contextKey{}).(*Request); ok && r != nil {
		return ctx, r
	}
	r := &Request{start: time.Now()}
	return context.WithValue(ctx, contextKey{}, r), r
}

// From returns the Request carried by ctx, or nil when there is none.
func From(ctx context.Context) *Request {
	r, _ := ctx.Value(contextKey{}).(*Request)
	return r
}

// ResetFirst forgets the recorded first byte, keeping the start. The retry
// path calls it at each attempt so a discarded attempt's first byte does not
// count; the start stays the first request's, so the discarded time still
// shows up in ttft and duration.
func (r *Request) ResetFirst() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.first = time.Time{}
}

// MarkFirst records the first byte's time; later calls change nothing.
func (r *Request) MarkFirst() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.first.IsZero() {
		r.first = time.Now()
	}
}

// LogArgs returns the latency log attributes for a response finished at now:
// duration always, ttft when a first byte was recorded, and the generation
// speed — tps over the span from the first byte for a streaming response
// (omitted without a first byte), tps_total over the whole request for a
// non-streaming one. The speed fields are omitted when outputTokens is at
// most 1 or the measuring span is under a millisecond.
func (r *Request) LogArgs(outputTokens int, now time.Time, streaming bool) []any {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	first := r.first
	r.mu.Unlock()

	var args []any
	if !first.IsZero() {
		args = append(args, "ttft", first.Sub(r.start).Round(time.Millisecond))
	}
	span := now.Sub(r.start)
	if streaming {
		// Without a first byte there is no generation span to measure.
		span = 0
		if !first.IsZero() {
			span = now.Sub(first)
		}
	}
	if outputTokens > 1 && span >= time.Millisecond {
		name := "tps_total"
		if streaming {
			name = "tps"
		}
		args = append(args, name, tpsValue(outputTokens, span))
	}
	args = append(args, "duration", now.Sub(r.start).Round(time.Millisecond))
	return args
}

// DurationArgs returns only the rounded total duration, for responses that
// carry no speed signal (e.g. provider error bodies).
func (r *Request) DurationArgs(now time.Time) []any {
	if r == nil {
		return nil
	}
	return []any{"duration", now.Sub(r.start).Round(time.Millisecond)}
}

// tpsValue renders tokens per second with one decimal.
func tpsValue(tokens int, span time.Duration) float64 {
	return math.Round(float64(tokens)/span.Seconds()*10) / 10
}
