package taskgo

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A task may stop its own queue. Stop cannot wait for the task it is called
// from, which is still running when Stop returns, but it waits for every other
// task: one another worker is running and the ones queued behind them.
func TestStopFromTaskWaitsForTheOthers(t *testing.T) {
	var q *Task[int]
	var ran atomic.Int32
	slowStarted, slowRelease := make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	var doneBeforeStop atomic.Int32
	q = NewTask(func(v int) {
		switch v {
		case 0: // the slow task another worker runs
			close(slowStarted)
			<-slowRelease
		case 1: // the task that stops the queue
			err := q.Stop(context.Background())
			doneBeforeStop.Store(ran.Load())
			result <- err
			return
		}
		ran.Add(1)
	}, WithConcurrency(4), WithTimeout(5*time.Second))
	q.Push(0)
	<-slowStarted
	for range 50 {
		q.Push(2) // queued work Stop must wait for
	}
	q.Push(1)
	select {
	case err := <-result:
		t.Fatalf("Stop returned %v while another task was still running", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(slowRelease)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Stop from a task = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop from a task waited for its own task")
	}
	if got := doneBeforeStop.Load(); got != 51 {
		t.Fatalf("%d of the 51 other tasks had finished when Stop returned", got)
	}
}

// The tasks after the calling one in its worker's batch would run only once
// the calling task returns: Stop hands them to other workers and waits for
// them like any other task.
func TestStopFromTaskRunsTheRestOfItsBatch(t *testing.T) {
	var q *Task[int]
	var rest, restAtStop atomic.Int32
	result := make(chan error, 1)
	q = NewTask(func(v int) {
		if v == 0 {
			err := q.Stop(context.Background())
			// Read here: once this task returns, its worker runs the rest.
			restAtStop.Store(rest.Load())
			result <- err
			return
		}
		rest.Add(1)
	}, WithConcurrency(2), WithTimeout(5*time.Second))
	q.setBaseForTest(1)
	q.queue.pushN(defaultMaxBatchSize, func(i int) taskItem[int] { return taskItem[int]{value: i} })
	startBatchWorkerForTest(q.taskQueue)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Stop = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop waited for the tasks held behind the calling task")
	}
	if got := restAtStop.Load(); got != defaultMaxBatchSize-1 {
		t.Fatalf("%d of the %d tasks behind the caller had run when Stop returned", got, defaultMaxBatchSize-1)
	}
}

// The panic handler runs on the worker of the task that panicked, and may
// stop the queue the same way.
func TestStopFromPanicHandler(t *testing.T) {
	var q *Task[int]
	result := make(chan error, 1)
	q = NewTask(func(int) { panic("boom") }, WithTimeout(5*time.Second),
		WithPanicHandler(func(any) { result <- q.Stop(context.Background()) }))
	q.Push(1)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Stop from the panic handler = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop from the panic handler waited for its own task")
	}
}

// A task of one queue that stops another is not one of that queue's tasks,
// and waits for that queue's running tasks like any caller.
func TestStopFromAnotherQueuesTaskWaits(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	b := NewTask(func(int) { close(started); <-release }, WithTimeout(5*time.Second))
	result := make(chan error, 1)
	a := NewTask(func(int) { result <- b.Stop(context.Background()) })
	defer a.Stop(context.Background())
	b.Push(1)
	<-started
	a.Push(1)
	select {
	case err := <-result:
		t.Fatalf("Stop of another queue returned %v with its task still running", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatalf("Stop = %v, want nil", err)
	}
}

// When every worker the limit allows is a task waiting in Stop, the queued
// tasks run only after those tasks return: Stop says so at once instead of
// waiting out its timeout, and the tasks still run.
func TestStopFromTaskReportsTasksOnlyItsWorkerCanRun(t *testing.T) {
	var q *Task[int]
	var ran atomic.Int32
	result := make(chan error, 1)
	q = NewTask(func(v int) {
		if v == 0 {
			q.Push(1) // queued behind the only worker
			result <- q.Stop(context.Background())
			return
		}
		ran.Add(1)
	}, WithConcurrency(1), WithTimeout(5*time.Second))
	q.Push(0)
	select {
	case err := <-result:
		if !errors.Is(err, ErrStopInTask) {
			t.Fatalf("Stop = %v, want ErrStopInTask", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop waited for a task only its own worker can run")
	}
	waitFor(t, time.Second, func() bool { return ran.Load() == 1 })
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("Stop after the queue drained = %v", err)
	}
}

// Every Stop waits for the drain, the ones made while another is waiting and
// the ones after a Stop that timed out included, and only a drained queue
// makes them return nil.
func TestConcurrentStopsWaitForTheDrain(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	q := NewTask(func(int) { close(started); <-release }, WithTimeout(5*time.Second))
	q.Push(1)
	<-started

	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := q.Stop(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Stop = %v, want its deadline", err)
	}
	// The first Stop timed out; later ones still wait.
	results := make(chan error, 3)
	for range 3 {
		go func() { results <- q.Stop(context.Background()) }()
	}
	select {
	case err := <-results:
		t.Fatalf("a Stop returned %v while a task was still running", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	for range 3 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("Stop = %v once the queue drained, want nil", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a Stop did not return once the queue drained")
		}
	}
}

// An external Stop includes a task waiting in Stop and any work that task
// does after its own Stop returns. Its deadline must not turn into success
// merely because all remaining workers are waiting inside Stop.
func TestExternalStopWaitsForTaskCallingStop(t *testing.T) {
	blockerStarted, blockerRelease := make(chan struct{}), make(chan struct{})
	taskRelease := make(chan struct{})
	releaseBlocker := sync.OnceFunc(func() { close(blockerRelease) })
	releaseTask := sync.OnceFunc(func() { close(taskRelease) })
	result := make(chan error, 1)
	q := New(WithConcurrency(2), WithMaxBatchSize(1), WithTimeout(2*time.Second))
	t.Cleanup(func() {
		releaseBlocker()
		releaseTask()
		q.Stop(context.Background())
	})
	q.Push(func() { close(blockerStarted); <-blockerRelease })
	<-blockerStarted
	q.Push(func() {
		result <- q.Stop(context.Background())
		<-taskRelease
	})
	waitFor(t, time.Second, func() bool { return atomic.LoadInt64(&q.selfStops) == 1 })
	releaseBlocker()
	waitFor(t, time.Second, func() bool { return q.runningCount() == 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := q.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("external Stop = %v with the stopping task still running, want DeadlineExceeded", err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Stop from the task = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("task Stop waited for itself or the external Stop")
	}
	// Even after the inner Stop returns, the task itself has not returned.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	if err := q.Stop(ctx2); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("external Stop = %v before the task returned, want DeadlineExceeded", err)
	}
	releaseTask()
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("Stop after the task returned = %v, want nil", err)
	}
}

// A producer finishing its dispatch keeps stop initialization open while two
// tasks call Stop. The second task owns a batch: excluding its worker before
// handing off that batch would let the first Stop overlook unstarted tasks.
func TestTaskStopWaitsForOtherStoppingWorkersBatch(t *testing.T) {
	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)
	firstStarted, secondStarted := make(chan struct{}), make(chan struct{})
	firstStop, secondStop := make(chan struct{}), make(chan struct{})
	stopFirst := sync.OnceFunc(func() { close(firstStop) })
	stopSecond := sync.OnceFunc(func() { close(secondStop) })
	type result struct {
		task int
		err  error
		done int32
	}
	results := make(chan result, 2)
	var completed atomic.Int32
	var q *Task[int]
	q = NewTask(func(v int) {
		switch v {
		case 0:
			close(firstStarted)
			<-firstStop
		case 1:
			close(secondStarted)
			<-secondStop
		default:
			completed.Add(1)
			return
		}
		err := q.Stop(context.Background())
		results <- result{v, err, completed.Load()}
	}, WithConcurrency(2), WithTimeout(2*time.Second))
	// Pin both slots and suppress the monitor so it cannot rescue the held
	// tail ahead of Stop. Returning either task suffices to drain this work.
	q.setBaseForTest(2)
	atomic.StoreUint32(&q.monitorOn, 1)
	if !q.enter() {
		t.Fatal("producer rejected")
	}
	leaveProducer := sync.OnceFunc(q.leave)
	t.Cleanup(func() {
		stopFirst()
		stopSecond()
		leaveProducer()
		q.Stop(context.Background())
		atomic.StoreUint32(&q.monitorOn, 0)
	})
	q.Push(0)
	<-firstStarted
	q.queue.pushN(defaultMaxBatchSize, func(i int) taskItem[int] {
		return taskItem[int]{value: i + 1}
	})
	startBatchWorkerForTest(q.taskQueue)
	<-secondStarted
	stopFirst()
	waitFor(t, time.Second, q.isStopped)
	stopSecond()
	// With one P the first task yields while waiting for the producer, giving
	// the second task a turn to enter Stop before initialization completes.
	for range 10 {
		runtime.Gosched()
	}
	leaveProducer()
	for range 2 {
		select {
		case r := <-results:
			if r.err == nil && r.done != defaultMaxBatchSize-1 {
				t.Errorf("task %d Stop returned nil with %d/%d batch tasks completed", r.task, r.done, defaultMaxBatchSize-1)
			}
			if r.err != nil && !errors.Is(r.err, ErrStopInTask) {
				t.Errorf("task %d Stop = %v, want nil or ErrStopInTask", r.task, r.err)
			}
		case <-time.After(time.Second):
			t.Fatal("tasks calling Stop did not release a worker for the held batch")
		}
	}
	if err := q.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := completed.Load(); got != defaultMaxBatchSize-1 {
		t.Fatalf("completed %d batch tasks, want %d", got, defaultMaxBatchSize-1)
	}
}

// Concurrent task callers exclude one another, but keep the concurrency cap:
// when all slots are waiting in Stop, one must return ErrStopInTask so queued
// work can run. Without queued work every caller can return nil.
func TestMultipleTasksCallStop(t *testing.T) {
	for _, queued := range []int{0, 7} {
		name := "empty"
		if queued > 0 {
			name = "queued"
		}
		t.Run(name, func(t *testing.T) {
			const workers = 3
			gate := make(chan struct{})
			release := sync.OnceFunc(func() { close(gate) })
			started := make(chan struct{}, workers)
			results := make(chan error, workers)
			var completed atomic.Int32
			var q *Task[int]
			q = NewTask(func(v int) {
				if v >= workers {
					completed.Add(1)
					return
				}
				started <- struct{}{}
				<-gate
				results <- q.Stop(context.Background())
			}, WithConcurrency(workers), WithMaxBatchSize(1), WithTimeout(2*time.Second))
			t.Cleanup(func() { release(); q.Stop(context.Background()) })
			for i := range workers {
				q.Push(i)
			}
			for range workers {
				select {
				case <-started:
				case <-time.After(time.Second):
					t.Fatal("workers did not start")
				}
			}
			for i := range queued {
				q.Push(workers + i)
			}
			release()
			blocked := 0
			for range workers {
				select {
				case err := <-results:
					if errors.Is(err, ErrStopInTask) {
						blocked++
					} else if err != nil {
						t.Fatalf("Stop = %v, want nil or ErrStopInTask", err)
					}
				case <-time.After(time.Second):
					t.Fatal("tasks calling Stop waited for one another")
				}
			}
			if (queued == 0 && blocked != 0) || (queued > 0 && blocked == 0) {
				t.Fatalf("%d ErrStopInTask results with %d queued tasks", blocked, queued)
			}
			if err := q.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := completed.Load(); got != int32(queued) {
				t.Fatalf("completed %d queued tasks, want %d", got, queued)
			}
			if got := atomic.LoadInt64(&q.selfStops); got != 0 {
				t.Fatalf("selfStops = %d after drain, want 0", got)
			}
		})
	}
}

// Stop ends the janitor at once instead of leaving it asleep for maxIdle with
// the queue reachable from it.
func TestStopEndsTheJanitor(t *testing.T) {
	q := NewTask(func(int) {}, WithMaxIdle(time.Hour))
	q.Push(1)
	waitFor(t, time.Second, func() bool { return q.idleLen() == 1 })
	q.mu.Lock()
	running := q.janitorRunning
	q.mu.Unlock()
	if !running {
		t.Fatal("no janitor was started for the parked worker")
	}
	before := runtime.NumGoroutine()
	if err := q.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The dismissed worker and the janitor both exit.
	waitFor(t, time.Second, func() bool { return runtime.NumGoroutine() <= before-2 })
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.janitorRunning || q.live != 0 {
		t.Fatalf("after Stop: janitorRunning=%v live=%d", q.janitorRunning, q.live)
	}
}

// A worker told to exit stays live until its goroutine has, so the
// concurrency limit never admits a replacement beside it; a task that arrives
// meanwhile runs once the slot is free.
func TestExpiredWorkerCountsUntilItExits(t *testing.T) {
	var now atomic.Int64
	now.Store(time.Now().UnixNano())
	var ran atomic.Int32
	q := NewTask(func(int) { ran.Add(1) }, WithConcurrency(1), WithMaxIdle(time.Minute),
		withManualJanitor(), withNowFunc(func() time.Time { return time.Unix(0, now.Load()) }))
	defer q.Stop(context.Background())
	q.Push(0)
	waitFor(t, time.Second, func() bool { return q.idleLen() == 1 })
	for i := range 200 {
		now.Add(int64(2 * time.Minute))
		q.cleanExpired(time.Unix(0, now.Load()))
		q.Push(0)
		q.mu.Lock()
		workers, live := len(q.workers), q.live
		q.mu.Unlock()
		if workers > 1 || live > 1 {
			t.Fatalf("round %d: %d workers registered, live=%d, under WithConcurrency(1)", i, workers, live)
		}
		want := int32(i + 2)
		waitFor(t, time.Second, func() bool { return ran.Load() == want && q.idleLen() == 1 })
	}
}
