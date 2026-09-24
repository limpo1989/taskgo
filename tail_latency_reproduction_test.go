package taskgo

import (
	"context"
	"os"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"
)

type tailLatencyStats struct {
	p50, p99, p999, max time.Duration
}

func summarizeTailLatency(values []time.Duration) tailLatencyStats {
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return tailLatencyStats{p50: sorted[len(sorted)*50/100], p99: sorted[len(sorted)*99/100], p999: sorted[len(sorted)*999/1000], max: sorted[len(sorted)-1]}
}

func busyFor(d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
	}
}

func runTailLatencyScenario(t *testing.T, total, batchSize int) (tailLatencyStats, tailLatencyStats, tailLatencyStats) {
	t.Helper()
	const producers, inFlight = 24, 10000
	q := New(WithConcurrency(10000))
	submittedAt := make([]time.Time, total)
	startedAt := make([]time.Duration, total)
	completedAt := make([]time.Duration, total)
	taskDurations := make([]time.Duration, total)
	tokens := make(chan struct{}, inFlight)
	var done sync.WaitGroup
	done.Add(total)
	var producerWG sync.WaitGroup
	producerWG.Add(producers)
	start := make(chan struct{})
	for p := 0; p < producers; p++ {
		go func(p int) {
			defer producerWG.Done()
			<-start
			for base := p * batchSize; base < total; base += producers * batchSize {
				end := base + batchSize
				if end > total {
					end = total
				}
				jobs := make([]Job, end-base)
				for i := base; i < end; i++ {
					tokens <- struct{}{}
					submittedAt[i] = time.Now()
					idx := i
					jobs[i-base] = func() {
						started := time.Now()
						startedAt[idx] = started.Sub(submittedAt[idx])
						taskStart := started
						busyFor(25 * time.Microsecond)
						taskDurations[idx] = time.Since(taskStart)
						completedAt[idx] = time.Since(submittedAt[idx])
						<-tokens
						done.Done()
					}
				}
				q.PushBatch(jobs)
			}
		}(p)
	}
	close(start)
	producerWG.Wait()
	done.Wait()
	if err := q.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	return summarizeTailLatency(taskDurations), summarizeTailLatency(startedAt), summarizeTailLatency(completedAt)
}

func TestTailLatencyReproduction(t *testing.T) {
	if os.Getenv("TASKGO_RUN_TAIL_REPRO") != "1" {
		t.Skip("set TASKGO_RUN_TAIL_REPRO=1 to run tail reproduction")
	}
	total := 200000
	if raw := os.Getenv("TASKGO_TAIL_TOTAL"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			t.Fatalf("invalid TASKGO_TAIL_TOTAL=%q", raw)
		}
		total = parsed
	}
	t.Logf("GOMAXPROCS=%d total=%d producers=24 in-flight=10000 task=25us", runtime.GOMAXPROCS(0), total)
	for _, batchSize := range []int{32, 1} {
		task, queued, completed := runTailLatencyScenario(t, total, batchSize)
		t.Logf("batch=%d | task p99=%v | submit-start p50=%v p99=%v p999=%v max=%v | submit-complete p99=%v p999=%v max=%v", batchSize, task.p99, queued.p50, queued.p99, queued.p999, queued.max, completed.p99, completed.p999, completed.max)
	}
}
