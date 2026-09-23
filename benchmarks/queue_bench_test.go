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

// Package benchmarks holds the cross-library comparison benchmarks for taskgo.
//
// These benchmarks pull in github.com/lxzan/concurrency for a side-by-side
// comparison. They live in their own module so that dependency never leaks into
// the main taskgo module: importing taskgo brings in nothing but the standard
// library.
//
// Run from this directory:
//
//	go test -run '^$' -bench 'BenchmarkBurst|BenchmarkSaturated' -benchmem
//	go test -run '^$' -bench 'BenchmarkHighConcurrency' -benchmem
//	go test -run '^$' -bench 'BenchmarkTypedBurst|BenchmarkTypedSaturated' -benchmem
//	go test -run '^$' -bench 'BenchmarkTypedHighConcurrency' -benchmem
//	go test -run '^$' -bench 'BenchmarkConcurrentProducers' -benchmem
package benchmarks

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/limpo1989/taskgo"
	lxzan "github.com/lxzan/concurrency/queues"
)

// ----------------------------------------------------------------------------
// Workloads
// ----------------------------------------------------------------------------

// fib is a shallow-stack compute load: its recursion depth is tiny, so the
// goroutine stack never grows and worker reuse cannot help.
func fib(n int) int {
	if n < 2 {
		return n
	}
	return fib(n-1) + fib(n-2)
}

// deepStackWork builds a deep call chain and then does a little computation, to
// amplify the cost of repeated stack growth. The larger depth is, the more a
// worker that exits after each task pays to grow its stack from 2k every time.
//
//go:noinline
func deepStackWork(depth int, acc *int) {
	if depth == 0 {
		// At the bottom do some work that cannot be optimized away.
		x := 0
		for i := 0; i < 64; i++ {
			x += i * i
		}
		*acc += x
		return
	}
	// Use some stack space to keep the compiler from optimizing the recursion away.
	var pad [16]int
	pad[depth&15] = depth
	deepStackWork(depth-1, acc)
	*acc += pad[depth&15]
}

const benchDepth = 256

// deepTask is a deep-call-chain task: it forces goroutine stack growth, so it
// benefits from reusing an already grown stack.
func deepTask() {
	var acc int
	deepStackWork(benchDepth, &acc)
	_ = acc
}

// shallowTask is a cheap, shallow-stack task used as a regression baseline:
// stacks never grow, so reuse cannot help here.
func shallowTask() { fib(10) }

// ----------------------------------------------------------------------------
// Engine abstraction
//
// All benchmarks drive a uniform pool interface so taskgo and lxzan/concurrency
// run the exact same workloads. taskgo appears twice, with parking off (NoReuse)
// and on (Reuse), to isolate the effect of worker reuse.
// ----------------------------------------------------------------------------

type pool interface {
	submit(job func())
	stop()
}

// taskgoPool adapts *taskgo.Queue to the pool interface.
type taskgoPool struct{ q *taskgo.Queue }

func (p *taskgoPool) submit(job func()) { p.q.Push(job) }
func (p *taskgoPool) stop()             { _ = p.q.Stop(context.Background()) }

// lxzanPool adapts github.com/lxzan/concurrency to the pool interface.
type lxzanPool struct{ q lxzan.Queue }

func (p *lxzanPool) submit(job func()) { p.q.Push(job) }
func (p *lxzanPool) stop()             { _ = p.q.Stop(context.Background()) }

// engine names a pool implementation and a factory that builds it at a given
// concurrency level.
type engine struct {
	name string
	make func(concurrency int) pool
}

func engines() []engine {
	return []engine{
		{"taskgo-NoReuse", func(c int) pool {
			return &taskgoPool{taskgo.New(taskgo.WithConcurrency(c))}
		}},
		{"taskgo-Reuse", func(c int) pool {
			return &taskgoPool{taskgo.New(taskgo.WithConcurrency(c), taskgo.WithMaxIdle(time.Second))}
		}},
		{"lxzan", func(c int) pool {
			return &lxzanPool{lxzan.New(lxzan.WithConcurrency(uint32(c)))}
		}},
	}
}

type workload struct {
	name string
	task func()
}

func workloads() []workload {
	return []workload{
		{"Deep", deepTask},
		{"Shallow", shallowTask},
	}
}

// typedPool drives Task[int]. The task function is bound once when the pool is
// created; submitBatch only passes integer values and waits on a shared channel
// for completion. This keeps the benchmark from reintroducing one closure per
// submitted value.
type typedPool interface {
	submitBatch(n int)
	stop()
}

type taskgoTypedPool struct {
	q    *taskgo.Task[int]
	work func()
	done chan struct{}
}

func newTaskgoTypedPool(concurrency int, reuse bool, work func()) *taskgoTypedPool {
	p := &taskgoTypedPool{work: work}
	opts := []taskgo.Option{taskgo.WithConcurrency(concurrency)}
	if reuse {
		opts = append(opts, taskgo.WithMaxIdle(time.Second))
	}
	p.q = taskgo.NewTask(func(int) {
		p.work()
		p.done <- struct{}{}
	}, opts...)
	return p
}

func (p *taskgoTypedPool) submitBatch(n int) {
	p.done = make(chan struct{}, n)
	for i := 0; i < n; i++ {
		p.q.Push(i)
	}
	for i := 0; i < n; i++ {
		<-p.done
	}
}

func (p *taskgoTypedPool) stop() { _ = p.q.Stop(context.Background()) }

type typedEngine struct {
	name string
	make func(concurrency int, work func()) typedPool
}

func typedEngines() []typedEngine {
	return []typedEngine{
		{"taskgo-Task-NoReuse", func(c int, work func()) typedPool {
			return newTaskgoTypedPool(c, false, work)
		}},
		{"taskgo-Task-Reuse", func(c int, work func()) typedPool {
			return newTaskgoTypedPool(c, true, work)
		}},
	}
}

// submitBatch submits n tasks to p and waits for all of them to finish.
func submitBatch(p pool, n int, task func()) {
	var wg sync.WaitGroup
	wg.Add(n)
	for j := 0; j < n; j++ {
		p.submit(func() {
			task()
			wg.Done()
		})
	}
	wg.Wait()
}

// ----------------------------------------------------------------------------
// Scenario A: bursty / intermittent load.
//
// Each batch submits exactly concurrency tasks, so every worker handles roughly
// one task before the queue drains, and the next batch starts cold. This is the
// real-world case behind the "deep stack plus workers that exit quickly"
// problem: pools that retire workers when the queue empties must regrow stacks
// every batch, while pools that keep workers parked reuse the grown stacks.
// ----------------------------------------------------------------------------

const (
	burstConcurrency = 48
	burstBatches     = 2000
)

func BenchmarkBurst(b *testing.B) {
	for _, wl := range workloads() {
		for _, eng := range engines() {
			b.Run(wl.name+"/"+eng.name, func(b *testing.B) {
				p := eng.make(burstConcurrency)
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					for k := 0; k < burstBatches; k++ {
						submitBatch(p, burstConcurrency, wl.task)
					}
				}
				// Exclude teardown from the measurement: pool Stop has its own
				// latency (some implementations poll on a timer) that is not the
				// dispatch throughput under test.
				b.StopTimer()
				p.stop()
			})
		}
	}
}

// ----------------------------------------------------------------------------
// Scenario B: sustained saturation.
//
// A large batch is submitted at once, so the queue stays non-empty and workers
// process tasks back to back, rarely exiting. Stacks stay reused in every pool,
// so this mainly checks that the queue/dispatch machinery itself is competitive
// and that enabling parking does not regress the saturated hot path.
// ----------------------------------------------------------------------------

const (
	satConcurrency = 48
	satBatch       = 100000
)

func BenchmarkSaturated(b *testing.B) {
	for _, wl := range workloads() {
		for _, eng := range engines() {
			b.Run(wl.name+"/"+eng.name, func(b *testing.B) {
				p := eng.make(satConcurrency)
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					submitBatch(p, satBatch, wl.task)
				}
				b.StopTimer()
				p.stop()
			})
		}
	}
}

// ----------------------------------------------------------------------------
// Scenario C: very high concurrency limit (10000) under varying load.
//
// The concurrency cap is far larger than typical, and each iteration submits a
// load that is under, equal to, or many times the cap. This stresses worker
// admission, the parked-worker set, and the wake-up/dispatch path at scale:
//   - load <  cap: under-subscribed; only `load` workers ever run, and Reuse
//     parks them for the next iteration.
//   - load == cap: every slot is used exactly once per iteration.
//   - load >  cap: over-subscribed; tasks queue and workers loop on the hot path.
// ----------------------------------------------------------------------------

const highConcurrency = 10000

func BenchmarkHighConcurrency(b *testing.B) {
	loads := []int{1000, 10000, 100000, 500000}
	for _, wl := range workloads() {
		for _, load := range loads {
			for _, eng := range engines() {
				name := fmt.Sprintf("%s/load=%d/%s", wl.name, load, eng.name)
				b.Run(name, func(b *testing.B) {
					p := eng.make(highConcurrency)
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						submitBatch(p, load, wl.task)
					}
					b.StopTimer()
					p.stop()
				})
			}
		}
	}
}

// ----------------------------------------------------------------------------
// Scenario D: typed Task comparison.
//
// These scenarios mirror the legacy benchmarks above, but use Task[int] and a
// function bound once at construction. The workload and worker settings match
// the corresponding legacy scenario, so the difference isolates submission
// overhead from per-value closures.
// ----------------------------------------------------------------------------

func BenchmarkTypedBurst(b *testing.B) {
	for _, wl := range workloads() {
		for _, eng := range typedEngines() {
			b.Run(wl.name+"/"+eng.name, func(b *testing.B) {
				p := eng.make(burstConcurrency, wl.task)
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					for k := 0; k < burstBatches; k++ {
						p.submitBatch(burstConcurrency)
					}
				}
				b.StopTimer()
				p.stop()
			})
		}
	}
}

func BenchmarkTypedSaturated(b *testing.B) {
	for _, wl := range workloads() {
		for _, eng := range typedEngines() {
			b.Run(wl.name+"/"+eng.name, func(b *testing.B) {
				p := eng.make(satConcurrency, wl.task)
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					p.submitBatch(satBatch)
				}
				b.StopTimer()
				p.stop()
			})
		}
	}
}

func BenchmarkTypedHighConcurrency(b *testing.B) {
	loads := []int{1000, 10000, 100000, 500000}
	for _, wl := range workloads() {
		for _, load := range loads {
			for _, eng := range typedEngines() {
				name := fmt.Sprintf("%s/load=%d/%s", wl.name, load, eng.name)
				b.Run(name, func(b *testing.B) {
					p := eng.make(highConcurrency, wl.task)
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						p.submitBatch(load)
					}
					b.StopTimer()
					p.stop()
				})
			}
		}
	}
}

// Scenario E: multiple producers submit to one saturated queue. Both paths
// execute the same shallow task; the legacy path captures the submitted value
// in a closure, while Task[int] passes it directly.
func BenchmarkConcurrentProducers(b *testing.B) {
	const producers = 16
	const concurrency = 48
	for _, reuse := range []bool{false, true} {
		mode := "NoReuse"
		opts := []taskgo.Option{taskgo.WithConcurrency(concurrency)}
		if reuse {
			mode = "Reuse"
			opts = append(opts, taskgo.WithMaxIdle(time.Second))
		}
		for _, typed := range []bool{false, true} {
			name := "Queue/" + mode
			if typed {
				name = "Task[int]/" + mode
			}
			b.Run(name, func(b *testing.B) {
				var done sync.WaitGroup
				done.Add(b.N)
				var push func(int)
				var stop func()
				if typed {
					q := taskgo.NewTask(func(value int) {
						if value < 0 {
							panic("unreachable")
						}
						shallowTask()
						done.Done()
					}, opts...)
					push = q.Push
					stop = func() { _ = q.Stop(context.Background()) }
				} else {
					q := taskgo.New(opts...)
					push = func(value int) {
						q.Push(func() {
							if value < 0 {
								panic("unreachable")
							}
							shallowTask()
							done.Done()
						})
					}
					stop = func() { _ = q.Stop(context.Background()) }
				}

				b.ResetTimer()
				var producersDone sync.WaitGroup
				producersDone.Add(producers)
				for producer := 0; producer < producers; producer++ {
					go func(producer int) {
						defer producersDone.Done()
						for value := producer; value < b.N; value += producers {
							push(value)
						}
					}(producer)
				}
				producersDone.Wait()
				done.Wait()
				b.StopTimer()
				stop()
			})
		}
	}
}
