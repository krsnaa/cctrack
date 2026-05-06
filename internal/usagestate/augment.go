package usagestate

import (
	"time"

	"github.com/ksred/cctrack/internal/store"
	"github.com/ksred/cctrack/internal/usagescheduler"
)

// SummaryStore is the slice of *store.Store SummaryProvider needs. Defining
// it as an interface keeps SummaryProvider test-fakeable without spinning
// up a real DB.
type SummaryStore interface {
	GetSummary() (*store.Summary, error)
	GetLatestAnchor(windowType string) (*store.WindowAnchor, error)
}

// SchedulerSnapshotter is the slice of *usagescheduler.Scheduler the
// provider needs. Just the read-only Snapshot accessor.
type SchedulerSnapshotter interface {
	Snapshot() usagescheduler.State
}

// SummaryProvider is the SINGLE CHOKEPOINT for emitting augmented
// /api/v1/summary payloads. All four summary-emission paths (REST,
// websocket-initial, watcher broadcast, scheduler broadcast) MUST call
// Build() rather than store.GetSummary() directly. Per F2 S2.3 EM
// ruling chat msg 20621: a single path prevents websocket events from
// silently dropping honest-state fields.
//
// Build's behavior is:
//
//  1. Fetch the cost-data summary via SummaryStore.GetSummary.
//  2. Fetch the latest WindowAnchor for each window type.
//  3. Snapshot the scheduler's runtime state.
//  4. Run DeriveWindowState for each window and populate the
//     additive State field on Window5h / Window7d.
//
// Augmentation is mutating: Build returns the same *Summary it received
// from GetSummary, with State fields populated. Callers should not
// rely on a fresh allocation per call.
type SummaryProvider struct {
	store     SummaryStore
	scheduler SchedulerSnapshotter
	now       func() time.Time
}

// NewSummaryProvider constructs a SummaryProvider with production
// defaults: time.Now for the clock. Tests can substitute via WithClock.
func NewSummaryProvider(s SummaryStore, sched SchedulerSnapshotter) *SummaryProvider {
	return &SummaryProvider{
		store:     s,
		scheduler: sched,
		now:       time.Now,
	}
}

// WithClock overrides the default clock; only useful for tests. Returns
// the provider for chained construction.
func (p *SummaryProvider) WithClock(now func() time.Time) *SummaryProvider {
	p.now = now
	return p
}

// Build returns the cost-data summary augmented with per-window honest
// state. Errors from GetSummary surface; per-window anchor errors are
// silently treated as "no anchor for that window" (the derivation then
// returns FallbackCascade for that window) so a transient anchor-read
// failure cannot break the dashboard.
func (p *SummaryProvider) Build() (*store.Summary, error) {
	summary, err := p.store.GetSummary()
	if err != nil {
		return nil, err
	}
	fiveAnchor, _ := p.store.GetLatestAnchor("5h")
	sevenAnchor, _ := p.store.GetLatestAnchor("7d")
	schedState := p.scheduler.Snapshot()
	now := p.now()

	fiveState := DeriveWindowState(schedState, fiveAnchor, now)
	sevenState := DeriveWindowState(schedState, sevenAnchor, now)
	fiveStr := fiveState.String()
	sevenStr := sevenState.String()
	summary.Window5h.State = &fiveStr
	summary.Window7d.State = &sevenStr

	return summary, nil
}
