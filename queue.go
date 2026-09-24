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
	q.taskQueue.push(value)
}

// PushBatch submits values and returns the number accepted. Values are stored
// directly in the queue; no per-value closure is created. A concurrent Stop
// can cause the result to be less than len(values).
func (q *Task[T]) PushBatch(values []T) int {
	return q.taskQueue.pushBatch(values)
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

// taskMessage is sent to a parked worker. A separate stop bit is used instead
// of a sentinel value so every T, including nil and its zero value, is valid.
type taskMessage[T any] struct {
	item taskItem[T]
	stop bool
}

type taskItem[T any] struct {
	value     T
	submitted bool
}

type taskDispatch[T any] struct {
	worker *worker[T]
	item   taskItem[T]
}

type worker[T any] struct {
	ch       chan taskMessage[T]
	lastUsed time.Time
}

// taskQueue contains the scheduling implementation shared by Queue and Task.
// run is bound once at construction and is called directly with each queued T.
type taskQueue[T any] struct {
	submitPending     int64 // atomically released by workers; admission is serialized by mu
	runningAtomic     int64
	shardCursor       uint64
	popCursor         uint64
	stoppedAtomic     int32
	mu                sync.Mutex
	opt               *options
	backlog           *taskRing[T]
	shards            []taskShard[T]
	wake              chan struct{}
	dispatchWake      chan struct{}
	dispatchStop      chan struct{}
	dispatcherStarted int32
	ingress           sync.RWMutex
	idle              []*worker[T]
	pool              sync.Pool
	running           int
	stopped           bool
	janitorRunning    bool

	onSpawnForTest func()
	run            func(T)
}

func newTaskQueue[T any](opts []Option, run func(T)) *taskQueue[T] {
	o := newOptions(opts)
	q := &taskQueue[T]{
		opt:     o,
		backlog: newTaskRing[T](8),
		run:     run,
	}
	if o.shards > 1 {
		q.shards = newTaskShards[T](o.shards)
		q.wake = make(chan struct{}, o.concurrency)
		q.dispatchWake = make(chan struct{}, 1)
		q.dispatchStop = make(chan struct{})
	}
	return q
}

func (q *taskQueue[T]) push(value T) bool {
	return q.pushItem(taskItem[T]{value: value})
}

func (q *taskQueue[T]) pushBatch(values []T) int {
	if len(values) == 0 {
		return 0
	}
	if len(q.shards) > 1 && atomic.LoadInt32(&q.stoppedAtomic) == 0 &&
		atomic.LoadInt64(&q.runningAtomic) >= int64(q.opt.concurrency) {
		accepted := 0
		for _, value := range values {
			if !q.enqueueSharded(taskItem[T]{value: value}) {
				break
			}
			accepted++
		}
		return accepted
	}

	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		return 0
	}
	actions := make([]taskDispatch[T], 0, q.batchDispatchCapacity(len(values)))
	for _, value := range values {
		q.enqueueItemLocked(taskItem[T]{value: value}, &actions)
	}
	q.mu.Unlock()
	q.dispatchBatch(actions)
	return len(values)
}

func (q *taskQueue[T]) submit(value T) bool {
	if q.opt.maxPending <= 0 {
		return q.push(value)
	}
	return q.submitItem(value)
}

func (q *taskQueue[T]) submitBatch(values []T, atomicBatch bool) (accepted int, allAccepted bool) {
	if len(values) == 0 {
		q.mu.Lock()
		stopped := q.stopped
		q.mu.Unlock()
		return 0, !stopped
	}

	accepted, ok := q.reservePendingBatch(len(values), atomicBatch)
	if !ok {
		return 0, false
	}
	submitted := q.opt.maxPending > 0
	if len(q.shards) > 1 && atomic.LoadInt32(&q.stoppedAtomic) == 0 &&
		atomic.LoadInt64(&q.runningAtomic) >= int64(q.opt.concurrency) {
		admitted := 0
		for _, value := range values[:accepted] {
			if !q.enqueueSharded(taskItem[T]{value: value, submitted: submitted}) {
				break
			}
			admitted++
		}
		if submitted && admitted < accepted {
			atomic.AddInt64(&q.submitPending, -int64(accepted-admitted))
		}
		return admitted, admitted == len(values)
	}

	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		if submitted {
			atomic.AddInt64(&q.submitPending, -int64(accepted))
		}
		return 0, false
	}
	actions := make([]taskDispatch[T], 0, q.batchDispatchCapacity(accepted))
	for _, value := range values[:accepted] {
		q.enqueueItemLocked(taskItem[T]{value: value, submitted: submitted}, &actions)
	}
	q.mu.Unlock()
	q.dispatchBatch(actions)
	return accepted, accepted == len(values)
}

func (q *taskQueue[T]) pushItem(item taskItem[T]) bool {
	if len(q.shards) > 1 && atomic.LoadInt32(&q.stoppedAtomic) == 0 &&
		atomic.LoadInt64(&q.runningAtomic) >= int64(q.opt.concurrency) {
		return q.enqueueSharded(item)
	}
	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		return false
	}
	w, spawn := q.enqueueSingleLocked(item)
	q.mu.Unlock()
	if w != nil {
		w.ch <- taskMessage[T]{item: item}
	} else if spawn {
		q.spawn(q.acquire(), item.value, item.submitted)
	}
	return true
}

func (q *taskQueue[T]) enqueueSharded(item taskItem[T]) bool {
	q.ingress.RLock()
	defer q.ingress.RUnlock()
	if atomic.LoadInt32(&q.stoppedAtomic) != 0 {
		return false
	}
	s := q.pickTaskShard()
	if s.push(item) {
		q.signalDispatcher()
	}
	return true
}

func (q *taskQueue[T]) signalDispatcher() {
	if atomic.LoadInt32(&q.stoppedAtomic) != 0 {
		return
	}
	if atomic.CompareAndSwapInt32(&q.dispatcherStarted, 0, 1) {
		go q.shardedDispatcher()
	}
	select {
	case q.dispatchWake <- struct{}{}:
	default:
	}
}

func (q *taskQueue[T]) shardedDispatcher() {
	for {
		select {
		case <-q.dispatchWake:
			q.replenishSharded()
		case <-q.dispatchStop:
			return
		}
	}
}

func (q *taskQueue[T]) replenishSharded() {
	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		return
	}
	var spawns []taskDispatch[T]
	for q.running < q.opt.concurrency {
		next, ok := q.popSharded()
		if !ok {
			break
		}
		if k := len(q.idle); k > 0 {
			w := q.idle[k-1]
			q.idle[k-1] = nil
			q.idle = q.idle[:k-1]
			q.running++
			atomic.AddInt64(&q.runningAtomic, 1)
			w.ch <- taskMessage[T]{item: next}
			continue
		}
		q.running++
		atomic.AddInt64(&q.runningAtomic, 1)
		spawns = append(spawns, taskDispatch[T]{item: next})
	}
	q.mu.Unlock()
	for _, action := range spawns {
		q.spawn(q.acquire(), action.item.value, action.item.submitted)
	}
}

func (q *taskQueue[T]) pickTaskShard() *taskShard[T] {
	n := uint64(len(q.shards))
	a := atomic.AddUint64(&q.shardCursor, 1) % n
	b := (a*6364136223846793005 + 1442695040888963407) % n
	if b == a {
		b = (b + 1) % n
	}
	if q.shards[b].len() < q.shards[a].len() {
		a = b
	}
	return &q.shards[a]
}

func (q *taskQueue[T]) popSharded() (taskItem[T], bool) {
	n := uint64(len(q.shards))
	start := atomic.AddUint64(&q.popCursor, 1) % n
	for i := uint64(0); i < n; i++ {
		if item, ok := q.shards[(start+i)%n].pop(); ok {
			return item, true
		}
	}
	return taskItem[T]{}, false
}

func (q *taskQueue[T]) removeIdleLocked(w *worker[T]) bool {
	for i, candidate := range q.idle {
		if candidate != w {
			continue
		}
		last := len(q.idle) - 1
		q.idle[i] = q.idle[last]
		q.idle[last] = nil
		q.idle = q.idle[:last]
		return true
	}
	return false
}

func (q *taskQueue[T]) submitItem(value T) bool {
	if !q.reservePending(1) {
		return false
	}
	item := taskItem[T]{value: value, submitted: true}
	if len(q.shards) > 1 && atomic.LoadInt32(&q.stoppedAtomic) == 0 &&
		atomic.LoadInt64(&q.runningAtomic) >= int64(q.opt.concurrency) {
		if !q.enqueueSharded(item) {
			q.releaseSubmitted()
			return false
		}
		return true
	}
	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		q.releaseSubmitted()
		return false
	}
	w, spawn := q.enqueueSingleLocked(item)
	q.mu.Unlock()
	if w != nil {
		w.ch <- taskMessage[T]{item: item}
	} else if spawn {
		q.spawn(q.acquire(), value, true)
	}
	return true
}

func (q *taskQueue[T]) reservePending(n int) bool {
	_, ok := q.reservePendingBatch(n, true)
	return ok
}

func (q *taskQueue[T]) reservePendingBatch(n int, atomicBatch bool) (int, bool) {
	if atomic.LoadInt32(&q.stoppedAtomic) != 0 {
		return 0, false
	}
	if q.opt.maxPending <= 0 {
		return n, true
	}
	for {
		current := atomic.LoadInt64(&q.submitPending)
		available := int64(q.opt.maxPending) - current
		accepted := int64(n)
		if available < accepted {
			if atomicBatch {
				return 0, false
			}
			accepted = available
		}
		if accepted <= 0 {
			return 0, false
		}
		if atomic.CompareAndSwapInt64(&q.submitPending, current, current+accepted) {
			return int(accepted), true
		}
	}
}

func (q *taskQueue[T]) enqueueSingleLocked(item taskItem[T]) (*worker[T], bool) {
	// Keep the single-item path allocation-free. The batch path collects
	// dispatch actions, but Push and Submit are hot enough that an action slice
	// here would escape once passed to dispatchBatch.
	if k := len(q.idle); k > 0 {
		w := q.idle[k-1]
		q.idle[k-1] = nil
		q.idle = q.idle[:k-1]
		q.running++
		if len(q.shards) > 1 {
			atomic.AddInt64(&q.runningAtomic, 1)
		}
		return w, false
	}
	if q.running < q.opt.concurrency {
		q.running++
		if len(q.shards) > 1 {
			atomic.AddInt64(&q.runningAtomic, 1)
		}
		return nil, true
	}
	q.backlog.push(item)
	return nil, false
}

func (q *taskQueue[T]) enqueueItemLocked(item taskItem[T], actions *[]taskDispatch[T]) {

	// 1) A parked worker exists: wake it to reuse its grown stack, taking the
	// most recently used one (LIFO).
	if k := len(q.idle); k > 0 {
		w := q.idle[k-1]
		q.idle[k-1] = nil
		q.idle = q.idle[:k-1]
		q.running++
		if len(q.shards) > 1 {
			atomic.AddInt64(&q.runningAtomic, 1)
		}
		*actions = append(*actions, taskDispatch[T]{worker: w, item: item})
		return
	}

	// 2) No parked worker and the concurrency limit is not reached: start one.
	if q.running < q.opt.concurrency {
		q.running++
		if len(q.shards) > 1 {
			atomic.AddInt64(&q.runningAtomic, 1)
		}
		*actions = append(*actions, taskDispatch[T]{item: item})
		return
	}

	// 3) At capacity: enqueue and let a looping worker pick it up.
	q.backlog.push(item)
}

func (q *taskQueue[T]) batchDispatchCapacity(n int) int {
	if n > q.opt.concurrency {
		return q.opt.concurrency
	}
	return n
}

func (q *taskQueue[T]) dispatchBatch(actions []taskDispatch[T]) {
	for _, action := range actions {
		if action.worker != nil {
			action.worker.ch <- taskMessage[T]{item: action.item}
			continue
		}
		q.spawn(q.acquire(), action.item.value, action.item.submitted)
	}
}

// Len returns the number of tasks queued but not yet started.
func (q *taskQueue[T]) Len() int {
	q.mu.Lock()
	length := q.backlog.len()
	q.mu.Unlock()
	for i := range q.shards {
		length += q.shards[i].len()
	}
	return length
}

// Stop shuts the queue down and waits for outstanding tasks to finish.
//
// After Stop is called, further Push calls are dropped. Queued and in-flight
// tasks still run to completion, until they all finish or the deadline implied
// by ctx and WithTimeout is reached. Stop returns nil on a clean drain, or the
// context error on timeout or cancellation.
func (q *taskQueue[T]) Stop(ctx context.Context) error {
	q.ingress.Lock()
	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		q.ingress.Unlock()
		return nil
	}
	q.stopped = true
	atomic.StoreInt32(&q.stoppedAtomic, 1)
	idle := q.idle
	q.idle = nil
	if q.dispatchStop != nil {
		close(q.dispatchStop)
	}
	q.mu.Unlock()
	q.ingress.Unlock()

	// Dismiss every parked worker; each is blocked on <-w.ch.
	for _, w := range idle {
		w.ch <- taskMessage[T]{stop: true}
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
	q.mu.Lock()
	finished := q.backlog.len()+q.running == 0
	q.mu.Unlock()
	if !finished {
		return false
	}
	for i := range q.shards {
		if q.shards[i].len() != 0 {
			return false
		}
	}
	return true
}

// worker is the main loop of a worker goroutine.
//
// On the hot path (the queue is non-empty and tasks are pulled back to back) it
// takes only the mutex and uses no channel. It parks on its own channel to wait
// for reuse or reclamation only when the queue is empty and parking is enabled.
func (q *taskQueue[T]) worker(w *worker[T], item taskItem[T]) {
	if len(q.shards) > 1 {
		q.workerSharded(w, item)
		return
	}
	n := 0
	for {
		q.exec(item.value)
		if item.submitted {
			q.releaseSubmitted()
		}
		n++
		capReached := q.opt.maxJobs > 0 && n >= q.opt.maxJobs

		q.mu.Lock()
		if next, ok := q.backlog.pop(); ok {
			if capReached && atomic.LoadInt32(&q.stoppedAtomic) == 0 {
				// The per-worker task cap was reached: hand the next task to a
				// fresh worker (with a fresh stack) and exit. The running count
				// is unchanged, as one leaves and one starts.
				q.mu.Unlock()
				q.spawn(q.acquire(), next.value, next.submitted)
				q.release(w)
				return
			}
			q.mu.Unlock()
			item = next
			continue
		}

		// The queue is empty.
		if q.stopped || capReached || q.opt.maxIdle <= 0 {
			q.running--
			if len(q.shards) > 1 {
				atomic.AddInt64(&q.runningAtomic, -1)
			}
			q.mu.Unlock()
			q.release(w)
			return
		}

		// Park: move from running to idle and wait to be woken or reclaimed.
		q.running--
		if len(q.shards) > 1 {
			atomic.AddInt64(&q.runningAtomic, -1)
		}
		w.lastUsed = q.opt.nowFn()
		q.idle = append(q.idle, w)
		q.ensureJanitorLocked()
		q.mu.Unlock()

		message := <-w.ch
		if message.stop {
			q.release(w)
			return
		}
		item = message.item
		// Woken from parking: the GC may have shrunk the stack meanwhile, so
		// reset the counter to avoid a premature maxJobs trigger.
		n = 0
	}
}

func (q *taskQueue[T]) workerSharded(w *worker[T], item taskItem[T]) {
	n := 0
	for {
		q.exec(item.value)
		if item.submitted {
			q.releaseSubmitted()
		}
		n++
		capReached := q.opt.maxJobs > 0 && n >= q.opt.maxJobs
		if n&15 == 0 {
			q.mu.Lock()
			if next, ok := q.backlog.pop(); ok {
				if capReached && !q.stopped {
					q.mu.Unlock()
					q.spawn(q.acquire(), next.value, next.submitted)
					q.release(w)
					return
				}
				q.mu.Unlock()
				item = next
				continue
			}
			q.mu.Unlock()
		}

		if next, ok := q.popSharded(); ok {
			if capReached && atomic.LoadInt32(&q.stoppedAtomic) == 0 {
				q.spawn(q.acquire(), next.value, next.submitted)
				q.release(w)
				return
			}
			item = next
			continue
		}

		q.mu.Lock()
		if next, ok := q.backlog.pop(); ok {
			if capReached && !q.stopped {
				q.mu.Unlock()
				q.spawn(q.acquire(), next.value, next.submitted)
				q.release(w)
				return
			}
			q.mu.Unlock()
			item = next
			continue
		}
		if next, ok := q.popSharded(); ok {
			if capReached && atomic.LoadInt32(&q.stoppedAtomic) == 0 {
				q.mu.Unlock()
				q.spawn(q.acquire(), next.value, next.submitted)
				q.release(w)
				return
			}
			q.mu.Unlock()
			item = next
			continue
		}

		if q.stopped || capReached || q.opt.maxIdle <= 0 {
			q.signalDispatcher()
			q.running--
			if len(q.shards) > 1 {
				atomic.AddInt64(&q.runningAtomic, -1)
			}
			q.mu.Unlock()
			q.release(w)
			return
		}

		q.running--
		atomic.AddInt64(&q.runningAtomic, -1)
		w.lastUsed = q.opt.nowFn()
		q.idle = append(q.idle, w)
		q.ensureJanitorLocked()
		q.mu.Unlock()

		for {
			select {
			case message := <-w.ch:
				if message.stop {
					q.release(w)
					return
				}
				item = message.item
				n = 0
				goto nextTask
			case <-q.wake:
				q.mu.Lock()
				if !q.removeIdleLocked(w) {
					q.mu.Unlock()
					continue
				}
				q.running++
				atomic.AddInt64(&q.runningAtomic, 1)
				q.mu.Unlock()
				if next, ok := q.popSharded(); ok {
					item = next
					n = 0
					goto nextTask
				}
				q.mu.Lock()
				if next, ok := q.backlog.pop(); ok {
					q.mu.Unlock()
					item = next
					n = 0
					goto nextTask
				}
				q.mu.Unlock()
				q.mu.Lock()
				if q.stopped {
					q.running--
					atomic.AddInt64(&q.runningAtomic, -1)
					q.mu.Unlock()
					q.release(w)
					return
				}
				q.running--
				atomic.AddInt64(&q.runningAtomic, -1)
				w.lastUsed = q.opt.nowFn()
				q.idle = append(q.idle, w)
				q.ensureJanitorLocked()
				q.mu.Unlock()
			}
		}
	nextTask:
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

func (q *taskQueue[T]) acquire() *worker[T] {
	if v := q.pool.Get(); v != nil {
		return v.(*worker[T])
	}
	return &worker[T]{
		ch: make(chan taskMessage[T], 1),
	}
}

func (q *taskQueue[T]) spawn(w *worker[T], value T, submitted bool) {
	if q.onSpawnForTest != nil {
		q.onSpawnForTest()
	}
	go q.worker(w, taskItem[T]{value: value, submitted: submitted})
}
func (q *taskQueue[T]) release(w *worker[T]) {
	w.lastUsed = time.Time{}
	q.pool.Put(w)
}

func (q *taskQueue[T]) releaseSubmitted() {
	atomic.AddInt64(&q.submitPending, -1)
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

func (q *taskQueue[T]) cleanExpired(now time.Time) (stop bool) {
	q.mu.Lock()
	var expired []*worker[T]
	dst := 0
	for _, w := range q.idle {
		if now.Sub(w.lastUsed) >= q.opt.maxIdle {
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
	if len(q.idle) == 0 {
		q.janitorRunning = false
		stop = true
	}
	q.mu.Unlock()

	for _, w := range expired {
		w.ch <- taskMessage[T]{stop: true}
	}
	return stop
}
