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
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// TestUnkeyedSpreadDrainsSingleQueue reproduces the transition where a burst
// sits in the single FIFO when producers begin to contend: every task of the
// burst must still run once the shards carry sustained traffic, rather than
// waiting behind it forever.
func TestUnkeyedSpreadDrainsSingleQueue(t *testing.T) {
	if runtime.GOMAXPROCS(0) < 2 {
		t.Skip("needs room for a submitter beside the workers")
	}
	q := New(WithConcurrency(64), WithMaxIdle(time.Second))
	tq := q.taskQueue
	if tq.shards <= 1 || tq.keyed {
		t.Fatalf("queue has %d shards, keyed=%v; want an unkeyed spreadable queue", tq.shards, tq.keyed)
	}
	block := make(chan struct{})
	var burst sync.WaitGroup
	// Hold every running worker in a task, so the submissions below all wait.
	for i := 0; i < int(atomic.LoadInt64(&tq.target)); i++ {
		burst.Add(1)
		q.Push(func() { <-block; burst.Done() })
	}
	waitFor(t, time.Second, func() bool { return int(atomic.LoadInt64(&tq.running)) >= 1 })
	// A burst lands in the single FIFO while the producers have not spread.
	const leftovers = 200
	for i := 0; i < leftovers; i++ {
		burst.Add(1)
		q.Push(func() { burst.Done() })
	}
	waitFor(t, time.Second, func() bool { return q.queue.len() == leftovers })
	// The producers contend; from here on every task lands on a shard, and a
	// steady stream keeps the shards from ever running dry. The stream is
	// counted apart, so waiting on the burst is not racing its Add calls.
	atomic.StoreUint32(&tq.contended, 1)
	var stream sync.WaitGroup
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			stream.Add(1)
			q.Push(func() { stream.Done() })
			runtime.Gosched()
		}
	}()
	// The blockers may go too; the whole queue must drain.
	close(block)
	drain := make(chan struct{})
	go func() { burst.Wait(); close(drain) }()
	select {
	case <-drain:
	case <-time.After(5 * time.Second):
		t.Fatalf("%d of the single-FIFO tasks never ran", q.queue.len())
	}
	close(stop)
	stream.Wait()
	if err := q.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestUnkeyedShardsRotate keeps a lone producer's pushes from concentrating
// on one shard: with the queue spread, consecutive runs of pushes must reach
// more than one shard.
func TestUnkeyedShardsRotate(t *testing.T) {
	q := New(WithConcurrency(64))
	tq := q.taskQueue
	atomic.StoreUint32(&tq.contended, 1)
	seen := make(map[uint32]bool)
	// One "producer": its turns advance, and the shards it picks vary.
	for i := 0; i < 4*pushRun; i++ {
		seen[tq.shardAt(nil, 1)] = true
	}
	if len(seen) < 2 {
		t.Fatalf("one producer's pushes reached only shard(s) %v", seen)
	}
	// The not-spread sentinel still maps to the first shard.
	if got := tq.shardAt(nil, 0); got != 0 {
		t.Fatalf("sentinel place picked shard %d, want 0", got)
	}
	if err := q.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestSpreadIngressAccounting checks the queues a queue's counters walk: the
// single queue before the spread, and the shards after.
func TestSpreadIngressAccounting(t *testing.T) {
	q := NewTask(func(v int) {}, WithConcurrency(64))
	tq := q.taskQueue
	// An unkeyed queue whose producers can spread keeps a stripe per P;
	// a keyed one spreads its producers by key and keeps a single stripe.
	if got := len(tq.producers); got < 2 {
		t.Fatalf("spreadable queue keeps %d producer stripes, want one per P", got)
	}
	keyed := NewTaskKeyed(func(v int) {}, func(int) uint32 { return 0 }, 4, WithConcurrency(64))
	if got := len(keyed.taskQueue.producers); got != 1 {
		t.Fatalf("keyed queue keeps %d producer stripes, want 1", got)
	}
	if err := keyed.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	// After contention, a queue wide enough to spread walks both stores.
	atomic.StoreUint32(&tq.contended, 1)
	n := 0
	tq.eachIngress(func(_ *lfQueue[taskItem[int]]) { n++ })
	if want := 1 + tq.shards; n != want {
		t.Fatalf("eachIngress visited %d queues after the spread, want %d", n, want)
	}
	if err := q.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestAtomicFieldAlignment pins the 64-bit atomic fields of the hot structs to
// 8-byte boundaries. The pad arithmetic in taskQueue and worker sums to whole
// cache lines, which keeps these fields aligned on 32-bit platforms too, where
// unaligned 64-bit atomics crash on arm, mips and riscv32.
func TestAtomicFieldAlignment(t *testing.T) {
	q := newTaskQueue[int](nil, func(int) {})
	for _, c := range []struct {
		name string
		off  uintptr
	}{
		{"running", unsafe.Offsetof(q.running)},
		{"target", unsafe.Offsetof(q.target)},
		{"closed", unsafe.Offsetof(q.closed)},
		{"pending", unsafe.Offsetof(q.pending)},
		{"holders", unsafe.Offsetof(q.holders)},
		{"rescued", unsafe.Offsetof(q.rescued)},
		{"watchNext", unsafe.Offsetof(q.watchNext)},
	} {
		if c.off%8 != 0 {
			t.Errorf("%s at offset %d, want 8-byte aligned", c.name, c.off)
		}
	}
	var w worker[int]
	if off := unsafe.Offsetof(w.state); off%8 != 0 {
		t.Errorf("worker.state at offset %d, want 8-byte aligned", off)
	}
}
