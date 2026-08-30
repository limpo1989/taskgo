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

package benchmarks

import (
	"context"
	"math/rand"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/limpo1989/taskgo"
	lxzan "github.com/lxzan/concurrency/queues"
)

// TestGoroutinePeakComparison settles, with the real lxzan library (not a model),
// whether taskgo and lxzan/concurrency differ in process-wide goroutine peak
// under the same heavy long-tail load at the same concurrency cap. It samples
// runtime.NumGoroutine() (exactly what the production Prometheus gauge reports),
// reports the baseline-adjusted peak/mean for each engine, and asserts they are
// in the same ballpark — because both libraries grow workers the same way
// (spawn while current < cap), so neither should produce a categorically higher
// goroutine peak than the other.
//
// Run from the benchmarks module:
//
//	go test -run TestGoroutinePeakComparison -v
func TestGoroutinePeakComparison(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping goroutine-peak comparison in -short mode")
	}

	const (
		totalTasks  = 40000
		arrivalRate = 4000 // tasks/s
		concurrency = 10000
		sliceEvery  = 5 * time.Millisecond
		sampleEvery = 10 * time.Millisecond
	)
	perSlice := arrivalRate * int(sliceEvery/time.Millisecond) / 1000

	// heavyCost: 70% fast (1-10ms), 20% slow (100-500ms), 10% very slow (1-2s).
	heavyCost := func(rng *rand.Rand) time.Duration {
		r := rng.Float64()
		switch {
		case r < 0.70:
			return time.Duration(1+rng.Intn(10)) * time.Millisecond
		case r < 0.90:
			return time.Duration(100+rng.Intn(400)) * time.Millisecond
		default:
			return time.Duration(1000+rng.Intn(1000)) * time.Millisecond
		}
	}

	// drive submits the paced heavy-tail load through push and waits for drain,
	// while sampling process-wide NumGoroutine. It returns the baseline-adjusted
	// peak and mean (baseline = goroutines already alive before the run).
	drive := func(push func(func()), stop func()) (peak int, mean float64) {
		// Let any goroutines from a previous run finish before measuring baseline.
		time.Sleep(300 * time.Millisecond)
		runtime.GC()
		baseline := runtime.NumGoroutine()

		var samples []int
		stopSampler := make(chan struct{})
		var samplerDone sync.WaitGroup
		samplerDone.Add(1)
		go func() {
			defer samplerDone.Done()
			tk := time.NewTicker(sampleEvery)
			defer tk.Stop()
			for {
				select {
				case <-stopSampler:
					return
				case <-tk.C:
					samples = append(samples, runtime.NumGoroutine())
				}
			}
		}()

		rng := rand.New(rand.NewSource(42))
		var completed int64
		ticker := time.NewTicker(sliceEvery)
		submitted := 0
		for submitted < totalTasks {
			n := perSlice
			if submitted+n > totalTasks {
				n = totalTasks - submitted
			}
			for i := 0; i < n; i++ {
				cost := heavyCost(rng)
				push(func() {
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
		close(stopSampler)
		samplerDone.Wait()
		stop()

		// Baseline-adjust and summarize.
		adj := make([]int, 0, len(samples))
		for _, s := range samples {
			v := s - baseline
			if v < 0 {
				v = 0
			}
			adj = append(adj, v)
		}
		sort.Ints(adj)
		if len(adj) > 0 {
			peak = adj[len(adj)-1]
			sum := 0
			for _, v := range adj {
				sum += v
			}
			mean = float64(sum) / float64(len(adj))
		}
		return peak, mean
	}

	t.Log("=== Real lxzan vs taskgo: process-wide goroutine peak under identical heavy-tail load ===")
	t.Logf("    (70%% 1-10ms, 20%% 100-500ms, 10%% 1-2s; paced ~%d/s; cap=%d)", arrivalRate, concurrency)

	// taskgo with parking on (its default-recommended config).
	tqReuse := taskgo.New(taskgo.WithConcurrency(concurrency), taskgo.WithMaxIdle(time.Second))
	pkTgReuse, mnTgReuse := drive(func(f func()) { tqReuse.Push(f) }, func() { _ = tqReuse.Stop(context.Background()) })

	// taskgo with parking off: closest analogue to lxzan's "exit when drained".
	tqNoReuse := taskgo.New(taskgo.WithConcurrency(concurrency))
	pkTgNo, mnTgNo := drive(func(f func()) { tqNoReuse.Push(f) }, func() { _ = tqNoReuse.Stop(context.Background()) })

	// Real lxzan single queue (sharding defaults to 1).
	lq := lxzan.New(lxzan.WithConcurrency(uint32(concurrency)))
	pkLx, mnLx := drive(func(f func()) { lq.Push(f) }, func() { _ = lq.Stop(context.Background()) })

	t.Logf("%-22s peak=%-6d mean=%-8.1f", "taskgo idle=1s", pkTgReuse, mnTgReuse)
	t.Logf("%-22s peak=%-6d mean=%-8.1f", "taskgo idle=0", pkTgNo, mnTgNo)
	t.Logf("%-22s peak=%-6d mean=%-8.1f", "lxzan (real)", pkLx, mnLx)

	// Both libraries grow workers identically (spawn while current < cap), so the
	// PEAK goroutine count is set by the working set and must be in the same
	// ballpark. Assert lxzan and taskgo-no-park (the matched configs) agree within
	// 25%, ruling out a categorical library difference.
	hi, lo := pkLx, pkTgNo
	if lo > hi {
		hi, lo = lo, hi
	}
	if hi-lo > hi*25/100 {
		t.Errorf("lxzan and taskgo(no-park) peaks differ by >25%%: lxzan=%d taskgo=%d "+
			"(same growth model expected to give similar peaks)", pkLx, pkTgNo)
	}
}
