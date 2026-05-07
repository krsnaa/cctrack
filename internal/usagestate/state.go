// Package usagestate is the pure derivation layer that maps cctrack's
// runtime data — scheduler state, latest window anchor, current time —
// into the per-window honest-state enum surfaced on the dashboard via
// /api/v1/summary.
//
// Per F2 S2.3 EM ruling chat msg 20621 option B: this layer is
// stateless and unit-tested with fakes; it does not reach into the DB,
// the scheduler, or any HTTP surface. Callers (cmd/serve, internal/api)
// gather the inputs and pass them in, then write the resulting enum
// into the additive WindowBucket fields.
//
// Important per EM ruling: WindowBucket alone is insufficient input
// because cctrack's existing cascading fallback can fire when an
// anchor's recorded window has expired, and the post-cascade summary
// loses the "anchor exists but is expired" distinction. The derivation
// MUST receive the explicit latest WindowAnchor for each window
// (typically via store.GetLatestAnchor) so the auto_stale vs
// fallback_cascade case can be told apart.
package usagestate

import (
	"time"

	"github.com/ksred/cctrack/internal/store"
	"github.com/ksred/cctrack/internal/usagescheduler"
)

// WindowHonestState is the per-window state enum the dashboard renders
// against. The six discrete values come from F2 evidence requirements
// (board item #637) and are listed in the v0.5 LAD.
type WindowHonestState int

const (
	// WindowHonestStateUnknown is the zero value used when the
	// derivation cannot classify (e.g. corrupt anchor data). UI should
	// treat as fallback cascade with a stronger "data is unreliable"
	// presentation, but in practice this should not occur with healthy
	// inputs.
	WindowHonestStateUnknown WindowHonestState = iota

	// WindowHonestStateAutoFresh: the scheduler is running, has
	// successfully fetched at least once, and the most-recent anchor's
	// recorded reset time is still in the future. Live, current data.
	WindowHonestStateAutoFresh

	// WindowHonestStateAutoStale: the scheduler is running and has
	// fetched successfully at some point, but the most-recent anchor's
	// reset moment has passed. The window has rolled over and we
	// haven't refreshed it yet (e.g. between scheduler ticks).
	WindowHonestStateAutoStale

	// WindowHonestStateTokenExpired: the scheduler stopped because
	// credentials are missing, expired, malformed, or rejected by the
	// provider (401/403). User must refresh Claude Code login.
	WindowHonestStateTokenExpired

	// WindowHonestStateProviderUnavailable: the scheduler is paused or
	// stopped because the provider returned schema drift, persistent
	// 5xx, or rate limiting. May recover automatically (transient
	// classes drive backoff; schema-drift class stops until restart).
	WindowHonestStateProviderUnavailable

	// WindowHonestStateManualAnchor: an anchor exists but the
	// scheduler isn't running or hasn't successfully fetched yet, so
	// the anchor must be from manual entry (or a previous-process
	// auto-sync we can't verify after restart). Mark as manual.
	WindowHonestStateManualAnchor

	// WindowHonestStateFallbackCascade: no anchor at all. cctrack is
	// computing usage from inferred cascading windows. Approximate.
	WindowHonestStateFallbackCascade
)

// String returns the canonical lower-snake-case form used in JSON
// responses. Stable across versions; UI matches against these strings.
func (s WindowHonestState) String() string {
	switch s {
	case WindowHonestStateAutoFresh:
		return "auto_fresh"
	case WindowHonestStateAutoStale:
		return "auto_stale"
	case WindowHonestStateTokenExpired:
		return "token_expired"
	case WindowHonestStateProviderUnavailable:
		return "provider_unavailable"
	case WindowHonestStateManualAnchor:
		return "manual_anchor"
	case WindowHonestStateFallbackCascade:
		return "fallback_cascade"
	default:
		return "unknown"
	}
}

// DeriveWindowState computes the honest state for one window
// (5h or 7d) from the scheduler's runtime snapshot, the latest
// WindowAnchor for that window (nil if none), and the current clock.
//
// windowType is "5h" or "7d" — the caller's intent, not the anchor's
// self-reported field. Trusting caller intent here keeps correctness
// independent of DB row payload integrity.
//
// The derivation is pure: same inputs always produce the same output.
// Callers gather the inputs from store.GetLatestAnchor +
// (*usagescheduler.Scheduler).Snapshot() + time.Now().
func DeriveWindowState(
	schedState usagescheduler.State,
	windowType string,
	anchor *store.WindowAnchor,
	now time.Time,
) WindowHonestState {
	// Permanent / blocking error classes take precedence over anchor
	// state because the user needs the actionable signal ("token
	// expired") not a stale auto_stale claim.
	switch schedState.LastErrorClass {
	case usagescheduler.ErrorClassCredentialsMissing,
		usagescheduler.ErrorClassTokenExpired,
		usagescheduler.ErrorClassCredentialsMalformed,
		usagescheduler.ErrorClassUnauthorized:
		return WindowHonestStateTokenExpired
	case usagescheduler.ErrorClassSchemaDrift,
		usagescheduler.ErrorClassProviderUnavailable,
		usagescheduler.ErrorClassRateLimited:
		return WindowHonestStateProviderUnavailable
	}

	// No anchor at all: cascade is the only data source.
	if anchor == nil {
		return WindowHonestStateFallbackCascade
	}

	// Compute the anchor's recorded reset moment. SyncedAt is the
	// observation moment; TimeLeftMinutes is the remaining duration
	// the upstream reported at observation time. resets_at is their
	// sum.
	resetsAt, ok := anchorResetsAt(anchor)
	if !ok {
		// Corrupt anchor SyncedAt; cannot classify. UI should treat
		// like fallback (cascade), but Unknown signals the data
		// integrity issue distinctly for ops.
		return WindowHonestStateUnknown
	}

	// If the scheduler is currently running AND has successfully
	// fetched at least once this process lifetime, the latest anchor
	// is auto-written by the scheduler. Fresh vs stale comes from the
	// anchor's own recorded reset moment.
	if schedState.Running && !schedState.LastFetchSucceeded.IsZero() {
		if resetsAt.After(now) {
			return WindowHonestStateAutoFresh
		}
		return WindowHonestStateAutoStale
	}

	// Scheduler is not actively producing fresh auto-syncs, but the
	// latest stored anchor for this window may still be provider-
	// written if it matches an in-memory fingerprint recorded by
	// writeAnchorIfFresh in this process. Match on row ID first; the
	// SyncedAt + TimeLeftMinutes equality guards against pathological
	// row reuse or in-place mutation.
	if meta, hasMeta := schedState.ProviderAnchors[windowType]; hasMeta {
		if meta.ID == anchor.ID &&
			meta.SyncedAt == anchor.SyncedAt &&
			meta.TimeLeftMinutes == anchor.TimeLeftMinutes {
			if resetsAt.After(now) {
				return WindowHonestStateAutoFresh
			}
			return WindowHonestStateAutoStale
		}
	}

	// Anchor exists but no matching provider-sync metadata. Treat as
	// manual entry.
	return WindowHonestStateManualAnchor
}

// anchorResetsAt extracts the recorded reset moment from a WindowAnchor.
// SyncedAt is parsed as RFC3339Nano (matching the format the scheduler
// and manual-sync handler write).
func anchorResetsAt(a *store.WindowAnchor) (time.Time, bool) {
	syncedAt, err := time.Parse(time.RFC3339Nano, a.SyncedAt)
	if err != nil {
		return time.Time{}, false
	}
	return syncedAt.Add(time.Duration(a.TimeLeftMinutes) * time.Minute), true
}
