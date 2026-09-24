package taskgo

import (
	"context"
	"os"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"
)

func runStallLatencyScenario(t *testing.T, slow int) time.Duration {
	t.Helper()
	q := New(WithConcurrency(10000))
	slowStarted := make(chan struct{}, slow)
	for i := 0; i < slow; i++ {
		q.Push(func() {
			slowStarted <- struct{}{}
			time.Sleep(50 * time.Millisecond)
		})
	}
	for i := 0; i < slow; i++ {
		select {
		case <-slowStarted:
		case <-time.After(time.Second):
			t.Fatalf("slow task %d did not start", i)
		}
	}

	const fastTasks = 1000
	latencies := make([]time.Duration, fastTasks)
	var done sync.WaitGroup
	done.Add(fastTasks)
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
