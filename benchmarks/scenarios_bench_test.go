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
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This file ports the scenario suite of the taskpool comparison benchmarks
// (github.com/lesismal/fib/taskpool/benchmark) so the engines this module
// already drives - taskgo with and without worker reuse, and lxzan - run the
// same loads those pools are compared with:
//
//   - Handoff        one submitter, one task at a time, waiting for each: the
//     round trip of a lightly loaded server.
//   - ParallelTiny   every P submits empty tasks at once: contention on the
//     pool itself.
//   - LoopCPUShort   GOMAXPROCS/4 submitters, as event loops, feeding
//     CPU-bound tasks of ~50 rounds.
//   - LoopCPULong    the same with ~2000 rounds.
//   - LoopBlocking   the same with tasks that sleep 1ms: how fast the pool
//     fans out.
//   - LoopMixed      one task in ten sleeps 500µs, the rest spin 500 rounds.
//   - Bursts         rounds of 256 short tasks with a 2ms idle gap, left out
//     of the timing: reuse of warm workers.
//
// Each reports ns/op and cpu-ns/op, the process CPU time spent per task.
// Run from this directory:
//
//	go test -run '^$' -bench 'BenchmarkScenarios' -benchtime 1s

// scenarioCeiling is the concurrency limit every engine gets, the default of
// the taskpool suite the scenarios come from: 1000 workers per P.
func scenarioCeiling() int { return 1000 * runtime.GOMAXPROCS(0) }

// scenarios lists the loads in the order the suite runs them.
var scenarios = []struct {
	name string
	run  func(b *testing.B, p pool)
}{
	{"Handoff", scenarioHandoff},
	{"ParallelTiny", scenarioParallelTiny},
	{"LoopCPUShort", func(b *testing.B, p pool) { scenarioLoops(b, p, func() { scenarioSpin(50) }) }},
	{"LoopCPULong", func(b *testing.B, p pool) { scenarioLoops(b, p, func() { scenarioSpin(2000) }) }},
	{"LoopBlocking", func(b *testing.B, p pool) { scenarioLoops(b, p, func() { time.Sleep(time.Millisecond) }) }},
	{"LoopMixed", scenarioLoopMixed},
	{"Bursts", scenarioBursts},
}

// BenchmarkScenarios runs every load against every engine, as
// BenchmarkScenarios/<scenario>/<engine>. Engines without worker reuse park
// nothing; the taskgo-Reuse engine parks its workers for a second, as the
// taskpool suite configures the pools it compares.
func BenchmarkScenarios(b *testing.B) {
	for _, sc := range scenarios {
		b.Run(sc.name, func(b *testing.B) {
			for _, eng := range engines() {
				b.Run(eng.name, func(b *testing.B) {
					p := eng.make(scenarioCeiling())
					defer p.stop()
					runtime.GC()
					start := cpuTime()
					b.ResetTimer()
					sc.run(b, p)
					b.StopTimer()
					b.ReportMetric(float64(cpuTime()-start)/float64(b.N), "cpu-ns/op")
				})
			}
		})
	}
}

var scenarioSink atomic.Uint64

// scenarioSpin burns CPU for n rounds of a generator the compiler cannot
// drop, the task shape of the CPU-bound loads.
func scenarioSpin(n int) {
	x := uint64(n)
	for i := 0; i < n; i++ {
		x = x*6364136223846793005 + 1442695040888963407
	}
	scenarioSink.Add(x & 1)
}

// scenarioHandoff submits one task at a time and waits for it: the latency of
// a lightly loaded server.
func scenarioHandoff(b *testing.B, p pool) {
	done := make(chan struct{})
	for i := 0; i < b.N; i++ {
		p.submit(func() { done <- struct{}{} })
		<-done
	}
}

// scenarioParallelTiny has every P submit empty tasks at once: raw contention
// on the pool's own structures.
func scenarioParallelTiny(b *testing.B, p pool) {
	var wg sync.WaitGroup
	wg.Add(b.N)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			p.submit(wg.Done)
		}
	})
	wg.Wait()
}

// scenarioLoops submits b.N tasks from GOMAXPROCS/4 goroutines, as an
// engine's event loops do, and waits for them all.
func scenarioLoops(b *testing.B, p pool, task func()) {
	n := max(1, runtime.GOMAXPROCS(0)/4)
	per := (b.N + n - 1) / n
	var wg sync.WaitGroup
	wg.Add(per * n)
	f := func() { task(); wg.Done() }
	for l := 0; l < n; l++ {
		go func() {
			for i := 0; i < per; i++ {
				p.submit(f)
			}
		}()
	}
	wg.Wait()
}

// scenarioLoopMixed is scenarioLoops with one task in ten blocking for 500µs
// and the rest CPU-bound.
func scenarioLoopMixed(b *testing.B, p pool) {
	var count atomic.Uint64
	scenarioLoops(b, p, func() {
		if count.Add(1)%10 == 0 {
			time.Sleep(500 * time.Microsecond)
		} else {
			scenarioSpin(500)
		}
	})
}

// scenarioBursts submits rounds of 256 short tasks separated by an idle gap,
// which is left out of the timing. A pool that parks or lingers its workers
// serves the next round from warm goroutines; one that retires them at once
// forks again.
func scenarioBursts(b *testing.B, p pool) {
	const round, gap = 256, 2 * time.Millisecond
	var wg sync.WaitGroup
	f := func() { scenarioSpin(200); wg.Done() }
	for done := 0; done < b.N; done += round {
		wg.Add(round)
		for i := 0; i < round; i++ {
			p.submit(f)
		}
		wg.Wait()
		b.StopTimer()
		time.Sleep(gap)
		b.StartTimer()
	}
}
