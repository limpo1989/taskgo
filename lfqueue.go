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
	"runtime"
	"sync/atomic"
	"unsafe"
)

// cacheLinePad separates fields written by different goroutines. 128 bytes
// also covers the adjacent-line prefetcher on amd64 and the 128-byte lines of
// Apple silicon.
const cacheLinePad = 128

// ringClosed marks a ring's tail once no further ticket may be claimed in it.
const ringClosed = uint64(1) << 63

// publishSpins bounds how long a consumer waits for a producer that has
// claimed the head ticket but not yet published its value. The window is a
// few instructions long, so the wait is almost always a handful of loads.
const publishSpins = 64

// casRetries is how many CAS races a goroutine loses in a row before it
// yields its P. Under heavy contention that lets the winners finish instead
// of every core bouncing the same cache line.
const casRetries = 4

// backoff counts a lost CAS race and yields after casRetries of them.
func backoff(fails *int) {
	if *fails++; *fails >= casRetries {
		*fails = 0
		runtime.Gosched()
	}
}

// lfQueue is an unbounded multi-producer multi-consumer FIFO queue.
//
// It is a chain of fixed-size rings. Every enqueued item takes a global
// ticket, which is also its position in the ring holding it, so tickets
// increase monotonically across the whole chain and tail-head is the length.
// Within a ring each slot carries a sequence number (Vyukov's bounded MPMC
// design): a slot is free for ticket t when its sequence is t, and holds the
// published value of ticket t when its sequence is t+1. A producer that finds
// its ring full closes it and appends a ring of twice the size, so consumers
// drain the old ring before moving on and FIFO order holds across rings.
//
// Neither side takes a lock, so a producer is never parked by a consumer and
// vice versa. The only waiting is a consumer spinning briefly on a ticket that
// a producer has claimed but not yet published.
type lfQueue[T any] struct {
	head unsafe.Pointer // *ring[T]: the ring consumers dequeue from
	_    [cacheLinePad - unsafe.Sizeof(unsafe.Pointer(nil))]byte
	tail unsafe.Pointer // *ring[T]: the ring producers enqueue into
	_    [cacheLinePad - unsafe.Sizeof(unsafe.Pointer(nil))]byte
	// minCapacity is the size of the first ring, and of the ring shrink
	// swaps in for an emptied one.
	minCapacity int
}

type ring[T any] struct {
	head uint64 // next ticket to dequeue
	_    [cacheLinePad - 8]byte
	tail uint64 // next ticket to enqueue, plus ringClosed once sealed
	_    [cacheLinePad - 8]byte
	next unsafe.Pointer // *ring[T], published once by the producer that closed this ring
	mask uint64
	// seq is kept apart from vals so every sequence number is a naturally
	// aligned uint64, which 64-bit atomics require on 32-bit platforms.
	seq  []uint64
	vals []T
}

func newRingAt[T any](capacity int, base uint64) *ring[T] {
	size := 1
	for size < capacity {
		size <<= 1
	}
	r := &ring[T]{
		head: base,
		tail: base,
		mask: uint64(size - 1),
		seq:  make([]uint64, size),
		vals: make([]T, size),
	}
	for i := uint64(0); i < uint64(size); i++ {
		ticket := base + i
		r.seq[ticket&r.mask] = ticket
	}
	return r
}

func (q *lfQueue[T]) init(capacity int) {
	r := newRingAt[T](capacity, 0)
	q.minCapacity = len(r.vals)
	q.head = unsafe.Pointer(r)
	q.tail = unsafe.Pointer(r)
}

// shrink replaces the ring with one of minimum size when it is empty and
// holds more than keep slots, so a burst does not pin its ring for the life
// of the queue. It closes the ring the same way a full one is closed, so
// concurrent producers simply move on to the new ring.
func (q *lfQueue[T]) shrink(keep int) {
	r := q.headRing()
	if r != q.tailRing() || len(r.vals) <= keep {
		return
	}
	h := atomic.LoadUint64(&r.head)
	if atomic.LoadUint64(&r.tail) != h || !atomic.CompareAndSwapUint64(&r.tail, h, h|ringClosed) {
		return
	}
	next := newRingAt[T](q.minCapacity, h)
	atomic.StorePointer(&r.next, unsafe.Pointer(next))
	atomic.CompareAndSwapPointer(&q.tail, unsafe.Pointer(r), unsafe.Pointer(next))
	atomic.CompareAndSwapPointer(&q.head, unsafe.Pointer(r), unsafe.Pointer(next))
}

func (q *lfQueue[T]) headRing() *ring[T] { return (*ring[T])(atomic.LoadPointer(&q.head)) }
func (q *lfQueue[T]) tailRing() *ring[T] { return (*ring[T])(atomic.LoadPointer(&q.tail)) }

// followTail moves the shared tail pointer past a closed ring. The producer
// that closed r publishes r.next right after closing it, so the wait is short.
func (q *lfQueue[T]) followTail(r *ring[T]) {
	next := atomic.LoadPointer(&r.next)
	if next == nil {
		runtime.Gosched()
		return
	}
	atomic.CompareAndSwapPointer(&q.tail, unsafe.Pointer(r), next)
}

// push enqueues one value.
func (q *lfQueue[T]) push(v T) {
	fails := 0
	for {
		r := q.tailRing()
		t := atomic.LoadUint64(&r.tail)
		if t&ringClosed != 0 {
			q.followTail(r)
			continue
		}
		i := t & r.mask
		s := atomic.LoadUint64(&r.seq[i])
		switch {
		case s == t:
			if atomic.CompareAndSwapUint64(&r.tail, t, t+1) {
				r.vals[i] = v
				atomic.StoreUint64(&r.seq[i], t+1)
				return
			}
			backoff(&fails)
		case int64(s-t) < 0:
			// The slot still holds ticket t-cap: the ring is full.
			if atomic.CompareAndSwapUint64(&r.tail, t, t|ringClosed) {
				q.grow(r, t, 1, func(int) T { return v })
				return
			}
		}
	}
}

// pushBatch enqueues values in order.
func (q *lfQueue[T]) pushBatch(values []T) {
	q.pushN(len(values), func(i int) T { return values[i] })
}

// pushN enqueues the n values at(0) through at(n-1) in order. Consecutive
// tickets are claimed with a single CAS whenever the ring has room for them.
func (q *lfQueue[T]) pushN(n int, at func(int) T) {
	fails := 0
	for next := 0; next < n; {
		r := q.tailRing()
		t := atomic.LoadUint64(&r.tail)
		if t&ringClosed != 0 {
			q.followTail(r)
			continue
		}
		want := uint64(n - next)
		free := uint64(0)
		for free < want && free <= r.mask {
			if atomic.LoadUint64(&r.seq[(t+free)&r.mask]) != t+free {
				break
			}
			free++
		}
		if free == 0 {
			if int64(atomic.LoadUint64(&r.seq[t&r.mask])-t) < 0 &&
				atomic.CompareAndSwapUint64(&r.tail, t, t|ringClosed) {
				q.grow(r, t, n-next, func(i int) T { return at(next + i) })
				return
			}
			continue
		}
		if !atomic.CompareAndSwapUint64(&r.tail, t, t+free) {
			backoff(&fails)
			continue
		}
		for k := uint64(0); k < free; k++ {
			i := (t + k) & r.mask
			r.vals[i] = at(next + int(k))
			atomic.StoreUint64(&r.seq[i], t+k+1)
		}
		next += int(free)
	}
}

// grow runs on the producer that closed r at ticket t. It links a larger ring
// starting at ticket t that already holds the n values at(0) to at(n-1), so
// they keep their order and precede anything enqueued after the new ring
// becomes visible.
func (q *lfQueue[T]) grow(r *ring[T], t uint64, n int, at func(int) T) {
	capacity := 2 * len(r.vals)
	for capacity < 2*n {
		capacity *= 2
	}
	next := newRingAt[T](capacity, t)
	for k := 0; k < n; k++ {
		ticket := t + uint64(k)
		i := ticket & next.mask
		next.vals[i] = at(k)
		next.seq[i] = ticket + 1
	}
	next.tail = t + uint64(n)
	atomic.StorePointer(&r.next, unsafe.Pointer(next))
	atomic.CompareAndSwapPointer(&q.tail, unsafe.Pointer(r), unsafe.Pointer(next))
}

// pop dequeues the oldest published value. It reports false when the queue is
// empty, or when the oldest ticket is claimed but still unpublished after a
// short wait; that producer checks for parked workers after publishing, so a
// value reported missing this way is never stranded.
func (q *lfQueue[T]) pop() (v T, ok bool) {
	var one [1]T
	if n, _ := q.popBatch(one[:]); n == 1 {
		return one[0], true
	}
	return v, false
}

// popBatch dequeues up to len(dst) consecutive published values with a single
// CAS. It returns how many it took, and whether it lost a race to another
// consumer on the way, which callers use to size their next batch. Like pop,
// it reports nothing when the head ticket is still unpublished.
func (q *lfQueue[T]) popBatch(dst []T) (n int, contended bool) {
	var zero T
	spins, fails := 0, 0
	for {
		r := q.headRing()
		h := atomic.LoadUint64(&r.head)
		k := uint64(0)
		for k < uint64(len(dst)) && atomic.LoadUint64(&r.seq[(h+k)&r.mask]) == h+k+1 {
			k++
		}
		if k > 0 {
			if !atomic.CompareAndSwapUint64(&r.head, h, h+k) {
				contended = true
				backoff(&fails)
				continue
			}
			for j := uint64(0); j < k; j++ {
				i := (h + j) & r.mask
				dst[j] = r.vals[i]
				r.vals[i] = zero
				atomic.StoreUint64(&r.seq[i], h+j+r.mask+1)
			}
			return int(k), contended
		}
		s := atomic.LoadUint64(&r.seq[h&r.mask])
		if int64(s-(h+1)) > 0 {
			contended = true // another consumer took ticket h
			continue
		}
		t := atomic.LoadUint64(&r.tail)
		if t&^ringClosed == h {
			if t&ringClosed == 0 {
				return 0, contended
			}
			// Closed and drained: continue in the next ring.
			next := atomic.LoadPointer(&r.next)
			if next == nil {
				runtime.Gosched()
				continue
			}
			atomic.CompareAndSwapPointer(&q.head, unsafe.Pointer(r), next)
			continue
		}
		if spins++; spins > publishSpins {
			return 0, contended
		}
	}
}

// ready reports whether pop would find a published value at the head.
func (q *lfQueue[T]) ready() bool {
	for {
		r := q.headRing()
		h := atomic.LoadUint64(&r.head)
		s := atomic.LoadUint64(&r.seq[h&r.mask])
		if s == h+1 {
			return true
		}
		if int64(s-(h+1)) > 0 {
			continue // another consumer took ticket h; look again
		}
		t := atomic.LoadUint64(&r.tail)
		if t&^ringClosed != h || t&ringClosed == 0 {
			return false
		}
		next := atomic.LoadPointer(&r.next)
		if next == nil {
			return false
		}
		atomic.CompareAndSwapPointer(&q.head, unsafe.Pointer(r), next)
	}
}

// enqueued returns how many tickets have been claimed since the queue was
// created, which is how many values were ever enqueued or are being enqueued.
func (q *lfQueue[T]) enqueued() uint64 {
	return atomic.LoadUint64(&q.tailRing().tail) &^ ringClosed
}

// len returns the number of claimed tickets not yet dequeued. It is a snapshot
// and may be stale by the time it returns.
func (q *lfQueue[T]) len() int {
	h := atomic.LoadUint64(&q.headRing().head)
	t := atomic.LoadUint64(&q.tailRing().tail) &^ ringClosed
	if int64(t-h) <= 0 {
		return 0
	}
	return int(t - h)
}
