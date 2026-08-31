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
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This file answers the production question directly: a goroutine profile showed
// ~1922 workers parked at queue.go:215 (the `job = <-w.ch` park point), i.e. idle
// workers held by WithMaxIdle. This experiment sweeps maxIdle and, for each
// value, reports the tradeoff it controls:
//
//   - idle: how many workers sit parked (the cost — what shows up in
//     NumGoroutine and the goroutine profile), sampled as mean and peak;
//   - reuse rate: the fraction of dispatched tasks served by an already-warm
//     worker rather than a freshly spawned one (the benefit — fewer spawns means
//     grown stacks are reused);
//   - spawns: how many worker goroutines were started in total.
//
// Reuse is measured without touching the hot path: spawns are counted via the
// existing onSpawnForTest hook, and reuse = dispatched - spawns.

// idleSampler periodically records q.idleLen() to capture the steady-state pool
// of parked workers.
type idleSampler struct {
	q        *Queue
	interval time.Duration
	mu       sync.Mutex
	samples  []int
	stopCh   chan struct{}
	doneCh   chan struct{}
}

func newIdleSampler(q *Queue, interval time.Duration) *idleSampler {
	return &idleSampler{q: q, interval: interval, stopCh: make(chan struct{}), doneCh: make(chan struct{})}
}

func (s *idleSampler) start() {
	go func() {
		defer close(s.doneCh)
		tk := time.NewTicker(s.interval)
		defer tk.Stop()
		for {
			select {
			case <-s.stopCh:
				return
			case <-tk.C:
				v := s.q.idleLen()
				s.mu.Lock()
				s.samples = append(s.samples, v)
				s.mu.Unlock()
			}
		}
	}()
}

func (s *idleSampler) stop() (mean float64, peak int) {
	close(s.stopCh)
	<-s.doneCh
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.samples) == 0 {
		return 0, 0
	}
	sum := 0
	for _, v := range s.samples {
		sum += v
		if v > peak {
			peak = v
		}
	}
	return float64(sum) / float64(len(s.samples)), peak
}

// idleSweepResult captures one maxIdle data point.
type idleSweepResult struct {
	maxIdle    time.Duration
	dispatched int64
	spawns     int64
	reuseRate  float64 // (dispatched - spawns) / dispatched
	idleMean   float64
	idlePeak   int
}

// runIdleSweep drives the heavy long-tail load through a queue configured with
// the given maxIdle and returns the cost/benefit metrics. maxIdle<=0 disables
// parking entirely (the lxzan-equivalent: workers exit when the queue drains).
func runIdleSweep(t *testing.T, maxIdle time.Duration) idleSweepResult {
	t.Helper()
	const (
		totalTasks  = 40000
		arrivalRate = 4000
		sliceEvery  = 5 * time.Millisecond
		concurrency = 10000
	)
	perSlice := arrivalRate * int(sliceEvery/time.Millisecond) / 1000

	opts := []Option{WithConcurrency(concurrency)}
	if maxIdle > 0 {
		opts = append(opts, WithMaxIdle(maxIdle))
	}
	q := New(opts...)

	var spawns int64
	q.onSpawnForTest = func() { atomic.AddInt64(&spawns, 1) }

	sampler := newIdleSampler(q, 10*time.Millisecond)
	sampler.start()

	rng := rand.New(rand.NewSource(42)) // fixed seed: same task sequence per config
	var completed int64
	var dispatched int64
	ticker := time.NewTicker(sliceEvery)
	submitted := 0
	for submitted < totalTasks {
		n := perSlice
		if submitted+n > totalTasks {
			n = totalTasks - submitted
		}
		for i := 0; i < n; i++ {
			cost := longTailCost(rng, costMix{fastP: 0.70, slowP: 0.90})
			atomic.AddInt64(&dispatched, 1)
			q.Push(func() {
				time.Sleep(cost)
				atomic.AddInt64(&completed, 1)
			})
		}
		submitted += n
		<-ticker.C
	}
	ticker.Stop()
	deadline := time.Now().Add(60 * time.Second)
	for atomic.LoadInt64(&completed) < int64(totalTasks) && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	idleMean, idlePeak := sampler.stop()
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}

	d := atomic.LoadInt64(&dispatched)
	sp := atomic.LoadInt64(&spawns)
	reuse := 0.0
	if d > 0 {
		reuse = float64(d-sp) / float64(d)
	}
	return idleSweepResult{
		maxIdle:    maxIdle,
		dispatched: d,
		spawns:     sp,
		reuseRate:  reuse,
		idleMean:   idleMean,
		idlePeak:   idlePeak,
	}
}

// TestMaxIdleSweep prints the maxIdle cost/benefit curve so a production value
// can be chosen on data: how many parked workers (the NumGoroutine cost) each
// maxIdle holds, against how much worker reuse it buys.
func TestMaxIdleSweep(t *testing.T) {
	requireRuntimeExperiment(t)

	idles := []time.Duration{
		0,
		100 * time.Millisecond,
		500 * time.Millisecond,
		1 * time.Second,
		5 * time.Second,
	}

	t.Log("=== maxIdle sweep: parked-worker cost vs reuse benefit ===")
	t.Log("    (heavy tail 70/20/10, paced ~4k/s, cap=10000; idle = workers parked at queue.go:215)")
	var results []idleSweepResult
	for _, d := range idles {
		r := runIdleSweep(t, d)
		results = append(results, r)
		label := r.maxIdle.String()
		if r.maxIdle == 0 {
			label = "0 (park off)"
		}
		t.Logf("maxIdle=%-8s | idle mean=%-8.1f peak=%-6d | reuse=%5.1f%% | spawns=%d",
			label, r.idleMean, r.idlePeak, r.reuseRate*100, r.spawns)
	}

	// Expected directions are logged rather than asserted because wall-clock
	// scheduling and timer resolution vary significantly across hosts.
	// 1) park off must hold ~zero idle workers and reuse ~0% (every task spawns or
	//    runs on the hot path; no parking means no cross-task reuse via the channel).
	off := results[0]
	if off.idlePeak > 5 {
		t.Logf("observation differs from expected direction: park-off idle peak=%d", off.idlePeak)
	}

	// 2) longer maxIdle must hold strictly more idle workers (the cost grows).
	for i := 1; i < len(results); i++ {
		if results[i].idleMean < results[i-1].idleMean {
			t.Logf("observation differs from expected direction: idle pool did not grow with maxIdle: %v=%.1f then %v=%.1f",
				results[i-1].maxIdle, results[i-1].idleMean,
				results[i].maxIdle, results[i].idleMean)
		}
	}

	// 3) and longer maxIdle must buy more reuse (the benefit grows), confirming
	//    the parked workers are not idle dead weight but are actually reused.
	if results[len(results)-1].reuseRate <= off.reuseRate {
		t.Logf("observation differs from expected direction: longest maxIdle did not yield more reuse than park-off: off=%.3f long=%.3f",
			off.reuseRate, results[len(results)-1].reuseRate)
	}
}
