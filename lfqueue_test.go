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
	"sync"
	"sync/atomic"
	"testing"
)

func newTestLFQueue[T any](capacity int) *lfQueue[T] {
	q := &lfQueue[T]{}
	q.init(capacity)
	return q
}

func TestLFQueueFIFOAcrossGrowth(t *testing.T) {
	q := newTestLFQueue[int](2)
	if _, ok := q.pop(); ok {
		t.Fatal("pop on empty queue succeeded")
	}
	if q.ready() {
		t.Fatal("empty queue reported ready")
	}
	next := 0
	for i := 0; i < 1000; i++ {
		q.push(i)
		if i%3 == 0 {
			v, ok := q.pop()
			if !ok || v != next {
				t.Fatalf("pop = %d,%v want %d", v, ok, next)
			}
			next++
		}
	}
	if got, want := q.len(), 1000-next; got != want {
		t.Fatalf("len = %d, want %d", got, want)
	}
	for ; next < 1000; next++ {
		v, ok := q.pop()
		if !ok || v != next {
			t.Fatalf("pop = %d,%v want %d", v, ok, next)
		}
	}
	if q.len() != 0 || q.ready() {
		t.Fatalf("drained queue len=%d ready=%v", q.len(), q.ready())
	}
}

func TestLFQueueBatchKeepsOrderAcrossRings(t *testing.T) {
	q := newTestLFQueue[int](4)
	q.push(-2)
	q.push(-1)
	values := make([]int, 37)
	for i := range values {
		values[i] = i
	}
	q.pushBatch(values)
	q.pushBatch(nil)
	q.push(37)
	for want := -2; want <= 37; want++ {
		v, ok := q.pop()
		if !ok || v != want {
			t.Fatalf("pop = %d,%v want %d", v, ok, want)
		}
	}
	if _, ok := q.pop(); ok {
		t.Fatal("pop after drain succeeded")
	}
}

func TestLFQueueReleasesPoppedValues(t *testing.T) {
	q := newTestLFQueue[*int](4)
	v := new(int)
	q.push(v)
	if got, _ := q.pop(); got != v {
		t.Fatal("pop returned a different pointer")
	}
	r := q.headRing()
	for i := range r.vals {
		if r.vals[i] != nil {
			t.Fatalf("slot %d still references a popped value", i)
		}
	}
}

// TestLFQueueConcurrentExactlyOnce drives producers (single and batch) and
// consumers concurrently through several ring growths and checks that every
// value is delivered exactly once and in per-producer order.
func TestLFQueueConcurrentExactlyOnce(t *testing.T) {
	const producers, consumers, perProducer = 8, 8, 20000
	q := newTestLFQueue[uint64](8)
	var seen [producers][]uint32
	for p := range seen {
		seen[p] = make([]uint32, perProducer)
	}
	var produced sync.WaitGroup
	produced.Add(producers)
	for p := 0; p < producers; p++ {
		go func(p int) {
			defer produced.Done()
			batch := make([]uint64, 0, 16)
			for i := 0; i < perProducer; i++ {
				v := uint64(p)<<32 | uint64(i)
				if p%2 == 0 {
					q.push(v)
					continue
				}
				batch = append(batch, v)
				if len(batch) == cap(batch) || i == perProducer-1 {
					q.pushBatch(batch)
					batch = batch[:0]
				}
			}
		}(p)
	}
	var consumed int64
	var failed int32
	var wg sync.WaitGroup
	wg.Add(consumers)
	for c := 0; c < consumers; c++ {
		go func() {
			defer wg.Done()
			last := make([]int64, producers)
			for i := range last {
				last[i] = -1
			}
			for atomic.LoadInt64(&consumed) < producers*perProducer {
				v, ok := q.pop()
				if !ok {
					runtime.Gosched()
					continue
				}
				p, i := int(v>>32), int64(uint32(v))
				if i <= last[p] {
					atomic.StoreInt32(&failed, 1)
				}
				last[p] = i
				atomic.AddUint32(&seen[p][i], 1)
				atomic.AddInt64(&consumed, 1)
			}
		}()
	}
	produced.Wait()
	wg.Wait()
	if atomic.LoadInt32(&failed) != 0 {
		t.Fatal("a consumer observed one producer's values out of order")
	}
	for p := range seen {
		for i, n := range seen[p] {
			if n != 1 {
				t.Fatalf("value %d/%d delivered %d times", p, i, n)
			}
		}
	}
	if q.len() != 0 {
		t.Fatalf("len = %d after drain", q.len())
	}
}

func TestLFQueueShrinkAfterDrain(t *testing.T) {
	q := newTestLFQueue[int](8)
	for i := 0; i < 5000; i++ {
		q.push(i)
	}
	head, tail := q.headRing(), q.tailRing()
	q.shrink(1024)
	if q.headRing() != head || q.tailRing() != tail {
		t.Fatal("shrink replaced a ring that still holds values")
	}
	for want := 0; want < 5000; want++ {
		if v, ok := q.pop(); !ok || v != want {
			t.Fatalf("pop = %d,%v want %d", v, ok, want)
		}
	}
	q.shrink(1024)
	if got := len(q.tailRing().vals); got != q.minCapacity {
		t.Fatalf("drained ring has %d slots, want %d", got, q.minCapacity)
	}
	if q.headRing() != q.tailRing() {
		t.Fatal("head and tail rings differ after shrink")
	}
	q.push(1)
	q.pushBatch([]int{2, 3})
	for want := 1; want <= 3; want++ {
		if v, ok := q.pop(); !ok || v != want {
			t.Fatalf("pop after shrink = %d,%v want %d", v, ok, want)
		}
	}
}

// TestLFQueueShrinkRacesProducers shrinks repeatedly while producers push, to
// check that closing an empty ring never loses or duplicates a value.
func TestLFQueueShrinkRacesProducers(t *testing.T) {
	const producers, perProducer = 4, 20000
	q := newTestLFQueue[int](8)
	var wg sync.WaitGroup
	wg.Add(producers)
	for p := 0; p < producers; p++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perProducer; i++ {
				q.push(1)
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
				q.shrink(0)
				runtime.Gosched()
			}
		}
	}()
	got := 0
	for got < producers*perProducer {
		if v, ok := q.pop(); ok {
			got += v
		} else {
			runtime.Gosched()
		}
	}
	close(done)
	wg.Wait()
	if _, ok := q.pop(); ok {
		t.Fatal("extra value after all were consumed")
	}
}
