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
- Low-latency dispatch: tasks wait in a lock-free FIFO queue, so submitting
  never takes a lock or blocks while workers are busy, and about `GOMAXPROCS`
  workers run until tasks block, instead of flooding the Go scheduler.
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

Tasks wait in a lock-free FIFO queue: a chain of fixed-size rings that doubles
when full and shrinks back once a burst has drained. Submitting publishes the
task and then checks whether enough workers are running:

1. **Enough workers are running** (the normal case under load) → nothing else
   to do. The submitter takes no lock and never blocks, so an event loop that
   hands work to the pool is never parked behind it.
2. **A running slot is free and a worker is parked** → wake it (LIFO, so the
   hottest stack is reused).
3. **A running slot is free and nobody is parked** → start a new worker, up to
   `WithConcurrency`.

Workers take one task at a time from the shared queue, so a worker that loses
its CPU never holds tasks another worker could run. (Only when workers collide
on the queue head, which takes tasks shorter than a CAS round trip, do they
claim small batches.) When the queue is empty a worker parks on its own channel
for up to `maxIdle`, or exits when parking is disabled, the queue is stopped,
or it has handled `maxJobs` tasks. Parked workers are excluded from the running
count, so `Stop` can tell when all real work is done.

**The running worker count follows the CPU, not the concurrency limit.** About
one worker per P (`GOMAXPROCS`) runs tasks. Once every P is busy, more runnable
workers add no throughput: they only lengthen the Go scheduler's run queues,
and every goroutine in the process, including the ones submitting tasks, then
waits longer for a CPU. A monitor goroutine, running only while tasks wait for
a worker, adds workers when they help, that is when running workers block:

- no task started during a 1 ms tick while workers are stuck in one: the target
  doubles (tasks queued behind blocking tasks need that many workers);
- tasks wait while the scheduler reports idle Ps: the target grows (Go 1.26+
  reads the scheduler's runnable and running goroutine counts from
  `runtime/metrics`; older runtimes use the monitor's own wake-up latency);
- tasks arrive more than twice as fast as they start, with the backlog
  growing: other goroutines are taking the CPU the workers need, and more
  workers claim a larger share of it. Past 65,536 queued tasks, submitters also
  yield their P after each submission until the backlog halves.

The extra workers go again when the run queues get long or the blocked tasks
finish. `WithConcurrency` stays the hard cap on live workers.

`PushBatch`, `SubmitBatch` and `TrySubmitBatch` enqueue their values as one
ordered run, claiming consecutive queue slots with a single CAS where they fit.
`TrySubmitBatch` keeps its all-or-nothing admission.

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
- **`WithConcurrency` is a ceiling for blocked work, not a worker count.** CPU
  bound tasks run on about `GOMAXPROCS` workers whatever the limit; tasks that
  block (I/O, locks, sleeps) get more workers automatically, up to the limit.
  Size it for the most tasks you expect to be blocked at once.
- Without `WithPanicHandler`, a panicking task propagates and crashes the
  process. Set it for untrusted tasks.

## Benchmarks

Measured on a Mac (Apple M4 Pro, 12 logical CPUs, Go 1.27.1) with
`GOMAXPROCS=12`, comparing two taskgo modes on identical workloads:

- **NoReuse**: `WithConcurrency(c)` (parking off).
- **Reuse**: `WithConcurrency(c)` + `WithMaxIdle(time.Second)`.

Two task shapes are used: **Deep** (a ~256-frame call chain that forces stack
growth) and **Shallow** (`fib(10)`, stacks never grow). Pool teardown is
excluded from timing. Numbers are average wall time per operation, the median
of three runs; lower is better.

### Bursty load, `WithConcurrency(48)`

Each operation submits 2,000 batches of 48 tasks and waits for each batch, so
the queue drains and the workers go idle between batches.

| Workload | Reuse | NoReuse |
|---|---:|---:|
| Deep | **63.4 ms** | 125.6 ms |
| Shallow | **30.5 ms** | 38.8 ms |

Parked workers keep their grown stacks, so deep-stack bursts run **2× faster**
with Reuse, and shallow bursts take 21% less time. Reuse also allocates less:
2.34 MB per operation, against 3.10 MB (Deep) and 3.42 MB (Shallow) without it.

### Sustained saturation, `WithConcurrency(48)`

Each operation submits 100,000 tasks at once, so the queue stays full and the
workers run tasks back to back.

| Workload | Reuse | NoReuse |
|---|---:|---:|
| Deep | 22.1 ms | 22.4 ms |
| Shallow | 7.30 ms | 7.36 ms |

The queue drains only at the end of each operation, so workers in both modes
keep their stacks for almost every task, and the two perform the same.

### High concurrency limit, `WithConcurrency(10000)`

Each operation submits the given number of tasks at once and waits for them.

| Load | Deep, Reuse | Deep, NoReuse | Shallow, Reuse | Shallow, NoReuse |
|---|---:|---:|---:|---:|
| 1,000 | **0.274 ms** | 0.296 ms | 0.101 ms | 0.102 ms |
| 10,000 | 2.27 ms | 2.29 ms | 0.774 ms | 0.783 ms |
| 100,000 | 22.1 ms | 22.0 ms | 7.39 ms | 7.39 ms |
| 500,000 | 109.1 ms | 109.5 ms | 36.9 ms | 36.6 ms |

About `GOMAXPROCS` workers run tasks whatever the limit, so NoReuse has few
stacks to regrow per operation, and the two modes stay within 8% of each other
at every load.

### Typed task path

The same scenarios with `Task[int]`, whose task function is bound once. Each
task signals completion on a shared channel rather than a `sync.WaitGroup`, so
these times include that channel traffic and are not comparable with the
tables above.

| Scenario | Task-Reuse | Task-NoReuse |
|---|---:|---:|
| Burst, Deep | **77.5 ms** | 125.8 ms |
| Burst, Shallow | **32.4 ms** | 37.7 ms |
| Saturated, Deep | 41.3 ms | 40.5 ms |
| Saturated, Shallow | 21.4 ms | 21.4 ms |

With `WithConcurrency(10000)` the two modes are within 5% of each other at
every load: Deep tasks take 0.44/0.46 ms at 1K, 4.11/4.12 ms at 10K,
41.2/41.1 ms at 100K and 204.5/203.8 ms at 500K for Reuse/NoReuse.

### Concurrent producers

16 producers push shallow tasks into one `WithConcurrency(48)` queue as fast as
they can. Time per task:

| Path | Reuse | NoReuse |
|---|---:|---:|
| `Queue` (a closure per task) | 211 ns | **174 ns** |
| `Task[int]` | 137 ns | **104 ns** |

Reuse costs 22–32% more per task here: the workers keep catching up with the
producers and going idle, and shallow tasks leave no grown stacks to reuse.
This is the shallow workload the Tuning section suggests running without
`WithMaxIdle`.

### Latency gates

The tail gate drives 24 producers, 10,000 tasks in flight and a 25 µs CPU task
through `WithConcurrency(10000)`, 2M tasks in total (`GOMAXPROCS=12`). The CPU
is saturated, so by Little's law the mean wait is about 20 ms; the gate checks
how evenly that wait is spread. Latency is measured from submission to task
start, median of three runs:

| Submission | p50 | p99 | p99.9 | max |
|---|---:|---:|---:|---:|
| batch32 | 18.8 ms | 48.1 ms | 137.4 ms | 408.3 ms |
| batch1 | 20.1 ms | 27.4 ms | 28.7 ms | 54.6 ms |

The batch32 maximum varied between 322 and 640 ms from run to run.

The stall gates check that blocked workers do not hold up other tasks. With
1,000 fast tasks submitted while 11, 12 (the base worker target at
`GOMAXPROCS=12`), 256, 1,000 or 5,000 workers are blocked, fast-task P99 is
0.02, 2.6, 2.2, 2.3 and 1.0 ms. When the fast tasks queue behind 256, 1,000 or
5,000 blocking tasks that have not started yet, they need that many workers
first, and their P99 is 7.1, 11.8 and 17.9 ms.

Reproduce with:

```sh
GOMAXPROCS=12 TASKGO_RUN_TAIL_REPRO=1 TASKGO_TAIL_TOTAL=2000000 \
go test -run '^TestTailLatencyReproduction$' -count=1 -v

GOMAXPROCS=12 TASKGO_RUN_STALL_REPRO=1 TASKGO_RUN_STALL_BURST_REPRO=1 \
go test -run '^TestStall(Burst)?LatencyReproduction$' -count=1 -v
```

The scheduling benchmarks live in their own module under `benchmarks/`:

```
cd benchmarks
GOMAXPROCS=12 go test -run '^$' -bench 'Benchmark(Burst|Saturated)/.*/taskgo-(NoReuse|Reuse)$' -benchmem -count=3
GOMAXPROCS=12 go test -run '^$' -bench 'BenchmarkHighConcurrency/.*/.*/taskgo-(NoReuse|Reuse)$' -benchmem -count=3
GOMAXPROCS=12 go test -run '^$' -bench 'BenchmarkTyped(Burst|Saturated)/.*/taskgo-Task-(NoReuse|Reuse)$' -benchmem -count=3
GOMAXPROCS=12 go test -run '^$' -bench 'BenchmarkTypedHighConcurrency/.*/.*/taskgo-Task-(NoReuse|Reuse)$' -benchmem -count=3
GOMAXPROCS=12 go test -run '^$' -bench 'BenchmarkConcurrentProducers' -benchmem -count=3
```

The typed-versus-closure submission benchmark is part of the main module:

```sh
GOMAXPROCS=12 go test -run '^$' -bench 'BenchmarkPush(Closure|Typed)$' -benchmem
```

`BenchmarkPushTyped` reports 0 allocations at 240 ns/op; `BenchmarkPushClosure`,
which captures the same value in a closure, reports one 24-byte allocation at
255 ns/op.

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
