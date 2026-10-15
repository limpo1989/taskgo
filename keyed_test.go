package taskgo

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func identityKey(k uint32) uint32 { return k }

func TestNewTaskKeyedWithoutKeyOrShardsIsUnkeyed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		key    func(uint32) uint32
		shards int
	}{
		{"nil key", nil, 8},
		{"one shard", identityKey, 1},
		{"no shards", identityKey, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ran atomic.Int32
			q := NewTaskKeyed(func(uint32) { ran.Add(1) }, tc.key, tc.shards)
			if q.shards != 0 || q.rings != nil || q.keyFn != nil {
				t.Fatalf("shards=%d rings=%d keyed=%v, want the single FIFO", q.shards, len(q.rings), q.keyFn != nil)
			}
			q.Push(7)
			if err := q.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			if ran.Load() != 1 {
				t.Fatalf("ran %d, want 1", ran.Load())
			}
		})
	}
}

// A keyed queue runs every task, whichever shard its key picks and whichever
// submission path brought it.
func TestTaskKeyedRunsEveryTask(t *testing.T) {
	var ran atomic.Int64
	q := NewTaskKeyed(func(uint32) { ran.Add(1) }, identityKey, 4)
	for i := range 1000 {
		switch i % 3 {
		case 0:
			q.Push(uint32(i) % 97)
		case 1:
			q.Submit(uint32(i) % 97)
		default:
			q.PushBatch([]uint32{uint32(i) % 97})
		}
	}
	if err := q.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 1000 {
		t.Fatalf("ran %d of 1000", ran.Load())
	}
}

// Floods spread over the shards drain: workers that run their own shard dry
// find the tasks queued on the others.
func TestTaskKeyedFloodDrains(t *testing.T) {
	var ran atomic.Int64
	q := NewTaskKeyed(func(uint32) { time.Sleep(50 * time.Microsecond); ran.Add(1) },
		identityKey, 8, WithMaxIdle(30*time.Second))
	defer func() {
		if err := q.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	for round := range 5 {
		keys := make([]uint32, 512)
		for i := range keys {
			keys[i] = uint32(i * 7)
		}
		if got := q.SubmitBatch(keys); got != len(keys) {
			t.Fatalf("round %d accepted %d", round, got)
		}
		want := int64(len(keys) * (round + 1))
		waitFor(t, 2*time.Second, func() bool { return ran.Load() >= want })
	}
}

// A shard that never runs dry - here a key whose task resubmits itself - must
// not keep the only worker from the other shards: the other shard is handed
// to it once its task has waited orphanStale. Without the
// handover the task on the other shard never runs.
func TestTaskKeyedHotShardDoesNotStarveOthers(t *testing.T) {
	var q *Task[uint32]
	var stop atomic.Bool
	other := make(chan struct{})
	q = NewTaskKeyed(func(k uint32) {
		if k == 1 {
			close(other)
			return
		}
		if !stop.Load() {
			q.Push(0)
		}
	}, identityKey, 2, WithConcurrency(1))
	defer func() {
		stop.Store(true)
		if err := q.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	q.Push(0)
	// Let the key resubmit itself for a while before the other task arrives.
	waitFor(t, time.Second, func() bool { return q.rings[0].enqueued() > 1000 })
	q.Push(1)
	select {
	case <-other:
	case <-time.After(time.Second):
		t.Fatal("the task on the other shard did not run while a key resubmitted itself")
	}
}

// Len counts the tasks queued on every shard.
func TestTaskKeyedLenCountsEveryShard(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var ran atomic.Int32
	q := NewTaskKeyed(func(k uint32) {
		if k == 100 {
			close(started)
			<-release
		}
		ran.Add(1)
	}, identityKey, 4, WithConcurrency(1))
	q.Push(100)
	<-started
	for k := range uint32(20) {
		q.Push(k)
	}
	if got := q.Len(); got != 20 {
		t.Fatalf("Len=%d, want the 20 tasks queued over 4 shards", got)
	}
	close(release)
	if err := q.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 21 {
		t.Fatalf("ran %d, want 21", ran.Load())
	}
}

// Stop waits for the tasks queued on every shard, not only the first.
func TestTaskKeyedStopDrainsEveryShard(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var ran [8]atomic.Int32
	q := NewTaskKeyed(func(k uint32) {
		if k == 8 {
			close(started)
			<-release
			return
		}
		ran[k].Add(1)
	}, identityKey, 8, WithConcurrency(1))
	q.Push(8)
	<-started
	for k := range uint32(8) {
		q.Push(k)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- q.Stop(context.Background()) }()
	waitFor(t, time.Second, q.isStopped)
	select {
	case err := <-stopped:
		t.Fatalf("Stop returned before the queued shards drained: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	for k := range ran {
		if got := ran[k].Load(); got != 1 {
			t.Fatalf("task on shard %d ran %d times, want 1", k, got)
		}
	}
}

// A held batch rescued from a blocked worker goes back to the shards its keys
// pick, and another worker finds it there.
func TestTaskKeyedBatchRescueReturnsToShards(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	fast := make(chan uint32, defaultMaxBatchSize)
	var calls [defaultMaxBatchSize]int32
	q := NewTaskKeyed(func(k uint32) {
		atomic.AddInt32(&calls[k], 1)
		if k == 0 {
			close(started)
			<-release
			return
		}
		fast <- k
	}, identityKey, 2, WithConcurrency(2), WithMaxPending(defaultMaxBatchSize))
	q.setBaseForTest(1)
	atomic.StoreUint32(&q.monitorOn, 1)
	defer func() {
		unblock()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := q.Stop(ctx); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()
	if q.reservePending(defaultMaxBatchSize, true) != defaultMaxBatchSize {
		t.Fatal("failed to reserve the batch")
	}
	// The whole batch on shard 0, the test worker's own, so it is held as one.
	q.rings[0].pushN(defaultMaxBatchSize, func(i int) taskItem[uint32] {
		return taskItem[uint32]{value: uint32(i), submitted: true}
	})
	startBatchWorkerForTest(q.taskQueue)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first task did not start")
	}
	if q.lenAny() != 0 || q.Len() != defaultMaxBatchSize-1 {
		t.Fatalf("expected only a held tail: queued=%d Len=%d", q.lenAny(), q.Len())
	}
	atomic.StoreUint32(&q.monitorOn, 0)
	q.startMonitor()
	deadline := time.After(time.Second)
	for i := 1; i < defaultMaxBatchSize; i++ {
		select {
		case <-fast:
		case <-deadline:
			t.Fatalf("task %d remained behind the blocked owner", i)
		}
	}
	unblock()
	for k := range calls {
		if got := atomic.LoadInt32(&calls[k]); got != 1 {
			t.Fatalf("task %d ran %d times, want 1", k, got)
		}
	}
}

// closedLoopGaps runs keys closed loops on a keyed queue with shards shards and
// workers workers: each key has one task in flight, and a task spins for 20µs
// and resubmits its key at once, so no shard ever runs dry and a worker never
// leaves its own. With block set, a task on shard 0 blocks for most of the
// run. It returns for each key the longest time without one of its tasks
// starting, measured after a warmup: between two starts, or from the last
// start to the end, or the whole run for a key that never started.
func closedLoopGaps(t *testing.T, shards, workers, keys int, block bool) []time.Duration {
	t.Helper()
	const (
		warmup  = 200 * time.Millisecond
		measure = time.Second
		blocked = uint32(1 << 31)
	)
	var stop, measuring atomic.Bool
	last := make([]atomic.Int64, keys)
	gap := make([]atomic.Int64, keys)
	var q *Task[uint32]
	q = NewTaskKeyed(func(k uint32) {
		if k >= blocked {
			time.Sleep(measure - measure/10)
			return
		}
		if measuring.Load() {
			now := time.Now().UnixNano()
			if prev := last[k].Swap(now); prev != 0 && now-prev > gap[k].Load() {
				gap[k].Store(now - prev)
			}
		}
		for end := time.Now().Add(20 * time.Microsecond); time.Now().Before(end); {
		}
		if !stop.Load() {
			q.Push(k)
		}
	}, func(k uint32) uint32 { return k % blocked }, shards, WithConcurrency(workers))
	for k := range uint32(keys) {
		q.Push(k)
	}
	time.Sleep(warmup)
	begin := time.Now().UnixNano()
	measuring.Store(true)
	if block {
		q.Push(blocked | uint32(shards)) // lands on shard 0
	}
	time.Sleep(measure)
	end := time.Now().UnixNano()
	measuring.Store(false)
	stop.Store(true)
	if err := q.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	gaps := make([]time.Duration, keys)
	for k := range gaps {
		g := gap[k].Load()
		if l := last[k].Load(); l == 0 {
			g = end - begin
		} else if tail := end - l; tail > g { // negative for a start after end
			g = tail
		}
		gaps[k] = time.Duration(g)
	}
	return gaps
}

// saturatingShards sizes the closed-loop tests: workers leave one P for the
// monitor, which must keep ticking while they spin, and there are four shards
// per worker.
func saturatingShards(t *testing.T) (shards, workers int) {
	t.Helper()
	if testing.Short() {
		t.Skip("keeps the workers spinning for about a second")
	}
	procs := runtime.GOMAXPROCS(0)
	if procs < 4 {
		t.Skipf("needs GOMAXPROCS >= 4, have %d", procs)
	}
	workers = min(8, procs-1)
	return 4 * workers, workers
}

// With more shards than workers and no shard ever running dry, the shards
// left waiting are handed over after orphanStale, so the
// workers rotate over all of them and no key waits much longer than that.
// Without the handover the shards beyond the workers' own are never served.
func TestTaskKeyedShardsOutnumberingWorkersAreAllServed(t *testing.T) {
	shards, workers := saturatingShards(t)
	gaps := closedLoopGaps(t, shards, workers, 20*shards, false)
	worst, key := time.Duration(0), 0
	for k, g := range gaps {
		if g > worst {
			worst, key = g, k
		}
	}
	if worst > 400*time.Millisecond {
		t.Fatalf("key %d (shard %d) went %v without a task starting; shards=%d workers=%d",
			key, key%shards, worst, shards, workers)
	}
}

// A task that blocks while every other worker is busy leaves its shard without
// a worker. The shard is handed over, so the keys queued behind the blocked
// task keep being served; without the handover they wait out the block.
func TestTaskKeyedBlockedTaskDoesNotStallItsShard(t *testing.T) {
	shards, workers := saturatingShards(t)
	gaps := closedLoopGaps(t, shards, workers, 20*shards, true)
	worst := time.Duration(0)
	for k := 0; k < len(gaps); k += shards { // the keys of shard 0
		worst = max(worst, gaps[k])
	}
	if worst > 400*time.Millisecond {
		t.Fatalf("a key on the blocked task's shard went %v without a task starting; shards=%d workers=%d",
			worst, shards, workers)
	}
}

// The worst case for a keyed queue: of its only two workers one blocks and the
// other is held by a key that resubmits itself, and the concurrency limit
// leaves the monitor no worker to add. The monitor hands the stuck worker's
// shard over at the point it rescues a held batch, so the tasks queued behind
// the blocked one run on the busy worker within a few ticks, long before the
// shard would count as unattended (orphanStale).
func TestTaskKeyedBlockedAndHotShardsLeaveNoTaskBehind(t *testing.T) {
	// The monitor needs a P beside the hot worker to tick on time; with one P
	// the shard waits for the hot worker's own watches (orphanStale).
	if procs := runtime.GOMAXPROCS(0); procs < 2 {
		t.Skipf("needs GOMAXPROCS >= 2, have %d", procs)
	}
	release := make(chan struct{})
	var stop atomic.Bool
	var behind sync.WaitGroup
	const queued = 20
	var q *Task[uint32]
	q = NewTaskKeyed(func(k uint32) {
		switch {
		case k == 0:
			<-release
		case k == 1:
			if !stop.Load() {
				q.Push(1)
			}
		default:
			behind.Done()
		}
	}, func(k uint32) uint32 { return k % 4 }, 4, WithConcurrency(2))
	defer func() {
		stop.Store(true)
		close(release)
		if err := q.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	q.Push(0)
	q.Push(1)
	waitFor(t, time.Second, func() bool { return q.rings[1].enqueued() > 1000 })
	behind.Add(queued)
	for i := range uint32(queued) {
		q.Push(4 * (i + 1)) // shard 0, behind the blocked task
	}
	done := make(chan struct{})
	go func() { behind.Wait(); close(done) }()
	// Measure in the monitor's own ticks, not wall time and not q.ticks:
	// worker-assisted watches advance q.ticks too, so it runs ahead whenever
	// the monitor is scheduled late, and the stuck worker's shard is handed
	// over a few monitor ticks after the tasks arrive, where the fallback for
	// an unattended shard needs orphanStale of them.
	start := atomic.LoadUint32(&q.monTicks)
	for {
		select {
		case <-done:
			if ticks := atomic.LoadUint32(&q.monTicks) - start; ticks > 15 {
				t.Fatalf("tasks queued behind a blocked task ran only after %d monitor ticks", ticks)
			}
			return
		default:
		}
		if atomic.LoadUint32(&q.monTicks)-start > 15 {
			t.Fatal("tasks queued behind a blocked task were not handed over within 15 monitor ticks")
		}
		time.Sleep(monitorTick)
	}
}

// With as many busy workers as Ps the monitor's ticks wait for a P; the
// workers then watch the shards themselves, so the watches, and with them
// the handover of shards left without a worker, keep their pace.
func TestTaskKeyedWatchesKeepPaceWhenWorkersHoldEveryP(t *testing.T) {
	if testing.Short() {
		t.Skip("keeps the workers spinning for about a second")
	}
	procs := runtime.GOMAXPROCS(0)
	if procs < 2 {
		t.Skipf("needs GOMAXPROCS >= 2, have %d", procs)
	}
	var stop atomic.Bool
	var q *Task[uint32]
	q = NewTaskKeyed(func(k uint32) {
		for end := time.Now().Add(20 * time.Microsecond); time.Now().Before(end); {
		}
		if !stop.Load() {
			q.Push(k)
		}
	}, identityKey, 2*procs, WithConcurrency(procs))
	defer func() {
		stop.Store(true)
		if err := q.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	for k := range uint32(40 * procs) {
		q.Push(k)
	}
	time.Sleep(200 * time.Millisecond)
	const window = 500 * time.Millisecond
	before := atomic.LoadUint32(&q.ticks)
	time.Sleep(window)
	// A watch is due every monitorTick, or every step of a coarser clock; a
	// shard's wait is counted in full as long as one comes every watchCredit.
	if got, want := atomic.LoadUint32(&q.ticks)-before, uint32(window/watchCredit); got < want {
		t.Fatalf("%d watches in %v with %d workers on %d Ps, want at least %d", got, window, procs, procs, want)
	}
}

// A key that panics runs before the queue admits anything, so the queue is left
// as it was: nothing of the batch it was in runs, a bounded queue keeps its
// room, and Stop drains.
func TestTaskKeyedKeyPanicAdmitsNothing(t *testing.T) {
	key := func(k uint32) uint32 {
		if k == 13 {
			panic("bad key")
		}
		return k
	}
	for _, tc := range []struct {
		name   string
		submit func(*Task[uint32])
	}{
		{"Push", func(q *Task[uint32]) { q.Push(13) }},
		{"PushBatch", func(q *Task[uint32]) { q.PushBatch([]uint32{1, 13, 2}) }},
		{"Submit", func(q *Task[uint32]) { q.Submit(13) }},
		{"SubmitBatch", func(q *Task[uint32]) { q.SubmitBatch([]uint32{1, 13, 2}) }},
		{"TrySubmitBatch", func(q *Task[uint32]) { q.TrySubmitBatch([]uint32{1, 13, 2}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ran atomic.Int32
			q := NewTaskKeyed(func(uint32) { ran.Add(1) }, key, 4, WithMaxPending(3))
			func() {
				defer func() {
					if recover() == nil {
						t.Fatal("the key did not panic")
					}
				}()
				tc.submit(q)
			}()
			if got := q.SubmitBatch([]uint32{5, 6, 7}); got != 3 {
				t.Fatalf("accepted %d of 3 after the panic: the bounded queue lost its room", got)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := q.Stop(ctx); err != nil {
				t.Fatalf("Stop after a panicking key: %v", err)
			}
			if got := ran.Load(); got != 3 {
				t.Fatalf("ran %d tasks, want only the 3 submitted after the panic", got)
			}
		})
	}
}

// The monitor hands over a stuck worker's shard whenever the shard holds
// tasks no worker took since the previous tick: tasks queued behind the
// blocked one from the tick the worker has been stuck for batchRescueTicks,
// and tasks that arrive later at the next tick. A shard another worker serves
// is left alone until it is unattended again. The test plays the monitor.
func TestTaskKeyedStuckWorkerShardHandover(t *testing.T) {
	for _, late := range []bool{false, true} {
		name := "Queued"
		if late {
			name = "Late"
		}
		t.Run(name, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			q := NewTaskKeyed(func(k uint32) {
				if k == 0 {
					close(started)
					<-release
				}
			}, func(k uint32) uint32 { return k % 4 }, 4, WithConcurrency(1))
			atomic.StoreUint32(&q.monitorOn, 1) // keep the real monitor out
			defer func() {
				close(release)
				atomic.StoreUint32(&q.monitorOn, 0)
				if err := q.Stop(context.Background()); err != nil {
					t.Error(err)
				}
			}()
			q.Push(0)
			<-started
			marked := func() bool { return atomic.LoadUint64(&q.orphaned[0])&1 != 0 }
			tick := func() {
				q.mu.Lock()
				q.observeWorkersLocked()
				q.mu.Unlock()
			}
			tick() // sees the task start; the worker counts as stuck from the next
			if !late {
				q.Push(4)
				q.Push(8)
			}
			for i := 1; i < batchRescueTicks; i++ {
				if tick(); marked() {
					t.Fatalf("handed over after %d ticks, before batchRescueTicks", i)
				}
			}
			if late {
				for i := 0; i < 3; i++ {
					if tick(); marked() {
						t.Fatal("handed over with nothing queued behind the blocked task")
					}
				}
				q.Push(4)
				q.Push(8)
			}
			if tick(); !marked() {
				t.Fatal("tasks behind the blocked task were not handed over")
			}
			// A worker claims the shard and takes a task from it.
			q.unmarkOrphan(0)
			var taken [1]taskItem[uint32]
			if k, _ := q.rings[0].popBatch(taken[:]); k != 1 {
				t.Fatal("nothing to take from the shard")
			}
			if tick(); marked() {
				t.Fatal("handed over again while another worker served the shard")
			}
			if tick(); !marked() {
				t.Fatal("not handed over again once nobody served the shard")
			}
		})
	}
}

// Tasks that reach a blocked worker's shard well after it got stuck are handed
// over at the monitor's next tick, not after orphanStale.
func TestTaskKeyedLateTaskBehindBlockedTaskIsHandedOver(t *testing.T) {
	if procs := runtime.GOMAXPROCS(0); procs < 2 {
		t.Skipf("needs GOMAXPROCS >= 2 for the monitor to tick beside the hot worker, have %d", procs)
	}
	release := make(chan struct{})
	var stop atomic.Bool
	ran := make(chan struct{}, 1)
	var q *Task[uint32]
	q = NewTaskKeyed(func(k uint32) {
		switch {
		case k == 0:
			<-release
		case k == 1:
			if !stop.Load() {
				q.Push(1)
			}
		default:
			ran <- struct{}{}
		}
	}, func(k uint32) uint32 { return k % 4 }, 4, WithConcurrency(2))
	defer func() {
		stop.Store(true)
		close(release)
		if err := q.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	q.Push(0)
	q.Push(1)
	time.Sleep(4 * batchRescueTicks * monitorTick) // the worker is long stuck
	start := atomic.LoadUint32(&q.monTicks)
	q.Push(4)
	select {
	case <-ran:
		if ticks := atomic.LoadUint32(&q.monTicks) - start; ticks > 15 {
			t.Fatalf("the task ran only after %d monitor ticks", ticks)
		}
	case <-time.After(time.Second):
		t.Fatal("a task that arrived behind a blocked task did not run")
	}
}

// A stopped queue runs no key: Push, PushBatch and Submit reject before the
// callback, so a key that panics cannot turn a submission that would be
// dropped into a panic. The batch methods already rejected before it.
func TestTaskKeyedNoKeyAfterStop(t *testing.T) {
	var calls atomic.Int32
	key := func(k uint32) uint32 {
		calls.Add(1)
		if k == 13 {
			panic("bad key")
		}
		return k % 4
	}
	q := NewTaskKeyed(func(uint32) {}, key, 4, WithMaxPending(4))
	q.Push(1)
	if err := q.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	for _, tc := range []struct {
		name string
		call func() bool
	}{
		{"Push", func() bool { q.Push(13); return false }},
		{"PushBatch", func() bool { return q.PushBatch([]uint32{13}) != 0 }},
		{"Submit", func() bool { return q.Submit(13) }},
		{"SubmitBatch", func() bool { return q.SubmitBatch([]uint32{13}) != 0 }},
		{"TrySubmitBatch", func() bool { return q.TrySubmitBatch([]uint32{13}) }},
	} {
		accepted := func() (a bool) {
			defer func() {
				if v := recover(); v != nil {
					t.Fatalf("%s ran the key after Stop: %v", tc.name, v)
				}
			}()
			return tc.call()
		}()
		if accepted {
			t.Fatalf("%s accepted after Stop", tc.name)
		}
	}
	if got := calls.Load() - before; got != 0 {
		t.Fatalf("key ran %d times after Stop", got)
	}
}

// occupiedBits returns the shards whose occupied bit is set.
func (q *taskQueue[T]) occupiedBits() []int {
	var set []int
	for r := range q.shards {
		if atomic.LoadUint64(&q.occupied[r/64])&(uint64(1)<<(r%64)) != 0 {
			set = append(set, r)
		}
	}
	return set
}

// A pop that runs a shard dry clears its bit only when the shard is still
// empty after the clear. The task here was published while the bit was set,
// so its producer left the bit as it was: clearing it for good would hide the
// task from every worker that looks for work.
func TestTaskKeyedSettleEmptyKeepsTaskArrivedMeanwhile(t *testing.T) {
	q := NewTaskKeyed(func(uint32) {}, identityKey, 8)
	defer q.Stop(context.Background())

	q.markOccupied(3)
	if q.settleEmpty(3) {
		t.Fatal("settleEmpty reported a task on an empty shard")
	}
	if got := q.occupiedBits(); len(got) != 0 {
		t.Fatalf("occupied = %v after an empty shard settled, want none", got)
	}

	q.markOccupied(5)
	q.rings[5].push(taskItem[uint32]{value: 5}) // published, bit already set
	if !q.settleEmpty(5) {
		t.Fatal("settleEmpty missed the task that arrived after the pop")
	}
	if got := q.occupiedBits(); len(got) != 1 || got[0] != 5 {
		t.Fatalf("occupied = %v, want [5]: the shard holds a task", got)
	}
	if !q.readyAny() {
		t.Fatal("readyAny = false with a task queued")
	}

	if q.settleEmpty(6) {
		t.Fatal("settleEmpty reported a task on an unmarked empty shard")
	}
	q.rings[5].pop() // no worker ever ran: leave Stop nothing to wait for
}

// A scan finds tasks on either side of its start, across words, and clears
// the bit of every marked shard it finds empty on the way.
func TestTaskKeyedScanFollowsOccupiedBits(t *testing.T) {
	for _, shards := range []int{8, 130} {
		q := NewTaskKeyed(func(uint32) {}, identityKey, shards)
		w := &worker[uint32]{}
		w.initBatch(1)
		for _, start := range []int{0, 1, shards / 2, shards - 1} {
			// Tasks just before and well after the start, so the scan has to
			// wrap to reach one of them, and a stale mark in between.
			before := (start + shards - 1) % shards
			after := (start + shards/3) % shards
			stale := (start + shards/4 + 1) % shards
			q.pushItem(taskItem[uint32]{value: uint32(before)}, uint32(before))
			q.pushItem(taskItem[uint32]{value: uint32(after)}, uint32(after))
			q.markOccupied(stale)
			w.seed, w.scans = uint32(start), ^uint32(0) // the next scan starts at start
			var got []int
			for {
				k, _ := q.scanBatch(w, w.batch[:1])
				if k == 0 {
					break
				}
				got = append(got, int(w.batch[0].value))
				if r := int(atomic.LoadInt32(&w.ring)); r != got[len(got)-1] {
					t.Fatalf("shards=%d start=%d: worker ring %d after taking from %d", shards, start, r, got[len(got)-1])
				}
				w.seed, w.scans = uint32(start), ^uint32(0)
			}
			if len(got) != 2 || got[0] != after || got[1] != before {
				t.Fatalf("shards=%d start=%d: scan took %v, want [%d %d]", shards, start, got, after, before)
			}
			if bits := q.occupiedBits(); len(bits) != 0 {
				t.Fatalf("shards=%d start=%d: occupied = %v after the scan drained every shard", shards, start, bits)
			}
		}
		q.Stop(context.Background())
	}
}

// Producers that each wait for their task before submitting the next run
// every shard dry after every task, so workers keep settling shards and
// parking while tasks arrive: a task that lands between a worker's clear and
// its look again, or between its last scan and its park, must still run.
func TestTaskKeyedNoTaskLostWhileShardsRunDry(t *testing.T) {
	const producers, rounds = 8, 20000
	var latest atomic.Int64
	done := make([]chan struct{}, producers)
	for i := range done {
		done[i] = make(chan struct{}, 1)
	}
	q := NewTaskKeyed(func(k uint32) { done[k%producers] <- struct{}{} },
		identityKey, 16, WithConcurrency(3), WithMaxIdle(30*time.Second))
	defer q.Stop(context.Background())
	var wg sync.WaitGroup
	for p := range producers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range rounds {
				begin := time.Now()
				q.Push(uint32(p + producers*(i%5)))
				select {
				case <-done[p]:
				case <-time.After(2 * time.Second):
					t.Errorf("producer %d round %d: task did not run", p, i)
					return
				}
				wait := int64(time.Since(begin))
				for seen := latest.Load(); wait > seen && !latest.CompareAndSwap(seen, wait); seen = latest.Load() {
				}
				if i%64 == 0 {
					runtime.Gosched()
				}
			}
		}()
	}
	wg.Wait()
	waitFor(t, time.Second, func() bool { return len(q.occupiedBits()) == 0 })
	t.Logf("longest wait %v", time.Duration(latest.Load()))
}
