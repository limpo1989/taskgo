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
	"sync/atomic"
	"testing"
	"time"
)

// This file holds test helpers shared across the various *_test.go files.

// waitFor polls until cond is true or the timeout elapses. It is used to wait
// for asynchronous state to settle.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}

// idleLen returns the number of currently parked workers (test only).
func (q *taskQueue[T]) idleLen() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.idle)
}

// runningCount returns the number of workers not parked (test only).
func (q *taskQueue[T]) runningCount() int {
	return int(atomic.LoadInt64(&q.running))
}

// liveCount returns running+idle workers: the live worker goroutines, which is
// exactly the gauge Prometheus would see for this pool (test only).
func (q *taskQueue[T]) liveCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.live
}

// setBaseForTest pins the soft worker target, as a small GOMAXPROCS would.
func (q *taskQueue[T]) setBaseForTest(n int64) {
	q.mu.Lock()
	q.base = n
	atomic.StoreInt64(&q.target, n)
	q.mu.Unlock()
}

// fib is a shallow-stack compute load shared by tests and benchmarks.
func fib(n int) int {
	if n < 2 {
		return n
	}
	return fib(n-1) + fib(n-2)
}
