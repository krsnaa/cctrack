package usagescheduler

import (
	"context"
	"errors"
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
func TestRun_OnAnchorsUpdatedErrorIsLoggedAndSwallowed(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	snap := validSnapshot(now, 2*time.Hour, 7*24*time.Hour)
	provider := newFakeProvider(fakeResp{snap: snap}, fakeResp{snap: snap})
	storeF := newFakeStore()

	cb := func(ctx context.Context) error {
		return errors.New("simulated callback failure")
	}
	sched := New(provider, func() (credentials.Credentials, error) { return validCreds(), nil }, storeF, nopLog).
		WithClock(newFakeClock(now)).
		WithOnAnchorsUpdated(cb)

	runUntilCalls(t, sched, provider, 2) // verifies loop continues to a second tick

	saved := storeF.savedAnchors()
	if len(saved) != 4 { // 2 ticks * 2 windows
		t.Errorf("saved %d anchors despite callback errors; want 4 (2 ticks * 2 windows; errors must not roll back)", len(saved))
	}
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
