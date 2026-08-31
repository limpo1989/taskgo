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
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This file models the reported production workload: a steady arrival rate
// (~4k req/s class) where the vast majority of tasks are very fast (<10ms) but a
// long tail runs for hundreds of milliseconds and a rare few for ~2s. That
// mismatch matters: the earlier burst experiment (sawtooth_test.go) used uniform
// sub-millisecond tasks arriving in batches, which does not capture how a few
// slow tasks pin workers in `running` for ~2s while fast tasks keep arriving and
// spawning new workers. Here arrivals are paced (not batched) and task cost is
// drawn from the real-world long-tail distribution, then two things are measured
// at once for each config:
//
//   - the live-worker gauge sawtooth (stddev / range / peak), and
//   - the queueing latency of fast tasks (p50 / p99 / max), which reveals whether
//     slow tasks are starving fast ones.

// latencyStats summarizes a set of latencies (here: time from submit to start).
type latencyStats struct {
	n    int
	p50  time.Duration
	p99  time.Duration
	max  time.Duration
	mean time.Duration
}

func computeLatency(ds []time.Duration) latencyStats {
	st := latencyStats{n: len(ds)}
	if len(ds) == 0 {
		return st
	}
	sorted := make([]time.Duration, len(ds))
	copy(sorted, ds)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var sum time.Duration
	for _, d := range sorted {
		sum += d
	}
	st.mean = sum / time.Duration(len(sorted))
	st.p50 = sorted[len(sorted)*50/100]
	st.p99 = sorted[minInt(len(sorted)-1, len(sorted)*99/100)]
	st.max = sorted[len(sorted)-1]
	return st
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// costMix describes a task-duration distribution by the cumulative probability
// of the fast and slow buckets. fastP is the share of fast (1-10ms) tasks; the
// next slowP-fastP share is slow (100-500ms); the remainder is very slow
// (1-2s). With fastP=0.94, slowP=0.99 this is the originally reported mix.
type costMix struct {
	fastP float64
	slowP float64
}

// longTailCost returns a task duration drawn from the given distribution.
func longTailCost(rng *rand.Rand, m costMix) time.Duration {
	r := rng.Float64()
	switch {
	case r < m.fastP:
		return time.Duration(1+rng.Intn(10)) * time.Millisecond
	case r < m.slowP:
		return time.Duration(100+rng.Intn(400)) * time.Millisecond
	default:
		return time.Duration(1000+rng.Intn(1000)) * time.Millisecond
	}
}

// longTailScenario drives a paced long-tail workload through a real queue and
// returns both the gauge sawtooth metrics and the fast-task queueing latency.
type longTailResult struct {
	saw  sawMetrics
	fast latencyStats // queueing latency of fast (<10ms) tasks only
}

func runLongTail(t *testing.T, sampleEvery time.Duration, mix costMix, opts ...Option) longTailResult {
	t.Helper()
	const (
		totalTasks  = 40000
		arrivalRate = 4000 // tasks per second
	)
	// Pace arrivals: submit in small slices on a fixed cadence to approximate a
	// steady rate without one goroutine per task.
	const sliceEvery = 5 * time.Millisecond
	perSlice := arrivalRate * int(sliceEvery/time.Millisecond) / 1000 // 20 tasks / 5ms

	q := New(opts...)
	rng := rand.New(rand.NewSource(42)) // fixed seed: comparable across configs

	var completed int64
	var (
		fastMu  sync.Mutex
		fastLat []time.Duration
	)
	s := newLiveSampler(sampleEvery, q.liveCount)
	s.start()

	ticker := time.NewTicker(sliceEvery)
	defer ticker.Stop()
	submitted := 0
	for submitted < totalTasks {
		n := perSlice
		if submitted+n > totalTasks {
			n = totalTasks - submitted
		}
		for i := 0; i < n; i++ {
			cost := longTailCost(rng, mix)
			isFast := cost <= 10*time.Millisecond
			submitTime := time.Now()
			q.Push(func() {
				if isFast {
					wait := time.Since(submitTime)
					fastMu.Lock()
					fastLat = append(fastLat, wait)
					fastMu.Unlock()
				}
				time.Sleep(cost)
				atomic.AddInt64(&completed, 1)
			})
		}
		submitted += n
		<-ticker.C
	}

	// Drain.
	deadline := time.Now().Add(60 * time.Second)
	for atomic.LoadInt64(&completed) < int64(totalTasks) && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	samples := s.stop()
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}

	fastMu.Lock()
	lat := computeLatency(fastLat)
	fastMu.Unlock()
	return longTailResult{saw: computeSawMetrics(samples), fast: lat}
}

// TestLongTailMitigations reproduces the production-like long-tail workload and
// reports, for each mitigation, both the goroutine sawtooth and the fast-task
// queueing latency, so the "flatten the gauge" goal and the "don't starve fast
// tasks" goal can be weighed against each other on data.
func TestLongTailMitigations(t *testing.T) {
	requireRuntimeExperiment(t)
	const sampleEvery = 10 * time.Millisecond

	type scenario struct {
		name string
		opts []Option
	}
	scenarios := []scenario{
		{"baseline  c=10000 idle=1s ", []Option{WithConcurrency(10000), WithMaxIdle(time.Second)}},
		{"lower     c=512   idle=1s ", []Option{WithConcurrency(512), WithMaxIdle(time.Second)}},
		{"verylow   c=128   idle=1s ", []Option{WithConcurrency(128), WithMaxIdle(time.Second)}},
		{"longidle  c=10000 idle=15s", []Option{WithConcurrency(10000), WithMaxIdle(15 * time.Second)}},
		{"lower+idle c=512  idle=15s", []Option{WithConcurrency(512), WithMaxIdle(15 * time.Second)}},
	}

	t.Log("=== Long-tail workload: gauge sawtooth AND fast-task queue latency ===")
	t.Log("    (94% 1-10ms, 5% 100-500ms, 1% 1-2s; paced ~4k/s)")
	mix := costMix{fastP: 0.94, slowP: 0.99}
	results := make(map[string]longTailResult)
	for _, sc := range scenarios {
		r := runLongTail(t, sampleEvery, mix, sc.opts...)
		results[sc.name] = r
		t.Logf("%-28s | gauge: stddev=%-7.1f range=%-6d peak=%-6d | fast-wait: p50=%-8v p99=%-8v max=%-8v",
			sc.name, r.saw.stddev, r.saw.rangeAbs, r.saw.max,
			r.fast.p50, r.fast.p99, r.fast.max)
	}

	base := results["baseline  c=10000 idle=1s "]
	low := results["lower     c=512   idle=1s "]
	verylow := results["verylow   c=128   idle=1s "]
	longidle := results["longidle  c=10000 idle=15s"]

	// Finding 1: under this long-tail workload the steady-state worker count is
	// set by Little's law (arrival_rate * mean_cost), here ~140-170. A cap of 512
	// or 10000 is never reached, so lowering the cap to 512 does NOT flatten the
	// gauge — it is well above the working set. Compare it with the baseline.
	if low.saw.peakDelta(base.saw) > 64 {
		t.Logf("observation differs from expected direction: c=512 changed the peak vs baseline (base=%d low=%d); "+
			"expected no real effect since the cap is above the working set",
			base.saw.max, low.saw.max)
	}

	// Finding 2: a cap BELOW the working set (c=128) does flatten the gauge, but
	// at a severe cost — slow tail tasks pin the few workers and fast tasks queue
	// behind them (head-of-line blocking). Log whether that expected tradeoff is
	// observed on this host.
	if verylow.fast.p99 <= base.fast.p99*10 {
		t.Logf("observation differs from expected direction: c=128 did not starve fast tasks (p99 >> baseline), "+
			"base p99=%v verylow p99=%v", base.fast.p99, verylow.fast.p99)
	}

	// Finding 3: lengthening maxIdle is the mitigation that helps here without
	// hurting latency — it keeps workers parked across the lulls between slow
	// tasks, smoothing the reclaim pulses. Compare fast-task p99 with baseline.
	if longidle.fast.p99 > base.fast.p99*2 {
		t.Logf("observation differs from expected direction: longer maxIdle hurt fast-task p99: base=%v longidle=%v",
			base.fast.p99, longidle.fast.p99)
	}
}

// TestHeavyTailCap reproduces the situation suspected in production: after
// switching from lxzan (a hard-capped pool, configured with a small cap) to
// taskgo with WithConcurrency(10000), the whole-process goroutine count grew
// peaks. With many slow/very-slow tasks the working set is large, and a 10000
// cap lets the live worker count climb toward it; a small cap (as lxzan was
// likely configured) holds the count flat instead.
//
// Distribution here is deliberately heavy: 70% fast (1-10ms), 20% slow
// (100-500ms), 10% very slow (1-2s). Mean cost ~= 0.7*5.5 + 0.2*300 + 0.1*1500
// ~= 214ms, so by Little's law a paced 4k/s feed needs ~850 workers in steady
// state — far above lxzan's small cap, which is exactly why the peak appears
// only after the cap was raised.
func TestHeavyTailCap(t *testing.T) {
	requireRuntimeExperiment(t)
	const sampleEvery = 10 * time.Millisecond
	mix := costMix{fastP: 0.70, slowP: 0.90} // 70% fast, 20% slow, 10% very slow

	type scenario struct {
		name string
		opts []Option
	}
	scenarios := []scenario{
		{"taskgo c=10000 idle=1s ", []Option{WithConcurrency(10000), WithMaxIdle(time.Second)}},
		{"taskgo c=10000 idle=15s", []Option{WithConcurrency(10000), WithMaxIdle(15 * time.Second)}},
		{"taskgo c=10000 idle=0  ", []Option{WithConcurrency(10000)}}, // park off: ~ lxzan's "exit when drained"
		{"lxzan-like c=512 idle=1s", []Option{WithConcurrency(512), WithMaxIdle(time.Second)}},
		{"lxzan-like c=256 idle=1s", []Option{WithConcurrency(256), WithMaxIdle(time.Second)}},
	}

	t.Log("=== Heavy-tail workload: does a high cap inflate the goroutine peak? ===")
	t.Log("    (70% 1-10ms, 20% 100-500ms, 10% 1-2s; paced ~4k/s; working set ~850)")
	results := make(map[string]longTailResult)
	for _, sc := range scenarios {
		r := runLongTail(t, sampleEvery, mix, sc.opts...)
		results[sc.name] = r
		t.Logf("%-26s | gauge: stddev=%-7.1f range=%-6d peak=%-6d | fast-wait: p50=%-10v p99=%-10v max=%-10v",
			sc.name, r.saw.stddev, r.saw.rangeAbs, r.saw.max,
			r.fast.p50, r.fast.p99, r.fast.max)
	}

	high := results["taskgo c=10000 idle=1s "]
	noPark := results["taskgo c=10000 idle=0  "]
	cap512 := results["lxzan-like c=512 idle=1s"]
	cap256 := results["lxzan-like c=256 idle=1s"]

	// Key finding (verified by data, correcting an earlier hypothesis): the live
	// PEAK is set by the working set (running workers), which both lxzan and
	// taskgo grow the same way (spawn while curConcurrency < max). Parking does
	// NOT raise the peak — turning it off (idle=0) leaves the peak essentially
	// unchanged. What parking changes is low-side smoothness: idle workers held
	// across lulls keep the count from collapsing, so park-off actually has a
	// LARGER stddev (jagged), not a smaller one. Record the observed direction.
	if noPark.saw.max < high.saw.max*8/10 {
		t.Logf("observation differs from expected direction: park-off cut the peak materially (idle1s=%d idle0=%d); "+
			"peak is set by the working set, not by parking",
			high.saw.max, noPark.saw.max)
	}
	if noPark.saw.stddev <= high.saw.stddev {
		t.Logf("observation differs from expected direction: park-off was not more jagged than idle=1s (no idle workers "+
			"to smooth the lows): idle1s stddev=%.1f idle0 stddev=%.1f",
			high.saw.stddev, noPark.saw.stddev)
	}

	// With this heavy tail the working set (~850) exceeds the small caps, so a
	// small cap clips the peak well below the high-cap peak. This is the cap
	// effect, independent of the library.
	if cap512.saw.max >= high.saw.max {
		t.Logf("observation differs from expected direction: c=512 did not clip the peak below c=10000: high=%d c512=%d",
			high.saw.max, cap512.saw.max)
	}
	if cap256.saw.max >= cap512.saw.max {
		t.Logf("observation differs from expected direction: c=256 did not clip the peak below c=512: c512=%d c256=%d",
			cap512.saw.max, cap256.saw.max)
	}

	// And the tradeoff is explicit: clipping the peak with a cap below the
	// working set makes fast tasks queue behind the slow tail, so fast-task p99
	// is far worse than the uncapped (c=10000) case.
	if cap256.fast.p99 <= high.fast.p99*5 {
		t.Logf("observation differs from expected direction: c=256 did not badly delay fast tasks vs c=10000: "+
			"high p99=%v c256 p99=%v", high.fast.p99, cap256.fast.p99)
	}
}
