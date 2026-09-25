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
	"runtime"
	"sync"
	"sync/atomic"
	"time"
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

const (
	// monitorTick is the period of the monitor that runs while tasks are
	// waiting for a worker.
	monitorTick = time.Millisecond
	// stuckTicks is how many consecutive ticks a worker must spend in one
	// task before the monitor counts it as stuck.
	stuckTicks = 2
	// monitorIdleTicks is how many consecutive ticks without a backlog the
	// monitor waits before it stops.
	monitorIdleTicks = 50
	// ringKeep is the largest queue ring kept once a backlog has drained.
	ringKeep = 1024
	// behindTicks is how many consecutive ticks tasks must arrive at more than
	// twice the rate workers start them, with the backlog growing, before the
	// monitor adds workers even though the scheduler is saturated.
	behindTicks = 3
	// throttleBacklog is the backlog above which a pool that is falling behind
	// makes submitters yield their P after every submission, until the backlog
	// halves. Without it, producers that never block could grow the queue
	// without bound while the workers they compete with for CPU fall behind.
	throttleBacklog = 1 << 16
)

// producerStopped is the top bit of taskQueue.producers. The low bits count
// submitters between their stop check and the end of their dispatch.
const producerStopped = int64(1) << 62

// worker is one worker goroutine's control block. The fields written for
// every task are padded so that workers never share those cache lines.
type worker[T any] struct {
	_    [cacheLinePad]byte
	seq  uint32 // atomic: bumped when the worker starts a task
	held int32  // atomic: claimed tasks not yet started
	_    [cacheLinePad - 8]byte

	ch chan bool // wake-up (false) or exit (true) while parked; buffered 1

	// Guarded by taskQueue.mu.
	lastUsed   time.Time
	parked     bool
	index      int    // position in taskQueue.workers
	lastSeq    uint32 // seq seen at the monitor's previous tick
	stuckTicks int
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
	running   int64 // atomic: workers not parked; written under mu
	_         [cacheLinePad - 8]byte
	producers int64 // atomic: see producerStopped
	_         [cacheLinePad - 8]byte
	pending   int64 // atomic: outstanding Submit tasks
	_         [cacheLinePad - 8]byte
	target    int64 // atomic: soft limit on running workers
	base      int64 // target without the monitor's compensation
	monitorOn uint32
	throttle  uint32 // atomic: submitters yield after submitting; see throttleBacklog
	_         [cacheLinePad - 24]byte

	queue lfQueue[taskItem[T]]

	mu             sync.Mutex
	live           int          // live workers, at most opt.concurrency
	idle           []*worker[T] // parked workers, most recently parked last
	workers        []*worker[T] // every live worker, scanned by the monitor
	stopped        bool
	janitorRunning bool
	pool           sync.Pool

	extra int64 // owned by the monitor: running slots added for blocked workers

	opt *options
	run func(T)

	onSpawnForTest func()
}

func newTaskQueue[T any](opts []Option, run func(T)) *taskQueue[T] {
	o := newOptions(opts)
	q := &taskQueue[T]{opt: o, run: run}
	base := int64(autoWorkerLimit(o.concurrency))
	q.base = base
	q.target = base
	q.queue.init(backlogCapacityHint(int(base)))
	return q
}

// Len returns the number of tasks queued but not yet started.
func (q *taskQueue[T]) Len() int {
	n := q.queue.len()
	q.mu.Lock()
	for _, w := range q.workers {
		n += int(atomic.LoadInt32(&w.held))
	}
	q.mu.Unlock()
	return n
}

// Stop shuts the queue down and waits for outstanding tasks to finish.
//
// After Stop is called, further Push calls are dropped. Queued and in-flight
// tasks still run to completion, until they all finish or the deadline implied
// by ctx and WithTimeout is reached. Stop returns nil on a clean drain, or the
// context error on timeout or cancellation.
func (q *taskQueue[T]) Stop(ctx context.Context) error {
	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		return nil
	}
	q.stopped = true
	q.mu.Unlock()

	// Refuse new submissions, then wait for the submitters already past the
	// stop check. Once they have dispatched, every accepted task has a
	// running worker that will reach it.
	for {
		v := atomic.LoadInt64(&q.producers)
		if atomic.CompareAndSwapInt64(&q.producers, v, v|producerStopped) {
			break
		}
	}
	for atomic.LoadInt64(&q.producers) != producerStopped {
		runtime.Gosched()
	}

	// Dismiss every parked worker; each is blocked on <-w.ch.
	q.mu.Lock()
	idle := q.idle
	q.idle = nil
	for _, w := range idle {
		w.parked = false
	}
	q.live -= len(idle)
	q.mu.Unlock()
	for _, w := range idle {
		w.ch <- true
	}

	ctx2, cancel := context.WithTimeout(ctx, q.opt.timeout)
	defer cancel()
	ticker := time.NewTicker(stopCheckInterval)
	defer ticker.Stop()

	for {
		if q.finished() {
			return nil
		}
		select {
		case <-ticker.C:
		case <-ctx2.Done():
			if q.finished() {
				return nil
			}
			return ctx2.Err()
		}
	}
}

func (q *taskQueue[T]) finished() bool {
	return q.queue.len() == 0 && atomic.LoadInt64(&q.running) == 0
}

func (q *taskQueue[T]) isStopped() bool {
	return atomic.LoadInt64(&q.producers)&producerStopped != 0
}

// enter registers a submitter. It fails once Stop has begun.
func (q *taskQueue[T]) enter() bool {
	if atomic.AddInt64(&q.producers, 1)&producerStopped != 0 {
		atomic.AddInt64(&q.producers, -1)
		return false
	}
	return true
}

func (q *taskQueue[T]) leave() {
	atomic.AddInt64(&q.producers, -1)
	if atomic.LoadUint32(&q.throttle) != 0 {
		runtime.Gosched()
	}
}

func (q *taskQueue[T]) push(item taskItem[T]) bool {
	if !q.enter() {
		return false
	}
	q.queue.push(item)
	q.dispatch(1)
	q.leave()
	return true
}

// pushValues enqueues values in order. With submitted set, the caller has
// already reserved one pending slot per value.
func (q *taskQueue[T]) pushValues(values []T, submitted bool) int {
	if len(values) == 0 || !q.enter() {
		return 0
	}
	q.queue.pushN(len(values), func(i int) taskItem[T] {
		return taskItem[T]{value: values[i], submitted: submitted}
	})
	q.dispatch(len(values))
	q.leave()
	return len(values)
}

func (q *taskQueue[T]) submit(value T) bool {
	if q.opt.maxPending <= 0 {
		return q.push(taskItem[T]{value: value})
	}
	if q.reservePending(1, true) == 0 {
		return false
	}
	if !q.push(taskItem[T]{value: value, submitted: true}) {
		atomic.AddInt64(&q.pending, -1)
		return false
	}
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
	if submitted {
		if accepted = q.reservePending(len(values), atomicBatch); accepted == 0 {
			return 0, false
		}
	}
	if q.pushValues(values[:accepted], submitted) == 0 {
		if submitted {
			atomic.AddInt64(&q.pending, -int64(accepted))
		}
		return 0, false
	}
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
			for int64(k+spawned) < free && q.live < q.opt.concurrency {
				q.spawnLocked()
				spawned++
			}
		}
		atomic.AddInt64(&q.running, int64(k+spawned))
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
	}
	q.live++
	w.index = len(q.workers)
	w.lastSeq = atomic.LoadUint32(&w.seq)
	w.stuckTicks = 0
	q.workers = append(q.workers, w)
	if q.onSpawnForTest != nil {
		q.onSpawnForTest()
	}
	go q.worker(w)
}

// removeLocked drops a worker from the registry and returns its block to the
// pool. The caller holds mu and has already taken the worker out of live.
func (q *taskQueue[T]) removeLocked(w *worker[T]) {
	last := len(q.workers) - 1
	moved := q.workers[last]
	q.workers[w.index] = moved
	moved.index = w.index
	q.workers[last] = nil
	q.workers = q.workers[:last]
	w.lastUsed = time.Time{}
	q.pool.Put(w)
}

// maxPopBatch caps how many tasks a worker claims at once.
const maxPopBatch = 8

// worker is the main loop of a worker goroutine.
//
// It takes one task at a time from the shared queue, so a worker that loses
// its P never holds tasks another worker could run. Only when workers collide
// on the queue head, which takes tasks shorter than a CAS round trip, does it
// claim small batches instead. After each claim it gives up its running slot
// when more workers run than the target allows.
func (q *taskQueue[T]) worker(w *worker[T]) {
	var buf [maxPopBatch]taskItem[T]
	batch, n := 1, 0
	for {
		limit := batch
		if q.opt.maxJobs > 0 && q.opt.maxJobs-n < limit {
			limit = q.opt.maxJobs - n
		}
		k, contended := q.queue.popBatch(buf[:limit])
		if k == 0 {
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
		if contended && batch < maxPopBatch {
			if batch *= 2; batch > maxPopBatch {
				batch = maxPopBatch
			}
		} else if !contended && batch > 1 {
			batch--
		}
		for j := 0; j < k; j++ {
			item := buf[j]
			buf[j] = taskItem[T]{}
			if k > 1 {
				atomic.StoreInt32(&w.held, int32(k-1-j))
			}
			atomic.AddUint32(&w.seq, 1)
			q.exec(item.value)
			if item.submitted {
				atomic.AddInt64(&q.pending, -1)
			}
		}
		n += k
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
	if q.queue.ready() && atomic.LoadInt64(&q.running) < atomic.LoadInt64(&q.target) {
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
	return idleWoken
}

func (q *taskQueue[T]) parkLocked(w *worker[T]) {
	w.parked = true
	w.lastUsed = q.opt.nowFn()
	q.idle = append(q.idle, w)
	q.ensureJanitorLocked()
}

// retire removes a worker that has already been taken out of live.
func (q *taskQueue[T]) retire(w *worker[T]) {
	q.mu.Lock()
	q.removeLocked(w)
	q.mu.Unlock()
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
	if q.queue.ready() {
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

func (q *taskQueue[T]) janitor() {
	for {
		time.Sleep(q.opt.maxIdle)
		if q.cleanExpired(q.opt.nowFn()) {
			return
		}
	}
}

// cleanExpired reclaims every worker idle for at least maxIdle. It reports
// whether the janitor should stop, which is the case when the idle set is empty.
func (q *taskQueue[T]) cleanExpired(now time.Time) (stop bool) {
	q.mu.Lock()
	var expired []*worker[T]
	dst := 0
	for _, w := range q.idle {
		if now.Sub(w.lastUsed) >= q.opt.maxIdle {
			w.parked = false
			expired = append(expired, w)
		} else {
			q.idle[dst] = w
			dst++
		}
	}
	for i := dst; i < len(q.idle); i++ {
		q.idle[i] = nil
	}
	q.idle = q.idle[:dst]
	q.live -= len(expired)
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

// monitor runs while tasks wait for a worker and adjusts, once per tick, how
// many workers beyond the base target may run. More workers help only while
// tasks wait and Ps sit idle, which means running workers are blocked rather
// than busy:
//
//   - When no task started during a tick while workers are stuck, every
//     running worker is blocked: the target doubles. Tasks queued behind
//     blocking tasks need that many workers to start at all.
//   - When the scheduler shows idle Ps for two ticks in a row, the target
//     doubles if most workers started no task in the tick, and grows by a
//     quarter otherwise (workers blocking for short stretches).
//   - When the run queues are long, a quarter of the extra slots go: the CPU
//     is the bottleneck and more workers would only queue for a P.
//   - Except when the pool is falling far behind: tasks have arrived at more
//     than twice the rate they start, with the backlog growing, for
//     behindTicks ticks in a row. Other goroutines are then taking the CPU the
//     workers need, and every further such tick a quarter more workers claim
//     a larger share of it. Past throttleBacklog, submitters also yield after
//     each submission until the backlog halves.
//
// Growth never exceeds the backlog, and the extra slots decay once no task
// is waiting.
func (q *taskQueue[T]) monitor() {
	var probe loadProbe
	quiet, idleTicks, behind := 0, 0, 0
	lastTail, lastBacklog := q.queue.enqueued(), int64(q.queue.len())
	for {
		start := time.Now()
		time.Sleep(monitorTick)
		lateness := time.Since(start) - monitorTick

		backlog := int64(q.queue.len())
		stuck, progress := int64(0), int64(0)
		q.mu.Lock()
		for _, w := range q.workers {
			if w.parked {
				w.stuckTicks = 0
				continue
			}
			if seq := atomic.LoadUint32(&w.seq); seq != w.lastSeq {
				progress += int64(seq - w.lastSeq)
				w.lastSeq = seq
				w.stuckTicks = 0
			} else if w.stuckTicks++; w.stuckTicks >= stuckTicks {
				stuck++
			}
		}
		base := q.base
		stopped := q.stopped
		q.mu.Unlock()

		extra := q.extra
		running := atomic.LoadInt64(&q.running)
		step := (base + extra) / 4
		if step < 1 {
			step = 1
		}
		load := probe.load(lateness)
		if load == loadIdle {
			idleTicks++
		} else {
			idleTicks = 0
		}
		tail := q.queue.enqueued()
		arrivals := int64(tail - lastTail)
		falling := backlog > lastBacklog && arrivals > 2*progress
		lastTail, lastBacklog = tail, backlog
		if falling {
			behind++
		} else {
			behind = 0
		}
		growth := int64(0)
		switch {
		case backlog == 0:
			if extra > stuck {
				extra -= step
				if extra < stuck {
					extra = stuck
				}
			}
		case progress == 0 && stuck > 0:
			growth = base + extra
		case behind >= behindTicks:
			growth = step
			if backlog > throttleBacklog {
				atomic.StoreUint32(&q.throttle, 1)
			}
		case falling:
			// Hold while a streak builds, rather than shrinking under load.
		case load == loadBusy:
			extra -= step
		case idleTicks >= 2 && progress < running:
			growth = base + extra
		case idleTicks >= 2:
			growth = step
		}
		if growth > backlog {
			growth = backlog
		}
		extra += growth
		if backlog < throttleBacklog/2 && atomic.LoadUint32(&q.throttle) != 0 {
			atomic.StoreUint32(&q.throttle, 0)
		}
		if limit := int64(q.opt.concurrency) - base; extra > limit {
			extra = limit
		}
		if extra < 0 {
			extra = 0
		}
		q.extra = extra
		atomic.StoreInt64(&q.target, base+extra)
		if backlog > 0 && atomic.LoadInt64(&q.running) < base+extra {
			q.dispatch(int(backlog))
		}

		if backlog > 0 {
			quiet = 0
			continue
		}
		if quiet++; quiet < monitorIdleTicks && !stopped {
			continue
		}
		q.extra = 0
		atomic.StoreInt64(&q.target, base)
		atomic.StoreUint32(&q.throttle, 0)
		q.queue.shrink(ringKeep)
		atomic.StoreUint32(&q.monitorOn, 0)
		// A submitter that saw the monitor still running did not start
		// another one; look once more before leaving.
		if q.queue.ready() && !stopped {
			q.startMonitor()
		}
		return
	}
}
