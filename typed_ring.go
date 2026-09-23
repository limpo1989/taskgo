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

// taskRing is a non-thread-safe circular FIFO queue for typed task values.
// It is only accessed while taskQueue.mu is held.
type taskRing[T any] struct {
	buf  []taskItem[T]
	head int
	tail int
	n    int
}

func newTaskRing[T any](capacity int) *taskRing[T] {
	if capacity < 1 {
		capacity = 1
	}
	return &taskRing[T]{buf: make([]taskItem[T], capacity)}
}

func (r *taskRing[T]) len() int { return r.n }

func (r *taskRing[T]) push(item taskItem[T]) {
	if r.n == len(r.buf) {
		r.grow()
	}
	r.buf[r.tail] = item
	r.tail++
	if r.tail == len(r.buf) {
		r.tail = 0
	}
	r.n++
}

func (r *taskRing[T]) pop() (taskItem[T], bool) {
	if r.n == 0 {
		return taskItem[T]{}, false
	}
	item := r.buf[r.head]
	r.buf[r.head] = taskItem[T]{}
	r.head++
	if r.head == len(r.buf) {
		r.head = 0
	}
	r.n--
	return item, true
}

func (r *taskRing[T]) grow() {
	nbuf := make([]taskItem[T], len(r.buf)*2)
	for i := 0; i < r.n; i++ {
		nbuf[i] = r.buf[(r.head+i)%len(r.buf)]
	}
	r.head = 0
	r.tail = r.n
	r.buf = nbuf
}
