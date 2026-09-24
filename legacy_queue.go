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

package taskgo

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// legacyWorker is kept separate from the generic worker so Queue's hot path
// passes Job directly to a goroutine without a typed task metadata wrapper.
type legacyWorker struct {
	ch       chan Job
	lastUsed time.Time
}

// Queue is the legacy no-argument task queue. Use NewTask when every task has
// an argument of the same type and the task function can be bound once.
//
// The zero value is not usable; create one with New.
type Queue struct {
	submitPending     int64 // atomically released by workers; admission is serialized by mu
	runningAtomic     int64
	shardCursor       uint64
	popCursor         uint64
	stoppedAtomic     int32
	mu                sync.Mutex
	opt               *options
	backlog           *ring
	shards            []jobShard
	wake              chan struct{}
	dispatchWake      chan struct{}
	dispatchStop      chan struct{}
	dispatcherStarted int32
	ingress           sync.RWMutex
	idle              []*legacyWorker
	pool              sync.Pool
	running           int
	stopped           bool
	janitorRunning    bool

	onSpawnForTest func()
}

// New creates a legacy no-argument task queue configured by opts.
func New(opts ...Option) *Queue {
	o := newOptions(opts)
	q := &Queue{opt: o, backlog: newRing(8)}
	if o.shards > 1 {
		q.shards = newJobShards(o.shards)
		q.wake = make(chan struct{}, o.concurrency)
		q.dispatchWake = make(chan struct{}, 1)
		q.dispatchStop = make(chan struct{})
	}
	return q
}

// Push submits a task. It runs immediately if a worker is available, otherwise
// it is queued. A nil job is ignored, since nil is not a valid legacy task.
func (q *Queue) Push(job Job) {
	if job == nil {
		return
	}
	q.pushJob(job)
}

// PushBatch submits a batch of legacy jobs and returns the number accepted.
// Nil jobs are ignored, matching Push. The non-nil jobs are admitted in input
// order under one queue lock.
func (q *Queue) PushBatch(jobs []Job) int {
	jobs = compactJobs(jobs)
	return q.pushBatch(jobs)
}

// Submit admits a legacy task without blocking. It returns false when the
// queue has been stopped or the optional WithMaxPending limit is full.
func (q *Queue) Submit(job Job) bool {
	if job == nil {
		return false
	}
	if q.opt.maxPending <= 0 {
		return q.pushJob(job)
	}
	return q.submitJob(job)
}

// SubmitBatch submits as many legacy jobs as the pending limit allows and
// returns the number accepted. Nil jobs are ignored, matching Submit.
func (q *Queue) SubmitBatch(jobs []Job) int {
	accepted, _ := q.submitBatch(jobs, false)
	return accepted
}

// TrySubmitBatch atomically submits the entire legacy job batch. Nil jobs are
// ignored, matching Submit. It returns false without admitting any job when
// the queue is stopped or the pending limit cannot hold the batch.
func (q *Queue) TrySubmitBatch(jobs []Job) bool {
	_, accepted := q.submitBatch(jobs, true)
	return accepted
}

func (q *Queue) submitBatch(jobs []Job, atomicBatch bool) (accepted int, allAccepted bool) {
	jobs = compactJobs(jobs)
	if len(jobs) == 0 {
		q.mu.Lock()
		stopped := q.stopped
		q.mu.Unlock()
		return 0, !stopped
	}
	accepted, ok := q.reservePendingBatch(len(jobs), atomicBatch)
	if !ok {
		return 0, false
	}
	submitted := q.opt.maxPending > 0
	if len(q.shards) > 1 && atomic.LoadInt32(&q.stoppedAtomic) == 0 &&
		atomic.LoadInt64(&q.runningAtomic) >= int64(q.opt.concurrency) {
		admitted := 0
		for _, job := range jobs[:accepted] {
			if submitted {
				original := job
				job = func() {
					defer q.releaseSubmitted()
					original()
				}
			}
			if !q.enqueueSharded(job) {
				break
			}
			admitted++
		}
		if submitted && admitted < accepted {
			atomic.AddInt64(&q.submitPending, -int64(accepted-admitted))
		}
		return admitted, admitted == len(jobs)
	}

	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		if submitted {
			atomic.AddInt64(&q.submitPending, -int64(accepted))
		}
		return 0, false
	}
	actions := make([]legacyDispatch, 0, q.dispatchCapacity(accepted))
	for _, job := range jobs[:accepted] {
		if submitted {
			original := job
			job = func() {
				defer q.releaseSubmitted()
				original()
			}
		}
		q.enqueueLocked(job, &actions)
	}
	q.mu.Unlock()
	q.dispatch(actions)
	return accepted, accepted == len(jobs)
}

// Len returns the number of tasks queued but not yet started.
func (q *Queue) Len() int {
	q.mu.Lock()
	length := q.backlog.len()
	q.mu.Unlock()
	for i := range q.shards {
		length += q.shards[i].len()
	}
	return length
}

// Stop shuts the queue down and waits for outstanding tasks to finish.
func (q *Queue) Stop(ctx context.Context) error {
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

	for _, w := range idle {
		w.ch <- nil
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

func (q *Queue) pushJob(job Job) bool {
	if len(q.shards) > 1 && atomic.LoadInt32(&q.stoppedAtomic) == 0 &&
		atomic.LoadInt64(&q.runningAtomic) >= int64(q.opt.concurrency) {
		if !q.enqueueSharded(job) {
			return false
		}
		return true
	}
	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		return false
	}
	w, spawn := q.enqueueSingleLocked(job)
	q.mu.Unlock()
	if w != nil {
		w.ch <- job
	} else if spawn {
		q.spawn(q.acquire(), job)
	}
	return true
}

func (q *Queue) enqueueSharded(job Job) bool {
	q.ingress.RLock()
	defer q.ingress.RUnlock()
	if atomic.LoadInt32(&q.stoppedAtomic) != 0 {
		return false
	}
	s := q.pickJobShard()
	if s.push(job) {
		q.signalDispatcher()
	}
	return true
}

func (q *Queue) signalDispatcher() {
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

func (q *Queue) shardedDispatcher() {
	for {
		select {
		case <-q.dispatchWake:
			q.replenishSharded()
		case <-q.dispatchStop:
			return
		}
	}
}

func (q *Queue) replenishSharded() {
	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		return
	}
	var spawns []legacyDispatch
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
			w.ch <- next
			continue
		}
		q.running++
		atomic.AddInt64(&q.runningAtomic, 1)
		spawns = append(spawns, legacyDispatch{job: next})
	}
	q.mu.Unlock()
	for _, action := range spawns {
		q.spawn(q.acquire(), action.job)
	}
}

func (q *Queue) pickJobShard() *jobShard {
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

func (q *Queue) popSharded() (Job, bool) {
	n := uint64(len(q.shards))
	start := atomic.AddUint64(&q.popCursor, 1) % n
	for i := uint64(0); i < n; i++ {
		if job, ok := q.shards[(start+i)%n].pop(); ok {
			return job, true
		}
	}
	return nil, false
}

func (q *Queue) removeIdleLocked(w *legacyWorker) bool {
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

func (q *Queue) submitJob(job Job) bool {
	if !q.reservePending(1) {
		return false
	}
	original := job
	job = func() {
		defer q.releaseSubmitted()
		original()
	}
	if len(q.shards) > 1 && atomic.LoadInt32(&q.stoppedAtomic) == 0 &&
		atomic.LoadInt64(&q.runningAtomic) >= int64(q.opt.concurrency) {
		if !q.enqueueSharded(job) {
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
	w, spawn := q.enqueueSingleLocked(job)
	q.mu.Unlock()
	if w != nil {
		w.ch <- job
	} else if spawn {
		q.spawn(q.acquire(), job)
	}
	return true
}

func (q *Queue) reservePending(n int) bool {
	_, ok := q.reservePendingBatch(n, true)
	return ok
}

func (q *Queue) reservePendingBatch(n int, atomicBatch bool) (int, bool) {
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

func (q *Queue) enqueueSingleLocked(job Job) (*legacyWorker, bool) {
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
	q.backlog.push(job)
	return nil, false
}

type legacyDispatch struct {
	worker *legacyWorker
	job    Job
}

func (q *Queue) pushBatch(jobs []Job) int {
	if len(jobs) == 0 {
		return 0
	}
	if len(q.shards) > 1 && atomic.LoadInt32(&q.stoppedAtomic) == 0 &&
		atomic.LoadInt64(&q.runningAtomic) >= int64(q.opt.concurrency) {
		accepted := 0
		for _, job := range jobs {
			if !q.enqueueSharded(job) {
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
	actions := make([]legacyDispatch, 0, q.dispatchCapacity(len(jobs)))
	for _, job := range jobs {
		q.enqueueLocked(job, &actions)
	}
	q.mu.Unlock()
	q.dispatch(actions)
	return len(jobs)
}

func (q *Queue) enqueueLocked(job Job, actions *[]legacyDispatch) {
	if k := len(q.idle); k > 0 {
		w := q.idle[k-1]
		q.idle[k-1] = nil
		q.idle = q.idle[:k-1]
		q.running++
		if len(q.shards) > 1 {
			atomic.AddInt64(&q.runningAtomic, 1)
		}
		*actions = append(*actions, legacyDispatch{worker: w, job: job})
		return
	}
	if q.running < q.opt.concurrency {
		q.running++
		if len(q.shards) > 1 {
			atomic.AddInt64(&q.runningAtomic, 1)
		}
		*actions = append(*actions, legacyDispatch{job: job})
		return
	}
	q.backlog.push(job)
}

func (q *Queue) dispatchCapacity(n int) int {
	if n > q.opt.concurrency {
		return q.opt.concurrency
	}
	return n
}

func (q *Queue) dispatch(actions []legacyDispatch) {
	for _, action := range actions {
		if action.worker != nil {
			action.worker.ch <- action.job
		} else {
			q.spawn(q.acquire(), action.job)
		}
	}
}

func compactJobs(jobs []Job) []Job {
	valid := 0
	for _, job := range jobs {
		if job != nil {
			valid++
		}
	}
	if valid == len(jobs) {
		return jobs
	}
	compact := make([]Job, 0, valid)
	for _, job := range jobs {
		if job != nil {
			compact = append(compact, job)
		}
	}
	return compact
}

func (q *Queue) releaseSubmitted() {
	atomic.AddInt64(&q.submitPending, -1)
}

func (q *Queue) finished() bool {
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

func (q *Queue) worker(w *legacyWorker, job Job) {
	if len(q.shards) > 1 {
		q.workerSharded(w, job)
		return
	}
	n := 0
	for {
		q.exec(job)
		n++
		capReached := q.opt.maxJobs > 0 && n >= q.opt.maxJobs

		q.mu.Lock()
		if next, ok := q.backlog.pop(); ok {
			if capReached && atomic.LoadInt32(&q.stoppedAtomic) == 0 {
				q.mu.Unlock()
				q.spawn(q.acquire(), next)
				q.release(w)
				return
			}
			q.mu.Unlock()
			job = next
			continue
		}

		if q.stopped || capReached || q.opt.maxIdle <= 0 {
			q.running--
			if len(q.shards) > 1 {
				atomic.AddInt64(&q.runningAtomic, -1)
			}
			q.mu.Unlock()
			q.release(w)
			return
		}

		q.running--
		if len(q.shards) > 1 {
			atomic.AddInt64(&q.runningAtomic, -1)
		}
		w.lastUsed = q.opt.nowFn()
		q.idle = append(q.idle, w)
		q.ensureJanitorLocked()
		q.mu.Unlock()

		job = <-w.ch
		if job == nil {
			q.release(w)
			return
		}
		n = 0
	}
}

func (q *Queue) workerSharded(w *legacyWorker, job Job) {
	n := 0
	for {
		q.exec(job)
		n++
		capReached := q.opt.maxJobs > 0 && n >= q.opt.maxJobs
		if n&15 == 0 {
			q.mu.Lock()
			if next, ok := q.backlog.pop(); ok {
				if capReached && !q.stopped {
					q.mu.Unlock()
					q.spawn(q.acquire(), next)
					q.release(w)
					return
				}
				q.mu.Unlock()
				job = next
				continue
			}
			q.mu.Unlock()
		}

		if next, ok := q.popSharded(); ok {
			if capReached && atomic.LoadInt32(&q.stoppedAtomic) == 0 {
				q.spawn(q.acquire(), next)
				q.release(w)
				return
			}
			job = next
			continue
		}

		q.mu.Lock()
		if next, ok := q.backlog.pop(); ok {
			if capReached && !q.stopped {
				q.mu.Unlock()
				q.spawn(q.acquire(), next)
				q.release(w)
				return
			}
			q.mu.Unlock()
			job = next
			continue
		}
		if next, ok := q.popSharded(); ok {
			if capReached && atomic.LoadInt32(&q.stoppedAtomic) == 0 {
				q.mu.Unlock()
				q.spawn(q.acquire(), next)
				q.release(w)
				return
			}
			q.mu.Unlock()
			job = next
			continue
		}

		if q.stopped || capReached || q.opt.maxIdle <= 0 {
			q.signalDispatcher()
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

		for {
			select {
			case next := <-w.ch:
				if next == nil {
					q.release(w)
					return
				}
				job = next
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
					job = next
					n = 0
					goto nextTask
				}
				q.mu.Lock()
				if next, ok := q.backlog.pop(); ok {
					q.mu.Unlock()
					job = next
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

func (q *Queue) exec(job Job) {
	if q.opt.panicFn != nil {
		defer func() {
			if v := recover(); v != nil {
				q.opt.panicFn(v)
			}
		}()
	}
	job()
}

func (q *Queue) acquire() *legacyWorker {
	if v := q.pool.Get(); v != nil {
		return v.(*legacyWorker)
	}
	return &legacyWorker{ch: make(chan Job, 1)}
}

func (q *Queue) spawn(w *legacyWorker, job Job) {
	if q.onSpawnForTest != nil {
		q.onSpawnForTest()
	}
	go q.worker(w, job)
}

func (q *Queue) release(w *legacyWorker) {
	w.lastUsed = time.Time{}
	q.pool.Put(w)
}

func (q *Queue) ensureJanitorLocked() {
	if q.opt.manualJanitor {
		return
	}
	if !q.janitorRunning {
		q.janitorRunning = true
		go q.janitor()
	}
}

func (q *Queue) janitor() {
	for {
		time.Sleep(q.opt.maxIdle)
		if q.cleanExpired(q.opt.nowFn()) {
			return
		}
	}
}

func (q *Queue) cleanExpired(now time.Time) (stop bool) {
	q.mu.Lock()
	var expired []*legacyWorker
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
		w.ch <- nil
	}
	return stop
}
