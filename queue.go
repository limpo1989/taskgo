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
	"time"
)

// Job is a unit of work with no arguments and no return value.
type Job func()

// stopCheckInterval is how often Stop polls for completion while draining.
const stopCheckInterval = 100 * time.Millisecond

// worker holds a wake-up channel and the time it last became idle.
type worker struct {
	// ch has a buffer of 1. Sending a job wakes the worker; sending nil tells
	// it to exit.
	ch chan Job
	// lastUsed records when the worker entered the idle set, so the janitor can
	// decide whether it has expired.
	lastUsed time.Time
}

// Queue is a concurrency-limited task queue. The zero value is not usable;
// create one with New.
type Queue struct {
	mu             sync.Mutex
	opt            *options
	backlog        *ring     // tasks queued (FIFO) while all workers are busy
	idle           []*worker // parked workers (LIFO, to reuse the hottest stack)
	pool           sync.Pool // recycles worker objects to avoid channel allocs
	running        int       // workers currently executing a task (parked excluded)
	stopped        bool
	janitorRunning bool

	onSpawnForTest func() // test hook: invoked whenever a worker goroutine starts
	submitPending  int    // outstanding jobs admitted through Submit
}

// New creates a task queue configured by opts.
func New(opts ...Option) *Queue {
	o := newOptions(opts)
	return &Queue{
		opt:     o,
		backlog: newRing(8),
	}
}

// Push submits a task. It runs immediately if a worker is available, otherwise
// it is queued. A nil job is ignored, since nil is used internally as a
// worker's exit signal.
func (q *Queue) Push(job Job) {
	if job == nil {
		return
	}

	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		return
	}

	// 1) A parked worker exists: wake it to reuse its grown stack, taking the
	// most recently used one (LIFO).
	if k := len(q.idle); k > 0 {
		w := q.idle[k-1]
		q.idle[k-1] = nil
		q.idle = q.idle[:k-1]
		q.running++
		q.mu.Unlock()
		w.ch <- job // buffered channel with a receiver waiting; never blocks
		return
	}

	// 2) No parked worker and the concurrency limit is not reached: start one.
	if q.running < q.opt.concurrency {
		q.running++
		q.mu.Unlock()
		q.spawn(q.acquire(), job)
		return
	}

	// 3) At capacity: enqueue and let a looping worker pick it up.
	q.backlog.push(job)
	q.mu.Unlock()
}

func (q *Queue) push(job Job) bool {
	if job == nil {
		return false
	}

	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		return false
	}

	// 1) A parked worker exists: wake it to reuse its grown stack, taking the
	// most recently used one (LIFO).
	if k := len(q.idle); k > 0 {
		w := q.idle[k-1]
		q.idle[k-1] = nil
		q.idle = q.idle[:k-1]
		q.running++
		q.mu.Unlock()
		w.ch <- job // buffered channel with a receiver waiting; never blocks
		return true
	}

	// 2) No parked worker and the concurrency limit is not reached: start one.
	if q.running < q.opt.concurrency {
		q.running++
		q.mu.Unlock()
		q.spawn(q.acquire(), job)
		return true
	}

	// 3) At capacity: enqueue and let a looping worker pick it up.
	q.backlog.push(job)
	q.mu.Unlock()
	return true
}

// Submit admits a task without blocking. It returns false when the queue has
// been stopped or the optional WithMaxPending limit is full. Unlike Push, the
// admitted task count includes both running and queued jobs, so a caller can
// use Submit as a bounded executor primitive.
func (q *Queue) Submit(job func()) bool {
	if job == nil {
		return false
	}
	if q.opt.maxPending <= 0 {
		return q.push(job)
	}

	q.mu.Lock()
	if q.stopped || q.submitPending >= q.opt.maxPending {
		q.mu.Unlock()
		return false
	}
	q.submitPending++
	q.mu.Unlock()

	accepted := q.push(func() {
		defer q.releaseSubmitted()
		job()
	})
	if !accepted {
		q.releaseSubmitted()
	}
	return accepted
}

func (q *Queue) releaseSubmitted() {
	q.mu.Lock()
	q.submitPending--
	q.mu.Unlock()
}

// Len returns the number of tasks queued but not yet started.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.backlog.len()
}

// Stop shuts the queue down and waits for outstanding tasks to finish.
//
// After Stop is called, further Push calls are dropped. Queued and in-flight
// tasks still run to completion, until they all finish or the deadline implied
// by ctx and WithTimeout is reached. Stop returns nil on a clean drain, or the
// context error on timeout or cancellation.
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

	// Dismiss every parked worker; each is blocked on <-w.ch.
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
			// Loop back and let finished() do the check.
		case <-ctx2.Done():
			// A task may finish at the exact moment of timeout; treat that as a
			// clean drain rather than an error.
			if q.finished() {
				return nil
			}
			return ctx2.Err()
		}
	}
}

// finished reports whether nothing is queued or running.
func (q *Queue) finished() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.backlog.len()+q.running == 0
}

// worker is the main loop of a worker goroutine.
//
// On the hot path (the queue is non-empty and tasks are pulled back to back) it
// takes only the mutex and uses no channel. It parks on its own channel to wait
// for reuse or reclamation only when the queue is empty and parking is enabled.
func (q *Queue) worker(w *worker, job Job) {
	n := 0
	for {
		q.exec(job)
		n++
		capReached := q.opt.maxJobs > 0 && n >= q.opt.maxJobs

		q.mu.Lock()
		if next, ok := q.backlog.pop(); ok {
			if capReached && !q.stopped {
				// The per-worker task cap was reached: hand the next task to a
				// fresh worker (with a fresh stack) and exit. The running count
				// is unchanged, as one leaves and one starts.
				q.mu.Unlock()
				q.spawn(q.acquire(), next)
				q.release(w)
				return
			}
			q.mu.Unlock()
			job = next
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

		job = <-w.ch
		if job == nil { // exit signal, from the janitor's timeout or from Stop
			q.release(w)
			return
		}
		// Woken from parking: the GC may have shrunk the stack meanwhile, so
		// reset the counter to avoid a premature maxJobs trigger.
		n = 0
	}
}

// exec runs a single job, recovering from a panic when a handler is configured.
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

// acquire returns a pooled worker, allocating a new one if the pool is empty.
func (q *Queue) acquire() *worker {
	if v := q.pool.Get(); v != nil {
		return v.(*worker)
	}
	return &worker{ch: make(chan Job, 1)}
}

// spawn starts a new worker goroutine. The caller is responsible for having
// already accounted for it in the running count.
func (q *Queue) spawn(w *worker, job Job) {
	if q.onSpawnForTest != nil {
		q.onSpawnForTest()
	}
	go q.worker(w, job)
}

// release returns a worker to the pool for later reuse.
func (q *Queue) release(w *worker) {
	w.lastUsed = time.Time{}
	q.pool.Put(w)
}

// ensureJanitorLocked starts the background reaper if it is not already
// running. It must be called while holding mu.
func (q *Queue) ensureJanitorLocked() {
	if q.opt.manualJanitor {
		return
	}
	if !q.janitorRunning {
		q.janitorRunning = true
		go q.janitor()
	}
}

// janitor periodically reclaims workers that have been idle too long. It exits
// once the idle set is empty and is restarted the next time a worker parks.
func (q *Queue) janitor() {
	for {
		time.Sleep(q.opt.maxIdle)
		if q.cleanExpired(q.opt.nowFn()) {
			return
		}
	}
}

// cleanExpired reclaims every worker idle for at least maxIdle. It reports
// whether the janitor should stop, which is the case when the idle set is empty.
func (q *Queue) cleanExpired(now time.Time) (stop bool) {
	q.mu.Lock()
	var expired []*worker
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
