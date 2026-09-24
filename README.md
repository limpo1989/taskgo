# taskgo

[![CI](https://github.com/limpo1989/taskgo/actions/workflows/ci.yml/badge.svg)](https://github.com/limpo1989/taskgo/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/limpo1989/taskgo.svg)](https://pkg.go.dev/github.com/limpo1989/taskgo)

`taskgo` is a lightweight, concurrency-limited task queue for Go whose workers
are **reused for a short while** instead of exiting after every task.

A naive pool starts a goroutine per task (or retires a worker as soon as the
queue drains). That is fine for shallow, short tasks, but it is wasteful for
tasks with **deep call chains**: every fresh goroutine starts with a 2 KB stack
and must grow it (`morestack`: 2 KB → 4 KB → 8 KB …) on every run, paying the
growth cost again and again.

`taskgo` lets a worker stay **parked** for up to `maxIdle` after the queue
drains, so the next task reuses its already-grown stack. Parking is not the same
as a resident pool: a worker is reclaimed once it has been idle longer than
`maxIdle`, or after it has handled `maxJobs` tasks.

## Features

- Concurrency-limited dispatch with a strict upper bound on live goroutines.
- **Short-lived worker reuse** to avoid repeated stack growth for deep-stack tasks.
- Two independent reclamation triggers: idle timeout (`WithMaxIdle`) and a
  per-worker task cap (`WithMaxJobs`).
- Zero-overhead hot path: when parking is disabled (the default) the dispatch
  path takes a single mutex and uses no channels.
- Graceful shutdown that drains outstanding work, with a deadline.
- Optional panic handler so a panicking task cannot crash the process.

## Install

```
go get github.com/limpo1989/taskgo@latest
```

## API

```go
type Job func()
type Task[T any] struct { ... }

func New(opts ...Option) *Queue
func NewTask[T any](fn func(T), opts ...Option) *Task[T]

func (q *Queue) Push(job Job)               // submit a task (nil is ignored)
func (q *Queue) PushBatch(jobs []Job) int    // submit jobs, return accepted count
func (q *Queue) Submit(job Job) bool        // non-blocking bounded submit
func (q *Queue) SubmitBatch(jobs []Job) int  // accept a prefix up to the limit
func (q *Queue) TrySubmitBatch(jobs []Job) bool // all-or-nothing batch submit
func (q *Queue) Len() int                   // tasks queued but not yet started
func (q *Queue) Stop(ctx context.Context) error // drain and shut down

func (q *Task[T]) Push(value T)              // submit a value to the bound fn
func (q *Task[T]) PushBatch(values []T) int   // submit a batch, return accepted count
func (q *Task[T]) Submit(value T) bool        // non-blocking bounded submit
func (q *Task[T]) SubmitBatch(values []T) int // accept a prefix up to the limit
func (q *Task[T]) TrySubmitBatch(values []T) bool // all-or-nothing batch submit
func (q *Task[T]) Len() int
func (q *Task[T]) Stop(ctx context.Context) error

// Options
func WithConcurrency(n int) Option           // max concurrent workers (default 8)
func WithMaxIdle(d time.Duration) Option     // park idle workers for d (default 0 = off)
func WithMaxJobs(n int) Option               // recycle a worker after n tasks (default 0 = off)
func WithMaxPending(n int) Option            // bound outstanding Submit jobs (default 0 = off)
func WithTimeout(d time.Duration) Option     // how long Stop waits (default 30s)
func WithPanicHandler(fn func(v any)) Option // recover task panics
```

## Example

```go
package main

import (
	"context"
	"sync"
	"time"

	"github.com/limpo1989/taskgo"
)

func fib(n int) int {
	if n < 2 {
		return n
	}
	return fib(n-1) + fib(n-2)
}

func main() {
	// Enable worker reuse so deep-stack tasks reuse grown stacks.
	q := taskgo.New(
		taskgo.WithConcurrency(16),
		taskgo.WithMaxIdle(time.Second),
	)

	const total = 10000
	var wg sync.WaitGroup
	wg.Add(total)
	for i := 0; i < total; i++ {
		q.Push(func() {
			fib(20)
			wg.Done()
		})
	}
	wg.Wait()

	_ = q.Stop(context.Background())
}
```

When all tasks call the same function with different data, bind that function
once with `NewTask`:

```go
q := taskgo.NewTask(func(value int) {
	_ = fib(value)
}, taskgo.WithConcurrency(16))
for i := 0; i < 10000; i++ {
	q.Push(20)
}
_ = q.Stop(context.Background())
```

`Task[T].Push` stores `T` directly in the queue. It avoids creating a new
closure for each submission, which can reduce allocations and submission-side
CPU when the task function is shared and the work itself is small. It does not
remove the cost of worker scheduling or make expensive task bodies faster.

Batch methods preserve input order when admitting values. `SubmitBatch` returns
the number accepted, and those accepted values are exactly `values[:n]`; the
rest were rejected by the pending limit or shutdown. Use `TrySubmitBatch` when
partial submission is not acceptable. FIFO controls queue order; with
concurrency greater than one, task start and completion order can still
interleave. Use `WithConcurrency(1)` when execution order must be strict.

## How it works

`Push` follows three paths, all under one mutex:

1. **A parked worker exists** → wake it (LIFO, so the hottest stack is reused).
2. **No parked worker and below the concurrency limit** → start a new worker.
3. **At the limit** → enqueue the task; a looping worker will pick it up.

When the queue is busy, a worker claims a small local batch from the central
backlog under the mutex and runs that batch without taking the mutex again. This
keeps the queue lock out of the per-task hot path while retaining FIFO backlog
order. When the queue drains it either exits (parking disabled, stopped, or task
cap reached) or **parks** on its own channel, waiting to be woken by the next
`Push` or reclaimed by the background janitor after `maxIdle`. Parked workers are
excluded from the running count, so `Stop` can tell when all real work is done.

`PushBatch` admits each value through a short critical section so workers can
claim backlog work between values in a large producer batch. `TrySubmitBatch`
keeps its single critical section and all-or-nothing behavior.

`Submit` is the executor-oriented API. With `WithMaxPending(n)`, it rejects
without blocking once `n` running or queued submissions are outstanding;
legacy `Push` remains unbounded for compatibility.

## Tuning

- **`WithMaxIdle` is the key knob.** It only helps **bursty / intermittent**
  workloads with **deep call stacks**, where workers would otherwise be retired
  between bursts. Under sustained saturation the queue never drains, so stacks
  are reused regardless and parking buys nothing. Leave it at `0` for shallow or
  steadily-saturated workloads. A good value is on the order of your burst
  interval (commonly 50 ms – 1 s); too short and workers expire and rebuild
  between bursts, defeating the purpose.
- **`WithMaxJobs` works against stack reuse** — the replacement worker regrows
  its stack from 2 KB. Its only purpose is to keep workers from becoming
  effectively permanent under endless saturation. Either leave it off (let
  `WithMaxIdle` reclaim workers when load drops) or set it large (e.g. 1000+) so
  the one regrowth is amortized over many tasks.
- The Go runtime shrinks stacks during GC, so a worker parked far longer than a
  GC cycle may have to regrow anyway; keep `maxIdle` modest.
- Without `WithPanicHandler`, a panicking task propagates and crashes the
  process. Set it for untrusted tasks.

## Benchmarks

Measured on the local Mac release workstation (`darwin/arm64`, Apple M4 Pro,
12 logical CPUs, Go 1.27.1) with `GOMAXPROCS=12`, comparing two taskgo modes on
identical workloads.

- **taskgo-NoReuse** — `WithConcurrency(c)` (parking off).
- **taskgo-Reuse** — `WithConcurrency(c)` + `WithMaxIdle(time.Second)`.

Two task shapes are used: **Deep** (a ~256-frame call chain that forces stack
growth) and **Shallow** (`fib(10)`, stacks never grow). Pool teardown is
excluded from timing via `b.StopTimer()`. Numbers are average wall time per
operation; lower is better. Go's normal benchmark calibration is used, so each
row may use a different `b.N`.

### Bursty load (each batch = concurrency, queue drains between batches), `concurrency = 48`

| Workload | taskgo-Reuse | taskgo-NoReuse |
|---|---:|---:|
| **Deep** | **77.226 ms** | 294.011 ms |
| Shallow | **39.416 ms** | 42.943 ms |

For deep-stack bursts, reuse is **3.7× faster** than taskgo without parking.
Allocations drop from about 3.95 MB to 2.36 MB per operation.

### Sustained saturation (one large batch, queue stays full), `concurrency = 48`

| Workload | taskgo-Reuse | taskgo-NoReuse |
|---|---:|---:|
| Deep | 57.366 ms | **41.440 ms** |
| Shallow | **36.902 ms** | 38.709 ms |

The worker batch prefetch keeps taskgo competitive under saturation. On this
host, parking reduces allocations but is slower for deep saturated work; the
fastest engine varies with task shape and host scheduling load.

### High concurrency limit `WithConcurrency(10000)`, varying load

Deep tasks, average ms per op:

| Load | taskgo-Reuse | taskgo-NoReuse |
|---|---:|---:|
| 1,000 | 0.560 ms | **0.413 ms** |
| 10,000 | 6.607 ms | **4.081 ms** |
| 100,000 | 70.221 ms | **39.491 ms** |
| 500,000 | 353.594 ms | **210.240 ms** |

Shallow tasks, average ms per op:

| Load | taskgo-Reuse | taskgo-NoReuse |
|---|---:|---:|
| 1,000 | **0.379 ms** | 0.413 ms |
| 10,000 | 4.080 ms | **3.791 ms** |
| 100,000 | 42.071 ms | **39.677 ms** |
| 500,000 | **190.113 ms** | 190.596 ms |

At a very high concurrency cap with deep tasks, taskgo-NoReuse is faster on
this host at 500K, while taskgo-Reuse uses about half the allocations. At large
shallow loads, the two modes are nearly equal and Reuse remains more memory
efficient.

### Typed task path

The typed path binds the task function once and avoids a closure per value.
Average wall time per operation on the same Mac host:

| Scenario | Task-Reuse | Task-NoReuse |
|---|---:|---:|
| Burst, Deep | **86.626 ms** | 287.508 ms |
| Burst, Shallow | **46.749 ms** | 53.461 ms |
| Saturated, Deep | 59.521 ms | **46.596 ms** |
| Saturated, Shallow | 43.995 ms | **35.113 ms** |

Typed high-concurrency Deep loads are `0.749/0.519 ms` at 1K,
`6.729/4.833 ms` at 10K, `68.402/46.586 ms` at 100K, and
`343.966/231.952 ms` at 500K for Reuse/NoReuse respectively.

### Submission and tail latency

The local release gate uses 24 producers, 10K in-flight tasks, a 25 us CPU task,
`GOMAXPROCS=12`, and 2M total tasks. Latency is measured from submission to
task start, excluding task execution:

| Submission | P99 submit-to-start | P99 submit-to-complete |
|---|---:|---:|
| batch32 | **331.85 ms** | **332.46 ms** |
| batch1 | 27.06 ms | 27.13 ms |

The Mac result is CPU-saturated because this test intentionally drives 24
producers and 10K in-flight busy-wait tasks on 12 logical CPUs.

The slow-task isolation gate also covers the soft worker target boundary. With
23 blocked workers, fast-task P99 is `0.08 ms`; with all 24 initial workers
blocked for 50 ms, the independent stall monitor creates overflow workers and
fast-task P99 is `1.23 ms` instead of waiting for the first slow task. The same
Mac run measured `2.05/1.59/0.02 ms` at 256/1,000/5,000 blocked workers.

Reproduce the release gate on this Mac with:

```sh
GOMAXPROCS=12 TASKGO_RUN_TAIL_REPRO=1 TASKGO_TAIL_TOTAL=2000000 \
go test -run '^TestTailLatencyReproduction$' -count=1 -v

GOMAXPROCS=12 TASKGO_RUN_STALL_REPRO=1 \
go test -run '^TestStallLatencyReproduction$' -count=1 -v
```

### Takeaways

- **Deep-stack bursty load → enable `WithMaxIdle`.** The Mac results show
  3.7x gains over taskgo-NoReuse in burst mode.
- **Shallow high-volume load → measure both settings.** Reuse reduces
  allocations and is competitive on this host; the fastest engine depends on
  load shape.
- **Batch32 submission is now bounded by worker waves rather than lock convoy.**
  Worker prefetch and short batch critical sections bring the release P99 to
  about 4 ms without adding a public mode or changing the stack reuse options.
- **Blocked workers do not permanently pin the soft target.** A backlog stall
  monitor grows the internal worker target in backlog-aware steps up to the
  hard `WithConcurrency` limit, without requiring task completion to trigger
  growth.

The benchmark cases live in their own module under `benchmarks/`. Reproduce the
taskgo modes with:

```
cd benchmarks
GOMAXPROCS=12 go test -run '^$' -bench 'Benchmark(Burst|Saturated)/.*/taskgo-(NoReuse|Reuse)' -benchmem
GOMAXPROCS=12 go test -run '^$' -bench 'BenchmarkHighConcurrency/.*/taskgo-(NoReuse|Reuse)' -benchmem
GOMAXPROCS=12 go test -run '^$' -bench 'BenchmarkTyped(Burst|Saturated)/.*/taskgo-Task-(NoReuse|Reuse)' -benchmem
GOMAXPROCS=12 go test -run '^$' -bench 'BenchmarkTypedHighConcurrency/.*/taskgo-Task-(NoReuse|Reuse)' -benchmem
GOMAXPROCS=12 go test -run '^$' -bench 'BenchmarkConcurrentProducers/(Queue|Task\[int\])/(NoReuse|Reuse)' -benchmem
```

The typed-versus-closure submission benchmark is part of the main module:

```sh
GOMAXPROCS=12 go test -run '^$' -bench 'BenchmarkPush(Closure|Typed)$' -benchmem
```

On this Mac, the typed push benchmark reports 0 allocations and
`330.8 ns/op`, while a closure capturing the same value reports 1 allocation,
`343.9 ns/op`, and 24 bytes.
The `BenchmarkTyped*` cases in `benchmarks/` compare `Task[int]` with the
legacy `Queue` under the same burst, saturation, and high-concurrency loads.

The real-time sawtooth, long-tail, and max-idle experiments depend on OS timer
resolution, CPU capacity, scheduler load, and race instrumentation. They are
opt-in so ordinary cross-platform unit tests do not assert statistical ordering
between different machines:

```sh
TASKGO_RUN_EXPERIMENTS=1 go test -v -run 'TestSawtoothMitigations|TestLongTailMitigations|TestHeavyTailCap|TestMaxIdleSweep'
```

`TestSawtoothJitter` uses a simulated clock and remains part of the default
deterministic test suite.

## License

Apache License 2.0. See [LICENSE](LICENSE).
