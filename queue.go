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

// Package taskgo provides a concurrency-limited task queue whose workers are
// reused for a short while instead of exiting after every task.
//
// When the queue drains, a worker may park for up to maxIdle (see WithMaxIdle)
// rather than exiting immediately. Subsequent tasks reuse that worker's already
// grown goroutine stack, which avoids repeatedly paying for stack growth
// (morestack) when running tasks with deep call chains. Parking is not the same
// as keeping workers resident forever: a worker is reclaimed once it has been
// idle longer than maxIdle, or after it has handled maxJobs tasks.
package taskgo

import (
	"context"
	"errors"
	"math/bits"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/petermattis/goid"
)

// Job is a unit of work with no arguments and no return value.
type Job func()

// stopCheckInterval is how often Stop polls for completion while draining.
const stopCheckInterval = 100 * time.Millisecond

// Task is a typed task queue. The function is bound when the queue is created,
// so Push only stores a value and does not allocate a per-task closure.
//
// The zero value is not usable; create one with NewTask.
type Task[T any] struct {
	*taskQueue[T]
}

// NewTask creates a typed task queue that invokes fn for every value submitted
// with Push or Submit. fn must be non-nil.
func NewTask[T any](fn func(T), opts ...Option) *Task[T] {
	if fn == nil {
		panic("taskgo: nil task function")
	}
	return &Task[T]{taskQueue: newTaskQueue(opts, fn)}
}

// Push submits value to the bound typed task function.
func (q *Task[T]) Push(value T) {
	q.taskQueue.push(taskItem[T]{value: value})
}

// PushBatch submits values and returns the number accepted. Values are stored
// directly in the queue; no per-value closure is created. A concurrent Stop
// can cause the result to be less than len(values).
func (q *Task[T]) PushBatch(values []T) int {
	return q.taskQueue.pushValues(values, false)
}

// Submit admits a typed task without blocking. It returns false when the queue
// has been stopped or the optional WithMaxPending limit is full.
func (q *Task[T]) Submit(value T) bool {
	return q.taskQueue.submit(value)
}

// SubmitBatch submits as many values as the pending limit allows and returns
// the number accepted. When the queue is bounded, accepted values are the
// prefix of values that fits in the currently available pending capacity.
func (q *Task[T]) SubmitBatch(values []T) int {
	accepted, _ := q.taskQueue.submitBatch(values, false)
	return accepted
}

// TrySubmitBatch atomically submits the entire batch. It returns false and
// accepts no values when the queue is stopped or the pending limit cannot hold
// the complete batch.
func (q *Task[T]) TrySubmitBatch(values []T) bool {
	_, ok := q.taskQueue.submitBatch(values, true)
	return ok
}

type taskItem[T any] struct {
	value     T
	submitted bool // admitted by Submit; releases a pending slot when done
}

// watchEvery is how many batch attempts a keyed worker makes between looks at
// whether a watch of the shards is due. Workers take the watch over when the
// monitor is late: while as many busy workers as there are Ps keep every P,
// the monitor's ticks wait for one, and 48 workers on 48 Ps left it 45 ticks
// a second, so a shard without a worker waited over 100ms to be handed over.
const watchEvery = 64

// orphanStale is how long a keyed shard may hold tasks without any worker
// taking one before it is handed over. Shards go unattended for a while all
// the time: a worker serves its own shard until it runs dry and only then
// takes over another, so in a burst over more shards than workers the others
// wait their turn. That wait is what keeps a key's tasks on one CPU, and the
// threshold sits above it: on a saturated echo server with 48 shards and 24
// workers, handing shards over after 5ms cost 6% of its throughput, after
// 50ms nothing measurable.
const orphanStale = 50 * time.Millisecond

// watchCredit is the longest time since the last watch that a watch adds to a
// shard's wait. A longer gap is a pause in the watching, with the queue idle
// and its monitor gone, and counts as one monitorTick, as a watch did before
// waits were timed. It sits above the 15.6ms steps of the Windows clock,
// which pace the watches there no closer than one step apart.
const watchCredit = 20 * time.Millisecond

// shardBatch is the stack room shardIndexes has for a batch before it
// allocates.
const shardBatch = 64

var monoEpoch = time.Now()

const (
	// monitorTick is the period of the monitor that runs while tasks are
	// waiting for a worker.
	monitorTick = time.Millisecond
	// stuckTicks is how many consecutive ticks a worker must spend in one
	// task before the monitor counts it as stuck.
	stuckTicks = 2
	// Moving a held batch is more disruptive than adding worker capacity.
	// Wait longer so short task or scheduler pauses do not trigger a rescue.
	batchRescueTicks = 5
	// monitorIdleTicks is how many consecutive ticks without a backlog the
	// monitor waits before it stops.
	monitorIdleTicks = 50
	// ringKeep is the largest queue ring kept once a backlog has drained.
	ringKeep = 1024
	// behindTicks is how many consecutive ticks tasks must arrive at more than
	// twice the rate workers start them, with the backlog growing, before the
	// monitor throttles the submitters (see throttleBacklog).
	behindTicks = 3
	// throttleBacklog is the backlog above which a pool that is falling behind
	// makes submitters yield their P after every submission, until the backlog
	// halves. Without it, producers that never block could grow the queue
	// without bound while the workers they compete with for CPU fall behind.
	throttleBacklog = 1 << 16
)

// producerStripe counts the submitters of one stripe between their stop check
// and the end of their dispatch. Submitters on different Ps count in
// different stripes, so that they do not contend for one cache line.
type producerStripe struct {
	n int64 // atomic
	_ [cacheLinePad - 8]byte
}

// maxProducerStripes bounds the stripes of a queue sharded over many Ps.
const maxProducerStripes = 64

// turnStripe counts one submitter stripe's pushes, on a line of its own so
// that submitters on different Ps do not pass it between them.
type turnStripe struct {
	n uint32 // atomic
	_ [cacheLinePad - 4]byte
}

// worker is one worker goroutine's control block. The fields written for
// every task are padded so that workers never share those cache lines.
type worker[T any] struct {
	_ [cacheLinePad]byte
	// state packs the task-start sequence in the high word and unstarted
	// batch tasks (or a rescue marker) in the low word. One atomic operation
	// both claims a task and records progress; the single-task path uses add.
	state uint64
	_     [cacheLinePad - 8]byte

	// The owner publishes batchSize and batch through state. A rescuer must
	// claim state before touching unstarted entries; the owner cannot reuse
	// the buffer until the rescuer publishes batchRescued.
	batchSize   int
	batch       []taskItem[T]
	inlineBatch [defaultMaxBatchSize]taskItem[T]

	ch chan bool // wake-up (false) or exit (true) while parked; buffered 1

	// Keyed queues only, and owned by the worker goroutine, except that the
	// monitor reads ring, atomically, to hand a stuck worker's shard over.
	ring      int32 // atomic
	pops      uint32
	scans     uint32
	seed      uint32
	claimTick uint32

	// lastEnd is when the worker last finished a task, from nanotime; a
	// worker that just finished one spins for its next instead of parking.
	lastEnd int64

	// Guarded by taskQueue.mu.
	lastUsed   int64  // q.nowNanos when it parked
	parked     bool   // on its channel: in idle, or dismissed and not yet retired
	goid       int64  // atomic: the worker goroutine's id, for Stop to recognize its caller
	index      int    // position in taskQueue.workers
	lastSeq    uint32 // seq seen at the monitor's previous tick
	stuckTicks int
	stuckHead  uint64 // keyed: dequeues of the worker's shard at the previous stuck tick
}

// initBatch keeps the default-size buffer inside the control block. Larger
// configured batches allocate once when the worker is created; pool reuse
// preserves that buffer.
func (w *worker[T]) initBatch(n int) {
	if n <= len(w.inlineBatch) {
		w.batch = w.inlineBatch[:n:n]
	} else {
		w.batch = make([]taskItem[T], n)
	}
}

// taskQueue is the scheduler shared by Queue and Task. run is bound once at
// construction and is called directly with each queued T.
//
// Tasks wait in a lock-free FIFO queue. A submitter publishes its task and
// then checks whether the running workers already reach the target; while
// they do, which is always the case under load, submitting takes no lock and
// never blocks. Worker transitions (start, park, wake, exit) are serialized
// by mu. A worker that finds the queue empty parks on its own channel, pushed
// on a LIFO stack so the most recently used stack is reused first.
//
// The number of running workers is held near a soft target of one per P
// rather than the concurrency limit. Once every P is busy, more runnable
// workers add no throughput; they only lengthen the Go scheduler's run queues
// and delay every goroutine, including the ones submitting tasks. A monitor
// raises the target while tasks wait and Ps sit idle, that is while workers
// block, up to the concurrency limit, which remains the hard cap on live
// workers.
type taskQueue[T any] struct {
	// The control words of one task's round trip share a line: a submitter
	// reads running and target and may write running when it wakes a worker,
	// the woken worker releases running when it parks, and the monitor moves
	// target now and then. They travel between cores together anyway, so
	// one line moves per task rather than one per word.
	running   int64 // atomic: workers not parked; written under mu
	target    int64 // atomic: soft limit on running workers
	seekers   int32 // atomic: workers woken or started and yet to reach the queue, or spinning on it
	spinners  int32 // atomic: the spinning ones
	monitorOn uint32
	throttle  uint32 // atomic: submitters yield after submitting; see throttleBacklog
	_         [cacheLinePad - 32]byte
	closed    int64 // atomic: set once Stop refuses new submissions
	_         [cacheLinePad - 8]byte
	// base is the target without the monitor's compensation; never written
	// after construction.
	base      int64
	producers []producerStripe
	pushTurn  []turnStripe
	pending   int64 // atomic: outstanding Submit tasks
	_         [cacheLinePad - 8]byte
	// contended is set once producers have lost races claiming slots in the
	// single queue they share, which more than one producer at work does. An
	// unkeyed queue then spreads its producers over the shards (see shardAt),
	// for good: until then every submitter uses the single queue, so that a
	// lone submitter - and a quiet one - pays nothing for the shards, and its
	// tasks sit together where every worker finds them.
	contended uint32 // atomic
	_         [cacheLinePad - 8]byte
	// holders counts the workers holding the unstarted tail of a batch. The
	// monitor scans the workers for stalled batches only while it is not zero.
	holders int64 // atomic
	_       [cacheLinePad - 8]byte
	// Keyed queues only. orphans counts the bits set in orphaned, one per
	// shard found unattended; every pop reads it. ticks counts the watches,
	// all of them, and paces the claims; monTicks counts the monitor's own
	// iterations, which pace the stuck detection while idle workers watch
	// too. watching is held by the goroutine running one watch, and watchNext
	// is when the next one is due.
	orphans   int32  // atomic
	ticks     uint32 // atomic: every watch's pace, for the orphan claims
	monTicks  uint32 // atomic: the monitor's own pace, for the stuck detection
	watching  uint32 // atomic
	_         uint32
	watchNext int64 // atomic: monoNow nanoseconds
	_         [cacheLinePad - 24]byte

	queue lfQueue[taskItem[T]] // the ingress when shards <= 1
	// Sharded ingress: one FIFO per shard. A keyed queue (see NewTaskKeyed)
	// picks the shard by key and watches for shards left without a worker; an
	// unkeyed queue with enough workers picks the shard of the submitter's P
	// (see ingressShards), and its workers take from any shard.
	keyed     bool
	rings     []lfQueue[taskItem[T]]
	occupied  []uint64 // atomic bits: shards that may hold tasks; see markOccupied
	orphaned  []uint64 // atomic bits: unattended shards
	heads     []uint64 // guarded by watching: dequeues at the last watch
	waited    []int64  // guarded by watching: nanoseconds held without progress
	lastWatch int64    // guarded by watching: monoNow at the last watch
	shards    int
	keyFn     func(T) uint32

	mu             sync.Mutex
	live           int          // live workers, at most opt.concurrency; counted until they exit
	spawned        uint32       // workers ever started, seeding keyed scans
	idle           []*worker[T] // parked workers, most recently parked last
	workers        []*worker[T] // every live worker, scanned by the monitor
	stopped        bool
	janitorRunning bool
	pool           sync.Pool

	// stopping is closed once the first Stop has refused new submissions and
	// dismissed the parked workers; every Stop waits for it before it waits
	// for the queue to drain, and the janitor exits on it. selfStops counts
	// the Stop calls waiting from inside a task of this queue after handing
	// off their batches. Only task callers exclude those workers.
	stopping  chan struct{}
	selfStops int64 // atomic

	opt *options
	run func(T)

	onSpawnForTest func()
}

func newTaskQueue[T any](opts []Option, run func(T)) *taskQueue[T] {
	return newShardedTaskQueue(opts, run, nil, 0)
}

// newShardedTaskQueue is newTaskQueue with its ingress spread over shards
// FIFOs by key, or a single FIFO when shards <= 1.
func newShardedTaskQueue[T any](opts []Option, run func(T), key func(T) uint32, shards int) *taskQueue[T] {
	o := newOptions(opts)
	q := &taskQueue[T]{opt: o, run: run, keyFn: key, shards: shards, stopping: make(chan struct{})}
	if key != nil && shards > 1 {
		q.keyed = true
	} else {
		q.keyFn, q.shards = nil, ingressShards(o.concurrency)
	}
	stripes := 1
	if q.shards > 1 {
		// A power of two, so that a submitter picks its stripe with a mask.
		stripes = 1
		for stripes < runtime.GOMAXPROCS(0) && stripes < maxProducerStripes {
			stripes <<= 1
		}
	}
	q.producers = make([]producerStripe, stripes)
	q.pushTurn = make([]turnStripe, stripes)
	base := int64(autoWorkerLimit(o.concurrency))
	q.base = base
	q.target = base
	q.initQueues(backlogCapacityHint(int(base)))
	return q
}

// workersPerShard is how many workers of the concurrency limit an unkeyed
// queue wants per ingress shard before it splits its FIFO.
const workersPerShard = 8

// ingressShards is how many FIFOs an unkeyed queue spreads its ingress over:
// one per P, so that submitters and workers on different Ps do not contend for
// one queue head and tail, but no more than a shard per workersPerShard
// workers the limit allows. A limit below 2*workersPerShard keeps the single
// FIFO, and with it the strict order WithConcurrency(1) relies on.
func ingressShards(concurrency int) int {
	shards := concurrency / workersPerShard
	if procs := runtime.GOMAXPROCS(0); shards > procs {
		shards = procs
	}
	if shards < 2 {
		return 0
	}
	return shards
}

// NewTaskKeyed creates a typed task queue whose ingress is spread over shards
// FIFOs keyed by key: a task lands on the FIFO of key(task) % shards. A worker
// keeps taking from the shard it served last and moves to another only when
// that one runs dry, so the tasks of one key stay with one worker - and so on
// one CPU - while a worker serves their shard, and workers that run out of
// work take over the shards still holding tasks.
//
// When no worker runs out of work, a shard can be left without one: its
// worker blocks in a task, or there are more shards than running workers.
// The monitor hands such a shard to another worker once it has held tasks
// for batchRescueTicks ticks in which no worker took one, the patience the
// held batch rescue has. A queued task therefore waits behind a blocked task
// about that long, a few milliseconds, where the single FIFO lets any other
// worker take it at once.
//
// Order holds only within a shard, and only while one worker serves it.
// Tasks of one key are claimed in submission order but may still overlap
// with more than one worker: after a handover, a task runs on another worker
// while the blocked task that triggered the handover is still in flight. And
// tasks of different shards are not ordered at all: unlike NewTask's queue,
// even WithConcurrency(1) runs them shard by shard rather than in submission
// order, and a batch is split over the shards its values pick.
//
// key must be cheap and return the same value for a value every time. It runs
// once per value, before the queue admits it: a key that panics leaves
// nothing admitted, a whole batch included. A stopped queue runs no key at
// all: Push, PushBatch and Submit reject before it, and the batch methods
// already did. Each shard's FIFO is allocated up front. shards <= 1, or a nil key, makes the plain single-FIFO queue NewTask
// makes. fn must be non-nil.
func NewTaskKeyed[T any](fn func(T), key func(T) uint32, shards int, opts ...Option) *Task[T] {
	if fn == nil {
		panic("taskgo: nil task function")
	}
	if key == nil || shards <= 1 {
		key, shards = nil, 0
	}
	return &Task[T]{taskQueue: newShardedTaskQueue(opts, fn, key, shards)}
}

// initQueues sizes the ingress queues: one FIFO when unkeyed, shards of them
// when a key function was given (see NewTaskKeyed).
func (q *taskQueue[T]) initQueues(hint int) {
	if q.shards > 1 {
		per := hint / q.shards
		if q.keyed {
			if per < 1024 {
				per = 1024
			}
		} else if per < minBacklogCapacity*4 {
			// Shards of an unkeyed queue follow the submitters' Ps, so most
			// of them stay small; each grows on its own when it fills.
			per = minBacklogCapacity * 4
		}
		q.rings = make([]lfQueue[taskItem[T]], q.shards)
		signal := func() { atomic.StoreUint32(&q.contended, 1) }
		for i := range q.rings {
			q.rings[i].init(per)
			q.rings[i].onContention = signal
		}
		if !q.keyed {
			// The ingress every producer shares until they contend for it.
			q.queue.init(hint)
			q.queue.onContention = signal
		}
		// Every push and every pop that runs a shard dry may write occupied,
		// so it gets cache lines of its own.
		words, pad := (q.shards+63)/64, cacheLinePad/8
		q.occupied = make([]uint64, pad+words+pad)[pad : pad+words]
		if q.keyed {
			q.orphaned = make([]uint64, (q.shards+63)/64)
			q.heads = make([]uint64, q.shards)
			q.waited = make([]int64, q.shards)
		}
		return
	}
	q.queue.init(hint)
}

// Len returns the number of tasks queued but not yet started.
func (q *taskQueue[T]) Len() int {
	q.mu.Lock()
	n := q.lenAny()
	for _, w := range q.workers {
		if held := int32(atomic.LoadUint64(&w.state)); held > 0 {
			n += int(held)
		}
	}
	q.mu.Unlock()
	return n
}

// ErrStopInTask is returned by a Stop called from a task when tasks remain
// that no worker can run before that task returns: every worker the
// concurrency limit allows is a task waiting in Stop. Those tasks still run,
// once the tasks that called Stop have returned.
var ErrStopInTask = errors.New("taskgo: Stop called from a task cannot wait for the tasks queued behind it")

// Stop shuts the queue down and waits for outstanding tasks to finish.
//
// After Stop is called, further Push calls are dropped. Queued and in-flight
// tasks still run to completion, until they all finish or the deadline implied
// by ctx and WithTimeout is reached. Stop returns nil on a clean drain, or the
// context error on timeout or cancellation.
//
// Every external call waits for all tasks to finish, including tasks that
// call Stop themselves, even after another call has timed out. A task or the
// panic handler may call Stop: that call excludes itself and tasks concurrently
// waiting in Stop, preventing self and mutual waits. It hands off its unstarted
// batch and waits for the remaining tasks, returning ErrStopInTask when the
// concurrency limit prevents them from running until a stopping task returns.
// An external Stop also waits for any code those tasks run after Stop returns.
func (q *taskQueue[T]) Stop(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, q.opt.timeout)
	defer cancel()

	q.mu.Lock()
	first := !q.stopped
	q.stopped = true
	self := q.callerWorkerLocked()
	q.mu.Unlock()
	if first {
		q.beginStop()
	} else {
		select {
		case <-q.stopping:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if self != nil {
		// The calling task's worker runs the rest of its batch only once the
		// task returns: hand that rest to the other workers.
		q.handOffHeld(self)
		defer atomic.AddInt64(&q.selfStops, -1)
	}

	ticker := time.NewTicker(stopCheckInterval)
	defer ticker.Stop()
	for {
		done, blocked := q.stopState(self != nil)
		if done {
			return nil
		}
		if blocked {
			// Every worker the limit allows is a task waiting here: what is
			// left runs only once those tasks return.
			return ErrStopInTask
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			if done, _ := q.stopState(self != nil); done {
				return nil
			}
			return ctx.Err()
		}
	}
}

// beginStop is the first Stop's part: it refuses new submissions, waits for
// the submitters already past the stop check, dismisses the parked workers
// and then closes stopping. Once the submitters have dispatched, every
// accepted task has a running worker that will reach it.
func (q *taskQueue[T]) beginStop() {
	// A submitter counts itself in before it looks at closed, and closed is
	// set before the counts are read, so a submitter either sees closed or
	// is counted here. Each stripe's count is never negative, so only a
	// stripe that reads zero has no submitter left in it.
	atomic.StoreInt64(&q.closed, 1)
	for i := range q.producers {
		for atomic.LoadInt64(&q.producers[i].n) != 0 {
			runtime.Gosched()
		}
	}

	// Dismiss every parked worker; each is blocked on <-w.ch. They stay
	// parked and live until they have retired (see retire).
	q.mu.Lock()
	idle := q.idle
	q.idle = nil
	q.mu.Unlock()
	for _, w := range idle {
		w.ch <- true
	}
	close(q.stopping)
}

// callerWorkerLocked returns the worker whose task is calling, or nil when
// the caller is not one of this queue's workers. The caller holds mu.
func (q *taskQueue[T]) callerWorkerLocked() *worker[T] {
	me := goid.Get()
	for _, w := range q.workers {
		if atomic.LoadInt64(&w.goid) == me && !w.parked {
			return w
		}
	}
	return nil
}

// handOffHeld returns the unstarted tail of the batch the calling task
// came from to the ingress queue, and makes sure workers come for it, as the
// monitor's rescue of a stalled batch does. It registers the caller's Stop
// only after the tail is visible to other workers, under the same lock.
func (q *taskQueue[T]) handOffHeld(w *worker[T]) {
	q.mu.Lock()
	n := 0
	if state := atomic.LoadUint64(&w.state); int32(state) > 0 {
		n = q.claimBatchLocked(w, state)
	}
	atomic.AddInt64(&q.selfStops, 1)
	q.mu.Unlock()
	if n > 0 {
		q.dispatch(n)
	}
}

func (q *taskQueue[T]) finished() bool {
	return q.lenAny() == 0 && atomic.LoadInt64(&q.running) == 0
}

// stopState keeps task exclusions and the handoff of held batches in one
// snapshot. External callers wait for every worker, including task callers.
func (q *taskQueue[T]) stopState(inTask bool) (done, blocked bool) {
	if !inTask {
		return q.finished(), false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	queued := q.lenAny()
	waiting := atomic.LoadInt64(&q.selfStops)
	running := atomic.LoadInt64(&q.running)
	return queued == 0 && running <= waiting,
		queued > 0 && waiting >= int64(q.opt.concurrency)
}

func (q *taskQueue[T]) isStopped() bool {
	return atomic.LoadInt64(&q.closed) != 0
}

// place is the P a submission is made on, which picks its producer stripe
// and, for a queue sharded by P, its shard. It is 0, pinning nothing, while
// the queue's producers have not begun to spread (and for keyed queues,
// whose shard comes from the key): the stripe is then the one every producer
// of a quiet queue shares, as a single queue's submitters always did.
func (q *taskQueue[T]) place() int {
	if q.shards <= 1 || q.keyed || !q.spread() {
		return 0
	}
	return currentP()
}

// enterAt registers a submitter in the stripe of place p. It fails once Stop
// has begun.
func (q *taskQueue[T]) enterAt(p int) bool {
	c := &q.producers[p&(len(q.producers)-1)].n
	atomic.AddInt64(c, 1)
	if atomic.LoadInt64(&q.closed) != 0 {
		atomic.AddInt64(c, -1)
		return false
	}
	return true
}

// leaveAt counts a submitter registered by enterAt(p) out.
func (q *taskQueue[T]) leaveAt(p int) {
	atomic.AddInt64(&q.producers[p&(len(q.producers)-1)].n, -1)
	if atomic.LoadUint32(&q.throttle) != 0 {
		runtime.Gosched()
	}
}

func (q *taskQueue[T]) enter() bool { return q.enterAt(0) }
func (q *taskQueue[T]) leave()      { q.leaveAt(0) }

// pushRun is how many pushes a submitter makes to one shard before it moves
// on to the next; see shardAt.
const pushRun = 64

// shardAt is the ingress shard of value submitted on place p, or 0 when
// unsharded. A keyed queue runs its key here. An unkeyed queue keeps every
// producer on the single queue until they contend for it (see contended), so
// a lone submitter's tasks sit together; once they spread, a submitter moves
// to the next shard every pushRun pushes, so a few submitters' tasks reach
// every shard and the workers that latch onto the shards spread with them.
func (q *taskQueue[T]) shardAt(value T, p int) uint32 {
	switch {
	case q.shards <= 1:
		return 0
	case q.keyed:
		return q.keyFn(value) % uint32(q.shards)
	case p == 0:
		return 0
	default:
		// p is the submitter's P, once producers spread; see place.
		turn := atomic.AddUint32(&q.pushTurn[p&(len(q.pushTurn)-1)].n, 1)
		return (uint32(p) + turn/pushRun) % uint32(q.shards)
	}
}

// spread reports whether an unkeyed queue's producers should use the shards
// of their Ps rather than the single queue they share; see contended.
func (q *taskQueue[T]) spread() bool {
	return q.keyed || atomic.LoadUint32(&q.contended) != 0
}

func (q *taskQueue[T]) push(item taskItem[T]) bool {
	// A stopped keyed queue runs no key: the submission is rejected before
	// the callback, and enter below still guards the stop race. An unkeyed
	// queue has no key to protect and enter alone rejects it.
	if q.keyed && q.isStopped() {
		return false
	}
	p := q.place()
	shard := q.shardAt(item.value, p)
	if !q.enterAt(p) {
		return false
	}
	q.pushItem(item, shard)
	q.dispatch(1)
	q.leaveAt(p)
	return true
}

// pushRun enqueues values as one ordered run, on the submitter's shard when
// the queue is sharded by P, or split by key when it is keyed. shards holds
// the keyed shard of each value.
func (q *taskQueue[T]) pushRun(values []T, shards []uint32, submitted bool, p int) {
	switch {
	case q.keyed:
		for i, v := range values {
			q.pushItem(taskItem[T]{value: v, submitted: submitted}, shards[i])
		}
	case q.shards > 1 && q.spread():
		// p is the submitter's P; see place.
		r := 0
		if p != 0 {
			r = p % q.shards
		}
		q.rings[r].pushN(len(values), func(i int) taskItem[T] {
			return taskItem[T]{value: values[i], submitted: submitted}
		})
		q.markOccupied(r)
	default:
		q.queue.pushN(len(values), func(i int) taskItem[T] {
			return taskItem[T]{value: values[i], submitted: submitted}
		})
	}
}

// pushValues enqueues values in order. With submitted set, the caller has
// already reserved one pending slot per value.
func (q *taskQueue[T]) pushValues(values []T, submitted bool) int {
	if len(values) == 0 {
		return 0
	}
	// A stopped keyed queue runs no key: the batch is rejected before the
	// callbacks, and enter below still guards the stop race.
	if q.keyed && q.isStopped() {
		return 0
	}
	var shards []uint32
	if q.keyed {
		var buf [shardBatch]uint32
		shards = q.shardIndexes(values, &buf)
	}
	p := q.place()
	if !q.enterAt(p) {
		return 0
	}
	q.pushRun(values, shards, submitted, p)
	q.dispatch(len(values))
	q.leaveAt(p)
	return len(values)
}

func (q *taskQueue[T]) submit(value T) bool {
	if q.opt.maxPending <= 0 {
		return q.push(taskItem[T]{value: value})
	}
	// A stopped keyed queue runs no key: the submission is rejected before
	// the callback, and reservePending below still guards the stop race.
	if q.keyed && q.isStopped() {
		return false
	}
	p := q.place()
	shard := q.shardAt(value, p)
	if q.reservePending(1, true) == 0 {
		return false
	}
	if !q.enterAt(p) {
		atomic.AddInt64(&q.pending, -1)
		return false
	}
	q.pushItem(taskItem[T]{value: value, submitted: true}, shard)
	q.dispatch(1)
	q.leaveAt(p)
	return true
}

func (q *taskQueue[T]) submitBatch(values []T, atomicBatch bool) (accepted int, allAccepted bool) {
	if q.isStopped() {
		return 0, false
	}
	if len(values) == 0 {
		return 0, true
	}
	accepted = len(values)
	submitted := q.opt.maxPending > 0
	// The key runs before the reservation below, so a key that panics leaves
	// nothing admitted, the whole batch included.
	var shards []uint32
	if q.keyed {
		var buf [shardBatch]uint32
		shards = q.shardIndexes(values, &buf)
	}
	if submitted {
		if accepted = q.reservePending(len(values), atomicBatch); accepted == 0 {
			return 0, false
		}
	}
	p := q.place()
	if !q.enterAt(p) {
		if submitted {
			atomic.AddInt64(&q.pending, -int64(accepted))
		}
		return 0, false
	}
	q.pushRun(values[:accepted], shards, submitted, p)
	q.dispatch(accepted)
	q.leaveAt(p)
	return accepted, accepted == len(values)
}

// reservePending reserves up to n pending slots, or exactly n when all is set,
// and returns how many it reserved.
func (q *taskQueue[T]) reservePending(n int, all bool) int {
	limit := int64(q.opt.maxPending)
	// Well below the limit a plain add cannot fail, unlike a CAS that loses
	// to every concurrent reservation and release. An add that crosses the
	// limit is undone at once and retried exactly below.
	if atomic.LoadInt64(&q.pending)+int64(n) <= limit {
		if atomic.AddInt64(&q.pending, int64(n)) <= limit {
			return n
		}
		atomic.AddInt64(&q.pending, -int64(n))
	}
	for {
		p := atomic.LoadInt64(&q.pending)
		available := limit - p
		if available <= 0 || (all && available < int64(n)) {
			return 0
		}
		take := int64(n)
		if take > available {
			take = available
		}
		if atomic.CompareAndSwapInt64(&q.pending, p, p+take) {
			return int(take)
		}
	}
}

// dispatch runs after n tasks were published. While the running workers
// reach the target it takes no lock: those workers will reach the tasks, and
// a worker that stops running checks the queue again after it has released
// its slot. Otherwise it wakes parked workers, or starts new ones, for up to
// n of the free slots.
func (q *taskQueue[T]) dispatch(n int) {
	if atomic.LoadInt64(&q.running) >= atomic.LoadInt64(&q.target) {
		q.startMonitor()
		return
	}
	if q.spread() {
		if atomic.LoadInt32(&q.seekers) > 0 {
			// A worker on its way to the queue, woken or spinning, takes the
			// tasks, and one that finds more behind the one it took wakes
			// others for them (see wakeMore). A seeker counts itself out
			// before it looks at the queue, so it finds the tasks published
			// before this load. Should the seekers block in their tasks
			// before they look, the monitor sees the rest waiting.
			q.startMonitor()
			return
		}
	} else if atomic.LoadInt32(&q.spinners) > 0 {
		// A worker that just finished a task is looking for the next: it
		// takes this one without a park or a wake-up. Should it stop looking
		// first, it checks the queue once more before it parks.
		return
	}
	q.wake(n)
}

// wakeMore runs after a worker took a task: while more wait, a free running
// slot and no other worker on its way to them, it wakes workers for them, so
// that a burst fans out a worker per hop instead of a wake-up per task.
func (q *taskQueue[T]) wakeMore(w *worker[T]) {
	if !q.spread() {
		return
	}
	if atomic.LoadInt64(&q.running) < atomic.LoadInt64(&q.target) &&
		atomic.LoadInt32(&q.seekers) == 0 && q.readyAfterPop(w) {
		q.wake(2)
	}
}

// readyAfterPop reports whether tasks remain after a worker took one: in the
// shard it took from, or in another shard marked in occupied. A pop leaves
// its shard's mark set, so marks are confirmed by looking at the shards, up
// to readyChecks of them; past that the monitor finds what waits.
func (q *taskQueue[T]) readyAfterPop(w *worker[T]) bool {
	if q.shards <= 1 {
		return q.queue.ready()
	}
	r := int(atomic.LoadInt32(&w.ring))
	if r < 0 {
		return q.queue.ready()
	}
	if q.rings[r].ready() {
		return true
	}
	checks := readyChecks
	for word := range q.occupied {
		marks := atomic.LoadUint64(&q.occupied[word])
		if word == r/64 {
			marks &^= 1 << (r % 64)
		}
		for ; marks != 0 && checks > 0; marks &= marks - 1 {
			if q.rings[word*64+bits.TrailingZeros64(marks)].ready() {
				return true
			}
			checks--
		}
	}
	return false
}

// readyChecks bounds the shards readyAfterPop looks at.
const readyChecks = 8

// spawnChunk bounds the workers one hold of mu starts, so that a large
// resize does not keep parking and waking workers waiting.
const spawnChunk = 64

// wake wakes parked workers, or starts new ones, for up to n of the free
// running slots, counting each as a seeker until it reaches the queue.
func (q *taskQueue[T]) wake(n int) {
	var woken [16]*worker[T]
	for n > 0 {
		q.mu.Lock()
		free := atomic.LoadInt64(&q.target) - atomic.LoadInt64(&q.running)
		if free <= 0 {
			q.mu.Unlock()
			q.startMonitor()
			return
		}
		if free > int64(n) {
			free = int64(n)
		}
		k := 0
		for int64(k) < free && k < len(woken) && len(q.idle) > 0 {
			w := q.idle[len(q.idle)-1]
			q.idle[len(q.idle)-1] = nil
			q.idle = q.idle[:len(q.idle)-1]
			w.parked = false
			woken[k] = w
			k++
		}
		spawned := 0
		if len(q.idle) == 0 {
			for int64(k+spawned) < free && q.live < q.opt.concurrency && spawned < spawnChunk {
				q.spawnLocked()
				spawned++
			}
		}
		atomic.AddInt64(&q.running, int64(k+spawned))
		atomic.AddInt32(&q.seekers, int32(k))
		q.mu.Unlock()
		for i := 0; i < k; i++ {
			woken[i].ch <- false
			woken[i] = nil
		}
		if k+spawned == 0 {
			// Every live worker is running or on its way out, at the
			// concurrency limit; the monitor retries while tasks wait.
			q.startMonitor()
			return
		}
		n -= k + spawned
	}
}

// spawnLocked starts a new worker. The caller holds mu and counts the worker
// as running.
func (q *taskQueue[T]) spawnLocked() {
	var w *worker[T]
	if v := q.pool.Get(); v != nil {
		w = v.(*worker[T])
	} else {
		w = &worker[T]{ch: make(chan bool, 1)}
		w.initBatch(q.opt.maxBatchSize)
	}
	if q.shards > 1 {
		w.seed = q.spawned * 0x9e3779b9
		atomic.StoreInt32(&w.ring, int32(w.seed%uint32(q.shards)))
	}
	q.spawned++
	q.live++
	w.index = len(q.workers)
	w.lastSeq = uint32(atomic.LoadUint64(&w.state) >> 32)
	w.stuckTicks = 0
	q.workers = append(q.workers, w)
	if q.onSpawnForTest != nil {
		q.onSpawnForTest()
	}
	atomic.AddInt32(&q.seekers, 1)
	go q.worker(w)
}

// removeLocked drops a worker from the registry and returns its block to the
// pool. The caller holds mu and has already taken the worker out of live.
func (q *taskQueue[T]) removeLocked(w *worker[T]) {
	w.parked = false
	atomic.StoreInt64(&w.goid, 0)
	last := len(q.workers) - 1
	moved := q.workers[last]
	q.workers[w.index] = moved
	moved.index = w.index
	q.workers[last] = nil
	q.workers = q.workers[:last]
	w.lastUsed = 0
	q.pool.Put(w)
}

const (
	// batchCooldown is the number of tasks handled singly after a rescue.
	// Contention can grow batches again once this worker has made progress.
	batchCooldown  = 64
	workerSeqStep  = uint64(1) << 32
	workerHeldMask = workerSeqStep - 1
	// Rescue markers prevent the owner from reusing a batch while
	// the monitor returns its unstarted entries to the shared queue.
	batchRescuing = uint32(1) << 31
	batchRescued  = batchRescuing | 1
)

// worker is the main loop of a worker goroutine.
//
// It claims small batches only when workers collide on the queue head. The
// monitor returns unstarted batch tasks to the shared queue when the owner
// stops making progress. After a rescue the owner handles tasks singly for
// a while; after each claim it yields excess running slots as before.
func (q *taskQueue[T]) worker(w *worker[T]) {
	atomic.StoreInt64(&w.goid, goid.Get())
	atomic.AddInt32(&q.seekers, -1) // started by wake
	q.runWorker(w, 1)
}

func (q *taskQueue[T]) runWorker(w *worker[T], batch int) {
	maxBatch := q.opt.maxBatchSize
	if batch > maxBatch {
		batch = maxBatch
	}
	n, cooldown := 0, 0
	for {
		limit := batch
		if limit > 1 && atomic.LoadInt64(&q.target) > q.base {
			// The monitor added workers for tasks that block: a batch would
			// hold tasks behind a blocked one that another worker could run.
			limit = 1
		}
		if q.opt.maxJobs > 0 && q.opt.maxJobs-n < limit {
			limit = q.opt.maxJobs - n
		}
		k, contended := q.popWorkerBatch(w, w.batch[:limit])
		if k == 0 {
			if q.spin(w) {
				continue
			}
			batch = 1
			switch q.idleWorker(w) {
			case idleExit:
				return
			case idleWoken:
				// The GC may have shrunk the stack while parked, so reset the
				// counter to avoid a premature maxJobs trigger.
				n = 0
			}
			continue
		}
		q.wakeMore(w)
		if cooldown > 0 {
			batch = 1
		} else if contended && batch < maxBatch {
			if batch > maxBatch/2 {
				batch = maxBatch
			} else {
				batch *= 2
			}
		} else if !contended && batch > 1 {
			batch--
		}
		w.lastEnd = nanotime()
		completed, rescued := 1, false
		if k == 1 {
			item := w.batch[0]
			w.batch[0] = taskItem[T]{}
			atomic.AddUint64(&w.state, workerSeqStep)
			q.exec(item.value)
			if item.submitted {
				atomic.AddInt64(&q.pending, -1)
			}
		} else {
			completed, rescued = q.execBatch(w, k)
		}
		if rescued {
			batch, cooldown = 1, batchCooldown
		} else if cooldown > 0 {
			cooldown -= completed
		}
		n += completed
		if q.opt.maxJobs > 0 && n >= q.opt.maxJobs {
			q.recycle(w)
			return
		}
		if atomic.LoadInt64(&q.running) > atomic.LoadInt64(&q.target) {
			switch q.shed(w) {
			case idleExit:
				return
			case idleWoken:
				n = 0
			}
		}
	}
}

// execBatch publishes the unstarted tail of a batch. The owner claims each
// remaining entry with a CAS on its own cache line; the monitor can instead
// claim the entire tail. No per-task timer or allocation is needed.
func (q *taskQueue[T]) execBatch(w *worker[T], k int) (completed int, rescued bool) {
	item := w.batch[0]
	w.batch[0] = taskItem[T]{}
	w.batchSize = k
	state := atomic.LoadUint64(&w.state) &^ workerHeldMask
	atomic.StoreUint64(&w.state, state+workerSeqStep|uint64(k-1))
	atomic.AddInt64(&q.holders, 1)
	defer atomic.AddInt64(&q.holders, -1)
	// A burst smaller than the worker target may not have started a monitor.
	// Once a batch is held it must remain observable even with an empty FIFO.
	if atomic.LoadUint32(&q.monitorOn) == 0 {
		q.startMonitor()
	}
	q.exec(item.value)
	if item.submitted {
		atomic.AddInt64(&q.pending, -1)
	}
	completed = 1
	for {
		state := atomic.LoadUint64(&w.state)
		held := uint32(state)
		switch held {
		case 0:
			return completed, false
		case batchRescuing:
			// The monitor owns the tail, but has not finished copying it.
			runtime.Gosched()
			continue
		case batchRescued:
			atomic.StoreUint64(&w.state, state&^workerHeldMask)
			return completed, true
		}
		if !atomic.CompareAndSwapUint64(&w.state, state, state+workerSeqStep-1) {
			continue
		}
		index := k - int(held)
		item = w.batch[index]
		w.batch[index] = taskItem[T]{}
		q.exec(item.value)
		if item.submitted {
			atomic.AddInt64(&q.pending, -1)
		}
		completed++
	}
}

// rescueBatchLocked returns a stalled worker's unstarted tasks to the FIFO.
// Holding mu keeps Len and worker retirement out of the ownership transfer.
// observed is the monitor's original sample: the owner can advance without
// mu, so claiming a newer state would apply old stall samples to a new task.
func (q *taskQueue[T]) rescueBatchLocked(w *worker[T], observed uint64) int {
	if w.stuckTicks < batchRescueTicks || q.opt.maxBatchSize <= 1 || q.opt.concurrency <= 1 {
		return 0
	}
	return q.claimBatchLocked(w, observed)
}

// claimBatchLocked moves the unstarted tail of w's batch, as observed, back to
// the ingress queue and reports how many tasks it moved; it moves nothing when
// the owner has advanced past observed. The caller holds mu.
func (q *taskQueue[T]) claimBatchLocked(w *worker[T], observed uint64) int {
	held := int32(observed)
	if held <= 0 || !atomic.CompareAndSwapUint64(&w.state, observed, observed&^workerHeldMask|uint64(batchRescuing)) {
		return 0
	}
	// The batch was popped from the worker's shard, or from the single queue
	// (-1) before the producers spread (see popWorkerBatch), and that does
	// not change while the batch is held, so the tail goes back there.
	shard := int(atomic.LoadInt32(&w.ring))
	first := w.batchSize - int(held)
	for i := 0; i < int(held); i++ {
		item := w.batch[first+i]
		w.batch[first+i] = taskItem[T]{}
		if shard < 0 {
			q.queue.push(item)
		} else {
			q.pushItem(item, uint32(shard))
		}
	}
	atomic.StoreUint64(&w.state, observed&^workerHeldMask|uint64(batchRescued))
	return int(held)
}

// shardIndex returns the ingress shard of value, or 0 when unkeyed. It is the
// only place the queue runs key, and it runs before a submission registers
// as a producer or reserves pending room: a key that panics then leaves the
// queue as it was, with nothing to roll back. The shard goes to pushItem with the
// task; it is not kept in the item.
func (q *taskQueue[T]) shardIndex(value T) uint32 {
	if !q.keyed {
		return 0
	}
	return q.keyFn(value) % uint32(q.shards)
}

// localShard is the shard of the P the caller runs on, for an unkeyed queue
// sharded by P.
func (q *taskQueue[T]) localShard() uint32 {
	return uint32(currentP()) % uint32(q.shards)
}

// shardIndexes runs shardIndex over values ahead of a batch's admission, into
// buf when it fits. The queue is keyed.
func (q *taskQueue[T]) shardIndexes(values []T, buf *[shardBatch]uint32) []uint32 {
	shards := buf[:0]
	if len(values) > len(buf) {
		shards = make([]uint32, 0, len(values))
	}
	for _, value := range values {
		shards = append(shards, q.shardIndex(value))
	}
	return shards
}

// pushItem lands one task on the shard's FIFO (the single FIFO when unkeyed),
// the shard from shardIndex before the task was admitted. The shard is not
// kept in the item: a worker's batch is popped from one shard, the worker's
// own at the time, and a rescued batch goes back there (see
// rescueBatchLocked).
func (q *taskQueue[T]) pushItem(item taskItem[T], shard uint32) {
	if q.shards <= 1 || !q.spread() {
		q.queue.push(item)
		return
	}
	q.rings[shard].push(item)
	q.markOccupied(int(shard))
}

// markOccupied sets shard r's bit in occupied after a task was published
// there. The bit only has to be set once the task is visible: a pop that ran
// the shard dry clears it and then looks at the shard again (see
// settleEmpty), so either that look finds the task or this call comes after
// the clear and sets the bit again. A bit that is set already is only read,
// so while shards stay busy their producers share the line instead of
// writing it.
func (q *taskQueue[T]) markOccupied(r int) {
	word, bit := &q.occupied[r/64], uint64(1)<<(r%64)
	if atomic.LoadUint64(word)&bit == 0 {
		atomic.OrUint64(word, bit)
	}
}

// settleEmpty clears shard r's bit after a pop found the shard empty, and
// reports whether a task arrived there meanwhile, in which case the bit is set
// again. A bit may stay set over an empty shard, when the task that set it was
// taken by a worker that did not run the shard dry; the next pop that finds
// the shard empty clears it. A bit is never clear over a task for longer than
// a settleEmpty in progress, which looks at the shard again before returning.
func (q *taskQueue[T]) settleEmpty(r int) bool {
	word, bit := &q.occupied[r/64], uint64(1)<<(r%64)
	if atomic.LoadUint64(word)&bit == 0 {
		return false
	}
	atomic.AndUint64(word, ^bit)
	if !q.rings[r].ready() {
		return false
	}
	atomic.OrUint64(word, bit)
	return true
}

// readyAny reports whether any ingress queue may hold a task. A keyed queue
// answers from occupied, which can report a shard whose tasks were all taken
// since: a worker that resumes for it finds it empty and clears its bit. It
// misses a task only while the task's producer has yet to mark it, and that
// producer checks the running workers after marking, so it sees the slot an
// idle worker released before asking; or while a settleEmpty is between its
// clear and its second look, whose running worker takes the task.
func (q *taskQueue[T]) readyAny() bool {
	if q.shards <= 1 {
		return q.queue.ready()
	}
	if !q.keyed && !q.spread() {
		return q.queue.ready()
	}
	for i := range q.occupied {
		if atomic.LoadUint64(&q.occupied[i]) != 0 {
			return true
		}
	}
	// Tasks published to the single queue before the producers spread.
	return !q.keyed && q.queue.ready()
}

// lenAny totals the tasks waiting across the ingress queues.
func (q *taskQueue[T]) lenAny() int {
	if q.shards <= 1 {
		return q.queue.len()
	}
	n := 0
	if !q.keyed {
		n = q.queue.len()
	}
	for i := range q.rings {
		n += q.rings[i].len()
	}
	return n
}

// enqueuedAny totals the tasks ever enqueued across the ingress queues.
func (q *taskQueue[T]) enqueuedAny() uint64 {
	if q.shards <= 1 {
		return q.queue.enqueued()
	}
	var n uint64
	if !q.keyed {
		n = q.queue.enqueued()
	}
	for i := range q.rings {
		n += q.rings[i].enqueued()
	}
	return n
}

// dequeuedAny totals the tasks ever taken from the ingress queues, which is
// how many tasks workers have started or hold in a batch.
func (q *taskQueue[T]) dequeuedAny() uint64 {
	if q.shards <= 1 {
		return q.queue.dequeued()
	}
	var n uint64
	if !q.keyed {
		n = q.queue.dequeued()
	}
	for i := range q.rings {
		n += q.rings[i].dequeued()
	}
	return n
}

// shrinkAll retires spare capacity in every ingress queue.
func (q *taskQueue[T]) shrinkAll() {
	if q.shards <= 1 {
		q.queue.shrink(ringKeep)
		return
	}
	if !q.keyed {
		q.queue.shrink(ringKeep)
	}
	for i := range q.rings {
		q.rings[i].shrink(ringKeep)
	}
}

// tailSmallEnough reports whether every ingress queue is already down to the
// kept size, the shrink fast path.
func (q *taskQueue[T]) tailSmallEnough() bool {
	if q.shards <= 1 {
		return len(q.queue.tailRing().vals) <= ringKeep
	}
	for i := range q.rings {
		if len(q.rings[i].tailRing().vals) > ringKeep {
			return false
		}
	}
	return true
}

// monoNow reads the monotonic clock in nanoseconds.
func monoNow() int64 { return int64(time.Since(monoEpoch)) }

// tryWatch runs watchShards unless one is running or the last one was less
// than a monitor tick ago. The monitor calls it every tick and workers every
// watchEvery batch attempts.
func (q *taskQueue[T]) tryWatch(now int64) {
	if now < atomic.LoadInt64(&q.watchNext) || !atomic.CompareAndSwapUint32(&q.watching, 0, 1) {
		return
	}
	if now >= atomic.LoadInt64(&q.watchNext) {
		atomic.StoreInt64(&q.watchNext, now+int64(monitorTick))
		atomic.AddUint32(&q.ticks, 1)
		q.watchShards(now)
	}
	atomic.StoreUint32(&q.watching, 0)
}

// watchShards marks a keyed shard orphaned once it has held tasks for
// orphanStale in which no worker took one, and unmarks a marked shard a
// worker has served since. The wait is measured on the clock rather than in
// watches, whose pace the clock's resolution and the monitor's lateness set.
// This hands over the shards left waiting when there are more shards than
// running workers and none of theirs runs dry; the shard of a worker blocked
// in a task the monitor hands over sooner (see orphanStuckShardLocked). The
// caller holds watching.
func (q *taskQueue[T]) watchShards(now int64) {
	step := now - q.lastWatch
	if step > int64(watchCredit) {
		step = int64(monitorTick)
	}
	q.lastWatch = now
	heads, waited := q.heads, q.waited
	for i := range q.rings {
		r := &q.rings[i]
		if head := r.dequeued(); head != heads[i] || !r.ready() {
			heads[i] = head
			waited[i] = 0
			q.unmarkOrphan(i)
			continue
		}
		if waited[i] += step; waited[i] >= int64(orphanStale) {
			waited[i] = 0
			q.markOrphan(i)
		}
	}
}

// markOrphan marks shard r orphaned unless it is already.
func (q *taskQueue[T]) markOrphan(r int) {
	if bit := uint64(1) << (r % 64); atomic.OrUint64(&q.orphaned[r/64], bit)&bit == 0 {
		atomic.AddInt32(&q.orphans, 1)
	}
}

// unmarkOrphan clears shard r's mark and reports whether it was the one to.
func (q *taskQueue[T]) unmarkOrphan(r int) bool {
	word, bit := r/64, uint64(1)<<(r%64)
	if atomic.LoadUint64(&q.orphaned[word])&bit == 0 ||
		atomic.AndUint64(&q.orphaned[word], ^bit)&bit == 0 {
		return false
	}
	atomic.AddInt32(&q.orphans, -1)
	return true
}

// claimOrphan makes an orphaned shard the worker's own, so it serves the
// tasks no worker took there next. A worker claims at most one shard per
// monitor tick: one that just moved serves its new shard for a while rather
// than hopping on to the next orphan at once. The shard it leaves is marked
// in turn once it has waited as long, so when no shard runs dry the workers
// rotate over all of them, and the wait before a mark ages the shards like a
// queue: marking a shard the moment its worker left let the same few be
// claimed again and again while others waited, which with six shards per
// worker stretched the longest wait of a key from 7ms to 37ms. Each worker
// looks from its own point of a rotating start, so claims spread over the
// marked shards and no shard is passed over for its index.
func (q *taskQueue[T]) claimOrphan(w *worker[T]) {
	tick := atomic.LoadUint32(&q.ticks)
	if tick == w.claimTick {
		return
	}
	shards := uint32(q.shards)
	start := (w.seed + tick) % shards
	for i := uint32(0); i < shards; i++ {
		r := (start + i) % shards
		if !q.unmarkOrphan(int(r)) {
			continue
		}
		atomic.StoreInt32(&w.ring, int32(r))
		w.claimTick = tick
		return
	}
}

// watchStuckShardLocked hands over the shard of a worker that has been in one
// task for batchRescueTicks ticks or more whenever the shard holds tasks that
// no worker took since the last tick: they wait behind the blocked task, and
// the held batch rescue reaches only the ones the worker took itself. Tasks
// can arrive there at any point while the worker stays blocked, so the shard
// is watched on every tick it does, and not just the one it got stuck on. A
// shard another worker took over keeps being served and is left alone:
// marking it again would pull yet another worker off its own shard each tick.
// It runs from the tick a worker counts as stuck, so the dequeue count it
// compares against is a tick old by the first tick it may hand over. The
// caller holds mu.
func (q *taskQueue[T]) watchStuckShardLocked(w *worker[T]) {
	r := int(atomic.LoadInt32(&w.ring))
	head := q.rings[r].dequeued()
	if w.stuckTicks >= batchRescueTicks && head == w.stuckHead && q.rings[r].ready() {
		q.markOrphan(r)
	}
	w.stuckHead = head
}

// clearOrphans drops every mark, once the monitor stops: whatever it marked
// has drained by then.
func (q *taskQueue[T]) clearOrphans() {
	for word := range q.orphaned {
		if old := atomic.SwapUint64(&q.orphaned[word], 0); old != 0 {
			atomic.AddInt32(&q.orphans, -int32(bits.OnesCount64(old)))
		}
	}
}

func (q *taskQueue[T]) observeWorkersLocked() (stuck, progress, held int64) {
	for _, w := range q.workers {
		if w.parked {
			w.stuckTicks = 0
			continue
		}
		state := atomic.LoadUint64(&w.state)
		if seq := uint32(state >> 32); seq != w.lastSeq {
			progress += int64(seq - w.lastSeq)
			w.lastSeq = seq
			w.stuckTicks = 0
		} else if w.stuckTicks++; w.stuckTicks >= stuckTicks {
			stuck++
			if w.stuckTicks >= batchRescueTicks && q.opt.maxBatchSize > 1 &&
				q.opt.concurrency > 1 && int32(state) > 0 {
				q.rescueBatchLocked(w, state)
				state = atomic.LoadUint64(&w.state)
			}
			if q.keyed {
				q.watchStuckShardLocked(w)
			}
		}
		if remaining := int32(state); remaining > 0 {
			held += int64(remaining)
		}
	}
	return stuck, progress, held
}

// popWorkerBatch takes the next batch. A keyed worker takes it from its own
// shard while that holds tasks, and otherwise from the first shard of a scan
// that holds any, which then becomes its own: shards follow the load to the
// workers without a hint on the wake. A shard the monitor found unattended
// is claimed first (see claimOrphan). Only workers whose shard it is take
// from a shard, so a shard's dequeues tell the monitor whether one serves it.
func (q *taskQueue[T]) popWorkerBatch(w *worker[T], dst []taskItem[T]) (int, bool) {
	if q.shards <= 1 {
		return q.queue.popBatch(dst)
	}
	if !q.keyed {
		if !q.spread() {
			n, contended := q.queue.popBatch(dst)
			if n > 0 {
				atomic.StoreInt32(&w.ring, -1)
			}
			return n, contended
		}
		return q.popLocal(w, dst)
	}
	if w.pops++; w.pops%watchEvery == 0 {
		q.tryWatch(monoNow())
	}
	if atomic.LoadInt32(&q.orphans) > 0 {
		q.claimOrphan(w)
	}
	if k, contended := q.rings[atomic.LoadInt32(&w.ring)].popBatch(dst); k > 0 {
		return k, contended
	}
	return q.scanBatch(w, dst)
}

// fairEvery is how often a worker of a queue sharded by P starts looking at
// a rotating shard rather than its P's own: a shard whose P runs no worker,
// while every worker's own shard keeps them busy, is still reached.
const fairEvery = 32

// popLocal takes the next batch for a queue sharded by P: from the shard the
// worker served last, so that the workers spread over the shards that hold
// their tasks - usually the Ps of the submitters - rather than all going for
// the shard of their own P, which most of them would find empty; then from
// the shard of its own P, where a submitter on it left its tasks; then from
// the first shard of a scan that holds any. Every fairEvery batches the
// worker starts at a rotating shard instead, so that no shard waits on the
// placement of the goroutines. The shard a batch came from is recorded in
// w.ring, for the next pop and for a rescue to return the batch's tail
// there.
func (q *taskQueue[T]) popLocal(w *worker[T], dst []taskItem[T]) (int, bool) {
	w.pops++
	if w.pops%fairEvery == 0 {
		// Now and then start from a rotating shard, so that no shard waits
		// on the placement of the goroutines.
		r := int((w.seed + w.pops/fairEvery) % uint32(q.shards))
		if k, contended := q.rings[r].popBatch(dst); k > 0 {
			atomic.StoreInt32(&w.ring, int32(r))
			return k, contended
		}
	}
	// The shard the worker served last first: a worker that found work on a
	// shard keeps serving it, so the workers spread over the shards that
	// hold tasks - three submitters' shards, say - rather than all going for
	// their own P's, which most of them would find empty.
	r := int(atomic.LoadInt32(&w.ring))
	if r >= 0 {
		if k, contended := q.rings[r].popBatch(dst); k > 0 {
			return k, contended
		}
	}
	if p := int(q.localShard()); p != r {
		if k, contended := q.rings[p].popBatch(dst); k > 0 {
			atomic.StoreInt32(&w.ring, int32(p))
			return k, contended
		}
	}
	start := r + 1
	if start == q.shards || start < 0 {
		start = 0
	}
	if k, contended := q.scanFrom(w, dst, start, false, r); k > 0 {
		return k, contended
	}
	if k, contended := q.scanFrom(w, dst, start, true, -1); k > 0 {
		return k, contended
	}
	// Tasks a producer published to the single queue before the producers
	// spread are drained here, before the worker parks.
	if q.queue.ready() {
		if k, contended := q.queue.popBatch(dst); k > 0 {
			atomic.StoreInt32(&w.ring, -1)
			return k, contended
		}
	}
	return 0, false
}

// scanBatch takes a batch from the first shard holding tasks and makes that
// shard the worker's own. It looks only at the shards marked in occupied, so a
// worker that finds no work reads a word per 64 shards rather than every
// shard's queue. A marked shard found empty keeps its mark while the scan
// goes on: under load the next scan finds work and clearing would only move
// the word between CPUs. Only a scan that found nothing settles the marks it
// passed, on the way to parking, so the marks idle workers read stay
// accurate. Successive scans of a worker start at successive shards, from an
// offset that differs per worker, so scanning workers spread over the shards.
func (q *taskQueue[T]) scanBatch(w *worker[T], dst []taskItem[T]) (int, bool) {
	w.scans++
	start := int((w.seed + w.scans) % uint32(q.shards))
	if k, contended := q.scanFrom(w, dst, start, false, -1); k > 0 {
		return k, contended
	}
	return q.scanFrom(w, dst, start, true, -1)
}

// scanFrom is one pass of scanBatch over the marked shards from start on,
// settling the empty ones when settle is set, and passing over shard skip.
func (q *taskQueue[T]) scanFrom(w *worker[T], dst []taskItem[T], start int, settle bool, skip int) (int, bool) {
	words := len(q.occupied)
	first, below := start/64, uint64(1)<<(start%64)-1
	// The word holding start is visited twice, its bits from start on first
	// and the ones below start last, so the scan wraps around once.
	for i := 0; i <= words; i++ {
		word := (first + i) % words
		marks := atomic.LoadUint64(&q.occupied[word])
		switch i {
		case 0:
			marks &^= below
		case words:
			marks &= below
		}
		for ; marks != 0; marks &= marks - 1 {
			r := word*64 + bits.TrailingZeros64(marks)
			if r == skip {
				continue
			}
			k, contended := q.rings[r].popBatch(dst)
			if k == 0 && settle && q.settleEmpty(r) {
				k, contended = q.rings[r].popBatch(dst)
			}
			if k > 0 {
				atomic.StoreInt32(&w.ring, int32(r))
				return k, contended
			}
		}
	}
	return 0, false
}

func (q *taskQueue[T]) exec(value T) {
	if q.opt.panicFn != nil {
		defer func() {
			if v := recover(); v != nil {
				q.opt.panicFn(v)
			}
		}()
	}
	q.run(value)
}

const (
	idleResume = iota // keep running
	idleWoken         // parked, then woken by a submitter
	idleExit          // the goroutine must return
)

// idleWorker handles a worker that found the queue empty: it parks, or exits
// when parking is disabled or the queue is stopped. Either way it releases its
// running slot first and then checks the queue again. A submitter that
// published a task before the release may have seen every slot busy and left
// the task to the running workers, so this worker must take it itself.
func (q *taskQueue[T]) idleWorker(w *worker[T]) int {
	q.mu.Lock()
	atomic.AddInt64(&q.running, -1)
	if q.readyAny() && atomic.LoadInt64(&q.running) < atomic.LoadInt64(&q.target) {
		atomic.AddInt64(&q.running, 1)
		q.mu.Unlock()
		return idleResume
	}
	if q.stopped || q.opt.maxIdle <= 0 {
		q.live--
		q.removeLocked(w)
		q.mu.Unlock()
		return idleExit
	}
	q.parkLocked(w)
	q.mu.Unlock()
	if <-w.ch {
		q.retire(w)
		return idleExit
	}
	atomic.AddInt32(&q.seekers, -1) // woken by wake
	return idleWoken
}

// spinNanos is how long a worker that just finished a task looks for the
// next one before it parks, and how recently it must have finished one to
// look at all: a task that arrives while a worker spins is taken without a
// park and a wake-up, both of which go through mu, and that is most of the
// round trip of a lightly loaded queue. A worker idle for longer parks at
// once rather than burn a P.
const spinNanos = 5_000

// spin looks for tasks without parking, for up to spinNanos, and reports
// whether it found some. Only a quarter of the base target spins at once, and
// only while the target is past twice its base: CPU-bound work, at its base,
// leaves the CPU a spinning worker would take to other goroutines. A spinner
// counts as a seeker, so submitters leave their tasks to it.
func (q *taskQueue[T]) spin(w *worker[T]) bool {
	if atomic.LoadInt64(&q.running) >= atomic.LoadInt64(&q.target) {
		// The workers there are cover the tasks: spin only while a woken
		// worker is still expected, not to hold a surplus one.
		return false
	}
	if w.lastEnd == 0 || nanotime()-w.lastEnd > spinNanos {
		return false
	}
	limit := int32(q.base / 4)
	if limit < 1 {
		limit = 1
	}
	if atomic.LoadInt32(&q.spinners) >= limit {
		return false
	}
	if atomic.AddInt32(&q.spinners, 1) > limit {
		atomic.AddInt32(&q.spinners, -1)
		return false
	}
	atomic.AddInt32(&q.seekers, 1)
	found := q.readyAny()
	for deadline := nanotime() + spinNanos; !found && nanotime() < deadline; {
		runtime.Gosched()
		found = q.readyAny()
	}
	w.lastEnd = 0
	atomic.AddInt32(&q.seekers, -1)
	atomic.AddInt32(&q.spinners, -1)
	return found
}

func (q *taskQueue[T]) parkLocked(w *worker[T]) {
	w.parked = true
	w.lastUsed = q.nowNanos()
	q.idle = append(q.idle, w)
	q.ensureJanitorLocked()
}

// retire removes a parked worker that was told to exit: dismissed by Stop or
// expired by the janitor. It stays live, and parked, until it gets here, so a
// replacement never starts beside a worker that is still on its way out.
// A task that arrived while the limit still counted it may be waiting for
// the slot it frees.
func (q *taskQueue[T]) retire(w *worker[T]) {
	q.mu.Lock()
	q.live--
	q.removeLocked(w)
	q.mu.Unlock()
	if q.readyAny() {
		q.dispatch(1)
	}
}

// shed gives up a running slot while more workers run than the target allows,
// which happens when the monitor lowers the target again. It never takes the
// running count below the target, so a backlog always keeps its workers.
func (q *taskQueue[T]) shed(w *worker[T]) int {
	q.mu.Lock()
	if q.stopped || atomic.LoadInt64(&q.running) <= atomic.LoadInt64(&q.target) {
		q.mu.Unlock()
		return idleResume
	}
	atomic.AddInt64(&q.running, -1)
	if q.opt.maxIdle <= 0 {
		q.live--
		q.removeLocked(w)
		q.mu.Unlock()
		return idleExit
	}
	q.parkLocked(w)
	q.mu.Unlock()
	if <-w.ch {
		q.retire(w)
		return idleExit
	}
	atomic.AddInt32(&q.seekers, -1) // woken by wake
	return idleWoken
}

// recycle retires a worker that reached maxJobs. When tasks are waiting, a
// fresh worker, with a fresh stack, takes over its slot.
func (q *taskQueue[T]) recycle(w *worker[T]) {
	q.mu.Lock()
	atomic.AddInt64(&q.running, -1)
	q.live--
	q.removeLocked(w)
	q.mu.Unlock()
	if q.readyAny() {
		q.dispatch(1)
	}
}

func (q *taskQueue[T]) ensureJanitorLocked() {
	if q.opt.manualJanitor {
		return
	}
	if !q.janitorRunning {
		q.janitorRunning = true
		go q.janitor()
	}
}

// janitor reclaims expired workers every maxIdle until no worker is parked,
// or until Stop, which dismisses the parked workers itself.
func (q *taskQueue[T]) janitor() {
	timer := time.NewTimer(q.opt.maxIdle)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
		case <-q.stopping:
			q.mu.Lock()
			q.janitorRunning = false
			q.mu.Unlock()
			return
		}
		if q.cleanExpiredAt(q.nowNanos()) {
			return
		}
		timer.Reset(q.opt.maxIdle)
	}
}

// nowNanos reads the clock idle workers are timed on: the runtime's monotonic
// clock, or the one a test injected.
func (q *taskQueue[T]) nowNanos() int64 {
	if q.opt.monoClock {
		return nanotime()
	}
	return q.opt.nowFn().UnixNano()
}

// cleanExpiredAt reclaims every worker idle for at least maxIdle at now, a
// reading of nowNanos. It reports whether the janitor should stop, which is
// the case when the idle set is empty.
func (q *taskQueue[T]) cleanExpiredAt(now int64) (stop bool) {
	q.mu.Lock()
	var expired []*worker[T]
	dst := 0
	for _, w := range q.idle {
		if now-w.lastUsed >= int64(q.opt.maxIdle) {
			expired = append(expired, w) // live and parked until it retires
			continue
		}
		q.idle[dst] = w
		dst++
	}
	for i := dst; i < len(q.idle); i++ {
		q.idle[i] = nil
	}
	q.idle = q.idle[:dst]
	if len(q.idle) == 0 {
		q.janitorRunning = false
		stop = true
	}
	q.mu.Unlock()

	for _, w := range expired {
		w.ch <- true
	}
	return stop
}

// startMonitor starts the monitor unless it is already running.
func (q *taskQueue[T]) startMonitor() {
	if atomic.LoadUint32(&q.monitorOn) == 0 && atomic.CompareAndSwapUint32(&q.monitorOn, 0, 1) {
		go q.monitor()
	}
}

// monitor runs while tasks wait for a worker, or while the target the load
// set lingers, and sets once per tick how many workers may run; see sizer for
// how. It also hands keyed shards over (see tryWatch), rescues the stalled
// batches of blocked workers, and throttles submitters that outrun the
// workers: when tasks have arrived at more than twice the rate they start,
// with the backlog growing, for behindTicks ticks in a row and the backlog is
// past throttleBacklog, submitters yield after each submission until the
// backlog halves.
func (q *taskQueue[T]) monitor() {
	q.mu.Lock()
	base := q.base
	q.mu.Unlock()
	sz := newSizer(base, int64(q.opt.concurrency), nanotime(), q.dequeuedAny(), q.enqueuedAny())
	quiet, behind := 0, 0
	lastTail, lastStarted, lastBacklog := q.enqueuedAny(), q.dequeuedAny(), int64(q.lenAny())
	for {
		time.Sleep(monitorTick)
		if q.keyed {
			atomic.AddUint32(&q.monTicks, 1)
			q.tryWatch(monoNow())
		}

		q.mu.Lock()
		var held int64
		if q.keyed || atomic.LoadInt64(&q.holders) > 0 {
			_, _, held = q.observeWorkersLocked()
		}
		stopped := q.stopped
		q.mu.Unlock()
		queued := int64(q.lenAny())
		backlog := queued + held
		// Tasks a worker holds in a batch were dequeued but not started; the
		// sizer counts tasks as they start.
		tail, started := q.enqueuedAny(), q.dequeuedAny()
		running := atomic.LoadInt64(&q.running)
		target := sz.step(nanotime(), started-uint64(held), tail, running, backlog, atomic.LoadInt64(&q.target))

		arrivals, progress := int64(tail-lastTail), int64(started-lastStarted)
		if backlog > lastBacklog && arrivals > 2*progress {
			behind++
		} else {
			behind = 0
		}
		lastTail, lastStarted, lastBacklog = tail, started, backlog
		if behind >= behindTicks && backlog > throttleBacklog {
			atomic.StoreUint32(&q.throttle, 1)
		} else if backlog < throttleBacklog/2 && atomic.LoadUint32(&q.throttle) != 0 {
			atomic.StoreUint32(&q.throttle, 0)
		}

		atomic.StoreInt64(&q.target, target)
		if queued > 0 && atomic.LoadInt64(&q.running) < target {
			q.wake(int(queued))
		}

		if backlog > 0 {
			quiet = 0
			continue
		}
		if quiet++; (quiet < monitorIdleTicks || target > base) && !stopped {
			continue
		}
		atomic.StoreInt64(&q.target, base)
		atomic.StoreUint32(&q.throttle, 0)
		q.shrinkAll()
		if q.keyed {
			q.clearOrphans()
		}
		q.finishMonitor()
		return
	}
}

func (q *taskQueue[T]) finishMonitor() {
	atomic.StoreUint32(&q.monitorOn, 0)
	// A submitter or batch owner may have observed the old monitor just
	// before it stopped. Recheck both the FIFO and held tails after clearing
	// the flag, including during Stop while accepted work remains.
	if q.Len() > 0 {
		q.startMonitor()
	}
}
