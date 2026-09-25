package taskgo

import (
	"context"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func runStallLatencyScenario(t *testing.T, slow int) time.Duration {
	t.Helper()
	q := New(WithConcurrency(10000))
	slowStarted := make(chan struct{}, slow)
	releaseSlow := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseSlow) }) }
	defer release()
	for i := 0; i < slow; i++ {
		q.Push(func() {
			slowStarted <- struct{}{}
			<-releaseSlow
		})
	}
	startDeadline := time.NewTimer(5 * time.Second)
	defer startDeadline.Stop()
	for i := 0; i < slow; i++ {
		select {
		case <-slowStarted:
		case <-startDeadline.C:
			t.Fatalf("slow task %d did not start", i)
		}
	}

	const fastTasks = 1000
	latencies := make([]time.Duration, fastTasks)
	var done sync.WaitGroup
	done.Add(fastTasks)
	releaseTimer := time.AfterFunc(50*time.Millisecond, release)
	defer releaseTimer.Stop()
	for i := 0; i < fastTasks; i++ {
		submitted := time.Now()
		idx := i
		q.Push(func() {
			latencies[idx] = time.Since(submitted)
			done.Done()
		})
	}
	done.Wait()
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	return latencies[len(latencies)*99/100]
}

func TestStallLatencyReproduction(t *testing.T) {
	if os.Getenv("TASKGO_RUN_STALL_REPRO") != "1" {
		t.Skip("set TASKGO_RUN_STALL_REPRO=1 to run stall latency reproduction")
	}
	target := autoWorkerLimit(10000)
	for _, slow := range []int{target - 1, target, 256, 1000, 5000} {
		p99 := runStallLatencyScenario(t, slow)
		t.Logf("GOMAXPROCS=%d target=%d slow=%d fast P99=%v", runtime.GOMAXPROCS(0), target, slow, p99)
	}
}

func runStallBurstScenario(t *testing.T, slow int) (time.Duration, int64) {
	t.Helper()
	q := New(WithConcurrency(10000))
	releaseSlow := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseSlow) }) }
	defer release()
	var started int64
	for i := 0; i < slow; i++ {
		q.Push(func() {
			atomic.AddInt64(&started, 1)
			<-releaseSlow
		})
	}
	startedAtFastSubmission := atomic.LoadInt64(&started)

	const fastTasks = 1000
	latencies := make([]time.Duration, fastTasks)
	var done sync.WaitGroup
	done.Add(fastTasks)
	releaseTimer := time.AfterFunc(50*time.Millisecond, release)
	defer releaseTimer.Stop()
	for i := 0; i < fastTasks; i++ {
		submitted := time.Now()
		idx := i
		q.Push(func() {
			latencies[idx] = time.Since(submitted)
			done.Done()
		})
	}
	done.Wait()
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	return latencies[len(latencies)*99/100], startedAtFastSubmission
}

func TestStallBurstLatencyReproduction(t *testing.T) {
	if os.Getenv("TASKGO_RUN_STALL_BURST_REPRO") != "1" {
		t.Skip("set TASKGO_RUN_STALL_BURST_REPRO=1 to run slow-burst latency reproduction")
	}
	target := autoWorkerLimit(10000)
	for _, slow := range []int{target, 256, 1000, 5000} {
		p99, started := runStallBurstScenario(t, slow)
		t.Logf("GOMAXPROCS=%d target=%d slowSubmitted=%d slowStartedAtFast=%d fast P99=%v", runtime.GOMAXPROCS(0), target, slow, started, p99)
	}
}
