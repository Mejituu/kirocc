package kirousage

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/d-kuro/kirocc/internal/kiroclient"
)

func fixedLimits(used, limit float64) *Limits {
	return &Limits{
		Plan:    "KIRO PRO",
		Base:    Bucket{Used: used, Limit: limit},
		ResetAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local),
	}
}

func TestMonitor_RecordWithoutLimits(t *testing.T) {
	m := NewMonitor()
	snap := m.Record("claude-sonnet-4.6", 100, 10, 1.5)
	if snap.SessionCredits != 1.5 || snap.HasRemaining {
		t.Fatalf("snapshot = %+v, want credits only", snap)
	}
}

func TestMonitor_RemainingEstimate(t *testing.T) {
	m := NewMonitor()
	fetch := func(context.Context) (*Limits, error) { return fixedLimits(800, 1000), nil }

	// Spend before the fetch is already reflected in the fetched figures and
	// must not be subtracted again.
	m.Record("a", 0, 0, 5)
	if err := m.Refresh(context.Background(), fetch); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	snap := m.Record("a", 0, 0, 2)
	if snap.SessionCredits != 7 {
		t.Errorf("SessionCredits = %v, want 7", snap.SessionCredits)
	}
	if !snap.HasRemaining || snap.RemainingEst != 198 {
		t.Errorf("RemainingEst = %v (has=%v), want 198", snap.RemainingEst, snap.HasRemaining)
	}

	m.RecordAborted("a", 500)
	if snap := m.Record("a", 0, 0, 0); snap.RemainingEst != 0 {
		t.Errorf("RemainingEst = %v, want floored at 0", snap.RemainingEst)
	}
}

func TestMonitor_RefreshError(t *testing.T) {
	m := NewMonitor()
	fetch := func(context.Context) (*Limits, error) { return nil, errors.New("boom") }
	if err := m.Refresh(context.Background(), fetch); err == nil {
		t.Fatal("Refresh: want error")
	}
	if snap := m.Record("a", 0, 0, 1); snap.HasRemaining {
		t.Fatal("failed refresh must not produce a remaining estimate")
	}
}

func TestMonitor_ConcurrentRecord(t *testing.T) {
	m := NewMonitor()
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			model := "a"
			if i%2 == 0 {
				model = "b"
			}
			m.Record(model, 1, 1, 0.5)
			m.RecordAborted(model, 0.5)
		})
	}
	wg.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.total.Requests != 50 || m.total.Credits != 50 || m.total.InputTokens != 50 {
		t.Fatalf("total = %+v", m.total)
	}
	if m.byModel["a"].Requests+m.byModel["b"].Requests != 50 {
		t.Fatalf("per-model requests do not sum to total: %+v %+v", m.byModel["a"], m.byModel["b"])
	}
}

func TestMonitor_QuotaExhausted(t *testing.T) {
	m := NewMonitor()
	fetch := func(context.Context) (*Limits, error) { return fixedLimits(1000, 1000), nil }
	if err := m.Refresh(context.Background(), fetch); err != nil {
		t.Fatal(err)
	}

	msg := m.QuotaExhausted(context.Background(), kiroclient.ReasonMonthlyRequestCount)
	for _, want := range []string{"quota exhausted", kiroclient.ReasonMonthlyRequestCount, "KIRO PRO", "resets at 2026-11-01 00:00:00"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
	// The monthly reset date says nothing about a daily cap.
	if msg := m.QuotaExhausted(context.Background(), kiroclient.ReasonDailyRequestCount); strings.Contains(msg, "resets at") {
		t.Errorf("daily refusal must not claim the monthly reset: %q", msg)
	}
}

func TestMonitor_RefreshBaselinePrecedesFetch(t *testing.T) {
	m := NewMonitor()
	fetch := func(context.Context) (*Limits, error) {
		// A response completing while the query is in flight.
		m.Record("a", 0, 0, 3)
		return fixedLimits(500, 1000), nil
	}
	if err := m.Refresh(context.Background(), fetch); err != nil {
		t.Fatal(err)
	}
	if snap := m.Record("a", 0, 0, 0); snap.RemainingEst != 497 {
		t.Fatalf("RemainingEst = %v, want 497 (in-flight spend subtracted)", snap.RemainingEst)
	}
}

func TestMonitor_QuotaExhaustedWithoutLimits(t *testing.T) {
	msg := NewMonitor().QuotaExhausted(context.Background(), "")
	if msg != "Kiro credit quota exhausted" {
		t.Fatalf("message = %q", msg)
	}
}
