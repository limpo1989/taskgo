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
	mu             sync.Mutex
	opt            *options
	backlog        *ring
	idle           []*legacyWorker
	pool           sync.Pool
	running        int
	stopped        bool
	janitorRunning bool

	onSpawnForTest func()
	submitPending  int
}

// New creates a legacy no-argument task queue configured by opts.
func New(opts ...Option) *Queue {
	o := newOptions(opts)
	return &Queue{opt: o, backlog: newRing(8)}
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

	q.mu.Lock()
	if q.stopped || q.submitPending >= q.opt.maxPending {
		q.mu.Unlock()
		return false
	}
	q.submitPending++
	q.mu.Unlock()

	accepted := q.pushJob(func() {
		defer q.releaseSubmitted()
		job()
	})
	if !accepted {
		q.releaseSubmitted()
	}
	return accepted
}

// SubmitBatch submits as many legacy jobs as the pending limit allows and
// returns the number accepted. Nil jobs are ignored, matching Submit.
func (q *Queue) SubmitBatch(jobs []Job) int {
	jobs = compactJobs(jobs)
	if q.opt.maxPending <= 0 {
		return q.pushBatch(jobs)
	}
	accepted := 0
	for _, job := range jobs {
		if !q.Submit(job) {
			break
		}
		accepted++
	}
	return accepted
}

// TrySubmitBatch atomically submits the entire legacy job batch. Nil jobs are
// ignored, matching Submit. It returns false without admitting any job when
// the queue is stopped or the pending limit cannot hold the batch.
func (q *Queue) TrySubmitBatch(jobs []Job) bool {
	jobs = compactJobs(jobs)
	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		return false
	}
	if q.opt.maxPending > 0 {
		if q.opt.maxPending-q.submitPending < len(jobs) {
			q.mu.Unlock()
			return false
		}
		q.submitPending += len(jobs)
	}

	actions := make([]legacyDispatch, 0, q.dispatchCapacity(len(jobs)))
	for _, job := range jobs {
		if q.opt.maxPending > 0 {
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
	return true
}

// Len returns the number of tasks queued but not yet started.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.backlog.len()
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
	if k := len(q.idle); k > 0 {
		w := q.idle[k-1]
		q.idle[k-1] = nil
		q.idle = q.idle[:k-1]
		q.running++
		q.mu.Unlock()
		w.ch <- job
		return true
	}
	if q.running < q.opt.concurrency {
		q.running++
		q.mu.Unlock()
		q.spawn(q.acquire(), job)
		return true
	}
	q.backlog.push(job)
	q.mu.Unlock()
	return true
}

type legacyDispatch struct {
	worker *legacyWorker
	job    Job
}

func (q *Queue) pushBatch(jobs []Job) int {
	if len(jobs) == 0 {
		return 0
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
		*actions = append(*actions, legacyDispatch{worker: w, job: job})
		return
	}
	if q.running < q.opt.concurrency {
		q.running++
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
	q.mu.Lock()
	q.submitPending--
	q.mu.Unlock()
}

func (q *Queue) finished() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.backlog.len()+q.running == 0
}

func (q *Queue) worker(w *legacyWorker, job Job) {
	n := 0
	for {
		q.exec(job)
		n++
		capReached := q.opt.maxJobs > 0 && n >= q.opt.maxJobs

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
