// Package usagescheduler periodically refreshes window_anchors rows from
// the live /api/oauth/usage endpoint via the proven internal/usageprovider
// adapter. It runs as a single goroutine spawned by cmd/serve and stops
// when its context is canceled.
//
// Architecture (per F2 S2.2 EM ruling chat msg 20565):
//
//   - Trigger model: fetch on Run() entry, then schedule the next fetch
//     at the earliest observed reset_at plus a small grace delay. No
//     periodic polling; the only time-based wakeup is "the window we
//     wrote an anchor for is about to reset."
//
//   - Lifecycle: cmd/serve owns the context. Run blocks until ctx.Done().
//     Shutdown cancels in-flight fetches and stops timers.
//
//   - Backoff: exponential (30s -> 5min cap) on transient provider errors
//     (unavailable, rate-limited). Auth/schema/credentials errors are
//     PERMANENT — scheduler stops auto-refresh until process restart, so
//     manual sync remains the user's path forward.
//
//   - Anchor writes use store.SaveWindowAnchor; cost computed via the
//     shared store.ObservedCostForWindow helper that the manual-sync
//     POST handler also calls. Drift between the two flows is impossible
//     by construction (one helper, two callers).
//
//   - Stale-snapshot rejection: a snapshot whose reset time is at or
//     before its observation time is unusable for that window; the
//     scheduler skips the write rather than producing a permanently-stale
//     zero-minute anchor.
//
// The package does NOT import internal/api or internal/hub; cmd/serve
// wires the broadcast callback if any.
package usagescheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ksred/cctrack/internal/credentials"
	"github.com/ksred/cctrack/internal/store"
	"github.com/ksred/cctrack/internal/usageprovider"
)

// Provider is the contract the scheduler needs from the usage endpoint
// adapter. usageprovider.Client satisfies it in production; tests use a
// fake.
type Provider interface {
	Fetch(ctx context.Context, creds credentials.Credentials) (usageprovider.Snapshot, error)
}

// CredentialsLoader is the contract for credential acquisition.
// credentials.Load satisfies it in production.
type CredentialsLoader func() (credentials.Credentials, error)

// AnchorStore is the slice of *store.Store the scheduler uses. Defining
// it as an interface keeps the scheduler test-fakeable without spinning
// up a real SQLite database.
type AnchorStore interface {
	SaveWindowAnchor(a store.WindowAnchor) (int64, error)
	ObservedCostForWindow(windowType string, observedAt, resetsAt time.Time) (float64, error)
}

// Clock abstracts time.Now and time.After/sleep so tests can advance a
// virtual clock without sleeping for real.
type Clock interface {
	Now() time.Time
	// Sleep returns ctx.Err() if the context cancels first, nil if d elapses.
	Sleep(ctx context.Context, d time.Duration) error
}

// Logger receives one-line operational messages. cmd/serve typically
// wires this to log.Printf with a "usagescheduler: " prefix; tests can
// pass a no-op to keep output clean.
type Logger func(format string, args ...any)

const (
	// graceDelay is added to the observed reset time before re-fetching,
	// so we don't race the upstream's own boundary roll.
	graceDelay = 30 * time.Second

	// backoffInitial / backoffMax / backoffFactor define the exponential
	// schedule for transient provider failures.
	backoffInitial = 30 * time.Second
	backoffMax     = 5 * time.Minute
	backoffFactor  = 2
)

// errStop is an internal sentinel that signals "permanent error; the
// outer Run loop should exit." It never escapes the scheduler package.
var errStop = errors.New("usagescheduler: permanent error, stopping")

// Scheduler is the main type. Construct via New; run via Run.
type Scheduler struct {
	provider  Provider
	loadCreds CredentialsLoader
	store     AnchorStore
	clock     Clock
	log       Logger
}

// New constructs a Scheduler with the supplied dependencies and
// production-default clock + logger. cmd/serve owns lifecycle ordering.
func New(p Provider, lc CredentialsLoader, s AnchorStore, log Logger) *Scheduler {
	return &Scheduler{
		provider:  p,
		loadCreds: lc,
		store:     s,
		clock:     realClock{},
		log:       log,
	}
}

// WithClock overrides the default clock; only useful for tests. Returns
// the scheduler for chained construction.
func (s *Scheduler) WithClock(c Clock) *Scheduler {
	s.clock = c
	return s
}

// Run blocks until ctx is canceled or a permanent error stops the loop.
// On permanent errors (credentials missing/expired, unauthorized, schema
// drift) the scheduler stops auto-refresh and returns. Transient errors
// drive exponential backoff capped at backoffMax.
func (s *Scheduler) Run(ctx context.Context) {
	backoff := backoffInitial
	for {
		delay, err := s.tick(ctx)
		if err != nil {
			if errors.Is(err, errStop) {
				return
			}
			if errors.Is(err, ctx.Err()) {
				return
			}
			s.log("transient error: %v; backing off %v", err, backoff)
			if sleepErr := s.clock.Sleep(ctx, backoff); sleepErr != nil {
				return
			}
			backoff *= backoffFactor
			if backoff > backoffMax {
				backoff = backoffMax
			}
			continue
		}
		// Successful tick — reset backoff.
		backoff = backoffInitial
		if delay <= 0 {
			// Defensive: tick must return a positive delay on success
			// because earliestFutureReset() is a strict-future check and
			// the both-stale case takes the errStop path below. A zero
			// here means a programming error introduced a periodic-poll
			// path that EM constraint #7 forbids; refuse rather than
			// fabricate a fallback delay.
			s.log("internal: tick returned non-positive delay on success; stopping")
			return
		}
		if sleepErr := s.clock.Sleep(ctx, delay); sleepErr != nil {
			return
		}
	}
}

// tick performs one fetch + write cycle. Returns (delayUntilNextFetch, err).
//   - On success: delay is the time until the earliest observed reset + grace.
//   - On a permanent error class: returns (0, errStop). Outer Run exits.
//   - On a transient error: returns (0, transientErr). Outer Run backs off.
//   - On context cancellation during sleep/fetch: returns (0, ctx.Err()).
func (s *Scheduler) tick(ctx context.Context) (time.Duration, error) {
	creds, err := s.loadCreds()
	if err != nil {
		if errors.Is(err, credentials.ErrCredentialsMissing) ||
			errors.Is(err, credentials.ErrTokenExpired) ||
			errors.Is(err, credentials.ErrCredentialsMalformed) {
			s.log("permanent credentials error: %v", err)
			return 0, errStop
		}
		return 0, fmt.Errorf("loadCreds: %w", err)
	}

	snap, err := s.provider.Fetch(ctx, creds)
	if err != nil {
		if errors.Is(err, ctx.Err()) {
			return 0, ctx.Err()
		}
		if errors.Is(err, usageprovider.ErrUnauthorized) ||
			errors.Is(err, usageprovider.ErrSchemaDrift) {
			s.log("permanent provider error: %v", err)
			return 0, errStop
		}
		// Transient: ErrProviderUnavailable, ErrRateLimited.
		return 0, err
	}

	// Write each window's anchor independently. A stale or per-window failure
	// does not abort the other window; row writes are atomic at the DB layer
	// (SaveWindowAnchor is one INSERT).
	s.writeAnchorIfFresh(snap, "5h", snap.FiveHourResetsAt)
	s.writeAnchorIfFresh(snap, "7d", snap.SevenDayResetsAt)

	// Schedule the next fetch at the EARLIEST observed reset + grace. If
	// both resets are stale-or-zero, we have no valid trigger time. Per EM
	// constraint #7 (no periodic polling) and the spirit of constraint #6
	// (no fabricating boundaries), this is a stop class — fall back to
	// manual sync until the next process restart.
	next := earliestFutureReset(snap)
	if next.IsZero() {
		s.log("both windows have non-future resets; stopping auto-refresh")
		return 0, errStop
	}
	delay := next.Add(graceDelay).Sub(s.clock.Now())
	if delay < 0 {
		delay = 0
	}
	return delay, nil
}

// writeAnchorIfFresh validates the window's resets_at vs observed_at and
// writes a SaveWindowAnchor row only if fresh. Per binding constraint #6:
// a snapshot whose reset is at or before observation is unusable for that
// window; we skip the write rather than fabricating a zero-minute anchor.
func (s *Scheduler) writeAnchorIfFresh(snap usageprovider.Snapshot, windowType string, resetsAt time.Time) {
	if !resetsAt.After(snap.Observed) {
		// Schema hard-rule (S2.1/S2.2): no raw response values in logs.
		// Parsed `resets_at` and `Observed` are derived response values;
		// log only the window class, not the timestamps themselves.
		s.log("%s: stale snapshot; skipping write", windowType)
		return
	}

	var pct float64
	switch windowType {
	case "5h":
		pct = float64(snap.FiveHourUtilizationPercent)
	case "7d":
		pct = float64(snap.SevenDayUtilizationPercent)
	default:
		s.log("%s: unknown window type; skipping", windowType)
		return
	}

	cost, err := s.store.ObservedCostForWindow(windowType, snap.Observed, resetsAt)
	if err != nil {
		s.log("%s: cost helper failed: %v", windowType, err)
		return
	}

	timeLeftMinutes := int(resetsAt.Sub(snap.Observed).Round(time.Minute).Minutes())
	if timeLeftMinutes < 1 {
		// Sub-minute future reset: round up to 1, not 0. Per binding
		// constraint #5: future-but-subminute resets must NOT collapse
		// into a permanently-stale zero-minute anchor.
		timeLeftMinutes = 1
	}

	anchor := store.WindowAnchor{
		SyncedAt:        snap.Observed.UTC().Format(time.RFC3339Nano),
		WindowType:      windowType,
		TimeLeftMinutes: timeLeftMinutes,
		AnthropicPct:    &pct,
		ObservedCost:    cost,
	}
	if _, err := s.store.SaveWindowAnchor(anchor); err != nil {
		s.log("%s: SaveWindowAnchor failed: %v", windowType, err)
	}
}

// earliestFutureReset returns the earliest of the two resets_at times that
// is strictly after snap.Observed. Returns zero time if both are stale.
func earliestFutureReset(snap usageprovider.Snapshot) time.Time {
	var earliest time.Time
	consider := func(t time.Time) {
		if !t.After(snap.Observed) {
			return
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	consider(snap.FiveHourResetsAt)
	consider(snap.SevenDayResetsAt)
	return earliest
}

// realClock is the production Clock implementation.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
