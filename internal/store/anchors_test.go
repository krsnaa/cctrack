package store_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ksred/cctrack/internal/store"
)

// TestObservedCostForWindow exercises the shared cost-window helper used by
// both the manual-sync POST /window-anchors handler and the auto-sync
// scheduler (F2 S2.2). The half-open interval [resetsAt - duration,
// observedAt) is verified at both endpoints to catch off-by-one regressions.
func TestObservedCostForWindow(t *testing.T) {
	s := newTestStore(t)
	f := newFixtureBuilder(t, s)

	// Reference clock: 12:00 UTC. WindowType "5h" -> duration 5h.
	// resets_at = 14:00 (2h into the future).
	// observed_at = 12:00.
	// Window: [resets_at - 5h, observed_at) = [09:00, 12:00).
	observedAt := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	resetsAt := observedAt.Add(2 * time.Hour)

	// Outside-left (before window start): 08:59:59 — excluded.
	f.ingest("s-before", "p", observedAt.Add(-3*time.Hour-time.Second), 1.0, 100, 100)
	// Boundary-left (window start, inclusive): 09:00:00 — included.
	f.ingest("s-left", "p", observedAt.Add(-3*time.Hour), 2.0, 100, 100)
	// Mid-window: 11:00:00 — included.
	f.ingest("s-mid", "p", observedAt.Add(-1*time.Hour), 4.0, 100, 100)
	// Boundary-right (observed_at, exclusive per half-open): 12:00:00 — excluded.
	f.ingest("s-right", "p", observedAt, 8.0, 100, 100)
	// Outside-right (after observed_at): 12:00:01 — excluded.
	f.ingest("s-after", "p", observedAt.Add(time.Second), 16.0, 100, 100)

	got, err := s.ObservedCostForWindow("5h", observedAt, resetsAt)
	if err != nil {
		t.Fatalf("ObservedCostForWindow: %v", err)
	}
	want := 2.0 + 4.0 // s-left + s-mid
	if !floatEq(got, want) {
		t.Errorf("ObservedCostForWindow = %v, want %v (half-open [09:00, 12:00))", got, want)
	}
}

// TestObservedCostForWindow_SevenDayDuration verifies the 7d window type
// resolves to 7*24h, not the 5h default.
func TestObservedCostForWindow_SevenDayDuration(t *testing.T) {
	s := newTestStore(t)
	f := newFixtureBuilder(t, s)

	observedAt := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	resetsAt := observedAt.Add(2 * time.Hour) // 2026-05-06 14:00

	// 7d window: [resets_at - 7d, observed_at) = [2026-04-29 14:00, 2026-05-06 12:00).
	f.ingest("s-d6-ago", "p", observedAt.Add(-6*24*time.Hour), 100.0, 100, 100) // included
	f.ingest("s-d8-ago", "p", observedAt.Add(-8*24*time.Hour), 200.0, 100, 100) // excluded (past 7d)

	got, err := s.ObservedCostForWindow("7d", observedAt, resetsAt)
	if err != nil {
		t.Fatalf("ObservedCostForWindow 7d: %v", err)
	}
	if !floatEq(got, 100.0) {
		t.Errorf("ObservedCostForWindow 7d = %v, want 100.0 (only the 6-day-ago request)", got)
	}
}

// TestObservedCostForWindow_UnknownType ensures programming errors fail loudly
// rather than silently returning zero.
func TestObservedCostForWindow_UnknownType(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	_, err := s.ObservedCostForWindow("unknown", now, now)
	if err == nil {
		t.Fatalf("expected error for unknown window type, got nil")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("error %q does not mention 'unknown'", err.Error())
	}
}

// TestWindowDuration is a small standalone check so callers can rely on the
// canonical mapping without round-tripping through ObservedCostForWindow.
func TestWindowDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"5h", 5 * time.Hour},
		{"7d", 7 * 24 * time.Hour},
		{"unknown", 0},
		{"", 0},
	}
	for _, c := range cases {
		got := store.WindowDuration(c.in)
		if got != c.want {
			t.Errorf("WindowDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
