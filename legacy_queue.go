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
	backlog            *ring
	idle               []*legacyWorker
	pool               sync.Pool
	running            int
	stopped            bool
	janitorRunning     bool
	stallMonitor       bool

	onSpawnForTest func()
}

// New creates a legacy no-argument task queue configured by opts.
func New(opts ...Option) *Queue {
	o := newOptions(opts)
	target := autoWorkerLimit(o.concurrency)
	return &Queue{opt: o, backlog: newRing(backlogCapacityHint(target)), workerTarget: int64(target), initialWorkers: int64(target)}
}

func (q *Queue) workerLimit() int { return int(atomic.LoadInt64(&q.workerTarget)) }

func (q *Queue) observeTask(d time.Duration) {
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

func (q *Queue) retireWorker() {
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

func (q *Queue) maybeSpillLocked(now time.Time) {
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
		q.spawn(q.acquire(), next)
	}
}

func (q *Queue) stallLoop() {
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
	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		return 0, false
	}
	accepted = len(jobs)
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
		for _, job := range jobs[:accepted] {
			if submitted {
				original := job
				job = func() {
					defer q.releaseSubmitted()
					original()
				}
			}
			w, spawn := q.enqueueSingleLocked(job)
			if w != nil {
				w.ch <- job
			} else if spawn {
				q.spawn(q.acquire(), job)
			}
		}
		q.mu.Unlock()
		return accepted, true
	}
	q.backlog.reserve(accepted)
	q.mu.Unlock()

	submitted := q.opt.maxPending > 0
	admitted := 0
	for _, job := range jobs[:accepted] {
		if q.opt.maxPending > 0 {
			original := job
			job = func() {
				defer q.releaseSubmitted()
				original()
			}
		}
		if !q.enqueueReservedJob(job) {
			break
		}
		admitted++
	}
	if submitted && admitted < accepted {
		atomic.AddInt64(&q.submitPending, -int64(accepted-admitted))
	}
	return admitted, admitted == len(jobs)
}

// Len returns the number of tasks queued but not yet started.
func (q *Queue) Len() int {
	q.mu.Lock()
	length := q.backlog.len()
	q.mu.Unlock()
	return length + int(atomic.LoadInt64(&q.prefetchedAtomic))
}

// Stop shuts the queue down and waits for outstanding tasks to finish.
func (q *Queue) Stop(ctx context.Context) error {
	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		return nil
	}
	q.stopped = true
	idle := q.idle
	q.idle = nil
	q.mu.Unlock()

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

func (q *Queue) submitJob(job Job) bool {
	q.mu.Lock()
	if q.stopped || atomic.LoadInt64(&q.submitPending) >= int64(q.opt.maxPending) {
		q.mu.Unlock()
		return false
	}
	atomic.AddInt64(&q.submitPending, 1)
	original := job
	job = func() {
		defer q.releaseSubmitted()
		original()
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

func (q *Queue) enqueueReservedJob(job Job) bool {
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

func (q *Queue) enqueueSingleLocked(job Job) (*legacyWorker, bool) {
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
	q.pushBacklogLocked(job)
	return nil, false
}

func (q *Queue) pushBatch(jobs []Job) int {
	if len(jobs) == 0 {
		return 0
	}
	accepted := 0
	for _, job := range jobs {
		if !q.pushJob(job) {
			break
		}
		accepted++
	}
	return accepted
}

func (q *Queue) pushBacklogLocked(job Job) {
	wasEmpty := q.backlog.len() == 0
	q.backlog.push(job)
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

func (q *Queue) popBacklogLocked() (Job, bool) {
	job, ok := q.backlog.pop()
	if ok {
		atomic.AddInt64(&q.backlogAtomic, -1)
		if q.backlog.len() == 0 {
			q.spillCheckNano = 0
		}
	}
	return job, ok
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
	defer q.mu.Unlock()
	return q.backlog.len()+q.running == 0
}

func (q *Queue) worker(w *legacyWorker, job Job) {
	n := 0
	var batch [workerBatchSize]Job
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
		q.exec(job)
		atomic.AddInt64(&q.completedAtomic, 1)
		if !taskStart.IsZero() {
			q.observeTask(time.Since(taskStart))
		}
		n++
		capReached := q.opt.maxJobs > 0 && n >= q.opt.maxJobs
		if batchI < batchN {
			job = batch[batchI]
			batch[batchI] = nil
			batchI++
			fromBatch = true
			continue
		}
		batchN, batchI = 0, 0

		q.mu.Lock()
		if next, ok := q.popBacklogLocked(); ok {
			if capReached && !q.stopped {
				q.mu.Unlock()
				q.spawn(q.acquire(), next)
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
			job = batch[0]
			batch[0] = nil
			batchI = 1
			continue
		}

		if q.stopped || capReached || q.opt.maxIdle <= 0 {
			q.running--
			q.mu.Unlock()
			q.release(w)
			return
		}

		q.running--
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
	q.retireWorker()
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
