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

func New(opts ...Option) *Queue

func (q *Queue) Push(job Job)               // submit a task (nil is ignored)
func (q *Queue) Submit(job Job) bool        // non-blocking bounded submit
func (q *Queue) Len() int                   // tasks queued but not yet started
func (q *Queue) Stop(ctx context.Context) error // drain and shut down

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

## How it works

`Push` follows three paths, all under one mutex:

1. **A parked worker exists** → wake it (LIFO, so the hottest stack is reused).
2. **No parked worker and below the concurrency limit** → start a new worker.
3. **At the limit** → enqueue the task; a looping worker will pick it up.

A worker runs its task, then while the queue is non-empty it pulls the next task
directly (the hot path — one mutex, no channel). When the queue drains it either
exits (parking disabled, stopped, or task cap reached) or **parks** on its own
channel, waiting to be woken by the next `Push` or reclaimed by the background
janitor after `maxIdle`. Parked workers are excluded from the running count, so
`Stop` can tell when all real work is done.

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

Measured on Apple M4 Pro (`darwin/arm64`, Go 1.26), comparing three engines on
identical workloads:

- **taskgo-NoReuse** — `WithConcurrency(c)` (parking off).
- **taskgo-Reuse** — `WithConcurrency(c)` + `WithMaxIdle(time.Second)`.
- **lxzan** — [`github.com/lxzan/concurrency`](https://github.com/lxzan/concurrency)
  `queues.New(WithConcurrency(c))`, a comparable single-queue pool that starts a
  goroutine on demand and retires it when the queue drains.

Two task shapes are used: **Deep** (a ~256-frame call chain that forces stack
growth) and **Shallow** (`fib(10)`, stacks never grow). Pool teardown is
excluded from timing via `b.StopTimer()`. Numbers are average wall time per
op; lower is better.

### Bursty load (each batch = concurrency, queue drains between batches), `concurrency = 48`

| Workload | taskgo-Reuse | taskgo-NoReuse | lxzan |
|---|---|---|---|
| **Deep** | **81 ms** | 462 ms | 457 ms |
| Shallow | 40 ms | 35 ms | 28 ms |

For deep-stack bursts, reuse is **~5.7× faster** than either pool that retires
its workers, and uses ~half the allocations. This is the workload `taskgo` is
built for.

### Sustained saturation (one large batch, queue stays full), `concurrency = 48`

| Workload | taskgo-Reuse | taskgo-NoReuse | lxzan |
|---|---|---|---|
| Deep | 61 ms | 59 ms | 61 ms |
| Shallow | 53 ms | 48 ms | 42 ms |

When the queue never drains, stacks are reused by everyone, so the three engines
are roughly on par. Reuse still halves allocations (parked workers are pooled).

### High concurrency limit `WithConcurrency(10000)`, varying load

Deep tasks, average ms per op:

| Load | taskgo-Reuse | taskgo-NoReuse | lxzan |
|---|---|---|---|
| 1,000 | **3.1 ms** | 5.3 ms | 8.2 ms |
| 10,000 | **37.5 ms** | 45.8 ms | 49.3 ms |
| 100,000 | **100.9 ms** | 350.8 ms | 360.9 ms |
| 500,000 | **431.3 ms** | 1692.1 ms | 1732.4 ms |

Shallow tasks, average ms per op:

| Load | taskgo-Reuse | taskgo-NoReuse | lxzan |
|---|---|---|---|
| 1,000 | 0.6 ms | 0.7 ms | 1.0 ms |
| 10,000 | 5.9 ms | 7.0 ms | 7.9 ms |
| 100,000 | 57.9 ms | 51.3 ms | 44.8 ms |
| 500,000 | 303.1 ms | 254.2 ms | 242.9 ms |

At a very high concurrency cap with deep tasks, reuse scales dramatically better
(up to ~4× at 500k tasks), because every retired worker would otherwise regrow
its stack. For shallow tasks the picture inverts at large loads: reuse pays for
channel wake-ups it cannot amortize, so it trails the lock-only engines — though
it consistently allocates about half as much memory.

### Takeaways

- **Deep-stack + bursty/under-saturated load → enable `WithMaxIdle`.** Large,
  sometimes multiple-× speedups, plus lower allocations.
- **Shallow tasks or steady saturation → leave parking off.** `taskgo-NoReuse`
  and `lxzan` are lock-only and slightly faster on pure dispatch; reuse adds
  channel overhead it cannot repay when stacks never grow.
- Reuse is **opt-in and zero-risk by default**: with `maxIdle = 0` the hot path
  is identical to the no-reuse engine.

The cross-library comparison lives in its own module under `benchmarks/`, so the
`lxzan/concurrency` dependency never enters the main module — importing `taskgo`
pulls in nothing but the standard library. Reproduce with:

```
cd benchmarks
go test -run '^$' -bench 'BenchmarkBurst|BenchmarkSaturated' -benchmem
go test -run '^$' -bench 'BenchmarkHighConcurrency' -benchmem
```

## License

Apache License 2.0. See [LICENSE](LICENSE).
