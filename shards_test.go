package taskgo

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestShardedQueueDrains(t *testing.T) {
	const total = 5000
	q := New(WithConcurrency(32), WithShards(16), WithMaxIdle(time.Second))
	var done int64
	var wg sync.WaitGroup
	wg.Add(total)
	for i := 0; i < total; i++ {
		q.Push(func() {
			atomic.AddInt64(&done, 1)
			wg.Done()
		})
	}
	wg.Wait()
	if got := atomic.LoadInt64(&done); got != total {
		t.Fatalf("completed = %d, want %d", got, total)
	}
	if err := q.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := q.Len(); got != 0 {
		t.Fatalf("Len = %d after drain", got)
	}
}

func TestShardedTaskDrains(t *testing.T) {
	const total = 5000
	var done int64
	q := NewTask(func(int) { atomic.AddInt64(&done, 1) }, WithConcurrency(32), WithShards(16))
	for i := 0; i < total; i++ {
		q.Push(i)
	}
	if err := q.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&done); got != total {
		t.Fatalf("completed = %d, want %d", got, total)
	}
}

func TestShardedQueueWakesParkedWorker(t *testing.T) {
	q := New(WithConcurrency(1), WithShards(4), WithMaxIdle(time.Minute))
	started := make(chan struct{})
	q.Push(func() { close(started) })
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("initial task did not start")
	}

	const total = 2000
	var done int64
	for i := 0; i < total; i++ {
		q.Push(func() { atomic.AddInt64(&done, 1) })
	}
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt64(&done) != total && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := atomic.LoadInt64(&done); got != total {
		t.Fatalf("completed = %d, want %d", got, total)
	}
	if err := q.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestShardedQueueDrainsWithoutParking(t *testing.T) {
	const (
		producers   = 8
		perProducer = 2000
	)
	q := New(WithConcurrency(1), WithShards(8))
	var completed int64
	var producerWG sync.WaitGroup
	producerWG.Add(producers)
	for p := 0; p < producers; p++ {
		go func() {
			defer producerWG.Done()
			for i := 0; i < perProducer; i++ {
				q.Push(func() {
					runtime.Gosched()
					atomic.AddInt64(&completed, 1)
				})
			}
		}()
	}
	producerWG.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt64(&completed) != producers*perProducer && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := atomic.LoadInt64(&completed); got != producers*perProducer {
		t.Fatalf("completed = %d, want %d", got, producers*perProducer)
	}
	if err := q.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestShardedSubmitHonorsPendingLimit(t *testing.T) {
	q := New(WithConcurrency(1), WithShards(4), WithMaxPending(3))
	release := make(chan struct{})
	started := make(chan struct{})
	q.Push(func() {
		close(started)
		<-release
	})
	<-started
	for i := 0; i < 3; i++ {
		if !q.Submit(func() {}) {
			t.Fatalf("Submit %d rejected", i)
		}
	}
	if q.Submit(func() {}) {
		t.Fatal("Submit exceeded pending limit")
	}
	close(release)
	if err := q.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestShardedTaskSubmitHonorsPendingLimit(t *testing.T) {
	block := make(chan struct{})
	started := make(chan struct{})
	q := NewTask(func(v int) {
		if v == 0 {
			close(started)
			<-block
		}
	}, WithConcurrency(1), WithShards(4), WithMaxPending(2))
	q.Push(0)
	<-started
	for i := 1; i <= 2; i++ {
		if !q.Submit(i) {
			t.Fatalf("Task Submit %d rejected", i)
		}
	}
	if q.Submit(3) {
		t.Fatal("Task Submit exceeded pending limit")
	}
	close(block)
	if err := q.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestShardedSubmitPendingAdmissionIsAtomic(t *testing.T) {
	for _, name := range []string{"queue", "task"} {
		t.Run(name, func(t *testing.T) {
			release := make(chan struct{})
			started := make(chan struct{})
			var submit func() bool
			var stop func() error
			if name == "queue" {
				q := New(WithConcurrency(1), WithShards(4), WithMaxPending(1))
				q.Push(func() { close(started); <-release })
				submit = func() bool { return q.Submit(func() { <-release }) }
				stop = func() error { return q.Stop(context.Background()) }
			} else {
				q := NewTask(func(v int) {
					if v == 0 {
						close(started)
						<-release
					}
				}, WithConcurrency(1), WithShards(4), WithMaxPending(1))
				q.Push(0)
				submit = func() bool { return q.Submit(1) }
				stop = func() error { return q.Stop(context.Background()) }
			}
			<-started
			var accepted int64
			var wg sync.WaitGroup
			wg.Add(64)
			for i := 0; i < 64; i++ {
				go func() {
					defer wg.Done()
					if submit() {
						atomic.AddInt64(&accepted, 1)
					}
				}()
			}
			wg.Wait()
			if got := atomic.LoadInt64(&accepted); got > 1 {
				t.Fatalf("accepted = %d, want at most 1", got)
			}
			close(release)
			if err := stop(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestShardedStopLinearizesIngress(t *testing.T) {
	for _, name := range []string{"queue", "task"} {
		t.Run(name, func(t *testing.T) {
			const producers = 8
			const perProducer = 2000
			var accepted, completed int64
			var submit func() bool
			var stop func() error
			if name == "queue" {
				q := New(WithConcurrency(1), WithShards(8), WithMaxPending(producers*perProducer))
				submit = func() bool {
					return q.Submit(func() { atomic.AddInt64(&completed, 1) })
				}
				stop = func() error { return q.Stop(context.Background()) }
			} else {
				q := NewTask(func(int) { atomic.AddInt64(&completed, 1) }, WithConcurrency(1), WithShards(8), WithMaxPending(producers*perProducer))
				submit = func() bool { return q.Submit(1) }
				stop = func() error { return q.Stop(context.Background()) }
			}
			var producerWG sync.WaitGroup
			producerWG.Add(producers)
			for p := 0; p < producers; p++ {
				go func() {
					defer producerWG.Done()
					for i := 0; i < perProducer; i++ {
						if submit() {
							atomic.AddInt64(&accepted, 1)
						}
					}
				}()
			}
			time.Sleep(time.Millisecond)
			if err := stop(); err != nil {
				t.Fatal(err)
			}
			producerWG.Wait()
			if got, want := atomic.LoadInt64(&completed), atomic.LoadInt64(&accepted); got != want {
				t.Fatalf("completed = %d, accepted = %d", got, want)
			}
		})
	}
}
