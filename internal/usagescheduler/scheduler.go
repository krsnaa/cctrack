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
	"sync"
	"sync/atomic"
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

// OnAnchorsUpdated is the optional callback fired after a tick in which
// at least one anchor row was successfully written. Per F2 S2.2 EM ruling
// chat msg 20591/20593: cmd/serve owns the concrete implementation (e.g.
// fetching a fresh summary and broadcasting "summary.updated" through
// the existing hub) so that automatic anchor writes drive the same live
// freshness path as other backend state changes. The scheduler invokes
// it but does not import internal/api or internal/hub.
//
// Callback errors are logged (redacted) and swallowed; a callback
// failure does NOT roll back the persisted anchors.
type OnAnchorsUpdated func(ctx context.Context) error

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

// ErrorClass classifies the most recent fetch failure (or none) the
// scheduler has observed. The State accessor surfaces this for the
// summary-augmentation derivation in internal/usagestate. Per F2 S2.3
// EM ruling chat msg 20621: "Track only fixed enums + timestamps, not
// raw error strings."
type ErrorClass int

const (
	// ErrorClassNone means no error since the last successful fetch
	// (or no fetch yet).
	ErrorClassNone ErrorClass = iota
	// ErrorClassCredentialsMissing maps to credentials.ErrCredentialsMissing.
	ErrorClassCredentialsMissing
	// ErrorClassTokenExpired maps to credentials.ErrTokenExpired.
	ErrorClassTokenExpired
	// ErrorClassCredentialsMalformed maps to credentials.ErrCredentialsMalformed.
	ErrorClassCredentialsMalformed
	// ErrorClassUnauthorized maps to usageprovider.ErrUnauthorized.
	ErrorClassUnauthorized
	// ErrorClassSchemaDrift maps to usageprovider.ErrSchemaDrift.
	ErrorClassSchemaDrift
	// ErrorClassProviderUnavailable maps to usageprovider.ErrProviderUnavailable.
	ErrorClassProviderUnavailable
	// ErrorClassRateLimited maps to usageprovider.ErrRateLimited.
	ErrorClassRateLimited
)

// ProviderAnchorMeta is the minimum fingerprint needed to recognize a
// stored WindowAnchor as written by THIS process's provider sync.
//
// All fields are copies of values already persisted on the same DB row
// (window_anchors.id, synced_at, time_left_minutes); no raw provider
// response data is added beyond what store.SaveWindowAnchor already
// accepts. ID is the primary discriminator; SyncedAt and
// TimeLeftMinutes are belt-and-suspenders checks against pathological
// row reuse or in-place mutation.
type ProviderAnchorMeta struct {
	ID              int64
	SyncedAt        string
	TimeLeftMinutes int
}

// State is a race-safe snapshot of the scheduler's runtime state. It is
// kept in memory only (no DB writes); on serve restart it begins empty
// and is repopulated by the first tick.
type State struct {
	// Running is true while Run() is in its main loop. Becomes false
	// after Run returns (context canceled or permanent errStop).
	Running bool
	// LastFetchSucceeded is the wall-clock time of the most recent
	// successful provider.Fetch. Zero if no fetch has succeeded since
	// process start.
	LastFetchSucceeded time.Time
	// LastErrorClass is the classification of the most recent fetch
	// failure. Reset to ErrorClassNone after a successful fetch.
	LastErrorClass ErrorClass
	// ProviderAnchors maps window_type ("5h" | "7d") to the fingerprint
	// of the most recent anchor this process wrote from a provider sync.
	// Empty on process start; populated only after writeAnchorIfFresh
	// observes a successful SaveWindowAnchor for that window. Consumed by
	// usagestate.DeriveWindowState so a provider-backed re-sync can
	// classify as auto_fresh / auto_stale even when Running=false (e.g.
	// after the background loop stopped on a permanent error class).
	//
	// On restart this is empty; old anchors degrade to manual_anchor
	// until a fresh provider sync succeeds. That preserves the honest
	// cold-state intent: cctrack does not vouch for anchor provenance it
	// did not personally observe.
	ProviderAnchors map[string]ProviderAnchorMeta
}

// Scheduler is the main type. Construct via New; run via Run.
type Scheduler struct {
	provider         Provider
	loadCreds        CredentialsLoader
	store            AnchorStore
	clock            Clock
	log              Logger
	onAnchorsUpdated OnAnchorsUpdated // optional; nil = no callback

	// stateMu guards state. All reads via Snapshot() return a copy.
	stateMu sync.Mutex
	state   State

	// syncing single-flights work between the background Run loop and
	// any SyncOnce caller. Acquired via CompareAndSwap so concurrent
	// callers can detect contention without blocking.
	syncing atomic.Bool
}

// SyncStatus is the small fixed-shape DTO returned by SyncOnce. Fields
// are intentionally limited to what is safe to surface in API JSON
// responses: a status enum string, an optional list of window types
// whose anchors were freshly written, and an optional success
// timestamp. No provider error strings, no credential bytes, no raw
// response shape.
type SyncStatus struct {
	// Status is one of:
	//   "ok"                       — fetch succeeded; anchors written (possibly empty).
	//   "in_progress"              — another sync was already in flight.
	//   "credentials_missing"      — ~/.claude/.credentials.json absent.
	//   "token_expired"            — credentials parsed but expiresAt past.
	//   "credentials_malformed"    — credentials present but unparseable.
	//   "unauthorized"             — provider returned 401/403.
	//   "schema_drift"             — provider response missing / wrong-typed required fields.
	//   "provider_unavailable"     — transport / 5xx / context.
	//   "rate_limited"             — provider returned 429.
	Status         string   `json:"status"`
	WindowsWritten []string `json:"windows_written,omitempty"`
	SyncedAt       *string  `json:"synced_at,omitempty"` // RFC3339Nano UTC; only on Status=="ok"
}

// runOnceResult carries the outcome of one fetch+write cycle. Used by
// both the background tick (to compute next-delay or classify error)
// and SyncOnce (to map into SyncStatus).
type runOnceResult struct {
	// Snapshot is populated only on a successful provider.Fetch.
	Snapshot       usageprovider.Snapshot
	// AnchorsWritten lists window types ("5h", "7d") whose
	// SaveWindowAnchor returned successfully this cycle. Possibly empty
	// even on success (e.g., both windows had stale resets_at).
	AnchorsWritten []string
	// ErrorClass is the typed enum equivalent of Err for the runtime
	// state surface and SyncStatus mapping. ErrorClassNone on success.
	ErrorClass ErrorClass
	// Err is the underlying error (errStop / transient / context
	// cancellation). The background tick consumes this directly; SyncOnce
	// maps via ErrorClass.
	Err error
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

// WithOnAnchorsUpdated installs the callback fired after a tick that
// wrote at least one anchor. Returns the scheduler for chained
// construction. cmd/serve typically passes a closure that broadcasts a
// fresh summary through the websocket hub.
func (s *Scheduler) WithOnAnchorsUpdated(cb OnAnchorsUpdated) *Scheduler {
	s.onAnchorsUpdated = cb
	return s
}

// Snapshot returns a copy of the scheduler's current runtime state.
// Safe to call concurrently with Run from any goroutine; readers see a
// stable point-in-time snapshot. Used by internal/usagestate to derive
// the per-window honest-state enum surfaced in /api/v1/summary.
//
// The ProviderAnchors map is deep-copied so callers may inspect or
// even mutate the returned snapshot without racing the scheduler's
// concurrent recordProviderAnchor writes.
func (s *Scheduler) Snapshot() State {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	out := s.state
	if s.state.ProviderAnchors != nil {
		out.ProviderAnchors = make(map[string]ProviderAnchorMeta, len(s.state.ProviderAnchors))
		for k, v := range s.state.ProviderAnchors {
			out.ProviderAnchors[k] = v
		}
	}
	return out
}

// setRunning is called at Run() entry/exit to track the loop's
// liveness. Mutex-protected for safe concurrent reads via Snapshot.
func (s *Scheduler) setRunning(running bool) {
	s.stateMu.Lock()
	s.state.Running = running
	s.stateMu.Unlock()
}

// recordFetchSuccess is called after a provider.Fetch returns nil. It
// records the success time and clears any previous error class.
func (s *Scheduler) recordFetchSuccess() {
	s.stateMu.Lock()
	s.state.LastFetchSucceeded = s.clock.Now()
	s.state.LastErrorClass = ErrorClassNone
	s.stateMu.Unlock()
}

// recordFetchError stores the typed error class for the most recent
// fetch failure. Raw error strings are NEVER stored — only the fixed
// enum.
func (s *Scheduler) recordFetchError(class ErrorClass) {
	s.stateMu.Lock()
	s.state.LastErrorClass = class
	s.stateMu.Unlock()
}

// recordProviderAnchor stamps in-memory metadata identifying the latest
// anchor this process wrote from a provider sync, keyed by window_type.
// Called only after store.SaveWindowAnchor returns nil for that window.
func (s *Scheduler) recordProviderAnchor(windowType string, meta ProviderAnchorMeta) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.state.ProviderAnchors == nil {
		s.state.ProviderAnchors = make(map[string]ProviderAnchorMeta)
	}
	s.state.ProviderAnchors[windowType] = meta
}

// Run blocks until ctx is canceled or a permanent error stops the loop.
// On permanent errors (credentials missing/expired, unauthorized, schema
// drift) the scheduler stops auto-refresh and returns. Transient errors
// drive exponential backoff capped at backoffMax.
func (s *Scheduler) Run(ctx context.Context) {
	s.setRunning(true)
	defer s.setRunning(false)
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

// runOnce performs one fetch + write cycle and returns the typed result.
// Both the background tick (for next-delay / errStop classification) and
// SyncOnce (for SyncStatus mapping) share this path so they cannot drift.
//
// The function records typed error classes onto the scheduler state so
// /api/v1/summary and the dashboard's honest-state badges reflect the
// most recent classification regardless of which entry point ran the
// cycle. On a successful fetch, recordFetchSuccess clears any prior
// error class.
func (s *Scheduler) runOnce(ctx context.Context) runOnceResult {
	creds, err := s.loadCreds()
	if err != nil {
		switch {
		case errors.Is(err, credentials.ErrCredentialsMissing):
			s.recordFetchError(ErrorClassCredentialsMissing)
			s.log("permanent credentials error: missing")
			return runOnceResult{ErrorClass: ErrorClassCredentialsMissing, Err: errStop}
		case errors.Is(err, credentials.ErrTokenExpired):
			s.recordFetchError(ErrorClassTokenExpired)
			s.log("permanent credentials error: token expired")
			return runOnceResult{ErrorClass: ErrorClassTokenExpired, Err: errStop}
		case errors.Is(err, credentials.ErrCredentialsMalformed):
			s.recordFetchError(ErrorClassCredentialsMalformed)
			s.log("permanent credentials error: malformed")
			return runOnceResult{ErrorClass: ErrorClassCredentialsMalformed, Err: errStop}
		default:
			// Transient I/O error; do not classify.
			return runOnceResult{Err: fmt.Errorf("loadCreds: %w", err)}
		}
	}

	snap, err := s.provider.Fetch(ctx, creds)
	if err != nil {
		if errors.Is(err, ctx.Err()) {
			return runOnceResult{Err: ctx.Err()}
		}
		switch {
		case errors.Is(err, usageprovider.ErrUnauthorized):
			s.recordFetchError(ErrorClassUnauthorized)
			s.log("permanent provider error: unauthorized")
			return runOnceResult{ErrorClass: ErrorClassUnauthorized, Err: errStop}
		case errors.Is(err, usageprovider.ErrSchemaDrift):
			s.recordFetchError(ErrorClassSchemaDrift)
			s.log("permanent provider error: schema drift")
			return runOnceResult{ErrorClass: ErrorClassSchemaDrift, Err: errStop}
		case errors.Is(err, usageprovider.ErrProviderUnavailable):
			s.recordFetchError(ErrorClassProviderUnavailable)
			return runOnceResult{ErrorClass: ErrorClassProviderUnavailable, Err: err}
		case errors.Is(err, usageprovider.ErrRateLimited):
			s.recordFetchError(ErrorClassRateLimited)
			return runOnceResult{ErrorClass: ErrorClassRateLimited, Err: err}
		default:
			// Untyped error: still surface as transient but don't classify.
			return runOnceResult{Err: err}
		}
	}
	s.recordFetchSuccess()

	// Write each window's anchor independently. A stale or per-window failure
	// does not abort the other window; row writes are atomic at the DB layer
	// (SaveWindowAnchor is one INSERT). Track which windows wrote so the
	// post-cycle callback fires only when state actually changed.
	var written []string
	if s.writeAnchorIfFresh(snap, "5h", snap.FiveHourResetsAt) {
		written = append(written, "5h")
	}
	if s.writeAnchorIfFresh(snap, "7d", snap.SevenDayResetsAt) {
		written = append(written, "7d")
	}
	if len(written) > 0 {
		s.invokeOnAnchorsUpdated(ctx)
	}
	return runOnceResult{Snapshot: snap, AnchorsWritten: written}
}

// tick performs one fetch + write cycle and computes the next scheduled
// wake. Returns (delayUntilNextFetch, err):
//   - On success: delay is the time until the earliest observed reset + grace.
//   - On a permanent error class: returns (0, errStop). Outer Run exits.
//   - On a transient error: returns (0, transientErr). Outer Run backs off.
//   - On context cancellation during sleep/fetch: returns (0, ctx.Err()).
//
// Single-flight: if SyncOnce is currently in flight (manual button), the
// tick yields its slot and returns a benign next-delay. The CAS-loss is
// NOT classified as a provider failure (per EM ruling chat 20726
// amendment 1) — honest-state stays untouched and the next scheduled
// wake fires normally.
func (s *Scheduler) tick(ctx context.Context) (time.Duration, error) {
	if !s.syncing.CompareAndSwap(false, true) {
		// SyncOnce in flight — back off briefly and retry. Do not log,
		// do not record any error class, do not change honest-state.
		return backoffInitial, nil
	}
	defer s.syncing.Store(false)

	result := s.runOnce(ctx)
	if result.Err != nil {
		return 0, result.Err
	}

	// Schedule the next fetch at the EARLIEST observed reset + grace. If
	// both resets are stale-or-zero, we have no valid trigger time. Per EM
	// constraint #7 (no periodic polling) and the spirit of constraint #6
	// (no fabricating boundaries), this is a stop class — fall back to
	// manual sync until the next process restart.
	next := earliestFutureReset(result.Snapshot)
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

// SyncOnce performs one fetch + write cycle synchronously and returns
// the user-visible SyncStatus. Reuses runOnce so its behavior is
// indistinguishable from a background tick except in the single-flight
// contention case.
//
// Single-flight: if a tick or another SyncOnce is currently in flight,
// returns SyncStatus{Status: "in_progress"} immediately without
// blocking the caller or starting a duplicate provider fetch. Repeated
// rapid clicks therefore short-circuit cleanly rather than queueing.
//
// Wired to POST /api/v1/usage-sync via the cmd/serve injection of
// sched.SyncOnce into api.New.
func (s *Scheduler) SyncOnce(ctx context.Context) SyncStatus {
	if !s.syncing.CompareAndSwap(false, true) {
		return SyncStatus{Status: "in_progress"}
	}
	defer s.syncing.Store(false)

	result := s.runOnce(ctx)

	// Map typed error class first so each enum value gets a stable
	// status string. ErrorClassNone falls through to the success path.
	switch result.ErrorClass {
	case ErrorClassCredentialsMissing:
		return SyncStatus{Status: "credentials_missing"}
	case ErrorClassTokenExpired:
		return SyncStatus{Status: "token_expired"}
	case ErrorClassCredentialsMalformed:
		return SyncStatus{Status: "credentials_malformed"}
	case ErrorClassUnauthorized:
		return SyncStatus{Status: "unauthorized"}
	case ErrorClassSchemaDrift:
		return SyncStatus{Status: "schema_drift"}
	case ErrorClassProviderUnavailable:
		return SyncStatus{Status: "provider_unavailable"}
	case ErrorClassRateLimited:
		return SyncStatus{Status: "rate_limited"}
	}
	// ErrorClassNone — but result.Err may still be non-nil for untyped
	// transient (loadCreds I/O) or context cancellation. Map both to
	// provider_unavailable so the user sees a recoverable state.
	if result.Err != nil {
		return SyncStatus{Status: "provider_unavailable"}
	}
	syncedAt := s.clock.Now().UTC().Format(time.RFC3339Nano)
	return SyncStatus{
		Status:         "ok",
		WindowsWritten: result.AnchorsWritten,
		SyncedAt:       &syncedAt,
	}
}

// writeAnchorIfFresh validates the window's resets_at vs observed_at and
// writes a SaveWindowAnchor row only if fresh. Per binding constraint #6:
// a snapshot whose reset is at or before observation is unusable for that
// window; we skip the write rather than fabricating a zero-minute anchor.
//
// Returns true iff a row was successfully written. Callers use this to
// gate the OnAnchorsUpdated callback.
func (s *Scheduler) writeAnchorIfFresh(snap usageprovider.Snapshot, windowType string, resetsAt time.Time) bool {
	if !resetsAt.After(snap.Observed) {
		// Schema hard-rule (S2.1/S2.2): no raw response values in logs.
		// Parsed `resets_at` and `Observed` are derived response values;
		// log only the window class, not the timestamps themselves.
		s.log("%s: stale snapshot; skipping write", windowType)
		return false
	}

	var pct float64
	switch windowType {
	case "5h":
		pct = float64(snap.FiveHourUtilizationPercent)
	case "7d":
		pct = float64(snap.SevenDayUtilizationPercent)
	default:
		s.log("%s: unknown window type; skipping", windowType)
		return false
	}

	cost, err := s.store.ObservedCostForWindow(windowType, snap.Observed, resetsAt)
	if err != nil {
		s.log("%s: cost helper failed: %v", windowType, err)
		return false
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
	id, err := s.store.SaveWindowAnchor(anchor)
	if err != nil {
		s.log("%s: SaveWindowAnchor failed: %v", windowType, err)
		return false
	}
	s.recordProviderAnchor(windowType, ProviderAnchorMeta{
		ID:              id,
		SyncedAt:        anchor.SyncedAt,
		TimeLeftMinutes: anchor.TimeLeftMinutes,
	})
	return true
}

// invokeOnAnchorsUpdated calls the optional update callback. Errors are
// logged with a FIXED REDACTED MESSAGE (no underlying error value) and
// swallowed so callback misbehavior cannot roll back the anchors that
// were just persisted. Per EM ruling chat msg 20591/20593 + verifier
// finding chat msg 20597: callback closures (e.g. cmd/serve's
// summary.updated broadcast) may wrap concrete database / serialization
// errors that should not appear in scheduler logs.
func (s *Scheduler) invokeOnAnchorsUpdated(ctx context.Context) {
	if s.onAnchorsUpdated == nil {
		return
	}
	if err := s.onAnchorsUpdated(ctx); err != nil {
		// Intentional: do NOT format `err` into the message. The callback
		// is owned by cmd/serve and may wrap concrete internals
		// (database paths, JSON marshaling diagnostics) that are not
		// safe for scheduler-level logs.
		_ = err
		s.log("OnAnchorsUpdated callback failed (details redacted)")
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
