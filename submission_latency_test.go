package taskgo

import (
	"context"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type submissionLatencyStats struct {
	p50  time.Duration
	p99  time.Duration
	p999 time.Duration
	max  time.Duration
}

func summarizeSubmissionLatency(samples []time.Duration) submissionLatencyStats {
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return submissionLatencyStats{
		p50:  sorted[len(sorted)*50/100],
		p99:  sorted[len(sorted)*99/100],
		p999: sorted[len(sorted)*999/1000],
		max:  sorted[len(sorted)-1],
	}
}

func runSubmissionLatency(t *testing.T, total, shards int, submitType string) submissionLatencyStats {
	t.Helper()
	const producers = 256
	qopts := []Option{WithConcurrency(10000), WithShards(shards), WithMaxIdle(time.Minute)}
	release := make(chan struct{})
	var completed sync.WaitGroup
	completed.Add(total)
	job := func() {
		<-release
		completed.Done()
	}

	var submit func() bool
	var stop func() error
	if submitType == "submit" {
		q := New(append(qopts, WithMaxPending(total))...)
		submit = func() bool { return q.Submit(job) }
		stop = func() error { return q.Stop(context.Background()) }
	} else {
		q := New(qopts...)
		submit = func() bool {
			q.Push(job)
			return true
		}
		stop = func() error { return q.Stop(context.Background()) }
	}

	latencies := make([]time.Duration, total)
	start := make(chan struct{})
	var submitted int64
	var producerWG sync.WaitGroup
	producerWG.Add(producers)
	for p := 0; p < producers; p++ {
		go func(p int) {
			defer producerWG.Done()
			<-start
			for i := p; i < total; i += producers {
				begin := time.Now()
				if !submit() {
					t.Errorf("submission %d rejected", i)
					return
				}
				latencies[i] = time.Since(begin)
				atomic.AddInt64(&submitted, 1)
			}
		}(p)
	}
	close(start)
	producerWG.Wait()
	if got := atomic.LoadInt64(&submitted); got != int64(total) {
		t.Fatalf("submitted = %d, want %d", got, total)
	}
	close(release)
	completed.Wait()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	return summarizeSubmissionLatency(latencies)
}

func TestSubmissionLatency(t *testing.T) {
	if os.Getenv("TASKGO_RUN_LATENCY") != "1" {
		t.Skip("set TASKGO_RUN_LATENCY=1 to run submission latency measurements")
	}
	for _, total := range []int{100000, 500000} {
		for _, submitType := range []string{"push", "submit"} {
			for _, shards := range []int{1, 32, 64} {
				stats := runSubmissionLatency(t, total, shards, submitType)
				t.Logf("total=%d type=%s shards=%d | p50=%v p99=%v p999=%v max=%v",
					total, submitType, shards, stats.p50, stats.p99, stats.p999, stats.max)
			}
		}
	}
}
