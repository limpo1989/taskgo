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

type worker[T any] struct {
	ch       chan taskMessage[T]
	lastUsed time.Time
}

// taskQueue contains the scheduling implementation shared by Queue and Task.
// run is bound once at construction and is called directly with each queued T.
type taskQueue[T any] struct {
	submitPending      int64 // atomically released by workers; admission is serialized by mu
	backlogAtomic      int64
	prefetchedAtomic   int64
	completedAtomic    int64
	spillLastCompleted int64
	spillCheckNano     int64
	workerTarget       int64
	initialWorkers     int64
	taskEWMA           int64
	mu                 sync.Mutex
	opt                *options
	backlog            *taskRing[T]
	idle               []*worker[T]
	pool               sync.Pool
	running            int
	stopped            bool
	janitorRunning     bool
	stallMonitor       bool

	onSpawnForTest func()
	run            func(T)
}

const workerBatchSize = 8
const workerStallThreshold = time.Millisecond

func newTaskQueue[T any](opts []Option, run func(T)) *taskQueue[T] {
	o := newOptions(opts)
	target := autoWorkerLimit(o.concurrency)
	return &taskQueue[T]{
		opt:            o,
		backlog:        newTaskRing[T](backlogCapacityHint(target)),
		run:            run,
		workerTarget:   int64(target),
		initialWorkers: int64(target),
	}
}

func (q *taskQueue[T]) workerLimit() int { return int(atomic.LoadInt64(&q.workerTarget)) }

func (q *taskQueue[T]) observeTask(d time.Duration) {
	ns := d.Nanoseconds()
	old := atomic.LoadInt64(&q.taskEWMA)
	if old == 0 {
		atomic.StoreInt64(&q.taskEWMA, ns)
	} else {
		atomic.StoreInt64(&q.taskEWMA, old+(ns-old)/8)
	}
	if ns < int64(250*time.Microsecond) || atomic.LoadInt64(&q.backlogAtomic) == 0 {
		return
	}
	for {
		oldTarget := atomic.LoadInt64(&q.workerTarget)
		if oldTarget >= int64(q.opt.concurrency) {
			return
		}
		step := oldTarget / 4
		if step < 1 {
			step = 1
		}
		newTarget := oldTarget + step
		if newTarget > int64(q.opt.concurrency) {
			newTarget = int64(q.opt.concurrency)
		}
		if atomic.CompareAndSwapInt64(&q.workerTarget, oldTarget, newTarget) {
			return
		}
	}
}

func (q *taskQueue[T]) retireWorker() {
	for {
		old := atomic.LoadInt64(&q.workerTarget)
		if old <= q.initialWorkers {
			return
		}
		if atomic.CompareAndSwapInt64(&q.workerTarget, old, old-1) {
			return
		}
	}
}

func (q *taskQueue[T]) maybeSpillLocked(now time.Time) {
	if q.stopped || q.backlog.len() == 0 || q.running < q.workerLimit() {
		return
	}
	completed := atomic.LoadInt64(&q.completedAtomic)
	if q.spillCheckNano == 0 || completed != q.spillLastCompleted {
		q.spillLastCompleted = completed
		q.spillCheckNano = now.UnixNano()
		return
	}
	if now.UnixNano()-q.spillCheckNano < int64(workerStallThreshold) {
		return
	}
	target := q.workerLimit()
	if target >= q.opt.concurrency {
		return
	}
	step := target / 4
	if step < 1 {
		step = 1
	}
	if q.backlog.len() > target {
		step = target
	}
	newTarget := target + step
	if newTarget > q.opt.concurrency {
		newTarget = q.opt.concurrency
	}
	atomic.StoreInt64(&q.workerTarget, int64(newTarget))
	q.spillLastCompleted = completed
	q.spillCheckNano = now.UnixNano()
	for q.running < newTarget {
		next, ok := q.popBacklogLocked()
		if !ok {
			return
		}
		q.running++
		q.spawn(q.acquire(), next.value, next.submitted)
	}
}

func (q *taskQueue[T]) stallLoop() {
	ticker := time.NewTicker(workerStallThreshold)
	defer ticker.Stop()
	for range ticker.C {
		q.mu.Lock()
		if q.stopped || q.backlog.len() == 0 || q.workerLimit() >= q.opt.concurrency {
			q.stallMonitor = false
			q.mu.Unlock()
			return
		}
		q.maybeSpillLocked(time.Now())
		q.mu.Unlock()
	}
}

func (q *taskQueue[T]) push(value T) bool {
	return q.pushItem(taskItem[T]{value: value})
}

func (q *taskQueue[T]) pushBatch(values []T) int {
	if len(values) == 0 {
		return 0
	}
	accepted := 0
	for _, value := range values {
		if !q.pushItem(taskItem[T]{value: value}) {
			break
		}
		accepted++
	}
	return accepted
}

func (q *taskQueue[T]) submit(value T) bool {
	if q.opt.maxPending <= 0 {
		return q.push(value)
	}
	return q.submitItem(value)
}

func (q *taskQueue[T]) submitBatch(values []T, atomicBatch bool) (accepted int, allAccepted bool) {
	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		return 0, false
	}
	if len(values) == 0 {
		q.mu.Unlock()
		return 0, true
	}

	accepted = len(values)
	if q.opt.maxPending > 0 {
		available := q.opt.maxPending - int(atomic.LoadInt64(&q.submitPending))
		if atomicBatch && available < accepted {
			q.mu.Unlock()
			return 0, false
		}
		if available < accepted {
			accepted = available
		}
		if accepted == 0 {
			q.mu.Unlock()
			return 0, false
		}
		atomic.AddInt64(&q.submitPending, int64(accepted))
	}
	if atomicBatch {
		submitted := q.opt.maxPending > 0
		q.backlog.reserve(accepted)
		for _, value := range values[:accepted] {
			item := taskItem[T]{value: value, submitted: submitted}
			w, spawn := q.enqueueSingleLocked(item)
			if w != nil {
				w.ch <- taskMessage[T]{item: item}
			} else if spawn {
				q.spawn(q.acquire(), item.value, item.submitted)
			}
		}
		q.mu.Unlock()
		return accepted, true
	}
	q.backlog.reserve(accepted)
	q.mu.Unlock()

	submitted := q.opt.maxPending > 0
	admitted := 0
	for _, value := range values[:accepted] {
		if !q.enqueueReservedItem(taskItem[T]{value: value, submitted: submitted}) {
			break
		}
		admitted++
	}
	if submitted && admitted < accepted {
		atomic.AddInt64(&q.submitPending, -int64(accepted-admitted))
	}
	return admitted, admitted == len(values)
}

func (q *taskQueue[T]) pushItem(item taskItem[T]) bool {
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

func (q *taskQueue[T]) submitItem(value T) bool {
	q.mu.Lock()
	if q.stopped || atomic.LoadInt64(&q.submitPending) >= int64(q.opt.maxPending) {
		q.mu.Unlock()
		return false
	}
	atomic.AddInt64(&q.submitPending, 1)
	item := taskItem[T]{value: value, submitted: true}
	w, spawn := q.enqueueSingleLocked(item)
	q.mu.Unlock()
	if w != nil {
		w.ch <- taskMessage[T]{item: item}
	} else if spawn {
		q.spawn(q.acquire(), value, true)
	}
	return true
}

func (q *taskQueue[T]) enqueueReservedItem(item taskItem[T]) bool {
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

func (q *taskQueue[T]) enqueueSingleLocked(item taskItem[T]) (*worker[T], bool) {
	if k := len(q.idle); k > 0 {
		w := q.idle[k-1]
		q.idle[k-1] = nil
		q.idle = q.idle[:k-1]
		q.running++
		return w, false
	}
	if q.running < q.workerLimit() {
		q.running++
		return nil, true
	}
	q.pushBacklogLocked(item)
	return nil, false
}

func (q *taskQueue[T]) pushBacklogLocked(item taskItem[T]) {
	wasEmpty := q.backlog.len() == 0
	q.backlog.push(item)
	atomic.AddInt64(&q.backlogAtomic, 1)
	if wasEmpty {
		q.spillLastCompleted = atomic.LoadInt64(&q.completedAtomic)
		q.spillCheckNano = time.Now().UnixNano()
		if !q.stallMonitor {
			q.stallMonitor = true
			go q.stallLoop()
		}
	}
}

func (q *taskQueue[T]) popBacklogLocked() (taskItem[T], bool) {
	item, ok := q.backlog.pop()
	if ok {
		atomic.AddInt64(&q.backlogAtomic, -1)
		if q.backlog.n == 0 {
			q.spillCheckNano = 0
		}
	}
	return item, ok
}

// Len returns the number of tasks queued but not yet started.
func (q *taskQueue[T]) Len() int {
	q.mu.Lock()
	length := q.backlog.len()
	q.mu.Unlock()
	return length + int(atomic.LoadInt64(&q.prefetchedAtomic))
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
	idle := q.idle
	q.idle = nil
	q.mu.Unlock()

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
	defer q.mu.Unlock()
	return q.backlog.len()+q.running == 0
}

// worker is the main loop of a worker goroutine.
//
// On the hot path it claims a small batch under the mutex and executes the
// remaining batch items without a channel or another queue lock. It parks on
// its own channel to wait for reuse or reclamation when the queue is empty.
func (q *taskQueue[T]) worker(w *worker[T], item taskItem[T]) {
	n := 0
	var batch [workerBatchSize]taskItem[T]
	batchN, batchI := 0, 0
	fromBatch := false
	for {
		if fromBatch {
			atomic.AddInt64(&q.prefetchedAtomic, -1)
			fromBatch = false
		}
		var taskStart time.Time
		if n&63 == 0 {
			taskStart = time.Now()
		}
		q.exec(item.value)
		atomic.AddInt64(&q.completedAtomic, 1)
		if !taskStart.IsZero() {
			q.observeTask(time.Since(taskStart))
		}
		if item.submitted {
			q.releaseSubmitted()
		}
		n++
		capReached := q.opt.maxJobs > 0 && n >= q.opt.maxJobs
		if batchI < batchN {
			item = batch[batchI]
			batch[batchI] = taskItem[T]{}
			batchI++
			fromBatch = true
			continue
		}
		batchN, batchI = 0, 0

		q.mu.Lock()
		if next, ok := q.popBacklogLocked(); ok {
			if capReached && !q.stopped {
				// The per-worker task cap was reached: hand the next task to a
				// fresh worker (with a fresh stack) and exit. The running count
				// is unchanged, as one leaves and one starts.
				q.mu.Unlock()
				q.spawn(q.acquire(), next.value, next.submitted)
				q.release(w)
				return
			}
			batch[0] = next
			batchN = 1
			limit := workerBatchSize
			if q.opt.maxJobs > 0 && q.opt.maxJobs-n < limit {
				limit = q.opt.maxJobs - n
			}
			for batchN < limit {
				next, ok := q.popBacklogLocked()
				if !ok {
					break
				}
				batch[batchN] = next
				batchN++
			}
			atomic.AddInt64(&q.prefetchedAtomic, int64(batchN-1))
			q.mu.Unlock()
			item = batch[0]
			batch[0] = taskItem[T]{}
			batchI = 1
			continue
		}

		// The queue is empty.
		if q.stopped || capReached || q.opt.maxIdle <= 0 {
			q.running--
			q.mu.Unlock()
			q.release(w)
			return
		}

		// Park: move from running to idle and wait to be woken or reclaimed.
		q.running--
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
	q.retireWorker()
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
