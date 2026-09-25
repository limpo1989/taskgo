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

import "context"

// Queue is the legacy no-argument task queue. Use NewTask when every task has
// an argument of the same type and the task function can be bound once.
//
// The zero value is not usable; create one with New.
type Queue struct {
	*taskQueue[Job]
}

// New creates a legacy no-argument task queue configured by opts.
func New(opts ...Option) *Queue {
	return &Queue{taskQueue: newTaskQueue(opts, runJob)}
}

func runJob(job Job) { job() }

// Push submits a task. It runs immediately if a worker is available, otherwise
// it is queued. A nil job is ignored, since nil is not a valid legacy task.
func (q *Queue) Push(job Job) {
	if job == nil {
		return
	}
	q.taskQueue.push(taskItem[Job]{value: job})
}

// PushBatch submits a batch of legacy jobs and returns the number accepted.
// Nil jobs are ignored, matching Push. The non-nil jobs are admitted in input
// order.
func (q *Queue) PushBatch(jobs []Job) int {
	return q.taskQueue.pushValues(compactJobs(jobs), false)
}

// Submit admits a legacy task without blocking. It returns false when the
// queue has been stopped or the optional WithMaxPending limit is full.
func (q *Queue) Submit(job Job) bool {
	if job == nil {
		return false
	}
	return q.taskQueue.submit(job)
}

// SubmitBatch submits as many legacy jobs as the pending limit allows and
// returns the number accepted. Nil jobs are ignored, matching Submit.
func (q *Queue) SubmitBatch(jobs []Job) int {
	accepted, _ := q.taskQueue.submitBatch(compactJobs(jobs), false)
	return accepted
}

// TrySubmitBatch atomically submits the entire legacy job batch. Nil jobs are
// ignored, matching Submit. It returns false without admitting any job when
// the queue is stopped or the pending limit cannot hold the batch.
func (q *Queue) TrySubmitBatch(jobs []Job) bool {
	_, accepted := q.taskQueue.submitBatch(compactJobs(jobs), true)
	return accepted
}

// Len returns the number of tasks queued but not yet started.
func (q *Queue) Len() int { return q.taskQueue.Len() }

// Stop shuts the queue down and waits for outstanding tasks to finish.
func (q *Queue) Stop(ctx context.Context) error { return q.taskQueue.Stop(ctx) }

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
