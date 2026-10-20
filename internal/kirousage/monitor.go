package kirousage

import (
	"context"
	"log/slog"
	"maps"
	"math"
	"slices"
	"sync"

	"github.com/d-kuro/kirocc/internal/kiroclient"
)

// Monitor holds the last known usage limits and the credits this process has
// spent, so per-request logs can show a running total and an estimate of what
// is left without querying upstream on every request.
type Monitor struct {
	mu     sync.Mutex
	limits *Limits
	// creditsAtFetch is the process total when limits was fetched; credits
	// spent after that are subtracted from limits.Remaining() for the estimate.
	creditsAtFetch float64
	total          Totals
	byModel        map[string]*Totals
}

// Totals is the consumption accumulated by this process.
type Totals struct {
	Requests     int
	Credits      float64
	InputTokens  int
	OutputTokens int
}

// NewMonitor creates a Monitor. Until Refresh succeeds it keeps only the
// process totals.
func NewMonitor() *Monitor {
	return &Monitor{byModel: make(map[string]*Totals)}
}

// Refresh fetches the limits with fetch, stores them and logs the usage summary.
func (m *Monitor) Refresh(ctx context.Context, fetch func(context.Context) (*Limits, error)) error {
	// Take the baseline before the call: spend recorded while it is in flight
	// may not be reflected upstream yet, and counting it as already included
	// would overstate the remaining estimate.
	m.mu.Lock()
	baseline := m.total.Credits
	m.mu.Unlock()
	l, err := fetch(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.limits = l
	m.creditsAtFetch = baseline
	m.mu.Unlock()
	l.log()
	return nil
}

// Snapshot is the running position after recording a response.
type Snapshot struct {
	SessionCredits float64
	// RemainingEst is the last fetched remaining credits minus what this
	// process spent since; valid only when HasRemaining is true.
	RemainingEst float64
	HasRemaining bool
}

// Record adds one completed response to the process totals.
func (m *Monitor) Record(model string, inputTokens, outputTokens int, credits float64) Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	add := func(t *Totals) {
		t.Requests++
		t.Credits += credits
		t.InputTokens += inputTokens
		t.OutputTokens += outputTokens
	}
	add(&m.total)
	add(m.modelTotals(model))
	return m.snapshotLocked()
}

// RecordAborted adds credits spent by an upstream attempt the proxy discarded
// (e.g. before a transparent retry). It is real spend but not a request.
func (m *Monitor) RecordAborted(model string, credits float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.total.Credits += credits
	m.modelTotals(model).Credits += credits
}

func (m *Monitor) modelTotals(model string) *Totals {
	t, ok := m.byModel[model]
	if !ok {
		t = &Totals{}
		m.byModel[model] = t
	}
	return t
}

func (m *Monitor) snapshotLocked() Snapshot {
	s := Snapshot{SessionCredits: m.total.Credits}
	if m.limits != nil && m.limits.Limit() > 0 {
		s.RemainingEst = max(m.limits.Remaining()-(m.total.Credits-m.creditsAtFetch), 0)
		s.HasRemaining = true
	}
	return s
}

// QuotaExhausted logs that upstream refused a request for lack of credits and
// returns a client-facing message built from the limits fetched at startup;
// running out does not move the reset date, so no re-query is needed. reason
// is the upstream throttling reason (kiroclient.ReasonMonthlyRequestCount /
// ReasonDailyRequestCount, or empty for a bare 402).
func (m *Monitor) QuotaExhausted(ctx context.Context, reason string) string {
	m.mu.Lock()
	l := m.limits
	m.mu.Unlock()

	msg := "Kiro credit quota exhausted"
	args := []any{}
	if reason != "" {
		msg += " (" + reason + ")"
		args = append(args, "reason", reason)
	}
	if l != nil {
		if l.Plan != "" {
			msg += " on plan " + l.Plan
			args = append(args, "plan", l.Plan)
		}
		// The reported reset date is the monthly one; it says nothing about
		// when a daily cap lifts.
		if !l.ResetAt.IsZero() && reason != kiroclient.ReasonDailyRequestCount {
			msg += "; resets at " + formatTime(l.ResetAt)
			args = append(args, "resets_at", formatTime(l.ResetAt))
		}
	}
	slog.WarnContext(ctx, "kiro quota exhausted", args...)
	return msg
}

// LogSummary logs the process totals, overall and per model.
func (m *Monitor) LogSummary() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.total.Requests == 0 && m.total.Credits == 0 {
		return
	}
	slog.Info("session usage", totalsArgs(m.total)...)
	for _, model := range slices.Sorted(maps.Keys(m.byModel)) {
		slog.Info("session usage", append([]any{"model", model}, totalsArgs(*m.byModel[model])...)...)
	}
}

func totalsArgs(t Totals) []any {
	return []any{
		"requests", t.Requests,
		"credits", round3(t.Credits),
		"input_tokens", t.InputTokens,
		"output_tokens", t.OutputTokens,
	}
}

// round3 matches the 3-decimal rounding of per-request credit logs.
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
