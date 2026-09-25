package taskgo

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// queuedBehindRunning is how many tasks the Len tests leave waiting behind a
// running task.
const queuedBehindRunning = 8

func TestTaskPushBatchLenCountsQueuedValues(t *testing.T) {
	var completed int64
	started0 := make(chan struct{})
	release0 := make(chan struct{})
	started1 := make(chan struct{})
	release1 := make(chan struct{})
	q := NewTask(func(value int) {
		if value == 0 {
			close(started0)
			<-release0
		}
		if value == 1 {
			close(started1)
			<-release1
		}
		atomic.AddInt64(&completed, 1)
	}, WithConcurrency(1))
	q.Push(0)
	<-started0
	values := make([]int, queuedBehindRunning)
	for i := range values {
		values[i] = i + 1
	}
	if accepted := q.PushBatch(values); accepted != len(values) {
		t.Fatalf("accepted = %d, want %d", accepted, len(values))
	}
	close(release0)
	<-started1
	waitFor(t, time.Second, func() bool { return q.Len() == queuedBehindRunning-1 })
	close(release1)
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := atomic.LoadInt64(&completed); got != int64(queuedBehindRunning+1) {
		t.Fatalf("completed = %d, want %d", got, queuedBehindRunning+1)
	}
}

func TestTaskParkingAndManualJanitor(t *testing.T) {
	now := time.Unix(3000, 0)
	var wg sync.WaitGroup
	wg.Add(2)
	startA, startB := make(chan struct{}), make(chan struct{})
	releaseA, releaseB := make(chan struct{}), make(chan struct{})
	q := NewTask(func(value int) {
		if value == 1 {
			close(startA)
			<-releaseA
		} else {
			close(startB)
			<-releaseB
		}
		wg.Done()
	}, WithConcurrency(2), WithMaxIdle(time.Second),
		withManualJanitor(), withNowFunc(func() time.Time { return now }))
	q.Push(1)
	q.Push(2)
	<-startA
	<-startB
	close(releaseA)
	close(releaseB)
	wg.Wait()
	waitFor(t, time.Second, func() bool { return q.idleLen() == 2 })
	if stop := q.cleanExpired(now.Add(500 * time.Millisecond)); stop {
		t.Fatal("janitor reclaimed workers too early")
	}
	if stop := q.cleanExpired(now.Add(2 * time.Second)); !stop {
		t.Fatal("janitor should stop after reclaiming all workers")
	}
	waitFor(t, time.Second, func() bool { return q.liveCount() == 0 })
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func TestTaskAutomaticJanitor(t *testing.T) {
	done := make(chan struct{})
	q := NewTask(func(int) { close(done) }, WithConcurrency(1), WithMaxIdle(5*time.Millisecond))
	q.Push(1)
	<-done
	waitFor(t, time.Second, func() bool { return q.idleLen() == 0 })
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func TestTaskPanicAndStopTimeout(t *testing.T) {
	var caught atomic.Value
	q := NewTask(func(int) { panic("typed boom") }, WithPanicHandler(func(v any) { caught.Store(v) }))
	q.Push(1)
	waitFor(t, time.Second, func() bool { return caught.Load() != nil })
	if got := caught.Load(); got != "typed boom" {
		t.Fatalf("caught = %v, want typed boom", got)
	}
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("stop panic queue: %v", err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	q2 := NewTask(func(int) { close(started); <-release }, WithConcurrency(1), WithTimeout(10*time.Millisecond))
	q2.Push(1)
	<-started
	if err := q2.Stop(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stop err = %v, want deadline exceeded", err)
	}
	close(release)
	waitFor(t, time.Second, func() bool { return q2.runningCount() == 0 })
	if accepted := q2.PushBatch([]int{1}); accepted != 0 {
		t.Fatalf("PushBatch after stop accepted %d tasks", accepted)
	}
}

func TestLegacyBatchAndNilSubmit(t *testing.T) {
	q := New(WithConcurrency(1))
	done := make(chan struct{}, 2)
	jobs := []Job{
		func() { done <- struct{}{} },
		nil,
		func() { done <- struct{}{} },
	}
	if accepted := q.PushBatch(jobs); accepted != 2 {
		t.Fatalf("PushBatch accepted = %d, want 2", accepted)
	}
	if q.Submit(nil) {
		t.Fatal("Submit(nil) accepted a nil job")
	}
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if len(done) != 2 {
		t.Fatalf("completed = %d, want 2", len(done))
	}
}

// TestStallMonitorSpillsBlockedWorkers pins the soft target to two workers,
// blocks both, and expects the monitor to add a running slot for the task
// queued behind them.
func TestStallMonitorSpillsBlockedWorkers(t *testing.T) {
	q := New(WithConcurrency(16))
	q.setBaseForTest(2)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	for i := 0; i < 2; i++ {
		q.Push(func() {
			started <- struct{}{}
			<-release
		})
	}
	<-started
	<-started

	fastStarted := make(chan struct{})
	q.Push(func() { close(fastStarted) })
	select {
	case <-fastStarted:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("stall monitor did not create an overflow worker")
	}
	close(release)
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func TestTypedStallMonitorSpillsBlockedWorkers(t *testing.T) {
	typedRelease := make(chan struct{})
	typedFastStarted := make(chan struct{})
	typedStarted := make(chan struct{}, 2)
	q := NewTask(func(value int) {
		if value < 2 {
			typedStarted <- struct{}{}
			<-typedRelease
			return
		}
		close(typedFastStarted)
	}, WithConcurrency(16))
	q.setBaseForTest(2)
	q.Push(0)
	q.Push(1)
	<-typedStarted
	<-typedStarted
	q.Push(2)
	select {
	case <-typedFastStarted:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("typed stall monitor did not create an overflow worker")
	}
	close(typedRelease)
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// TestStallCompensationShrinksBack checks that slots added for stuck workers
// are withdrawn once they finish, so the running count returns to the soft
// target instead of ratcheting up.
func TestStallCompensationShrinksBack(t *testing.T) {
	release := make(chan struct{})
	var fast int64
	q := NewTask(func(block bool) {
		if block {
			<-release
			return
		}
		atomic.AddInt64(&fast, 1)
		time.Sleep(50 * time.Microsecond)
	}, WithConcurrency(64), WithMaxIdle(time.Minute), withManualJanitor())
	q.setBaseForTest(4)
	for i := 0; i < 4; i++ {
		q.Push(true)
	}
	waitFor(t, time.Second, func() bool { return q.runningCount() == 4 })
	for i := 0; i < 200; i++ {
		q.Push(false)
	}
	// Every base slot is held by a blocked task, so the fast tasks can only
	// finish on workers the monitor added.
	waitFor(t, 2*time.Second, func() bool { return atomic.LoadInt64(&fast) == 200 })
	close(release)
	waitFor(t, 2*time.Second, func() bool { return atomic.LoadInt64(&q.target) == 4 })
	waitFor(t, 2*time.Second, func() bool { return q.runningCount() == 0 })
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// TestLoadProbeFallback covers the lateness heuristic used when the runtime
// has no runnable-goroutine metric.
func TestLoadProbeFallback(t *testing.T) {
	var p loadProbe
	p.load(0) // resolve metric support
	if schedMetricsSupported {
		t.Skip("runtime reports scheduler goroutine metrics; fallback not used")
	}
	if got := p.load(0); got != loadIdle {
		t.Fatalf("no lateness = %v, want idle", got)
	}
	if got := p.load(monitorTick / 2); got != loadNormal {
		t.Fatalf("half-tick lateness = %v, want normal", got)
	}
	if got := p.load(3 * monitorTick); got != loadBusy {
		t.Fatalf("long lateness = %v, want busy", got)
	}
}
