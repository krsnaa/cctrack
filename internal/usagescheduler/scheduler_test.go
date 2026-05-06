package usagescheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ksred/cctrack/internal/credentials"
	"github.com/ksred/cctrack/internal/store"
	"github.com/ksred/cctrack/internal/usageprovider"
)

// --- Fakes ---------------------------------------------------------------

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Sleep records the requested duration and returns immediately (or ctx.Err
// if the context is already canceled). Tests bound iteration via the fake
// provider's response sequence, not via wall-clock waiting.
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.sleeps = append(c.sleeps, d)
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func (c *fakeClock) recordedSleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]time.Duration, len(c.sleeps))
	copy(out, c.sleeps)
	return out
}

// fakeProvider returns a programmed sequence of (snapshot, err) responses.
// After the sequence is exhausted, Fetch blocks until ctx is canceled —
// this gives tests a deterministic upper bound on fetches without racing.
type fakeProvider struct {
	mu        sync.Mutex
	responses []fakeResp
	calls     int32
	calledCh  chan struct{} // pulsed on each call
}

type fakeResp struct {
	snap usageprovider.Snapshot
	err  error
}

func newFakeProvider(responses ...fakeResp) *fakeProvider {
	return &fakeProvider{
		responses: responses,
		calledCh:  make(chan struct{}, 16),
	}
}

func (p *fakeProvider) Fetch(ctx context.Context, _ credentials.Credentials) (usageprovider.Snapshot, error) {
	n := int(atomic.AddInt32(&p.calls, 1))
	select {
	case p.calledCh <- struct{}{}:
	default:
	}
	p.mu.Lock()
	if n-1 >= len(p.responses) {
		// Sequence exhausted: block until ctx is canceled.
		p.mu.Unlock()
		<-ctx.Done()
		return usageprovider.Snapshot{}, ctx.Err()
	}
	resp := p.responses[n-1]
	p.mu.Unlock()
	return resp.snap, resp.err
}

func (p *fakeProvider) callCount() int { return int(atomic.LoadInt32(&p.calls)) }

// fakeStore captures SaveWindowAnchor + ObservedCostForWindow calls.
//
// costErrFor / saveErrFor allow per-window-type error injection so tests
// can pin the row-level "at most one successful row before an error" bar
// from EM chat msg 20565 (constraint #8).
type fakeStore struct {
	mu          sync.Mutex
	saved       []store.WindowAnchor
	costCalls   []costCall
	costReturns map[string]float64 // windowType -> cost; default 0
	costErrFor  map[string]error   // windowType -> err for ObservedCostForWindow (nil = no error)
	saveErrFor  map[string]error   // windowType -> err for SaveWindowAnchor (nil = no error)
}

type costCall struct {
	WindowType string
	ObservedAt time.Time
	ResetsAt   time.Time
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		costReturns: map[string]float64{},
		costErrFor:  map[string]error{},
		saveErrFor:  map[string]error{},
	}
}

func (s *fakeStore) SaveWindowAnchor(a store.WindowAnchor) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.saveErrFor[a.WindowType]; err != nil {
		return 0, err
	}
	s.saved = append(s.saved, a)
	return int64(len(s.saved)), nil
}

func (s *fakeStore) ObservedCostForWindow(windowType string, observedAt, resetsAt time.Time) (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.costCalls = append(s.costCalls, costCall{windowType, observedAt, resetsAt})
	if err := s.costErrFor[windowType]; err != nil {
		return 0, err
	}
	return s.costReturns[windowType], nil
}

func (s *fakeStore) savedAnchors() []store.WindowAnchor {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]store.WindowAnchor, len(s.saved))
	copy(out, s.saved)
	return out
}

func (s *fakeStore) recordedCostCalls() []costCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]costCall, len(s.costCalls))
	copy(out, s.costCalls)
	return out
}

// validCreds returns a non-empty credentials value for the fake loader.
func validCreds() credentials.Credentials {
	return credentials.Credentials{
		AccessToken: "fake-token-only-for-tests",
		ExpiresAt:   time.Now().Add(time.Hour),
	}
}

// nopLog discards log output.
func nopLog(string, ...any) {}

// runUntilCalls runs the scheduler in a goroutine and cancels ctx as soon
// as `wantCalls` Fetch calls have been observed. This bounds tests
// deterministically without relying on wall-clock timing.
func runUntilCalls(t *testing.T, s *Scheduler, p *fakeProvider, wantCalls int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()

	// Wait for wantCalls fetches.
	for i := 0; i < wantCalls; i++ {
		select {
		case <-p.calledCh:
		case <-time.After(2 * time.Second):
			t.Fatalf("waiting for Fetch call %d/%d; got %d total", i+1, wantCalls, p.callCount())
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not exit after context cancel")
	}
}

// runUntilDone runs the scheduler in a goroutine and waits for it to exit
// on its own (i.e. errStop fired). Returns the call count.
func runUntilDone(t *testing.T, s *Scheduler) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not exit on permanent error")
	}
}

// --- Tests ---------------------------------------------------------------

// validSnapshot constructs a healthy snapshot anchored to observedAt with
// reset times in the future.
func validSnapshot(observedAt time.Time, fiveHourReset, sevenDayReset time.Duration) usageprovider.Snapshot {
	return usageprovider.Snapshot{
		FiveHourUtilizationPercent: 42,
		SevenDayUtilizationPercent: 73,
		FiveHourResetsAt:           observedAt.Add(fiveHourReset),
		SevenDayResetsAt:           observedAt.Add(sevenDayReset),
		Observed:                   observedAt,
	}
}

// TestRun_StartupFetchWritesBothAnchors verifies the scheduler fetches
// once on Run entry and writes one anchor per window through the shared
// store. Both windows should be written via SaveWindowAnchor.
func TestRun_StartupFetchWritesBothAnchors(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(now)

	snap := validSnapshot(now, 2*time.Hour, 7*24*time.Hour)
	provider := newFakeProvider(fakeResp{snap: snap})
	storeF := newFakeStore()
	storeF.costReturns["5h"] = 1.23
	storeF.costReturns["7d"] = 4.56

	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, storeF, nopLog).WithClock(clock)

	runUntilCalls(t, sched, provider, 1)

	saved := storeF.savedAnchors()
	if len(saved) != 2 {
		t.Fatalf("saved %d anchors, want 2", len(saved))
	}

	// Verify exactly one 5h and one 7d anchor.
	byType := map[string]store.WindowAnchor{}
	for _, a := range saved {
		byType[a.WindowType] = a
	}
	if _, ok := byType["5h"]; !ok {
		t.Errorf("missing 5h anchor")
	}
	if _, ok := byType["7d"]; !ok {
		t.Errorf("missing 7d anchor")
	}
}

// TestRun_AnchorUsesSnapshotObservedNotClockNow pins binding constraint #5:
// SyncedAt must derive from snap.Observed, not the scheduler's clock.Now().
// We set the fake clock to a wildly different time than the snapshot's
// Observed and verify SyncedAt matches Observed.
func TestRun_AnchorUsesSnapshotObservedNotClockNow(t *testing.T) {
	snapObserved := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	clockNow := snapObserved.Add(15 * time.Minute) // clock has drifted
	clock := newFakeClock(clockNow)

	snap := validSnapshot(snapObserved, 2*time.Hour, 7*24*time.Hour)
	provider := newFakeProvider(fakeResp{snap: snap})
	storeF := newFakeStore()
	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, storeF, nopLog).WithClock(clock)

	runUntilCalls(t, sched, provider, 1)

	for _, a := range storeF.savedAnchors() {
		want := snapObserved.UTC().Format(time.RFC3339Nano)
		if a.SyncedAt != want {
			t.Errorf("%s SyncedAt = %q, want %q (must derive from snap.Observed, not clock.Now())", a.WindowType, a.SyncedAt, want)
		}
	}
}

// TestRun_StaleSnapshotRejection verifies binding constraint #6: a window
// whose resets_at is at or before observed_at must NOT produce an anchor
// row. The other window with a future reset should still write.
func TestRun_StaleSnapshotRejection(t *testing.T) {
	observed := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	snap := usageprovider.Snapshot{
		FiveHourUtilizationPercent: 42,
		SevenDayUtilizationPercent: 73,
		FiveHourResetsAt:           observed.Add(-1 * time.Minute), // STALE
		SevenDayResetsAt:           observed.Add(7 * 24 * time.Hour),
		Observed:                   observed,
	}
	provider := newFakeProvider(fakeResp{snap: snap})
	storeF := newFakeStore()
	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, storeF, nopLog).WithClock(newFakeClock(observed))

	runUntilCalls(t, sched, provider, 1)

	saved := storeF.savedAnchors()
	if len(saved) != 1 {
		t.Fatalf("saved %d anchors, want exactly 1 (5h is stale)", len(saved))
	}
	if saved[0].WindowType != "7d" {
		t.Errorf("saved anchor for %q, want 7d only", saved[0].WindowType)
	}
}

// TestRun_SubMinuteFutureResetRoundsUp verifies binding constraint #5:
// a future-but-subminute reset must produce TimeLeftMinutes=1, not 0.
func TestRun_SubMinuteFutureResetRoundsUp(t *testing.T) {
	observed := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	snap := usageprovider.Snapshot{
		FiveHourUtilizationPercent: 1,
		SevenDayUtilizationPercent: 2,
		FiveHourResetsAt:           observed.Add(20 * time.Second),       // sub-minute future
		SevenDayResetsAt:           observed.Add(7 * 24 * time.Hour),     // normal
		Observed:                   observed,
	}
	provider := newFakeProvider(fakeResp{snap: snap})
	storeF := newFakeStore()
	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, storeF, nopLog).WithClock(newFakeClock(observed))

	runUntilCalls(t, sched, provider, 1)

	for _, a := range storeF.savedAnchors() {
		if a.WindowType == "5h" && a.TimeLeftMinutes < 1 {
			t.Errorf("5h sub-minute reset gave TimeLeftMinutes=%d; want >=1 (no permanently-stale zero)", a.TimeLeftMinutes)
		}
	}
}

// TestRun_CostHelperCalledWithSnapshotObserved pins binding constraint #4:
// the scheduler must invoke ObservedCostForWindow with snap.Observed, not
// the scheduler's clock.Now(), so the cost window matches the upstream
// observation moment exactly. Drift between scheduler and manual sync is
// thereby impossible.
func TestRun_CostHelperCalledWithSnapshotObserved(t *testing.T) {
	snapObserved := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	clockNow := snapObserved.Add(7 * time.Minute) // scheduler clock has drifted forward
	clock := newFakeClock(clockNow)

	snap := validSnapshot(snapObserved, 2*time.Hour, 7*24*time.Hour)
	provider := newFakeProvider(fakeResp{snap: snap})
	storeF := newFakeStore()
	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, storeF, nopLog).WithClock(clock)

	runUntilCalls(t, sched, provider, 1)

	calls := storeF.recordedCostCalls()
	if len(calls) != 2 {
		t.Fatalf("ObservedCostForWindow call count = %d, want 2", len(calls))
	}
	for _, c := range calls {
		if !c.ObservedAt.Equal(snapObserved) {
			t.Errorf("%s ObservedCostForWindow observedAt = %v, want snap.Observed=%v (NOT clock.Now=%v)",
				c.WindowType, c.ObservedAt, snapObserved, clockNow)
		}
	}
}

// TestRun_StopsOnUnauthorized verifies the loop exits without further
// fetches when the provider returns ErrUnauthorized (token rejected by
// upstream).
func TestRun_StopsOnUnauthorized(t *testing.T) {
	provider := newFakeProvider(fakeResp{err: usageprovider.ErrUnauthorized})
	storeF := newFakeStore()
	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, storeF, nopLog).WithClock(newFakeClock(time.Now()))

	runUntilDone(t, sched)

	if got := provider.callCount(); got != 1 {
		t.Errorf("Fetch called %d times after ErrUnauthorized; want 1 (stop after first)", got)
	}
	if anchors := storeF.savedAnchors(); len(anchors) != 0 {
		t.Errorf("got %d anchors written after ErrUnauthorized; want 0", len(anchors))
	}
}

// TestRun_StopsOnSchemaDrift verifies the loop exits on ErrSchemaDrift —
// schema changed unexpectedly, so manual sync is the only path.
func TestRun_StopsOnSchemaDrift(t *testing.T) {
	provider := newFakeProvider(fakeResp{err: usageprovider.ErrSchemaDrift})
	storeF := newFakeStore()
	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, storeF, nopLog).WithClock(newFakeClock(time.Now()))

	runUntilDone(t, sched)
	if got := provider.callCount(); got != 1 {
		t.Errorf("Fetch called %d times after ErrSchemaDrift; want 1", got)
	}
}

// TestRun_StopsOnCredentialsMissing verifies the loop exits when
// credentials.Load returns ErrCredentialsMissing — no provider call should
// be made because we have no token.
func TestRun_StopsOnCredentialsMissing(t *testing.T) {
	provider := newFakeProvider() // never called
	storeF := newFakeStore()
	loader := func() (credentials.Credentials, error) {
		return credentials.Credentials{}, credentials.ErrCredentialsMissing
	}
	sched := New(provider, loader, storeF, nopLog).WithClock(newFakeClock(time.Now()))

	runUntilDone(t, sched)
	if got := provider.callCount(); got != 0 {
		t.Errorf("Fetch called %d times after ErrCredentialsMissing; want 0 (no token to use)", got)
	}
}

// TestRun_StopsOnTokenExpired same as missing — permanent class.
func TestRun_StopsOnTokenExpired(t *testing.T) {
	provider := newFakeProvider()
	storeF := newFakeStore()
	loader := func() (credentials.Credentials, error) {
		return credentials.Credentials{}, credentials.ErrTokenExpired
	}
	sched := New(provider, loader, storeF, nopLog).WithClock(newFakeClock(time.Now()))

	runUntilDone(t, sched)
	if got := provider.callCount(); got != 0 {
		t.Errorf("Fetch called %d times after ErrTokenExpired; want 0", got)
	}
}

// TestRun_BackoffOnTransientError verifies that a transient provider
// failure causes the scheduler to sleep with the initial backoff, retry,
// and on second-try success cleanly progresses (resetting backoff). We
// record the sleep durations and verify the first one matches the initial
// backoff constant.
func TestRun_BackoffOnTransientError(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(now)
	snap := validSnapshot(now, 2*time.Hour, 7*24*time.Hour)
	provider := newFakeProvider(
		fakeResp{err: usageprovider.ErrProviderUnavailable},
		fakeResp{snap: snap}, // success on retry
	)
	storeF := newFakeStore()
	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, storeF, nopLog).WithClock(clock)

	runUntilCalls(t, sched, provider, 2) // wait for both calls

	sleeps := clock.recordedSleeps()
	if len(sleeps) < 1 {
		t.Fatalf("no sleeps recorded; want at least 1 backoff sleep")
	}
	// First sleep after the transient error must be the initial backoff.
	if sleeps[0] != backoffInitial {
		t.Errorf("first sleep = %v, want backoffInitial=%v", sleeps[0], backoffInitial)
	}
}

// TestRun_BackoffOnRateLimited mirrors TestRun_BackoffOnTransientError but
// with ErrRateLimited (the other transient class). 429 must drive the same
// exponential schedule, not a different code path.
func TestRun_BackoffOnRateLimited(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(now)
	snap := validSnapshot(now, 2*time.Hour, 7*24*time.Hour)
	provider := newFakeProvider(
		fakeResp{err: usageprovider.ErrRateLimited},
		fakeResp{snap: snap},
	)
	storeF := newFakeStore()
	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, storeF, nopLog).WithClock(clock)

	runUntilCalls(t, sched, provider, 2)
	sleeps := clock.recordedSleeps()
	if len(sleeps) < 1 {
		t.Fatalf("no sleeps recorded; want at least 1 backoff sleep")
	}
	if sleeps[0] != backoffInitial {
		t.Errorf("first sleep after ErrRateLimited = %v, want backoffInitial=%v", sleeps[0], backoffInitial)
	}
}

// TestRun_BackoffSequenceCapsAtMax verifies the exponential backoff
// schedule and its cap. With backoffInitial=30s, factor=2, max=5min, the
// schedule is: 30s, 60s, 120s, 240s, 300s (cap), 300s, ... This pins the
// cap behavior at scheduler.go's backoff doubling logic.
func TestRun_BackoffSequenceCapsAtMax(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(now)
	snap := validSnapshot(now, 2*time.Hour, 7*24*time.Hour)
	// Six consecutive transient errors then a success — covers initial,
	// three doublings, the cap clamp, and one post-cap sleep.
	provider := newFakeProvider(
		fakeResp{err: usageprovider.ErrProviderUnavailable}, // sleep[0]: 30s
		fakeResp{err: usageprovider.ErrProviderUnavailable}, // sleep[1]: 60s
		fakeResp{err: usageprovider.ErrProviderUnavailable}, // sleep[2]: 120s
		fakeResp{err: usageprovider.ErrProviderUnavailable}, // sleep[3]: 240s
		fakeResp{err: usageprovider.ErrProviderUnavailable}, // sleep[4]: 300s (cap)
		fakeResp{err: usageprovider.ErrProviderUnavailable}, // sleep[5]: 300s (still cap)
		fakeResp{snap: snap},                                // success — backoff resets
	)
	storeF := newFakeStore()
	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, storeF, nopLog).WithClock(clock)

	runUntilCalls(t, sched, provider, 7)
	sleeps := clock.recordedSleeps()
	if len(sleeps) < 6 {
		t.Fatalf("recorded %d sleeps; want at least 6 backoff sleeps before success", len(sleeps))
	}
	want := []time.Duration{
		30 * time.Second,
		60 * time.Second,
		120 * time.Second,
		240 * time.Second,
		300 * time.Second, // cap reached
		300 * time.Second, // still capped
	}
	for i, w := range want {
		if sleeps[i] != w {
			t.Errorf("sleeps[%d] = %v, want %v (exponential schedule with cap at backoffMax=%v)", i, sleeps[i], w, backoffMax)
		}
	}
	// Final cap matches the constant.
	if sleeps[4] != backoffMax {
		t.Errorf("at-cap sleep = %v, want backoffMax=%v", sleeps[4], backoffMax)
	}
}

// TestRun_FetchesAreSequentialNotOverlapping verifies that scheduler
// fetches do not overlap. The scheduler is single-goroutine by construction
// (Run loop is sequential), but the test pins the property: fetch N+1
// strictly starts after fetch N returns, so concurrent provider calls
// cannot occur in this codepath. Combined with the provider's own
// process-wide mutex (usageprovider.Client.mu), this gives end-to-end
// single-flight.
func TestRun_FetchesAreSequentialNotOverlapping(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(now)
	snap := validSnapshot(now, 2*time.Hour, 7*24*time.Hour)

	// Fake provider that records (start, end) timestamps for each Fetch
	// call. We block briefly inside Fetch to give any racing call a chance
	// to overlap — if scheduler ever spawned a second Fetch in parallel, the
	// recorded intervals would overlap and the assertion would fail.
	type interval struct{ start, end time.Time }
	var (
		intervalsMu sync.Mutex
		intervals   []interval
	)
	provider := &recordingProvider{
		fakeProvider: newFakeProvider(
			fakeResp{snap: snap},
			fakeResp{snap: snap},
			fakeResp{snap: snap},
		),
		onCall: func() (start, end time.Time) {
			s := time.Now()
			time.Sleep(5 * time.Millisecond) // simulate non-trivial in-flight time
			e := time.Now()
			intervalsMu.Lock()
			intervals = append(intervals, interval{s, e})
			intervalsMu.Unlock()
			return s, e
		},
	}
	storeF := newFakeStore()
	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, storeF, nopLog).WithClock(clock)

	runUntilCalls(t, sched, provider.fakeProvider, 3)

	intervalsMu.Lock()
	defer intervalsMu.Unlock()
	if len(intervals) < 3 {
		t.Fatalf("recorded %d intervals; want >=3", len(intervals))
	}
	// Verify no overlap: interval[i+1].start must be at-or-after interval[i].end.
	for i := 0; i < len(intervals)-1; i++ {
		if intervals[i+1].start.Before(intervals[i].end) {
			t.Errorf("fetch %d started at %v before fetch %d ended at %v (overlap = single-flight broken)",
				i+1, intervals[i+1].start, i, intervals[i].end)
		}
	}
}

// recordingProvider wraps fakeProvider with a hook that fires on each call.
// Used only by TestRun_FetchesAreSequentialNotOverlapping.
type recordingProvider struct {
	*fakeProvider
	onCall func() (start, end time.Time)
}

func (r *recordingProvider) Fetch(ctx context.Context, c credentials.Credentials) (usageprovider.Snapshot, error) {
	r.onCall()
	return r.fakeProvider.Fetch(ctx, c)
}

// TestRun_PostSuccessDelayMatchesEarliestReset verifies binding constraint
// #7: after a successful fetch, the next sleep duration is approximately
// (earliest reset + grace) - clock.Now(). With 5h and 7d resets, the 5h
// reset wins (earliest).
func TestRun_PostSuccessDelayMatchesEarliestReset(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(now)
	const fiveHourLead = 5 * time.Hour
	snap := validSnapshot(now, fiveHourLead, 7*24*time.Hour)
	provider := newFakeProvider(fakeResp{snap: snap})
	storeF := newFakeStore()
	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, storeF, nopLog).WithClock(clock)

	runUntilCalls(t, sched, provider, 1)

	sleeps := clock.recordedSleeps()
	if len(sleeps) == 0 {
		t.Fatalf("no sleeps recorded; want at least 1 post-success sleep")
	}
	want := fiveHourLead + graceDelay
	if sleeps[0] != want {
		t.Errorf("post-success sleep = %v, want %v (earliest reset + grace)", sleeps[0], want)
	}
}

// TestRun_ContextCancellationExitsCleanly verifies that an in-flight
// scheduler exits when its context is canceled. We use a slow fake provider
// (blocks on the calledCh consumer) and cancel context mid-flight.
func TestRun_ContextCancellationExitsCleanly(t *testing.T) {
	provider := newFakeProvider() // sequence exhausted -> Fetch blocks on ctx
	storeF := newFakeStore()
	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, storeF, nopLog).WithClock(newFakeClock(time.Now()))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sched.Run(ctx)
		close(done)
	}()

	// Wait for first Fetch call to start (sequence empty -> Fetch blocks).
	select {
	case <-provider.calledCh:
	case <-time.After(time.Second):
		t.Fatal("scheduler never started Fetch")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not exit on context cancel")
	}
}

// TestRun_BothWindowsStaleStops verifies that a snapshot with both reset
// times at-or-before observed_at causes the loop to stop (per codex-2 chat
// msg 20587 finding #1: no periodic-poll fallback). Manual sync remains
// available; auto-refresh waits for process restart.
func TestRun_BothWindowsStaleStops(t *testing.T) {
	observed := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	snap := usageprovider.Snapshot{
		FiveHourUtilizationPercent: 1,
		SevenDayUtilizationPercent: 2,
		FiveHourResetsAt:           observed.Add(-1 * time.Minute), // STALE
		SevenDayResetsAt:           observed.Add(-2 * time.Minute), // STALE
		Observed:                   observed,
	}
	provider := newFakeProvider(fakeResp{snap: snap})
	storeF := newFakeStore()
	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, storeF, nopLog).WithClock(newFakeClock(observed))

	runUntilDone(t, sched)

	if got := provider.callCount(); got != 1 {
		t.Errorf("Fetch called %d times after both-stale; want exactly 1 (stop)", got)
	}
	if anchors := storeF.savedAnchors(); len(anchors) != 0 {
		t.Errorf("got %d anchors written when both windows stale; want 0", len(anchors))
	}
}

// TestRun_CostErrorOnOneWindowSkipsThatAnchor verifies the row-level write
// behavior (per codex-2 chat msg 20587 finding #3): when ObservedCostForWindow
// fails for one window but succeeds for the other, exactly one anchor is
// written. The scheduler does not abort the loop on a per-window cost error.
func TestRun_CostErrorOnOneWindowSkipsThatAnchor(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	snap := validSnapshot(now, 2*time.Hour, 7*24*time.Hour)
	provider := newFakeProvider(fakeResp{snap: snap})
	storeF := newFakeStore()
	storeF.costErrFor["5h"] = errors.New("simulated cost helper failure for 5h")
	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, storeF, nopLog).WithClock(newFakeClock(now))

	runUntilCalls(t, sched, provider, 1)

	saved := storeF.savedAnchors()
	if len(saved) != 1 {
		t.Fatalf("saved %d anchors, want exactly 1 (5h cost failed; 7d should still write)", len(saved))
	}
	if saved[0].WindowType != "7d" {
		t.Errorf("saved anchor for %q, want 7d only", saved[0].WindowType)
	}
}

// TestRun_SaveErrorOnOneWindowDoesNotBlockOther mirrors the cost-error case
// for the SaveWindowAnchor path: one window's save fails, the other succeeds.
// The scheduler logs the failure and continues; no anchor is observable for
// the failed window. Per EM chat msg 20565 constraint #8: "at most one
// successful row before an error" is acceptable, must be tested.
func TestRun_SaveErrorOnOneWindowDoesNotBlockOther(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	snap := validSnapshot(now, 2*time.Hour, 7*24*time.Hour)
	provider := newFakeProvider(fakeResp{snap: snap})
	storeF := newFakeStore()
	storeF.saveErrFor["5h"] = errors.New("simulated save failure for 5h")
	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, storeF, nopLog).WithClock(newFakeClock(now))

	runUntilCalls(t, sched, provider, 1)

	saved := storeF.savedAnchors()
	if len(saved) != 1 {
		t.Fatalf("saved %d anchors, want exactly 1 (5h save failed; 7d should still write)", len(saved))
	}
	if saved[0].WindowType != "7d" {
		t.Errorf("saved anchor for %q, want 7d only", saved[0].WindowType)
	}
	// Cost helper was called for BOTH windows (the 5h save failed AFTER cost).
	calls := storeF.recordedCostCalls()
	if len(calls) != 2 {
		t.Errorf("ObservedCostForWindow called %d times; want 2 (both windows attempted)", len(calls))
	}
}

// TestRun_OnAnchorsUpdatedFiresAfterSuccessfulWrite verifies the EM
// callback contract (chat msg 20591/20593): cmd/serve installs an update
// callback; scheduler invokes it after at least one anchor write succeeds.
// This is the live-dashboard-freshness path for auto-sync.
func TestRun_OnAnchorsUpdatedFiresAfterSuccessfulWrite(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	snap := validSnapshot(now, 2*time.Hour, 7*24*time.Hour)
	provider := newFakeProvider(fakeResp{snap: snap})
	storeF := newFakeStore()

	var callbackCount int32
	cb := func(ctx context.Context) error {
		atomic.AddInt32(&callbackCount, 1)
		return nil
	}
	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, storeF, nopLog).
		WithClock(newFakeClock(now)).
		WithOnAnchorsUpdated(cb)

	runUntilCalls(t, sched, provider, 1)

	if got := atomic.LoadInt32(&callbackCount); got != 1 {
		t.Errorf("callback called %d times after successful tick; want 1", got)
	}
}

// TestRun_OnAnchorsUpdatedNotCalledWhenAllWritesFail verifies the gate:
// if no anchor row is written (cost or save errors on both windows), the
// callback must NOT fire. Otherwise downstream (e.g. summary.updated
// broadcast) would tell clients there's new state when there isn't.
func TestRun_OnAnchorsUpdatedNotCalledWhenAllWritesFail(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	snap := validSnapshot(now, 2*time.Hour, 7*24*time.Hour)
	provider := newFakeProvider(fakeResp{snap: snap})
	storeF := newFakeStore()
	storeF.saveErrFor["5h"] = errors.New("simulated 5h save failure")
	storeF.saveErrFor["7d"] = errors.New("simulated 7d save failure")

	var callbackCount int32
	cb := func(ctx context.Context) error {
		atomic.AddInt32(&callbackCount, 1)
		return nil
	}
	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, storeF, nopLog).
		WithClock(newFakeClock(now)).
		WithOnAnchorsUpdated(cb)

	runUntilCalls(t, sched, provider, 1)

	if got := atomic.LoadInt32(&callbackCount); got != 0 {
		t.Errorf("callback called %d times when both saves failed; want 0", got)
	}
}

// TestRun_OnAnchorsUpdatedFiresWhenAtLeastOneWriteSucceeds — partial
// success path: 5h fails, 7d succeeds. The callback fires (because at
// least one anchor row was persisted). Combined with the previous test,
// this pins "at least one" semantics, not "all must succeed."
func TestRun_OnAnchorsUpdatedFiresWhenAtLeastOneWriteSucceeds(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	snap := validSnapshot(now, 2*time.Hour, 7*24*time.Hour)
	provider := newFakeProvider(fakeResp{snap: snap})
	storeF := newFakeStore()
	storeF.saveErrFor["5h"] = errors.New("simulated 5h save failure")

	var callbackCount int32
	cb := func(ctx context.Context) error {
		atomic.AddInt32(&callbackCount, 1)
		return nil
	}
	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, storeF, nopLog).
		WithClock(newFakeClock(now)).
		WithOnAnchorsUpdated(cb)

	runUntilCalls(t, sched, provider, 1)

	if got := atomic.LoadInt32(&callbackCount); got != 1 {
		t.Errorf("callback called %d times when 7d succeeded; want 1 (at-least-one semantics)", got)
	}
}

// TestRun_OnAnchorsUpdatedErrorIsLoggedAndSwallowed verifies the EM bar:
// "callback failure must not roll back persisted anchors; log a redacted
// generic error and let the scheduler continue." A callback that returns
// an error must not crash the loop or unwind the anchor writes.
//
// Also pins the redaction discipline (per codex-2 chat msg 20597): the
// scheduler's log line for callback failure must NOT contain the
// underlying error text. cmd/serve's callback may wrap concrete
// database / serialization internals; those must not leak into
// scheduler-level logs.
func TestRun_OnAnchorsUpdatedErrorIsLoggedAndSwallowed(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	snap := validSnapshot(now, 2*time.Hour, 7*24*time.Hour)
	provider := newFakeProvider(fakeResp{snap: snap}, fakeResp{snap: snap})
	storeF := newFakeStore()

	const sentinel = "REDACTED-PROBE-DO-NOT-LEAK-12345"
	cb := func(ctx context.Context) error {
		return errors.New(sentinel + " inner database details")
	}

	// Capture logger so we can assert the redaction.
	var (
		logMu   sync.Mutex
		logBuf  []string
	)
	captureLog := func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		logBuf = append(logBuf, fmt.Sprintf(format, args...))
	}

	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, storeF, captureLog).
		WithClock(newFakeClock(now)).
		WithOnAnchorsUpdated(cb)

	runUntilCalls(t, sched, provider, 2) // verifies loop continues to a second tick

	saved := storeF.savedAnchors()
	if len(saved) != 4 { // 2 ticks * 2 windows
		t.Errorf("saved %d anchors despite callback errors; want 4 (2 ticks * 2 windows; errors must not roll back)", len(saved))
	}

	// The redaction discipline: the callback error sentinel MUST NOT appear
	// in any captured log line. The scheduler's line is a fixed redacted
	// message; the underlying error text is dropped on the floor.
	logMu.Lock()
	defer logMu.Unlock()
	for _, line := range logBuf {
		if strings.Contains(line, sentinel) {
			t.Errorf("scheduler log line leaked callback error sentinel: %q (full log: %v)", line, logBuf)
		}
	}
	// Sanity check: the callback DID fail and SOMETHING was logged about it.
	foundFailureLog := false
	for _, line := range logBuf {
		if strings.Contains(line, "callback failed") {
			foundFailureLog = true
			break
		}
	}
	if !foundFailureLog {
		t.Errorf("expected a 'callback failed' log entry; got %v", logBuf)
	}
}

// --- SyncOnce tests (F7 T7.1) -----------------------------------

func TestSyncOnce_HappyPathReturnsOkWithWindowsWritten(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	snap := validSnapshot(now, 2*time.Hour, 7*24*time.Hour)
	provider := newFakeProvider(fakeResp{snap: snap})
	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, newFakeStore(), nopLog).WithClock(newFakeClock(now))

	got := sched.SyncOnce(context.Background())
	if got.Status != "ok" {
		t.Errorf("Status = %q, want \"ok\"", got.Status)
	}
	if len(got.WindowsWritten) != 2 {
		t.Errorf("WindowsWritten = %v, want both 5h and 7d", got.WindowsWritten)
	}
	if got.SyncedAt == nil || *got.SyncedAt == "" {
		t.Errorf("SyncedAt = %v, want populated RFC3339Nano timestamp", got.SyncedAt)
	}
}

// TestSyncOnce_TokenExpiredFromCredentials maps credentials.ErrTokenExpired
// to status "token_expired" — DISTINCT from "unauthorized" per EM
// amendment #2 (chat msg 20726). Credential-loader expiration is the case
// where the user reauthenticates Claude Code.
func TestSyncOnce_TokenExpiredFromCredentials(t *testing.T) {
	loader := func() (credentials.Credentials, error) {
		return credentials.Credentials{}, credentials.ErrTokenExpired
	}
	sched := New(newFakeProvider(), loader, newFakeStore(), nopLog).WithClock(newFakeClock(time.Now()))
	got := sched.SyncOnce(context.Background())
	if got.Status != "token_expired" {
		t.Errorf("Status = %q, want \"token_expired\"", got.Status)
	}
	if got.WindowsWritten != nil || got.SyncedAt != nil {
		t.Errorf("non-nil WindowsWritten/SyncedAt on error path: %+v", got)
	}
}

// TestSyncOnce_UnauthorizedFromProvider maps provider.ErrUnauthorized to
// "unauthorized" — DISTINCT from "token_expired". Provider 401/403 means
// the token reached Anthropic but was rejected (revoked, scope mismatch).
func TestSyncOnce_UnauthorizedFromProvider(t *testing.T) {
	provider := newFakeProvider(fakeResp{err: usageprovider.ErrUnauthorized})
	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, newFakeStore(), nopLog).WithClock(newFakeClock(time.Now()))
	if got := sched.SyncOnce(context.Background()); got.Status != "unauthorized" {
		t.Errorf("Status = %q, want \"unauthorized\"", got.Status)
	}
}

func TestSyncOnce_CredentialsMissing(t *testing.T) {
	loader := func() (credentials.Credentials, error) {
		return credentials.Credentials{}, credentials.ErrCredentialsMissing
	}
	sched := New(newFakeProvider(), loader, newFakeStore(), nopLog).WithClock(newFakeClock(time.Now()))
	if got := sched.SyncOnce(context.Background()); got.Status != "credentials_missing" {
		t.Errorf("Status = %q, want \"credentials_missing\"", got.Status)
	}
}

func TestSyncOnce_CredentialsMalformed(t *testing.T) {
	loader := func() (credentials.Credentials, error) {
		return credentials.Credentials{}, credentials.ErrCredentialsMalformed
	}
	sched := New(newFakeProvider(), loader, newFakeStore(), nopLog).WithClock(newFakeClock(time.Now()))
	if got := sched.SyncOnce(context.Background()); got.Status != "credentials_malformed" {
		t.Errorf("Status = %q, want \"credentials_malformed\"", got.Status)
	}
}

func TestSyncOnce_SchemaDrift(t *testing.T) {
	provider := newFakeProvider(fakeResp{err: usageprovider.ErrSchemaDrift})
	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, newFakeStore(), nopLog).WithClock(newFakeClock(time.Now()))
	if got := sched.SyncOnce(context.Background()); got.Status != "schema_drift" {
		t.Errorf("Status = %q, want \"schema_drift\"", got.Status)
	}
}

func TestSyncOnce_ProviderUnavailable(t *testing.T) {
	provider := newFakeProvider(fakeResp{err: usageprovider.ErrProviderUnavailable})
	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, newFakeStore(), nopLog).WithClock(newFakeClock(time.Now()))
	if got := sched.SyncOnce(context.Background()); got.Status != "provider_unavailable" {
		t.Errorf("Status = %q, want \"provider_unavailable\"", got.Status)
	}
}

func TestSyncOnce_RateLimited(t *testing.T) {
	provider := newFakeProvider(fakeResp{err: usageprovider.ErrRateLimited})
	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, newFakeStore(), nopLog).WithClock(newFakeClock(time.Now()))
	if got := sched.SyncOnce(context.Background()); got.Status != "rate_limited" {
		t.Errorf("Status = %q, want \"rate_limited\"", got.Status)
	}
}

// TestSyncOnce_OkWithEmptyWindowsWritten validates EM amendment 3 (chat msg
// 20726): a successful fetch where both windows had stale resets_at
// produces Status=ok with empty WindowsWritten, NOT a provider failure.
// The fetch itself succeeded; nothing was fresher to write.
func TestSyncOnce_OkWithEmptyWindowsWritten(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	snap := usageprovider.Snapshot{
		FiveHourUtilizationPercent: 1,
		SevenDayUtilizationPercent: 2,
		FiveHourResetsAt:           now.Add(-1 * time.Minute),
		SevenDayResetsAt:           now.Add(-2 * time.Minute),
		Observed:                   now,
	}
	provider := newFakeProvider(fakeResp{snap: snap})
	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, newFakeStore(), nopLog).WithClock(newFakeClock(now))

	got := sched.SyncOnce(context.Background())
	if got.Status != "ok" {
		t.Errorf("Status = %q, want \"ok\" (fetch succeeded; no anchor was fresher)", got.Status)
	}
	if len(got.WindowsWritten) != 0 {
		t.Errorf("WindowsWritten = %v, want empty (both windows stale)", got.WindowsWritten)
	}
	if got.SyncedAt == nil {
		t.Error("SyncedAt is nil; want populated on Status=ok even with empty windows_written")
	}
}

// TestSyncOnce_ConcurrentDoubleClickReturnsInProgress validates the
// single-flight contract: two simultaneous SyncOnce calls — one wins
// the CAS and runs the fetch, the other returns Status="in_progress"
// immediately without starting a duplicate Fetch.
//
// Uses a started/release barrier to deterministically hold the first
// SyncOnce inside Fetch while the test fires the second call.
func TestSyncOnce_ConcurrentDoubleClickReturnsInProgress(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	snap := validSnapshot(now, 2*time.Hour, 7*24*time.Hour)

	var fetchCount int32
	started := make(chan struct{})
	release := make(chan struct{})
	provider := &recordingProvider{
		fakeProvider: newFakeProvider(fakeResp{snap: snap}, fakeResp{snap: snap}),
		onCall: func() (start, end time.Time) {
			atomic.AddInt32(&fetchCount, 1)
			// Only the first caller signals started; subsequent calls
			// would have already short-circuited at the CAS check.
			select {
			case <-started: // already closed: do nothing
			default:
				close(started)
			}
			<-release
			return time.Now(), time.Now()
		},
	}
	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, newFakeStore(), nopLog).WithClock(newFakeClock(now))

	firstResult := make(chan SyncStatus, 1)
	go func() {
		firstResult <- sched.SyncOnce(context.Background())
	}()

	// Wait for the first call to acquire syncing and enter Fetch.
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first SyncOnce did not reach Fetch within 1s")
	}

	// While first is held inside onCall (release not yet closed), the
	// CAS in the second call must fail, returning in_progress without
	// reaching the provider.
	secondResult := sched.SyncOnce(context.Background())
	if secondResult.Status != "in_progress" {
		t.Errorf("second SyncOnce returned %q, want \"in_progress\" (single-flight)", secondResult.Status)
	}

	close(release)
	first := <-firstResult
	if first.Status != "ok" {
		t.Errorf("first SyncOnce returned %q, want \"ok\"", first.Status)
	}

	if got := atomic.LoadInt32(&fetchCount); got != 1 {
		t.Errorf("provider.Fetch entered %d times, want exactly 1 (single-flight prevented duplicate)", got)
	}
}

// TestSyncOnce_TickCASLossIsBenign validates EM amendment 1 (chat msg
// 20726): when SyncOnce holds the syncing flag, a scheduled tick that
// loses the CAS must NOT classify as a provider failure. No error class
// change, no transient-error log, no honest-state poisoning. The tick
// returns a benign next-delay and the next scheduled wake fires normally.
//
// Uses a started/release barrier so the SyncOnce goroutine deterministically
// holds the syncing flag while the test exercises tick — without the
// barrier the test races against SyncOnce's completion.
func TestSyncOnce_TickCASLossIsBenign(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(now)
	snap := validSnapshot(now, 2*time.Hour, 7*24*time.Hour)

	var (
		logMu  sync.Mutex
		logBuf []string
	)
	captureLog := func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		logBuf = append(logBuf, fmt.Sprintf(format, args...))
	}

	started := make(chan struct{})
	release := make(chan struct{})
	provider := &recordingProvider{
		fakeProvider: newFakeProvider(fakeResp{snap: snap}),
		onCall: func() (start, end time.Time) {
			close(started)
			<-release // block until test releases
			return time.Now(), time.Now()
		},
	}
	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, newFakeStore(), captureLog).WithClock(clock)

	syncDone := make(chan SyncStatus, 1)
	go func() {
		syncDone <- sched.SyncOnce(context.Background())
	}()

	// Wait for SyncOnce to acquire the syncing flag and enter Fetch.
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("SyncOnce did not reach Fetch within 1s")
	}

	preState := sched.Snapshot()
	delay, err := sched.tick(context.Background())
	if err != nil {
		t.Errorf("tick returned err = %v on CAS loss; want nil (benign)", err)
	}
	if delay <= 0 {
		t.Errorf("tick returned delay = %v on CAS loss; want positive (benign next-delay)", delay)
	}
	postState := sched.Snapshot()
	if postState.LastErrorClass != preState.LastErrorClass {
		t.Errorf("tick CAS-loss changed LastErrorClass from %v to %v; want unchanged", preState.LastErrorClass, postState.LastErrorClass)
	}

	logMu.Lock()
	for _, line := range logBuf {
		if strings.Contains(line, "transient error") || strings.Contains(line, "permanent") {
			t.Errorf("tick CAS-loss logged %q; want no error/transient log", line)
		}
	}
	logMu.Unlock()

	close(release)
	<-syncDone
}

// --- State accessor tests (T2.3.1 #648) -----------------------------------

// TestSnapshot_InitialStateIsZero verifies a freshly-constructed scheduler
// reports the zero-value State (no run, no fetches, no errors). This is the
// baseline that internal/usagestate's derivation logic interprets as
// "auto-sync hasn't tried yet" — anchor presence then drives the user-facing
// state to manual_anchor or fallback_cascade.
func TestSnapshot_InitialStateIsZero(t *testing.T) {
	provider := newFakeProvider()
	storeF := newFakeStore()
	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, storeF, nopLog).WithClock(newFakeClock(time.Now()))

	got := sched.Snapshot()
	if got.Running {
		t.Errorf("initial Running = true; want false (Run not yet called)")
	}
	if !got.LastFetchSucceeded.IsZero() {
		t.Errorf("initial LastFetchSucceeded = %v; want zero", got.LastFetchSucceeded)
	}
	if got.LastErrorClass != ErrorClassNone {
		t.Errorf("initial LastErrorClass = %v; want ErrorClassNone", got.LastErrorClass)
	}
}

// TestSnapshot_RunningTrackedFromRunEntryToExit verifies the Running flag
// flips to true while the loop is active and back to false after Run returns.
// Tests the setRunning(true) at entry + defer setRunning(false) at exit
// pattern.
func TestSnapshot_RunningTrackedFromRunEntryToExit(t *testing.T) {
	provider := newFakeProvider(fakeResp{err: usageprovider.ErrUnauthorized})
	storeF := newFakeStore()
	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, storeF, nopLog).WithClock(newFakeClock(time.Now()))

	runUntilDone(t, sched) // Unauthorized → errStop → Run exits

	// After Run returns, Running must be false.
	got := sched.Snapshot()
	if got.Running {
		t.Errorf("Running = true after Run exit; want false")
	}
}

// TestSnapshot_SuccessClearsLastErrorAndSetsTimestamp pins the
// recordFetchSuccess path: a successful tick updates LastFetchSucceeded
// to clock.Now() AND clears any previously-recorded error class.
func TestSnapshot_SuccessClearsLastErrorAndSetsTimestamp(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(now)
	snap := validSnapshot(now, 2*time.Hour, 7*24*time.Hour)
	// Provider sequence: transient error first (sets LastErrorClass), then
	// success (must clear it and set LastFetchSucceeded).
	provider := newFakeProvider(
		fakeResp{err: usageprovider.ErrProviderUnavailable},
		fakeResp{snap: snap},
	)
	storeF := newFakeStore()
	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, storeF, nopLog).WithClock(clock)

	runUntilCalls(t, sched, provider, 2)

	got := sched.Snapshot()
	if got.LastErrorClass != ErrorClassNone {
		t.Errorf("LastErrorClass after success = %v; want ErrorClassNone (success clears prior error)", got.LastErrorClass)
	}
	if !got.LastFetchSucceeded.Equal(now) {
		t.Errorf("LastFetchSucceeded = %v; want clock.Now()=%v", got.LastFetchSucceeded, now)
	}
}

// TestSnapshot_ErrorClassRecording verifies each scheduler error-class
// pathway updates LastErrorClass to the right typed enum.
func TestSnapshot_ErrorClassRecording(t *testing.T) {
	cases := []struct {
		name       string
		credErr    error
		providerErr error
		want       ErrorClass
	}{
		{"CredentialsMissing", credentials.ErrCredentialsMissing, nil, ErrorClassCredentialsMissing},
		{"TokenExpired", credentials.ErrTokenExpired, nil, ErrorClassTokenExpired},
		{"CredentialsMalformed", credentials.ErrCredentialsMalformed, nil, ErrorClassCredentialsMalformed},
		{"Unauthorized", nil, usageprovider.ErrUnauthorized, ErrorClassUnauthorized},
		{"SchemaDrift", nil, usageprovider.ErrSchemaDrift, ErrorClassSchemaDrift},
		{"ProviderUnavailable", nil, usageprovider.ErrProviderUnavailable, ErrorClassProviderUnavailable},
		{"RateLimited", nil, usageprovider.ErrRateLimited, ErrorClassRateLimited},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			loader := func() (credentials.Credentials, error) {
				if c.credErr != nil {
					return credentials.Credentials{}, c.credErr
				}
				return validCreds(), nil
			}
			var provider *fakeProvider
			if c.providerErr != nil {
				provider = newFakeProvider(fakeResp{err: c.providerErr})
			} else {
				provider = newFakeProvider() // never called when creds fail
			}
			sched := New(provider, loader, newFakeStore(), nopLog).WithClock(newFakeClock(time.Now()))

			// For permanent error classes (all of these except provider-transient),
			// runUntilDone is the right shape. Transient classes don't stop, so
			// runUntilCalls is needed.
			isTransient := c.want == ErrorClassProviderUnavailable || c.want == ErrorClassRateLimited
			if isTransient {
				runUntilCalls(t, sched, provider, 1)
				// After 1 transient failure, scheduler is in backoff sleep;
				// we cancel via runUntilCalls's deferred cancel. State should
				// reflect the recorded error.
			} else {
				runUntilDone(t, sched)
			}

			got := sched.Snapshot()
			if got.LastErrorClass != c.want {
				t.Errorf("LastErrorClass = %v, want %v", got.LastErrorClass, c.want)
			}
			if !got.LastFetchSucceeded.IsZero() {
				t.Errorf("LastFetchSucceeded = %v on error path; want zero (no successful fetch)", got.LastFetchSucceeded)
			}
		})
	}
}

// TestSnapshot_ConcurrentReadIsRaceSafe verifies the mutex protects
// concurrent Snapshot reads against in-flight state updates from Run.
// Run with -race for full effect; without race detector this still
// exercises the codepath under concurrency.
func TestSnapshot_ConcurrentReadIsRaceSafe(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	snap := validSnapshot(now, 2*time.Hour, 7*24*time.Hour)
	// Many successful fetches to drive state mutations.
	resps := make([]fakeResp, 20)
	for i := range resps {
		resps[i] = fakeResp{snap: snap}
	}
	provider := newFakeProvider(resps...)
	storeF := newFakeStore()
	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, storeF, nopLog).WithClock(newFakeClock(now))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		sched.Run(ctx)
		close(done)
	}()

	// Concurrent readers.
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = sched.Snapshot() // race detector validates safety
			}
		}()
	}
	wg.Wait()
	cancel()
	<-done
}

// TestEarliestFutureReset verifies the helper picks the strict-future
// minimum and returns zero when both are stale.
func TestEarliestFutureReset(t *testing.T) {
	observed := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		fiveHrReset time.Time
		sevenDReset time.Time
		want        time.Time
	}{
		{
			name:        "5h earlier",
			fiveHrReset: observed.Add(2 * time.Hour),
			sevenDReset: observed.Add(7 * 24 * time.Hour),
			want:        observed.Add(2 * time.Hour),
		},
		{
			name:        "7d earlier",
			fiveHrReset: observed.Add(8 * 24 * time.Hour),
			sevenDReset: observed.Add(2 * 24 * time.Hour),
			want:        observed.Add(2 * 24 * time.Hour),
		},
		{
			name:        "5h stale, 7d future -> 7d wins",
			fiveHrReset: observed.Add(-1 * time.Minute),
			sevenDReset: observed.Add(2 * 24 * time.Hour),
			want:        observed.Add(2 * 24 * time.Hour),
		},
		{
			name:        "both stale -> zero",
			fiveHrReset: observed.Add(-1 * time.Minute),
			sevenDReset: observed.Add(-2 * time.Minute),
			want:        time.Time{},
		},
		{
			name:        "both equal to observed (not strictly future) -> zero",
			fiveHrReset: observed,
			sevenDReset: observed,
			want:        time.Time{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			snap := usageprovider.Snapshot{
				Observed:         observed,
				FiveHourResetsAt: c.fiveHrReset,
				SevenDayResetsAt: c.sevenDReset,
			}
			got := earliestFutureReset(snap)
			if !got.Equal(c.want) {
				t.Errorf("earliestFutureReset = %v, want %v", got, c.want)
			}
		})
	}
}

// _ marks errors imported but not always referenced (depending on test
// case selection); keeps the import list stable.
var _ = errors.Is
