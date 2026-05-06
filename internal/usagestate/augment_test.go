package usagestate

import (
	"errors"
	"testing"
	"time"

	"github.com/ksred/cctrack/internal/store"
	"github.com/ksred/cctrack/internal/usagescheduler"
)

// fakeSummaryStore implements SummaryStore for SummaryProvider tests
// without spinning up a real DB.
type fakeSummaryStore struct {
	summary       *store.Summary
	summaryErr    error
	fiveAnchor    *store.WindowAnchor
	sevenAnchor   *store.WindowAnchor
	anchorErrFor  map[string]error
}

func (f *fakeSummaryStore) GetSummary() (*store.Summary, error) {
	if f.summaryErr != nil {
		return nil, f.summaryErr
	}
	return f.summary, nil
}

func (f *fakeSummaryStore) GetLatestAnchor(windowType string) (*store.WindowAnchor, error) {
	if err := f.anchorErrFor[windowType]; err != nil {
		return nil, err
	}
	switch windowType {
	case "5h":
		return f.fiveAnchor, nil
	case "7d":
		return f.sevenAnchor, nil
	}
	return nil, nil
}

type fakeSchedSnapshotter struct {
	state usagescheduler.State
}

func (f *fakeSchedSnapshotter) Snapshot() usagescheduler.State { return f.state }

func freshSummary() *store.Summary {
	return &store.Summary{
		Window5h: store.WindowBucket{Cost: 1.0},
		Window7d: store.WindowBucket{Cost: 2.0},
	}
}

func TestSummaryProvider_BuildPopulatesStateFields(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	st := &fakeSummaryStore{
		summary:     freshSummary(),
		fiveAnchor:  validAnchor(now.Add(-30*time.Minute), 240), // future reset
		sevenAnchor: validAnchor(now.Add(-1*time.Hour), 7*24*60), // future reset
	}
	sched := &fakeSchedSnapshotter{state: usagescheduler.State{
		Running:            true,
		LastFetchSucceeded: now.Add(-1 * time.Minute),
		LastErrorClass:     usagescheduler.ErrorClassNone,
	}}
	p := NewSummaryProvider(st, sched).WithClock(func() time.Time { return now })

	got, err := p.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got.Window5h.State == nil {
		t.Fatal("Window5h.State is nil; want populated")
	}
	if *got.Window5h.State != "auto_fresh" {
		t.Errorf("Window5h.State = %q, want auto_fresh", *got.Window5h.State)
	}
	if got.Window7d.State == nil {
		t.Fatal("Window7d.State is nil; want populated")
	}
	if *got.Window7d.State != "auto_fresh" {
		t.Errorf("Window7d.State = %q, want auto_fresh", *got.Window7d.State)
	}
}

func TestSummaryProvider_BuildNoAnchorsReturnsFallbackCascade(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	st := &fakeSummaryStore{summary: freshSummary()} // both anchors nil
	sched := &fakeSchedSnapshotter{state: usagescheduler.State{Running: true, LastFetchSucceeded: now}}
	p := NewSummaryProvider(st, sched).WithClock(func() time.Time { return now })

	got, err := p.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if *got.Window5h.State != "fallback_cascade" {
		t.Errorf("Window5h.State = %q, want fallback_cascade", *got.Window5h.State)
	}
	if *got.Window7d.State != "fallback_cascade" {
		t.Errorf("Window7d.State = %q, want fallback_cascade", *got.Window7d.State)
	}
}

func TestSummaryProvider_BuildSummaryErrorPropagates(t *testing.T) {
	wantErr := errors.New("simulated GetSummary failure")
	st := &fakeSummaryStore{summaryErr: wantErr}
	sched := &fakeSchedSnapshotter{}
	p := NewSummaryProvider(st, sched)

	got, err := p.Build()
	if !errors.Is(err, wantErr) {
		t.Errorf("Build err = %v, want %v", err, wantErr)
	}
	if got != nil {
		t.Errorf("Build returned non-nil summary on GetSummary error: %v", got)
	}
}

// TestSummaryProvider_BuildAnchorErrorIsTreatedAsNoAnchor verifies the
// "graceful degradation" choice: if GetLatestAnchor fails for one window
// (transient DB hiccup, etc.), the derivation treats that window as if no
// anchor existed, yielding fallback_cascade for it. The other window is
// unaffected. This prevents a transient anchor-read error from breaking
// the dashboard.
func TestSummaryProvider_BuildAnchorErrorIsTreatedAsNoAnchor(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	st := &fakeSummaryStore{
		summary:    freshSummary(),
		sevenAnchor: validAnchor(now.Add(-1*time.Hour), 7*24*60),
		anchorErrFor: map[string]error{
			"5h": errors.New("simulated 5h anchor read failure"),
		},
	}
	sched := &fakeSchedSnapshotter{state: usagescheduler.State{
		Running:            true,
		LastFetchSucceeded: now.Add(-1 * time.Minute),
	}}
	p := NewSummaryProvider(st, sched).WithClock(func() time.Time { return now })

	got, err := p.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if *got.Window5h.State != "fallback_cascade" {
		t.Errorf("Window5h.State = %q, want fallback_cascade (anchor err treated as no anchor)", *got.Window5h.State)
	}
	if *got.Window7d.State != "auto_fresh" {
		t.Errorf("Window7d.State = %q, want auto_fresh (other window unaffected)", *got.Window7d.State)
	}
}
