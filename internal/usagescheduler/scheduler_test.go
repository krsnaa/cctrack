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
type fakeStore struct {
	mu          sync.Mutex
	saved       []store.WindowAnchor
	costCalls   []costCall
	costReturns map[string]float64 // windowType -> cost; default 0
	costErr     error
	saveErr     error
}

type costCall struct {
	WindowType string
	ObservedAt time.Time
	ResetsAt   time.Time
}

func newFakeStore() *fakeStore {
	return &fakeStore{costReturns: map[string]float64{}}
}

func (s *fakeStore) SaveWindowAnchor(a store.WindowAnchor) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return 0, s.saveErr
	}
	s.saved = append(s.saved, a)
	return int64(len(s.saved)), nil
}

func (s *fakeStore) ObservedCostForWindow(windowType string, observedAt, resetsAt time.Time) (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.costCalls = append(s.costCalls, costCall{windowType, observedAt, resetsAt})
	if s.costErr != nil {
		return 0, s.costErr
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
