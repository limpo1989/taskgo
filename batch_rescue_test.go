package taskgo

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Start at a full batch so the tests exercise a blocked batch independently
// of the timing of CAS contention between consumers.
func startBatchWorkerForTest[T any](q *taskQueue[T]) {
	q.mu.Lock()
	w := &worker[T]{ch: make(chan bool, 1), index: len(q.workers)}
	w.initBatch(q.opt.maxBatchSize)
	q.workers = append(q.workers, w)
	q.live++
	atomic.AddInt64(&q.running, 1)
	go q.runWorker(w, q.opt.maxBatchSize)
	q.mu.Unlock()
}

func TestWorkerMaxBatchSize(t *testing.T) {
	for _, size := range []int{1, 2, 3, 8, 16, 33} {
		for _, maxJobs := range []int{0, 2} {
			t.Run(fmt.Sprintf("batch=%d/maxJobs=%d", size, maxJobs), func(t *testing.T) {
				const timeout = time.Second
				total := size + 3
				started, release := make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				fast := make(chan struct{}, total-1)
				calls := make([]int32, total)
				q := NewTask(func(value int) {
					atomic.AddInt32(&calls[value], 1)
					if value == 0 {
						close(started)
						<-release
					} else {
						fast <- struct{}{}
					}
				}, WithConcurrency(2), WithMaxBatchSize(size), WithMaxJobs(maxJobs))
				q.setBaseForTest(1)
				// Keep the fixture in one ring so its first claim reaches the
				// configured cap instead of stopping at a ring boundary.
				q.queue.init(total)
				atomic.StoreUint32(&q.monitorOn, 1)
				defer func() { unblock(); _ = q.Stop(context.Background()) }()
				q.queue.pushN(total, func(value int) taskItem[int] { return taskItem[int]{value: value} })
				startBatchWorkerForTest(q.taskQueue)
				select {
				case <-started:
				case <-time.After(timeout):
					t.Fatal("first task did not start")
				}
				claimed := size
				if maxJobs > 0 && maxJobs < claimed {
					claimed = maxJobs
				}
				q.mu.Lock()
				w := q.workers[0]
				held := int32(atomic.LoadUint64(&w.state))
				bufferSize, bufferCap := len(w.batch), cap(w.batch)
				q.mu.Unlock()
				if got := q.queue.len(); got != total-claimed || held != int32(claimed-1) {
					t.Fatalf("claimed %d: shared=%d held=%d", claimed, got, held)
				}
				if bufferSize != size || bufferCap != size {
					t.Fatalf("buffer len=%d cap=%d, want %d", bufferSize, bufferCap, size)
				}
				atomic.StoreUint32(&q.monitorOn, 0)
				q.startMonitor()
				deadline := time.NewTimer(timeout)
				defer deadline.Stop()
				for i := 1; i < total; i++ {
					select {
					case <-fast:
					case <-deadline.C:
						t.Fatalf("task %d remained behind the blocked owner", i)
					}
				}
				unblock()
				if err := q.Stop(context.Background()); err != nil {
					t.Fatal(err)
				}
				for value, count := range calls {
					if count != 1 {
						t.Fatalf("task %d ran %d times", value, count)
					}
				}
			})
		}
	}
}

func TestSingleBatchSizeKeepsStallCompensation(t *testing.T) {
	started, release, fast := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	q := NewTask(func(value int) {
		if value == 0 {
			close(started)
			<-release
		} else {
			close(fast)
		}
	}, WithConcurrency(2), WithMaxBatchSize(1))
	q.setBaseForTest(1)
	defer func() { unblock(); _ = q.Stop(context.Background()) }()
	q.queue.pushN(2, func(value int) taskItem[int] { return taskItem[int]{value: value} })
	startBatchWorkerForTest(q.taskQueue)
	<-started
	q.mu.Lock()
	state := atomic.LoadUint64(&q.workers[0].state)
	q.mu.Unlock()
	if uint32(state) != 0 || atomic.LoadUint32(&q.monitorOn) != 0 || q.queue.len() != 1 {
		t.Fatal("single-task worker published a held batch or started batch monitoring")
	}
	q.startMonitor()
	select {
	case <-fast:
	case <-time.After(time.Second):
		t.Fatal("disabling batch rescue also disabled blocked-worker compensation")
	}
}

func TestMaxBatchSizeConcurrent(t *testing.T) {
	const producers, perProducer = 8, 2000
	for _, size := range []int{1, 3, 16} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			calls := make([]int32, producers*perProducer)
			var done sync.WaitGroup
			done.Add(len(calls))
			q := NewTask(func(value int) {
				atomic.AddInt32(&calls[value], 1)
				if value%512 == 0 {
					time.Sleep(2 * time.Millisecond)
				}
				done.Done()
			}, WithConcurrency(8), WithMaxBatchSize(size), WithMaxJobs(32), WithMaxIdle(time.Second))
			var submitted sync.WaitGroup
			submitted.Add(producers)
			for p := 0; p < producers; p++ {
				go func(p int) {
					defer submitted.Done()
					for i := 0; i < perProducer; i++ {
						q.Push(p*perProducer + i)
					}
				}(p)
			}
			submitted.Wait()
			done.Wait()
			if err := q.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			for value, count := range calls {
				if count != 1 {
					t.Fatalf("task %d ran %d times", value, count)
				}
			}
		})
	}
}

func TestBatchRescueBlockedTail(t *testing.T) {
	for _, stopWhileBlocked := range []bool{false, true} {
		name := "Running"
		if stopWhileBlocked {
			name = "Stopping"
		}
		t.Run(name, func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			fast := make(chan struct{}, defaultMaxBatchSize-1)
			var calls [defaultMaxBatchSize]int32
			q := NewTask(func(value int) {
				atomic.AddInt32(&calls[value], 1)
				if value == 0 {
					close(started)
					<-release
				} else {
					fast <- struct{}{}
				}
			}, WithConcurrency(2), WithMaxPending(defaultMaxBatchSize))
			q.setBaseForTest(1)
			// Stage the held tail before allowing the monitor to reclaim it.
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
				t.Fatal("failed to reserve initial batch")
			}
			q.queue.pushN(defaultMaxBatchSize, func(value int) taskItem[int] {
				return taskItem[int]{value: value, submitted: true}
			})
			startBatchWorkerForTest(q.taskQueue)
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("first task did not start")
			}
			if q.queue.len() != 0 || q.Len() != defaultMaxBatchSize-1 {
				t.Fatalf("expected only a held tail: shared=%d Len=%d", q.queue.len(), q.Len())
			}
			var stopped chan error
			if stopWhileBlocked {
				stopped = make(chan error, 1)
				go func() { stopped <- q.Stop(context.Background()) }()
				waitFor(t, time.Second, q.isStopped)
			}
			atomic.StoreUint32(&q.monitorOn, 0)
			q.startMonitor()
			deadline := time.NewTimer(250 * time.Millisecond)
			defer deadline.Stop()
			for i := 1; i < defaultMaxBatchSize; i++ {
				select {
				case <-fast:
				case <-deadline.C:
					t.Fatalf("task %d remained behind blocked owner", i)
				}
			}
			waitFor(t, time.Second, func() bool { return atomic.LoadInt64(&q.pending) == 1 })
			if q.Len() != 0 {
				t.Fatalf("rescued tail still counted as queued: Len=%d", q.Len())
			}
			if stopWhileBlocked {
				if q.Submit(defaultMaxBatchSize - 1) {
					t.Fatal("submission accepted after Stop")
				}
			} else {
				if !q.Submit(defaultMaxBatchSize - 1) {
					t.Fatal("rescued tasks did not release pending capacity")
				}
				<-fast
			}
			if stopped != nil {
				select {
				case err := <-stopped:
					t.Fatalf("Stop returned while first task was blocked: %v", err)
				default:
				}
			}
			unblock()
			if stopped != nil {
				if err := <-stopped; err != nil {
					t.Fatal(err)
				}
			} else if err := q.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			for value := range calls {
				want := int32(1)
				if !stopWhileBlocked && value == defaultMaxBatchSize-1 {
					want++
				}
				if got := atomic.LoadInt32(&calls[value]); got != want {
					t.Fatalf("task %d ran %d times, want %d", value, got, want)
				}
			}
		})
	}
}

func TestBatchRescueRespectsConcurrencyLimit(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	fast := make(chan int, defaultMaxBatchSize)
	q := NewTask(func(value int) {
		if value == 0 {
			close(started)
			<-release
		} else {
			fast <- value
		}
	}, WithConcurrency(1))
	defer func() { unblock(); _ = q.Stop(context.Background()) }()
	q.queue.pushN(defaultMaxBatchSize, func(value int) taskItem[int] { return taskItem[int]{value: value} })
	startBatchWorkerForTest(q.taskQueue)
	<-started
	q.startMonitor()
	q.Push(defaultMaxBatchSize)
	time.Sleep(10 * monitorTick)
	if len(fast) != 0 {
		t.Fatal("executed tasks while the sole slot was blocked")
	}
	if q.Len() != defaultMaxBatchSize || q.liveCount() != 1 {
		t.Fatalf("Len=%d live=%d", q.Len(), q.liveCount())
	}
	unblock()
	if err := q.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	for value := 1; value <= defaultMaxBatchSize; value++ {
		select {
		case got := <-fast:
			if got != value {
				t.Fatalf("started task %d, want %d", got, value)
			}
		default:
			t.Fatalf("task %d did not complete", value)
		}
	}
}

func TestBatchRescueConcurrentCompletion(t *testing.T) {
	for iteration := 0; iteration < 1000; iteration++ {
		var calls [defaultMaxBatchSize]int32
		started, release := make(chan struct{}), make(chan struct{})
		q := newTaskQueue([]Option{WithMaxPending(defaultMaxBatchSize)}, func(value int) {
			atomic.AddInt32(&calls[value], 1)
			if value == 0 {
				close(started)
				<-release
			}
		})
		// This unit test races ownership directly without background dispatch.
		atomic.StoreUint32(&q.monitorOn, 1)
		q.reservePending(defaultMaxBatchSize, true)
		w := &worker[int]{stuckTicks: batchRescueTicks}
		w.initBatch(q.opt.maxBatchSize)
		var initialSeq uint32
		if iteration%2 == 0 {
			initialSeq = ^uint32(0) - 3
			w.state = uint64(initialSeq) << 32
		}
		for value := range w.batch {
			w.batch[value] = taskItem[int]{value: value, submitted: true}
		}
		completed := make(chan int, 1)
		go func() {
			n, _ := q.execBatch(w, defaultMaxBatchSize)
			completed <- n
		}()
		<-started
		rescued := make(chan int, 1)
		go func() {
			<-release
			q.mu.Lock()
			n := q.rescueBatchLocked(w, atomic.LoadUint64(&w.state))
			q.mu.Unlock()
			rescued <- n
		}()
		close(release)
		owned, transferred := <-completed, <-rescued
		if owned+transferred != defaultMaxBatchSize {
			t.Fatalf("iteration %d: owner=%d rescued=%d", iteration, owned, transferred)
		}
		state := atomic.LoadUint64(&w.state)
		if uint32(state) != 0 || uint32(state>>32)-initialSeq != uint32(owned) {
			t.Fatalf("iteration %d: task-start progress corrupted by transfer: state=%x", iteration, state)
		}
		for {
			item, ok := q.queue.pop()
			if !ok {
				break
			}
			q.exec(item.value)
			atomic.AddInt64(&q.pending, -1)
		}
		for value := range calls {
			if got := atomic.LoadInt32(&calls[value]); got != 1 {
				t.Fatalf("iteration %d: task %d ran %d times", iteration, value, got)
			}
			if w.batch[value] != (taskItem[int]{}) {
				t.Fatalf("iteration %d: batch retained task %d", iteration, value)
			}
		}
		if got := atomic.LoadInt64(&q.pending); got != 0 {
			t.Fatalf("iteration %d: pending=%d", iteration, got)
		}
	}
}

func TestBatchRescueWaitsBeyondWorkerCompensation(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var calls [defaultMaxBatchSize]int32
	q := NewTask(func(value int) {
		atomic.AddInt32(&calls[value], 1)
		if value == 0 {
			close(started)
			<-release
		}
	}, WithConcurrency(2), WithMaxPending(defaultMaxBatchSize))
	q.setBaseForTest(1)
	atomic.StoreUint32(&q.monitorOn, 1)
	defer func() { unblock(); _ = q.Stop(context.Background()) }()
	q.reservePending(defaultMaxBatchSize, true)
	q.queue.pushN(defaultMaxBatchSize, func(value int) taskItem[int] {
		return taskItem[int]{value: value, submitted: true}
	})
	startBatchWorkerForTest(q.taskQueue)
	<-started
	q.mu.Lock()
	w := q.workers[0]
	initialState := atomic.LoadUint64(&w.state)
	for tick := 0; tick < batchRescueTicks; tick++ {
		w.stuckTicks = tick
		if n := q.rescueBatchLocked(w, initialState); n != 0 || q.queue.len() != 0 ||
			atomic.LoadUint64(&w.state) != initialState {
			q.mu.Unlock()
			t.Fatalf("rescued a batch after only %d no-progress samples", tick)
		}
	}
	w.stuckTicks = batchRescueTicks
	n := q.rescueBatchLocked(w, initialState)
	q.mu.Unlock()
	if n != defaultMaxBatchSize-1 || q.Len() != defaultMaxBatchSize-1 {
		t.Fatalf("sustained stall: rescued=%d Len=%d", n, q.Len())
	}
	unblock()
	if err := q.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	for value, count := range calls {
		if count != 1 {
			t.Fatalf("task %d ran %d times", value, count)
		}
	}
	if got := atomic.LoadInt64(&q.pending); got != 0 {
		t.Fatalf("pending=%d after rescued batch completed", got)
	}
}

func TestBatchRescueRejectsStaleProgressSample(t *testing.T) {
	firstStarted, firstRelease := make(chan struct{}), make(chan struct{})
	secondStarted, secondRelease := make(chan struct{}), make(chan struct{})
	var firstOnce, secondOnce sync.Once
	unblockFirst := func() { firstOnce.Do(func() { close(firstRelease) }) }
	unblockSecond := func() { secondOnce.Do(func() { close(secondRelease) }) }
	defer unblockFirst()
	defer unblockSecond()
	var calls [defaultMaxBatchSize]int32
	q := newTaskQueue([]Option{WithMaxPending(defaultMaxBatchSize)}, func(value int) {
		atomic.AddInt32(&calls[value], 1)
		switch value {
		case 0:
			close(firstStarted)
			<-firstRelease
		case 1:
			close(secondStarted)
			<-secondRelease
		}
	})
	atomic.StoreUint32(&q.monitorOn, 1)
	q.reservePending(defaultMaxBatchSize, true)
	w := &worker[int]{stuckTicks: batchRescueTicks}
	w.initBatch(q.opt.maxBatchSize)
	for value := range w.batch {
		w.batch[value] = taskItem[int]{value: value, submitted: true}
	}
	completed := make(chan int, 1)
	go func() {
		n, _ := q.execBatch(w, defaultMaxBatchSize)
		completed <- n
	}()
	<-firstStarted
	q.mu.Lock()
	observed := atomic.LoadUint64(&w.state)
	w.lastSeq = uint32(observed >> 32)
	// The owner advances while the monitor still holds mu. Its new task
	// must not inherit the preceding task's accumulated no-progress samples.
	unblockFirst()
	<-secondStarted
	rescued := q.rescueBatchLocked(w, observed)
	q.mu.Unlock()
	unblockSecond()
	owned := <-completed
	if rescued != 0 || owned != defaultMaxBatchSize || q.queue.len() != 0 {
		t.Fatalf("stale sample: rescued=%d owned=%d queued=%d", rescued, owned, q.queue.len())
	}
	for value, count := range calls {
		if count != 1 {
			t.Fatalf("task %d ran %d times", value, count)
		}
	}
	if got := atomic.LoadInt64(&q.pending); got != 0 {
		t.Fatalf("pending=%d after uninterrupted owner completion", got)
	}
}

func TestBatchRescueStartsMonitor(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	fast := make(chan struct{}, defaultMaxBatchSize-1)
	q := NewTask(func(value int) {
		if value == 0 {
			<-release
		} else {
			fast <- struct{}{}
		}
	}, WithConcurrency(2))
	q.setBaseForTest(1)
	defer func() { unblock(); _ = q.Stop(context.Background()) }()
	q.queue.pushN(defaultMaxBatchSize, func(value int) taskItem[int] { return taskItem[int]{value: value} })
	startBatchWorkerForTest(q.taskQueue)
	deadline := time.NewTimer(250 * time.Millisecond)
	defer deadline.Stop()
	for i := 1; i < defaultMaxBatchSize; i++ {
		select {
		case <-fast:
		case <-deadline.C:
			t.Fatal("held batch did not start its own monitor")
		}
	}
}

func TestBatchRescueMonitorExit(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	started := make(chan struct{})
	fast := make(chan struct{}, defaultMaxBatchSize-1)
	q := NewTask(func(value int) {
		if value == 0 {
			close(started)
			<-release
		} else {
			fast <- struct{}{}
		}
	}, WithConcurrency(2))
	q.setBaseForTest(1)
	defer func() { unblock(); _ = q.Stop(context.Background()) }()
	// Simulate a monitor whose last scan saw no backlog. A newly published
	// batch observes it as still running, then the old monitor shuts down.
	atomic.StoreUint32(&q.monitorOn, 1)
	q.queue.pushN(defaultMaxBatchSize, func(value int) taskItem[int] { return taskItem[int]{value: value} })
	startBatchWorkerForTest(q.taskQueue)
	<-started
	q.finishMonitor()
	deadline := time.NewTimer(250 * time.Millisecond)
	defer deadline.Stop()
	for i := 1; i < defaultMaxBatchSize; i++ {
		select {
		case <-fast:
		case <-deadline.C:
			t.Fatal("monitor exit lost the newly held batch")
		}
	}
}
