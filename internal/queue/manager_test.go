package queue

import (
	"sync"
	"testing"
	"time"

	"ymca-wellness-dapp/internal/service"
)

// fakeClock drives Manager.now/sleep deterministically. sleep advances
// the clock instead of blocking, and records every requested duration.
type fakeClock struct {
	mu     sync.Mutex
	t      time.Time
	sleeps []time.Duration
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) sleep(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sleeps = append(c.sleeps, d)
	c.t = c.t.Add(d)
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *fakeClock) sleepLog() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.sleeps...)
}

// runRecord is one observed job execution.
type runRecord struct {
	requestID string
	start     time.Time
}

func newTestManager(t *testing.T, clock *fakeClock, minInterval time.Duration) *Manager {
	t.Helper()
	m := NewManager(nil, 10, time.Minute)
	m.now = clock.now
	m.sleep = clock.sleep
	m.SetMinInterval(minInterval)
	return m
}

func job(admin, id string) *TransferJob {
	return &TransferJob{
		RequestID:  id,
		Input:      service.TransferRewardInput{AdminDID: admin},
		EnqueuedAt: time.Now(),
	}
}

func waitOrFail(t *testing.T, wg *sync.WaitGroup, what string) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestSameAdminSpacing: three jobs for one admin, each taking 300ms of
// (fake) time, with a 1s minimum interval. Every job after the first must
// start exactly minInterval after the previous one finished, and FIFO
// order must hold.
func TestSameAdminSpacing(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}
	const interval = time.Second
	const jobDuration = 300 * time.Millisecond
	m := newTestManager(t, clock, interval)

	var (
		mu   sync.Mutex
		runs []runRecord
		wg   sync.WaitGroup
	)
	m.run = func(j *TransferJob) {
		defer wg.Done()
		mu.Lock()
		runs = append(runs, runRecord{requestID: j.RequestID, start: clock.now()})
		mu.Unlock()
		clock.advance(jobDuration) // the transfer takes this long
	}

	ids := []string{"r1", "r2", "r3"}
	wg.Add(len(ids))
	for _, id := range ids {
		if err := m.Enqueue(job("adminA", id)); err != nil {
			t.Fatalf("enqueue %s: %v", id, err)
		}
	}
	waitOrFail(t, &wg, "three jobs")

	mu.Lock()
	defer mu.Unlock()
	if len(runs) != 3 {
		t.Fatalf("ran %d jobs, want 3", len(runs))
	}
	for i, r := range runs {
		if r.requestID != ids[i] {
			t.Errorf("run %d = %s, want %s (FIFO broken)", i, r.requestID, ids[i])
		}
	}
	for i := 1; i < len(runs); i++ {
		prevEnd := runs[i-1].start.Add(jobDuration)
		gap := runs[i].start.Sub(prevEnd)
		if gap != interval {
			t.Errorf("gap between end of %s and start of %s = %s, want %s", ids[i-1], ids[i], gap, interval)
		}
	}
	sleeps := clock.sleepLog()
	if len(sleeps) != 2 {
		t.Fatalf("sleep called %d times, want 2 (never before the first job)", len(sleeps))
	}
	for i, d := range sleeps {
		if d != interval {
			t.Errorf("sleep %d = %s, want %s", i, d, interval)
		}
	}
}

// TestSpacingUsesRemainingTime: if wall time has already passed since the
// previous job finished, only the remainder of the interval is slept.
func TestSpacingUsesRemainingTime(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}
	m := newTestManager(t, clock, time.Second)

	var wg sync.WaitGroup
	first := make(chan struct{})
	m.run = func(j *TransferJob) {
		defer wg.Done()
		if j.RequestID == "r1" {
			close(first)
		}
	}

	wg.Add(1)
	if err := m.Enqueue(job("adminA", "r1")); err != nil {
		t.Fatal(err)
	}
	<-first
	waitOrFail(t, &wg, "first job")
	// 400ms of real-world time elapses before the next request arrives.
	clock.advance(400 * time.Millisecond)

	wg.Add(1)
	if err := m.Enqueue(job("adminA", "r2")); err != nil {
		t.Fatal(err)
	}
	waitOrFail(t, &wg, "second job")

	sleeps := clock.sleepLog()
	if len(sleeps) != 1 || sleeps[0] != 600*time.Millisecond {
		t.Fatalf("sleeps = %v, want [600ms]", sleeps)
	}
}

// TestDifferentAdminsNotSerialised: while admin A's job is blocked inside
// the transfer, admin B's job must still start, and no spacing sleep may
// be charged to B on account of A.
func TestDifferentAdminsNotSerialised(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}
	m := newTestManager(t, clock, time.Second)

	releaseA := make(chan struct{})
	bStarted := make(chan struct{})
	var wg sync.WaitGroup
	m.run = func(j *TransferJob) {
		defer wg.Done()
		switch j.Input.AdminDID {
		case "adminA":
			<-releaseA
		case "adminB":
			close(bStarted)
		}
	}

	wg.Add(3)
	if err := m.Enqueue(job("adminA", "a1")); err != nil {
		t.Fatal(err)
	}
	if err := m.Enqueue(job("adminA", "a2")); err != nil {
		t.Fatal(err)
	}
	if err := m.Enqueue(job("adminB", "b1")); err != nil {
		t.Fatal(err)
	}

	select {
	case <-bStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("admin B's job did not start while admin A's job was in flight")
	}
	if n := len(clock.sleepLog()); n != 0 {
		t.Fatalf("sleep called %d times before any job finished, want 0", n)
	}

	close(releaseA)
	waitOrFail(t, &wg, "all jobs")

	// Exactly one spacing sleep: between a1 and a2. None for b1.
	sleeps := clock.sleepLog()
	if len(sleeps) != 1 || sleeps[0] != time.Second {
		t.Fatalf("sleeps = %v, want [1s] (a1->a2 only)", sleeps)
	}
}

// TestSpacingDisabled: PAYOUT_MIN_INTERVAL_MS=0 never sleeps.
func TestSpacingDisabled(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}
	m := newTestManager(t, clock, 0)

	var wg sync.WaitGroup
	m.run = func(*TransferJob) { wg.Done() }
	wg.Add(3)
	for _, id := range []string{"r1", "r2", "r3"} {
		if err := m.Enqueue(job("adminA", id)); err != nil {
			t.Fatal(err)
		}
	}
	waitOrFail(t, &wg, "three jobs")
	if n := len(clock.sleepLog()); n != 0 {
		t.Fatalf("sleep called %d times with spacing disabled, want 0", n)
	}
}

func TestSetMinIntervalClampsNegative(t *testing.T) {
	m := NewManager(nil, 10, time.Minute)
	m.SetMinInterval(-time.Second)
	if got := m.MinInterval(); got != 0 {
		t.Fatalf("MinInterval = %s, want 0", got)
	}
}

// TestPerAdminOverride: adminA has a 5s override, adminB uses the 1s
// default, adminC is set to 0. Each admin's second job is spaced by its
// own interval only.
func TestPerAdminOverride(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}
	m := newTestManager(t, clock, time.Second)
	m.SetAdminMinInterval("adminA", 5*time.Second)
	m.SetAdminMinInterval("adminC", 0)
	m.SetAdminMinInterval("adminD", -time.Second)
	if m.IntervalFor("adminA") != 5*time.Second || m.IntervalFor("adminB") != time.Second ||
		m.IntervalFor("adminC") != 0 || m.IntervalFor("adminD") != 0 {
		t.Fatalf("IntervalFor: A=%s B=%s C=%s D=%s", m.IntervalFor("adminA"), m.IntervalFor("adminB"), m.IntervalFor("adminC"), m.IntervalFor("adminD"))
	}

	var (
		mu     sync.Mutex
		sleeps = map[string][]time.Duration{}
		wg     sync.WaitGroup
	)
	// Attribute each sleep to the admin whose worker is about to run.
	var current string
	m.sleep = func(d time.Duration) {
		mu.Lock()
		sleeps[current] = append(sleeps[current], d)
		mu.Unlock()
		clock.sleep(d)
	}
	m.run = func(j *TransferJob) { defer wg.Done() }

	// Run admins one at a time so the shared `current` label is exact.
	for _, admin := range []string{"adminA", "adminB", "adminC"} {
		mu.Lock()
		current = admin
		mu.Unlock()
		wg.Add(2)
		if err := m.Enqueue(job(admin, admin+"-1")); err != nil {
			t.Fatal(err)
		}
		if err := m.Enqueue(job(admin, admin+"-2")); err != nil {
			t.Fatal(err)
		}
		waitOrFail(t, &wg, admin)
	}

	mu.Lock()
	defer mu.Unlock()
	if got := sleeps["adminA"]; len(got) != 1 || got[0] != 5*time.Second {
		t.Errorf("adminA sleeps = %v, want [5s]", got)
	}
	if got := sleeps["adminB"]; len(got) != 1 || got[0] != time.Second {
		t.Errorf("adminB sleeps = %v, want [1s]", got)
	}
	if got := sleeps["adminC"]; len(got) != 0 {
		t.Errorf("adminC sleeps = %v, want none", got)
	}
}
