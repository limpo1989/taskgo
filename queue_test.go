/*
 * Copyright 2023 the taskgo project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package taskgo

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var _ interface{ Submit(func()) bool } = (*Queue)(nil)

// ---------- Push behavior ----------

func TestPushExecutesAllTasks(t *testing.T) {
	q := New(WithConcurrency(4))
	var done int64
	var wg sync.WaitGroup
	const total = 5000
	wg.Add(total)
	for i := 0; i < total; i++ {
		q.Push(func() {
			atomic.AddInt64(&done, 1)
			wg.Done()
		})
	}
	wg.Wait()
	if got := atomic.LoadInt64(&done); got != total {
		t.Fatalf("done = %d, want %d", got, total)
	}
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func TestPushNilIgnored(t *testing.T) {
	q := New()
	q.Push(nil) // must not panic and must not start a worker
	if q.runningCount() != 0 {
		t.Fatal("nil push should not start a worker")
	}
	_ = q.Stop(context.Background())
}

func TestConcurrencyLimit(t *testing.T) {
	const limit = 3
	q := New(WithConcurrency(limit))
	var cur, max int64
	var release = make(chan struct{})
	var wg sync.WaitGroup
	const total = 20
	wg.Add(total)
	for i := 0; i < total; i++ {
		q.Push(func() {
			c := atomic.AddInt64(&cur, 1)
			for {
				m := atomic.LoadInt64(&max)
				if c <= m || atomic.CompareAndSwapInt64(&max, m, c) {
					break
				}
			}
			<-release
			atomic.AddInt64(&cur, -1)
			wg.Done()
		})
	}
	// Give the scheduler a moment to let concurrency ramp up to the limit.
	waitFor(t, time.Second, func() bool { return atomic.LoadInt64(&max) == limit })
	close(release)
	wg.Wait()
	if atomic.LoadInt64(&max) > limit {
		t.Fatalf("max concurrency = %d, exceeds limit %d", max, limit)
	}
	_ = q.Stop(context.Background())
}

func TestBacklogLen(t *testing.T) {
	q := New(WithConcurrency(1))
	block := make(chan struct{})
	started := make(chan struct{})
	q.Push(func() { close(started); <-block }) // occupy the only worker
	<-started
	q.Push(func() {})
	q.Push(func() {})
	waitFor(t, time.Second, func() bool { return q.Len() == 2 })
	close(block)
	_ = q.Stop(context.Background())
}

// ---------- worker reuse / parking ----------

func TestWorkerReuseWhenIdleEnabled(t *testing.T) {
	q := New(WithConcurrency(1), WithMaxIdle(time.Minute), withManualJanitor())
	run := func() {
		var wg sync.WaitGroup
		wg.Add(1)
		q.Push(func() { wg.Done() })
		wg.Wait()
	}
	run()
	// After the task completes the worker should be parked in idle.
	waitFor(t, time.Second, func() bool { return q.idleLen() == 1 })
	run()
	// The second run should reuse the same single worker.
	waitFor(t, time.Second, func() bool { return q.idleLen() == 1 })
	_ = q.Stop(context.Background())
}

func TestNoIdleMeansImmediateExit(t *testing.T) {
	q := New(WithConcurrency(1)) // maxIdle=0
	var wg sync.WaitGroup
	wg.Add(1)
	q.Push(func() { wg.Done() })
	wg.Wait()
	// Parking disabled: the worker should exit promptly, idle stays empty, and
	// running returns to zero.
	waitFor(t, time.Second, func() bool {
		return q.idleLen() == 0 && q.runningCount() == 0
	})
	_ = q.Stop(context.Background())
}

func TestJanitorReclaimsExpired(t *testing.T) {
	now := time.Unix(1000, 0)
	q := New(WithConcurrency(2), WithMaxIdle(time.Second), withManualJanitor(),
		withNowFunc(func() time.Time { return now }))

	var wg sync.WaitGroup
	wg.Add(2)
	q.Push(func() { wg.Done() })
	q.Push(func() { wg.Done() })
	wg.Wait()
	waitFor(t, time.Second, func() bool { return q.idleLen() == 2 })

	// Before maxIdle elapses nothing is reclaimed and the janitor keeps running.
	stop := q.cleanExpired(now.Add(500 * time.Millisecond))
	if stop || q.idleLen() != 2 {
		t.Fatalf("should not reclaim early: stop=%v idle=%d", stop, q.idleLen())
	}

	// Once the deadline passes everything is reclaimed, idle empties, and the
	// janitor should stop.
	stop = q.cleanExpired(now.Add(2 * time.Second))
	if !stop {
		t.Fatal("cleanExpired should signal stop when idle empties")
	}
	waitFor(t, time.Second, func() bool { return q.idleLen() == 0 })
	_ = q.Stop(context.Background())
}

func TestJanitorPartialReclaim(t *testing.T) {
	base := time.Unix(2000, 0)
	cur := base
	q := New(WithConcurrency(2), WithMaxIdle(time.Second), withManualJanitor(),
		withNowFunc(func() time.Time { return cur }))

	// Occupy two workers at once so two independent workers are created rather
	// than one being reused.
	blockA, blockB := make(chan struct{}), make(chan struct{})
	startA, startB := make(chan struct{}), make(chan struct{})
	q.Push(func() { close(startA); <-blockA })
	q.Push(func() { close(startB); <-blockB })
	<-startA
	<-startB
	waitFor(t, time.Second, func() bool { return q.runningCount() == 2 })

	// Park A at t=base.
	cur = base
	close(blockA)
	waitFor(t, time.Second, func() bool { return q.idleLen() == 1 })

	// Park B at t=base+800ms.
	cur = base.Add(800 * time.Millisecond)
	close(blockB)
	waitFor(t, time.Second, func() bool { return q.idleLen() == 2 })

	// At base+1.2s: A has expired (>=1s) but B has not, so only A is reclaimed.
	stop := q.cleanExpired(base.Add(1200 * time.Millisecond))
	if stop {
		t.Fatal("should not stop while one worker remains idle")
	}
	if q.idleLen() != 1 {
		t.Fatalf("idle = %d, want 1", q.idleLen())
	}
	_ = q.Stop(context.Background())
}

func TestRealJanitorAutoReclaim(t *testing.T) {
	// No manual hook here, so this exercises the real background janitor.
	q := New(WithConcurrency(1), WithMaxIdle(20*time.Millisecond))
	var wg sync.WaitGroup
	wg.Add(1)
	q.Push(func() { wg.Done() })
	wg.Wait()
	// The real janitor should reclaim the worker within a few maxIdle periods.
	waitFor(t, 2*time.Second, func() bool {
		return q.idleLen() == 0 && q.runningCount() == 0
	})
	_ = q.Stop(context.Background())
}

// ---------- maxJobs ----------

func TestMaxJobsRecyclesWorkerUnderLoad(t *testing.T) {
	// A single worker under sustained load with maxJobs=10: every 10 tasks it
	// should be replaced by a fresh worker. Approximating via goroutine id is
	// not feasible, so count worker starts instead.
	var spawns int64
	q := New(WithConcurrency(1), WithMaxJobs(10))
	q.onSpawnForTest = func() { atomic.AddInt64(&spawns, 1) }

	var wg sync.WaitGroup
	const total = 100
	wg.Add(total)
	// Occupy the worker first to build up a backlog, so the path taken is
	// "queue non-empty and capReached".
	block := make(chan struct{})
	started := make(chan struct{})
	q.Push(func() { close(started); <-block; wg.Done() })
	<-started
	for i := 0; i < total-1; i++ {
		q.Push(func() { wg.Done() })
	}
	close(block)
	wg.Wait()
	// total=100, maxJobs=10 => the worker should rotate several times (>1 spawn).
	if atomic.LoadInt64(&spawns) < 2 {
		t.Fatalf("spawns = %d, want >= 2 (worker should recycle under maxJobs)", spawns)
	}
	_ = q.Stop(context.Background())
}

func TestMaxJobsWithIdleResetsCounter(t *testing.T) {
	// maxJobs=2 with parking enabled: the counter resets after each wake-up, so
	// the worker must not be recycled because of a count accumulated across
	// multiple parking cycles.
	q := New(WithConcurrency(1), WithMaxJobs(2), WithMaxIdle(time.Minute), withManualJanitor())
	for i := 0; i < 5; i++ {
		var wg sync.WaitGroup
		wg.Add(1)
		q.Push(func() { wg.Done() })
		wg.Wait()
		waitFor(t, time.Second, func() bool { return q.idleLen() == 1 })
	}
	_ = q.Stop(context.Background())
}

// ---------- panic handling ----------

func TestPanicHandlerRecovers(t *testing.T) {
	var caught atomic.Value
	q := New(WithConcurrency(1), WithPanicHandler(func(v any) { caught.Store(v) }))
	var wg sync.WaitGroup
	wg.Add(2)
	q.Push(func() { defer wg.Done(); panic("boom") })
	// The worker should keep handling later tasks after a panic.
	q.Push(func() { defer wg.Done() })
	wg.Wait()
	if got := caught.Load(); got != "boom" {
		t.Fatalf("caught = %v, want boom", got)
	}
	_ = q.Stop(context.Background())
}

// ---------- Stop ----------

func TestStopDrainsBacklog(t *testing.T) {
	q := New(WithConcurrency(2))
	var done int64
	for i := 0; i < 200; i++ {
		q.Push(func() {
			time.Sleep(time.Millisecond)
			atomic.AddInt64(&done, 1)
		})
	}
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := atomic.LoadInt64(&done); got != 200 {
		t.Fatalf("done = %d, want 200", got)
	}
}

func TestStopIdempotent(t *testing.T) {
	q := New()
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("first stop: %v", err)
	}
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("second stop: %v", err)
	}
}

func TestPushAfterStopDropped(t *testing.T) {
	q := New(WithConcurrency(1))
	_ = q.Stop(context.Background())
	var ran int64
	q.Push(func() { atomic.AddInt64(&ran, 1) })
	time.Sleep(20 * time.Millisecond)
	if atomic.LoadInt64(&ran) != 0 {
		t.Fatal("task after stop should be dropped")
	}
}

func TestSubmitRejectsAtPendingLimit(t *testing.T) {
	q := New(WithConcurrency(1), WithMaxPending(2))
	started := make(chan struct{})
	release := make(chan struct{})
	if !q.Submit(func() {
		close(started)
		<-release
	}) {
		t.Fatal("first Submit rejected")
	}
	<-started
	if !q.Submit(func() {}) {
		t.Fatal("second Submit rejected")
	}
	if q.Submit(func() {}) {
		t.Fatal("third Submit accepted past pending limit")
	}
	close(release)
	if err := q.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitReleasesPendingSlotAfterCompletion(t *testing.T) {
	q := New(WithConcurrency(1), WithMaxPending(1))
	done := make(chan struct{})
	if !q.Submit(func() { close(done) }) {
		t.Fatal("first Submit rejected")
	}
	<-done
	if !q.Submit(func() {}) {
		t.Fatal("Submit rejected after completed task")
	}
	if err := q.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitRejectsAfterStop(t *testing.T) {
	q := New(WithMaxPending(1))
	if err := q.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if q.Submit(func() {}) {
		t.Fatal("Submit accepted after Stop")
	}
}

func TestStopDispersesIdleWorkers(t *testing.T) {
	q := New(WithConcurrency(3), WithMaxIdle(time.Minute), withManualJanitor())
	var wg sync.WaitGroup
	wg.Add(3)
	for i := 0; i < 3; i++ {
		q.Push(func() { wg.Done() })
	}
	wg.Wait()
	waitFor(t, time.Second, func() bool { return q.idleLen() == 3 })
	// Stop should dismiss every parked worker and return promptly, since they
	// are not counted as running.
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if q.idleLen() != 0 {
		t.Fatalf("idle = %d after stop, want 0", q.idleLen())
	}
}

func TestStopTimeout(t *testing.T) {
	q := New(WithConcurrency(1), WithTimeout(30*time.Millisecond))
	start := make(chan struct{})
	q.Push(func() {
		close(start)
		time.Sleep(500 * time.Millisecond) // exceeds the timeout
	})
	<-start
	err := q.Stop(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}

func TestStopParentCtxCancel(t *testing.T) {
	q := New(WithConcurrency(1), WithTimeout(time.Hour))
	start := make(chan struct{})
	q.Push(func() {
		close(start)
		time.Sleep(300 * time.Millisecond)
	})
	<-start
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	err := q.Stop(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want Canceled", err)
	}
}

// TestStopFinishAtDeadline covers the finished() recheck in Stop's ctx.Done
// branch: when a task finishes right at the timeout, Stop should report a clean
// drain (nil) rather than a timeout. It retries to reliably hit this narrow
// race window.
func TestStopFinishAtDeadline(t *testing.T) {
	const attempts = 400
	hit := false
	for i := 0; i < attempts && !hit; i++ {
		q := New(WithConcurrency(1), WithTimeout(15*time.Millisecond))
		start := make(chan struct{})
		q.Push(func() {
			close(start)
			time.Sleep(15 * time.Millisecond) // same order as the timeout, to create a "just finished" window
		})
		<-start
		if err := q.Stop(context.Background()); err == nil {
			hit = true // hit: ctx timed out but the task finished, so the finished() recheck returns nil
		}
	}
	if !hit {
		t.Skip("did not hit the finish-at-deadline race window in this run")
	}
}

// ---------- long-running soak test (run with -race) ----------

// soakDuration is how long TestSoakHighConcurrency keeps the queue under load.
// It is kept above 10s so the run spans many janitor cycles, worker-recycle
// rotations, and park/wake transitions.
const soakDuration = 12 * time.Second

// TestSoakHighConcurrency drives the queue at high concurrency for an extended
// period to confirm it stays stable under sustained pressure. Every reuse path
// is exercised at once: parking with a short maxIdle (heavy janitor churn),
// worker recycling via maxJobs, recovered panics, and a deep/shallow task mix
// pushed by many concurrent producers.
//
// It asserts the load-bearing invariants the whole time: live workers never
// exceed the concurrency limit, parked workers never exceed it either, no task
// is lost or run twice, every panic is recovered, and the queue shuts down
// cleanly afterward. Run with -race to also catch data races.
//
// The test is skipped under `go test -short`.
func TestSoakHighConcurrency(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping soak test in -short mode")
	}

	const (
		concurrency = 256
		producers   = 32
		panicEveryN = 997 // a prime, so panics scatter across producers
		deepDepth   = 200
	)

	var (
		recovered int64 // tasks whose panic the handler caught
		completed int64 // tasks that returned normally
		panicked  int64 // tasks that chose to panic
		invariant int64 // set to 1 on the first invariant violation
	)

	q := New(
		WithConcurrency(concurrency),
		WithMaxIdle(2*time.Millisecond), // tight idle window: constant park/reclaim churn
		WithMaxJobs(128),                // force periodic worker recycling under load
		WithPanicHandler(func(any) { atomic.AddInt64(&recovered, 1) }),
	)

	// Monitor goroutine: continuously sample the live/parked counts and flag any
	// breach of the concurrency cap. Reads go through the locked helpers, so this
	// is race-clean.
	stopMonitor := make(chan struct{})
	var monWG sync.WaitGroup
	monWG.Add(1)
	go func() {
		defer monWG.Done()
		for {
			select {
			case <-stopMonitor:
				return
			default:
			}
			if r := q.runningCount(); r > concurrency {
				atomic.StoreInt64(&invariant, 1)
				t.Errorf("running = %d exceeds concurrency limit %d", r, concurrency)
				return
			}
			if idle := q.idleLen(); idle > concurrency {
				atomic.StoreInt64(&invariant, 1)
				t.Errorf("idle = %d exceeds concurrency limit %d", idle, concurrency)
				return
			}
		}
	}()

	// deepWork builds a deep call chain so some tasks force stack growth, the
	// scenario worker reuse is meant to help.
	var deepWork func(depth int, acc *int)
	deepWork = func(depth int, acc *int) {
		if depth == 0 {
			*acc += fib(12)
			return
		}
		var pad [8]int
		pad[depth&7] = depth
		deepWork(depth-1, acc)
		*acc += pad[depth&7]
	}

	deadline := time.Now().Add(soakDuration)
	var submitted int64
	var prodWG sync.WaitGroup
	prodWG.Add(producers)
	for p := 0; p < producers; p++ {
		go func(seed int) {
			defer prodWG.Done()
			i := seed
			for time.Now().Before(deadline) {
				i++
				n := atomic.AddInt64(&submitted, 1)
				doPanic := n%panicEveryN == 0
				deep := i&1 == 0
				q.Push(func() {
					if doPanic {
						atomic.AddInt64(&panicked, 1)
						panic("soak boom")
					}
					if deep {
						var acc int
						deepWork(deepDepth, &acc)
						_ = acc
					} else {
						fib(12)
					}
					atomic.AddInt64(&completed, 1)
				})
			}
		}(p * 7)
	}
	prodWG.Wait()

	// Every submitted task must reach a terminal state: it either completed or
	// its panic was recovered. Wait for that to settle.
	total := atomic.LoadInt64(&submitted)
	waitFor(t, 30*time.Second, func() bool {
		return atomic.LoadInt64(&completed)+atomic.LoadInt64(&recovered) == total
	})

	close(stopMonitor)
	monWG.Wait()

	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("stop after soak: %v", err)
	}

	// Final invariant checks.
	if atomic.LoadInt64(&invariant) != 0 {
		t.Fatal("concurrency invariant was violated during the run")
	}
	if got := atomic.LoadInt64(&completed) + atomic.LoadInt64(&recovered); got != total {
		t.Fatalf("accounted tasks = %d, want %d (completed=%d recovered=%d)",
			got, total, atomic.LoadInt64(&completed), atomic.LoadInt64(&recovered))
	}
	if c, p := atomic.LoadInt64(&recovered), atomic.LoadInt64(&panicked); c != p {
		t.Fatalf("recovered = %d, want %d (every panic must be caught)", c, p)
	}
	// After a clean stop nothing should be left running or parked.
	if r, idle := q.runningCount(), q.idleLen(); r != 0 || idle != 0 {
		t.Fatalf("after stop running=%d idle=%d, want 0/0", r, idle)
	}
	t.Logf("soak ok: submitted=%d completed=%d recovered=%d over %v",
		total, atomic.LoadInt64(&completed), atomic.LoadInt64(&recovered), soakDuration)
}

// ---------- concurrency stress (run with -race) ----------

func TestConcurrentPushStress(t *testing.T) {
	q := New(WithConcurrency(8), WithMaxIdle(2*time.Millisecond), WithMaxJobs(64))
	var done int64
	var wg sync.WaitGroup
	const producers = 16
	const each = 2000
	wg.Add(producers * each)
	var pwg sync.WaitGroup
	pwg.Add(producers)
	for p := 0; p < producers; p++ {
		go func() {
			defer pwg.Done()
			for i := 0; i < each; i++ {
				q.Push(func() {
					fib(10)
					atomic.AddInt64(&done, 1)
					wg.Done()
				})
			}
		}()
	}
	pwg.Wait()
	wg.Wait()
	if got := atomic.LoadInt64(&done); got != producers*each {
		t.Fatalf("done = %d, want %d", got, producers*each)
	}
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
}
