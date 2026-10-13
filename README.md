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
- Optional keyed ingress (`NewTaskKeyed`): a task lands on the
  FIFO of `key(task) % shards`, so the tasks of one key stay with one worker,
  and so on one CPU, while a worker serves their shard; a shard left without
  a worker is handed over, trading strict per-key order for progress behind a
  blocked task.
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
func NewTaskKeyed[T any](fn func(T), key func(T) uint32, shards int, opts ...Option) *Task[T]

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
func WithMaxBatchSize(n int) Option          // max tasks claimed together (default 8; 1 disables batching/rescue)
func WithMaxIdle(d time.Duration) Option     // park idle workers for d (default 0 = off)
func WithMaxJobs(n int) Option               // recycle a worker after n tasks (default 0 = off)
func WithMaxPending(n int) Option            // bound outstanding Submit jobs (default 0 = off)
func WithTimeout(d time.Duration) Option     // how long Stop waits (default 30s)
func WithPanicHandler(fn func(v any)) Option // recover task panics
```

`NewTaskKeyed` (new in v1.7.0, see "How it works") spreads a keyed queue's
ingress over one FIFO per shard. A worker serves the shard it served last and
takes over other shards once its own runs dry; a shard left without a worker,
because its worker blocks in a task or because there are more shards than
workers, is handed over after a few watches. `shards <= 1` or a nil key makes
the plain single-FIFO queue `NewTask` makes. Order holds only within a shard,
and only while one worker serves it: after a handover, a shard's tasks can
run on a second worker, concurrently with the blocked task that triggered the
handover. `key` runs once per value before the queue admits it — a stopped
queue runs no key at all.

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

Workers usually take one task at a time from the shared queue. When they
collide on the queue head they grow batches up to `WithMaxBatchSize` (default
eight). Setting the limit to one disables batching and stalled-batch rescue;
the monitor still compensates for blocked workers. Unstarted
batch tasks remain reclaimable: after five monitor ticks without progress, the
monitor returns them to the shared queue so other workers can execute them.
The original worker then handles 64 tasks singly before contention can grow
its batches again. Task claims and progress share one atomic state, with no
per-task clock, timer or allocation. The monitor samples every millisecond,
so rescue typically starts after roughly 6–8 ms without progress, allowing
short task and scheduler pauses to settle. Blocked-worker compensation keeps
its separate, shorter observation window. Scheduler delays can extend the
observation window, so it does not impose a hard 10 ms bound. A
concurrency limit of one retains serial execution order.

When the queue is empty a worker parks on its own channel
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

A keyed queue (`NewTaskKeyed`) adds one FIFO per shard and sends each value to
the FIFO of `key(value) % shards`. A worker serves the shard it served last,
taking its tasks in submission order, and moves to the shard of a rotating
scan only once its own runs dry, so the tasks of one key normally keep
running on one worker, and so on one CPU. A shard can be left without a
worker - its worker blocks in a task, or there are more shards than workers
and none of theirs runs dry - so the monitor watches for it: a shard whose
head has not moved for a few ticks while it holds tasks, and the shard of a
worker stuck in one task, are marked and taken over by the next worker that
looks. Workers keep watch as they take tasks, so the handover stays timely
while the monitor's own ticks are delayed. Order holds only within a shard,
and only while one worker serves it: after a handover, tasks claimed from
that shard in submission order can run on a second worker, concurrently with
the blocked task that triggered the handover. Not starving the tasks behind a
blocked one is the stronger promise here; strict per-key serialization is
not. Tasks of different shards are not ordered, and a batch is split over the
shards its values pick.

## Tuning

- **`WithMaxBatchSize` bounds worker claims**, independently of submission
  batch sizes. The default is 8; use `WithMaxBatchSize(1)` to always take one
  task and disable stalled-batch rescue, or another positive limit such as
  4 or 16 to keep adaptive batching within that limit. Larger buffers are
  created once per worker and reused. Values outside 1 through 2^31-1 use
  the default.
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

## License

Apache License 2.0. See [LICENSE](LICENSE).
