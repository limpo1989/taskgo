package taskgo

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func taskIdleLen[T any](q *taskQueue[T]) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.idle)
}

func taskRunningLen[T any](q *taskQueue[T]) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.running
}

func TestTaskPushBatchPrefetchAndLen(t *testing.T) {
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
	values := make([]int, workerBatchSize)
	for i := range values {
		values[i] = i + 1
	}
	if accepted := q.PushBatch(values); accepted != len(values) {
		t.Fatalf("accepted = %d, want %d", accepted, len(values))
	}
	close(release0)
	<-started1
	waitFor(t, time.Second, func() bool { return q.Len() == workerBatchSize-1 })
	close(release1)
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := atomic.LoadInt64(&completed); got != int64(workerBatchSize+1) {
		t.Fatalf("completed = %d, want %d", got, workerBatchSize+1)
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
	waitFor(t, time.Second, func() bool { return taskIdleLen(q.taskQueue) == 2 })
	if stop := q.cleanExpired(now.Add(500 * time.Millisecond)); stop {
		t.Fatal("janitor reclaimed workers too early")
	}
	if stop := q.cleanExpired(now.Add(2 * time.Second)); !stop {
		t.Fatal("janitor should stop after reclaiming all workers")
	}
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func TestTaskAutomaticJanitor(t *testing.T) {
	done := make(chan struct{})
	q := NewTask(func(int) { close(done) }, WithConcurrency(1), WithMaxIdle(5*time.Millisecond))
	q.Push(1)
	<-done
	waitFor(t, time.Second, func() bool { return taskIdleLen(q.taskQueue) == 0 })
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
	waitFor(t, time.Second, func() bool { return taskRunningLen(q2.taskQueue) == 0 })
	if accepted := q2.PushBatch([]int{1}); accepted != 0 {
		t.Fatalf("PushBatch after stop accepted %d tasks", accepted)
	}
}

func TestTaskAdaptiveBudgetAndTypedRingReserve(t *testing.T) {
	q := newTaskQueue([]Option{WithConcurrency(10000)}, func(int) {})
	atomic.StoreInt64(&q.workerTarget, q.initialWorkers)
	atomic.StoreInt64(&q.backlogAtomic, 1)
	q.observeTask(500 * time.Microsecond)
	grown := atomic.LoadInt64(&q.workerTarget)
	if grown <= q.initialWorkers {
		t.Fatalf("worker target = %d, want growth above %d", grown, q.initialWorkers)
	}
	q.retireWorker()
	if got := atomic.LoadInt64(&q.workerTarget); got != grown-1 {
		t.Fatalf("worker target = %d, want %d after retirement", got, grown-1)
	}
	atomic.StoreInt64(&q.backlogAtomic, 0)
	q.observeTask(time.Nanosecond)

	ring := newTaskRing[int](2)
	ring.push(taskItem[int]{value: 1})
	ring.push(taskItem[int]{value: 2})
	ring.reserve(8)
	if len(ring.buf) < 10 {
		t.Fatalf("typed ring capacity = %d, want at least 10", len(ring.buf))
	}
	for want := 1; ring.len() > 0; want++ {
		item, ok := ring.pop()
		if !ok || item.value != want {
			t.Fatalf("typed ring item = %+v, ok=%v, want %d", item, ok, want)
		}
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

func TestStallMonitorSpillsBlockedWorkers(t *testing.T) {
	q := New(WithConcurrency(16))
	atomic.StoreInt64(&q.workerTarget, 2)
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
	atomic.StoreInt64(&q.workerTarget, 2)
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
