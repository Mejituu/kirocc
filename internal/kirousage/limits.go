package kirousage

import (
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"
)

// statusActive marks a free trial or bonus grant that currently adds credits.
const statusActive = "ACTIVE"

// Limits is the account's credit position as reported by GetUsageLimits.
type Limits struct {
	Plan  string // subscription title, e.g. "KIRO PRO+"
	Email string

	// ResetAt is when the monthly quota resets; zero when not reported.
	ResetAt time.Time

	// Base is the plan's own monthly allowance.
	Base      Bucket
	FreeTrial *Grant
	Bonuses   []Grant

	// Overage (pay-as-you-go beyond the limit), when the account has it.
	OverageStatus  string
	Overages       float64
	OverageCharges float64
	Currency       string
}

// Bucket is a used/limit pair in credits.
type Bucket struct {
	Used  float64
	Limit float64
}

// Grant is a free trial or bonus credit allotment layered on top of the plan.
type Grant struct {
	Status    string // ACTIVE / EXPIRED
	Used      float64
	Limit     float64
	ExpiresAt time.Time // free trial only; zero when not reported
}

// Active reports whether the grant currently contributes credits.
func (g Grant) Active() bool { return strings.EqualFold(g.Status, statusActive) }

// activeGrants yields the free trial (when present) and bonuses that are active.
func (l *Limits) activeGrants() []Grant {
	var out []Grant
	if l.FreeTrial != nil && l.FreeTrial.Active() {
		out = append(out, *l.FreeTrial)
	}
	for _, b := range l.Bonuses {
		if b.Active() {
			out = append(out, b)
		}
	}
	return out
}

// Limit is the total spendable allowance: the plan plus active grants.
func (l *Limits) Limit() float64 {
	total := l.Base.Limit
	for _, g := range l.activeGrants() {
		total += g.Limit
	}
	return total
}

// Used is the total consumed across the plan and active grants.
func (l *Limits) Used() float64 {
	total := l.Base.Used
	for _, g := range l.activeGrants() {
		total += g.Used
	}
	return total
}

// Remaining is Limit minus Used, floored at zero.
func (l *Limits) Remaining() float64 { return max(l.Limit()-l.Used(), 0) }

// log emits the startup usage summary and, when the account has trial or bonus
// grants or overage, one line per component.
func (l *Limits) log() {
	args := []any{}
	if l.Plan != "" {
		args = append(args, "plan", l.Plan)
	}
	limit := l.Limit()
	args = append(args,
		"used", round2(l.Used()),
		"limit", round2(limit),
		"remaining", round2(l.Remaining()),
	)
	if limit > 0 {
		args = append(args, "used_pct", fmt.Sprintf("%.1f%%", l.Used()*100/limit))
	}
	if !l.ResetAt.IsZero() {
		args = append(args, "resets_at", formatTime(l.ResetAt))
	}
	if l.Email != "" {
		args = append(args, "email", l.Email)
	}
	slog.Info("usage", args...)

	if l.FreeTrial != nil || len(l.Bonuses) > 0 {
		slog.Info("usage breakdown", "kind", "base", "used", round2(l.Base.Used), "limit", round2(l.Base.Limit))
		if t := l.FreeTrial; t != nil {
			slog.Info("usage breakdown", grantArgs("free_trial", *t)...)
		}
		for _, b := range l.Bonuses {
			slog.Info("usage breakdown", grantArgs("bonus", b)...)
		}
	}
	if l.OverageStatus != "" || l.Overages > 0 {
		args := []any{"status", l.OverageStatus, "overages", round2(l.Overages)}
		if l.OverageCharges > 0 {
			args = append(args, "charges", round2(l.OverageCharges), "currency", l.Currency)
		}
		slog.Info("usage overage", args...)
	}
}

func grantArgs(kind string, g Grant) []any {
	args := []any{"kind", kind, "status", g.Status, "used", round2(g.Used), "limit", round2(g.Limit)}
	if !g.ExpiresAt.IsZero() {
		args = append(args, "expires_at", formatTime(g.ExpiresAt))
	}
	return args
}

// round2 rounds credits to 2 decimals; upstream precision values are noisy.
func round2(v float64) float64 { return math.Round(v*100) / 100 }

// formatTime renders a timestamp in local time, matching the console log clock.
func formatTime(t time.Time) string { return t.Local().Format(time.DateTime) }
