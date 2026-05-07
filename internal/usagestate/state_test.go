package usagestate

import (
	"testing"
	"time"

	"github.com/ksred/cctrack/internal/store"
	"github.com/ksred/cctrack/internal/usagescheduler"
)

// fixedNow is the reference clock for all derivation tests so anchor
// timestamps can be expressed relative to a stable point.
var fixedNow = time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)

// validAnchor builds a WindowAnchor whose recorded reset moment is
// `syncedAt + timeLeft`. The synced_at string is RFC3339Nano matching
// what scheduler + manual-sync write.
func validAnchor(syncedAt time.Time, timeLeftMinutes int) *store.WindowAnchor {
	return &store.WindowAnchor{
		WindowType:      "5h",
		SyncedAt:        syncedAt.UTC().Format(time.RFC3339Nano),
		TimeLeftMinutes: timeLeftMinutes,
		ObservedCost:    1.23,
	}
}

// runningWithSuccess builds a State for a scheduler that's actively
// running and has fetched successfully. Fresh data = "auto" branch.
func runningWithSuccess() usagescheduler.State {
	return usagescheduler.State{
		Running:            true,
		LastFetchSucceeded: fixedNow.Add(-1 * time.Minute), // succeeded 1 minute ago
		LastErrorClass:     usagescheduler.ErrorClassNone,
	}
}

func TestDeriveWindowState_AutoFresh(t *testing.T) {
	// Scheduler running + last fetch successful + anchor with future reset.
	anchor := validAnchor(fixedNow.Add(-30*time.Minute), 240) // resets 3.5h from now
	got := DeriveWindowState(runningWithSuccess(), "5h", anchor, fixedNow)
	if got != WindowHonestStateAutoFresh {
		t.Errorf("got %v (%q), want WindowHonestStateAutoFresh", got, got.String())
	}
}

func TestDeriveWindowState_AutoStale(t *testing.T) {
	// Scheduler running + last fetch successful + anchor with past reset.
	anchor := validAnchor(fixedNow.Add(-6*time.Hour), 60) // resets 5h ago
	got := DeriveWindowState(runningWithSuccess(), "5h", anchor, fixedNow)
	if got != WindowHonestStateAutoStale {
		t.Errorf("got %v (%q), want WindowHonestStateAutoStale", got, got.String())
	}
}

// TestDeriveWindowState_TokenExpired_AllCredentialAndAuthClasses verifies
// all four error classes that map to TokenExpired (UI prompts user to
// refresh Claude Code) take that branch regardless of anchor state.
func TestDeriveWindowState_TokenExpired_AllCredentialAndAuthClasses(t *testing.T) {
	cases := []struct {
		name string
		ec   usagescheduler.ErrorClass
	}{
		{"CredentialsMissing", usagescheduler.ErrorClassCredentialsMissing},
		{"TokenExpired", usagescheduler.ErrorClassTokenExpired},
		{"CredentialsMalformed", usagescheduler.ErrorClassCredentialsMalformed},
		{"Unauthorized", usagescheduler.ErrorClassUnauthorized},
	}
	freshAnchor := validAnchor(fixedNow.Add(-30*time.Minute), 240)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := usagescheduler.State{
				Running:        false, // permanent error stops Run()
				LastErrorClass: c.ec,
			}
			got := DeriveWindowState(st, "5h", freshAnchor, fixedNow)
			if got != WindowHonestStateTokenExpired {
				t.Errorf("error class %v gave %v (%q); want WindowHonestStateTokenExpired", c.ec, got, got.String())
			}
		})
	}
}

// TestDeriveWindowState_ProviderUnavailable_AllProviderClasses verifies
// the three provider-failure classes (schema drift, unavailable, rate
// limited) map to ProviderUnavailable.
func TestDeriveWindowState_ProviderUnavailable_AllProviderClasses(t *testing.T) {
	cases := []struct {
		name string
		ec   usagescheduler.ErrorClass
	}{
		{"SchemaDrift", usagescheduler.ErrorClassSchemaDrift},
		{"ProviderUnavailable", usagescheduler.ErrorClassProviderUnavailable},
		{"RateLimited", usagescheduler.ErrorClassRateLimited},
	}
	freshAnchor := validAnchor(fixedNow.Add(-30*time.Minute), 240)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := usagescheduler.State{
				Running:        c.ec != usagescheduler.ErrorClassSchemaDrift, // schema drift stops; transient classes continue
				LastErrorClass: c.ec,
			}
			got := DeriveWindowState(st, "5h", freshAnchor, fixedNow)
			if got != WindowHonestStateProviderUnavailable {
				t.Errorf("error class %v gave %v (%q); want WindowHonestStateProviderUnavailable", c.ec, got, got.String())
			}
		})
	}
}

func TestDeriveWindowState_ManualAnchor_SchedulerNotRunning(t *testing.T) {
	// Anchor exists, scheduler not running, no error class — assume manual.
	anchor := validAnchor(fixedNow.Add(-1*time.Hour), 120)
	st := usagescheduler.State{
		Running:        false,
		LastErrorClass: usagescheduler.ErrorClassNone,
	}
	got := DeriveWindowState(st, "5h", anchor, fixedNow)
	if got != WindowHonestStateManualAnchor {
		t.Errorf("got %v (%q), want WindowHonestStateManualAnchor", got, got.String())
	}
}

func TestDeriveWindowState_ManualAnchor_SchedulerRunningButNeverSucceeded(t *testing.T) {
	// Edge case: scheduler is running but hasn't fetched yet (fresh start).
	// Anchor exists from a previous-process write or manual entry — we can't
	// claim auto-state because LastFetchSucceeded is zero.
	anchor := validAnchor(fixedNow.Add(-1*time.Hour), 120)
	st := usagescheduler.State{
		Running:            true,
		LastFetchSucceeded: time.Time{}, // never succeeded
		LastErrorClass:     usagescheduler.ErrorClassNone,
	}
	got := DeriveWindowState(st, "5h", anchor, fixedNow)
	if got != WindowHonestStateManualAnchor {
		t.Errorf("got %v (%q), want WindowHonestStateManualAnchor (no fetch yet → cannot claim auto)", got, got.String())
	}
}

func TestDeriveWindowState_FallbackCascade_NoAnchor(t *testing.T) {
	// No anchor exists: cascade only.
	st := runningWithSuccess()
	got := DeriveWindowState(st, "5h", nil, fixedNow)
	if got != WindowHonestStateFallbackCascade {
		t.Errorf("got %v (%q), want WindowHonestStateFallbackCascade", got, got.String())
	}
}

// TestDeriveWindowState_FallbackCascade_NoAnchorOverridesSchedulerState
// verifies that a missing anchor takes priority over scheduler-running
// auto state (since auto needs an anchor to render).
func TestDeriveWindowState_FallbackCascade_NoAnchorOverridesSchedulerState(t *testing.T) {
	got := DeriveWindowState(runningWithSuccess(), "5h", nil, fixedNow)
	if got != WindowHonestStateFallbackCascade {
		t.Errorf("got %v; running scheduler with no anchor should still be cascade (no data to render auto)", got)
	}
}

// TestDeriveWindowState_ErrorClassPrecedesAnchor pins the precedence rule:
// when scheduler has a permanent error class, the user-actionable state
// takes priority over auto/manual classification of any anchor.
func TestDeriveWindowState_ErrorClassPrecedesAnchor(t *testing.T) {
	// Fresh anchor exists, but token is expired. State must be TokenExpired,
	// not AutoFresh.
	anchor := validAnchor(fixedNow.Add(-30*time.Minute), 240)
	st := usagescheduler.State{
		Running:            true,                                         // momentarily before stop
		LastFetchSucceeded: fixedNow.Add(-2 * time.Hour),                  // some prior success
		LastErrorClass:     usagescheduler.ErrorClassTokenExpired,
	}
	got := DeriveWindowState(st, "5h", anchor, fixedNow)
	if got != WindowHonestStateTokenExpired {
		t.Errorf("got %v; permanent error class must take precedence over anchor freshness", got)
	}
}

// TestDeriveWindowState_Unknown_CorruptAnchorSyncedAt verifies a corrupt
// SyncedAt string maps to Unknown rather than crashing or silently
// returning an arbitrary state.
func TestDeriveWindowState_Unknown_CorruptAnchorSyncedAt(t *testing.T) {
	corrupt := &store.WindowAnchor{
		WindowType:      "5h",
		SyncedAt:        "not-a-date-at-all",
		TimeLeftMinutes: 60,
	}
	got := DeriveWindowState(runningWithSuccess(), "5h", corrupt, fixedNow)
	if got != WindowHonestStateUnknown {
		t.Errorf("got %v; corrupt anchor SyncedAt should map to Unknown", got)
	}
}

func TestWindowHonestStateString(t *testing.T) {
	cases := []struct {
		s    WindowHonestState
		want string
	}{
		{WindowHonestStateAutoFresh, "auto_fresh"},
		{WindowHonestStateAutoStale, "auto_stale"},
		{WindowHonestStateTokenExpired, "token_expired"},
		{WindowHonestStateProviderUnavailable, "provider_unavailable"},
		{WindowHonestStateManualAnchor, "manual_anchor"},
		{WindowHonestStateFallbackCascade, "fallback_cascade"},
		{WindowHonestStateUnknown, "unknown"},
	}
	for _, c := range cases {
		if got := c.s.String(); got != c.want {
			t.Errorf("State(%d).String() = %q, want %q", c.s, got, c.want)
		}
	}
}

// TestDeriveWindowState_AnchorResetsAtBoundary covers the precise
// equal-to-now case: an anchor whose computed reset moment equals
// `now` exactly. By the convention used elsewhere (scheduler +
// fail-on-stale-snapshot), `resets_at <= observed` is unusable; in the
// derivation this means the anchor's reset has just elapsed → AutoStale
// when scheduler is running.
func TestDeriveWindowState_AnchorResetsAtBoundary(t *testing.T) {
	// resets_at = fixedNow exactly.
	anchor := validAnchor(fixedNow.Add(-30*time.Minute), 30)
	st := runningWithSuccess()
	got := DeriveWindowState(st, "5h", anchor, fixedNow)
	if got != WindowHonestStateAutoStale {
		t.Errorf("got %v; resets_at == now (not strictly future) should be AutoStale", got)
	}
}

// --- F8 tests: provider-written anchor recognition with Running=false --

// withProviderMeta clones a State and stamps a ProviderAnchorMeta entry
// for the given windowType. Returned State is otherwise unchanged.
func withProviderMeta(base usagescheduler.State, windowType string, meta usagescheduler.ProviderAnchorMeta) usagescheduler.State {
	out := base
	out.ProviderAnchors = make(map[string]usagescheduler.ProviderAnchorMeta)
	for k, v := range base.ProviderAnchors {
		out.ProviderAnchors[k] = v
	}
	out.ProviderAnchors[windowType] = meta
	return out
}

// stoppedSchedulerNoError simulates the scheduler having exited cleanly:
// Run() set Running=false, no error class outstanding (e.g. earlier
// permanent error was followed by a successful manual SyncOnce that
// cleared the class via recordFetchSuccess).
func stoppedSchedulerNoError() usagescheduler.State {
	return usagescheduler.State{
		Running:            false,
		LastFetchSucceeded: fixedNow.Add(-1 * time.Minute),
		LastErrorClass:     usagescheduler.ErrorClassNone,
	}
}

// TestDeriveWindowState_F8_PostStopSyncOnceAuto pins codex bar #1: after
// a permanent-error scheduler stop is followed by a successful SyncOnce,
// the latest stored anchor matches in-memory provider metadata, so the
// derivation returns AutoFresh even though Running=false.
func TestDeriveWindowState_F8_PostStopSyncOnceAuto(t *testing.T) {
	anchor := validAnchor(fixedNow.Add(-30*time.Minute), 240) // 3.5h future reset
	anchor.ID = 42
	st := withProviderMeta(stoppedSchedulerNoError(), "5h", usagescheduler.ProviderAnchorMeta{
		ID:              42,
		SyncedAt:        anchor.SyncedAt,
		TimeLeftMinutes: 240,
	})
	got := DeriveWindowState(st, "5h", anchor, fixedNow)
	if got != WindowHonestStateAutoFresh {
		t.Errorf("got %v (%q), want AutoFresh — provider-written anchor must classify auto despite Running=false", got, got.String())
	}
}

// TestDeriveWindowState_F8_NewerManualAnchorOverridesProviderMeta pins
// codex bar #2: provider metadata recorded a previous anchor X, but the
// latest stored anchor is Y (different ID). Derivation must classify Y
// as ManualAnchor, NOT auto, so old provider metadata cannot retroactively
// bless a manual entry.
func TestDeriveWindowState_F8_NewerManualAnchorOverridesProviderMeta(t *testing.T) {
	anchorX := validAnchor(fixedNow.Add(-30*time.Minute), 240)
	anchorX.ID = 42
	// Manual anchor Y written after X, different content.
	anchorY := validAnchor(fixedNow.Add(-10*time.Minute), 120)
	anchorY.ID = 43
	st := withProviderMeta(stoppedSchedulerNoError(), "5h", usagescheduler.ProviderAnchorMeta{
		ID:              anchorX.ID,
		SyncedAt:        anchorX.SyncedAt,
		TimeLeftMinutes: anchorX.TimeLeftMinutes,
	})
	got := DeriveWindowState(st, "5h", anchorY, fixedNow)
	if got != WindowHonestStateManualAnchor {
		t.Errorf("got %v (%q), want ManualAnchor — newer manual anchor must NOT inherit prior provider metadata", got, got.String())
	}
}

// TestDeriveWindowState_F8_PartialProviderWrite pins codex bar #3:
// SyncOnce wrote 5h but skipped 7d (stale resets_at). ProviderAnchors
// has only "5h"; 7d's stored anchor is some pre-existing manual entry.
// Derivation classifies 5h auto and 7d manual.
func TestDeriveWindowState_F8_PartialProviderWrite(t *testing.T) {
	anchor5h := validAnchor(fixedNow.Add(-30*time.Minute), 240)
	anchor5h.ID = 100
	anchor5h.WindowType = "5h"
	anchor7d := validAnchor(fixedNow.Add(-1*time.Hour), 600)
	anchor7d.ID = 99
	anchor7d.WindowType = "7d"
	// Only 5h has provider metadata recorded.
	st := withProviderMeta(stoppedSchedulerNoError(), "5h", usagescheduler.ProviderAnchorMeta{
		ID:              100,
		SyncedAt:        anchor5h.SyncedAt,
		TimeLeftMinutes: 240,
	})
	if got := DeriveWindowState(st, "5h", anchor5h, fixedNow); got != WindowHonestStateAutoFresh {
		t.Errorf("5h: got %v, want AutoFresh (matching provider meta)", got)
	}
	if got := DeriveWindowState(st, "7d", anchor7d, fixedNow); got != WindowHonestStateManualAnchor {
		t.Errorf("7d: got %v, want ManualAnchor (no provider meta for 7d)", got)
	}
}

// TestDeriveWindowState_F8_StaleProviderAnchor pins the auto_stale path
// for the new branch: provider metadata matches but the anchor's reset
// is in the past. Returns AutoStale, not ManualAnchor.
func TestDeriveWindowState_F8_StaleProviderAnchor(t *testing.T) {
	anchor := validAnchor(fixedNow.Add(-6*time.Hour), 60) // resets 5h ago
	anchor.ID = 7
	st := withProviderMeta(stoppedSchedulerNoError(), "5h", usagescheduler.ProviderAnchorMeta{
		ID:              7,
		SyncedAt:        anchor.SyncedAt,
		TimeLeftMinutes: 60,
	})
	got := DeriveWindowState(st, "5h", anchor, fixedNow)
	if got != WindowHonestStateAutoStale {
		t.Errorf("got %v (%q), want AutoStale — provider-written anchor with elapsed reset is stale, not manual", got, got.String())
	}
}

// TestDeriveWindowState_F8_RunningSchedulerStillUsesRunningPath pins
// codex bar #4: when Running=true && LastFetchSucceeded != zero, the
// existing path is taken regardless of whether ProviderAnchors is set.
// Behavior of the running scheduler must be unchanged.
func TestDeriveWindowState_F8_RunningSchedulerStillUsesRunningPath(t *testing.T) {
	anchor := validAnchor(fixedNow.Add(-30*time.Minute), 240)
	anchor.ID = 555
	// (a) Running with ProviderAnchors unset.
	if got := DeriveWindowState(runningWithSuccess(), "5h", anchor, fixedNow); got != WindowHonestStateAutoFresh {
		t.Errorf("Running=true, no ProviderAnchors: got %v, want AutoFresh", got)
	}
	// (b) Running with ProviderAnchors deliberately mismatched (different
	// ID). The existing Running-path classifies first, so the mismatched
	// metadata is irrelevant.
	stMismatch := withProviderMeta(runningWithSuccess(), "5h", usagescheduler.ProviderAnchorMeta{
		ID: 999, SyncedAt: "different", TimeLeftMinutes: 1,
	})
	if got := DeriveWindowState(stMismatch, "5h", anchor, fixedNow); got != WindowHonestStateAutoFresh {
		t.Errorf("Running=true, mismatched ProviderAnchors: got %v, want AutoFresh — running path takes precedence", got)
	}
}

// TestDeriveWindowState_F8_IDMismatchWithStringMatch pins codex bar from
// msg 20764: even when SyncedAt and TimeLeftMinutes happen to match, an
// ID mismatch must classify as ManualAnchor. Belt-and-suspenders fields
// alone are not sufficient.
func TestDeriveWindowState_F8_IDMismatchWithStringMatch(t *testing.T) {
	anchor := validAnchor(fixedNow.Add(-30*time.Minute), 240)
	anchor.ID = 7
	st := withProviderMeta(stoppedSchedulerNoError(), "5h", usagescheduler.ProviderAnchorMeta{
		ID:              8, // differs from anchor.ID
		SyncedAt:        anchor.SyncedAt,
		TimeLeftMinutes: anchor.TimeLeftMinutes,
	})
	got := DeriveWindowState(st, "5h", anchor, fixedNow)
	if got != WindowHonestStateManualAnchor {
		t.Errorf("got %v, want ManualAnchor — ID mismatch must override SyncedAt+TimeLeftMinutes equality", got)
	}
}
