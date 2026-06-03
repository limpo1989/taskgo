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

// ring is a non-thread-safe circular FIFO queue with amortized O(1) push and
// pop. It is only accessed while Queue.mu is held.
type ring struct {
	buf  []Job
	head int
	tail int
	n    int
}

// newRing returns a ring with the given initial capacity, clamped to at least 1.
func newRing(capacity int) *ring {
	if capacity < 1 {
		capacity = 1
	}
	return &ring{buf: make([]Job, capacity)}
}

// len returns the number of buffered jobs.
func (r *ring) len() int { return r.n }

// push appends a job, growing the buffer if it is full.
func (r *ring) push(j Job) {
	if r.n == len(r.buf) {
		r.grow()
	}
	r.buf[r.tail] = j
	r.tail++
	if r.tail == len(r.buf) {
		r.tail = 0
	}
	r.n++
}

// pop removes and returns the oldest job. The second result is false when the
// ring is empty.
func (r *ring) pop() (Job, bool) {
	if r.n == 0 {
		return nil, false
	}
	j := r.buf[r.head]
	r.buf[r.head] = nil
	r.head++
	if r.head == len(r.buf) {
		r.head = 0
	}
	r.n--
	return j, true
}

// grow doubles the buffer, re-laying the jobs out from index 0.
func (r *ring) grow() {
	nbuf := make([]Job, len(r.buf)*2)
	for i := 0; i < r.n; i++ {
		nbuf[i] = r.buf[(r.head+i)%len(r.buf)]
	}
	r.head = 0
	r.tail = r.n
	r.buf = nbuf
}
