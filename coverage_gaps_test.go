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
	"sync/atomic"
	"testing"
	"time"
)

func TestShardIndexUnkeyedIsZero(t *testing.T) {
	q := New(WithConcurrency(64))
	if got := q.taskQueue.shardIndex(nil); got != 0 {
		t.Fatalf("unkeyed shardIndex = %d, want 0", got)
	}
}

// A keyed batch splits over the shards its values pick, in value order.
func TestPushBatchKeyedSplitsByValue(t *testing.T) {
	var ran int64
	q := NewTaskKeyed(func(int) { atomic.AddInt64(&ran, 1) },
		func(v int) uint32 { return uint32(v % 7) }, 8, WithConcurrency(64))
	values := make([]int, 21)
	for i := range values {
		values[i] = i
	}
	if got := q.PushBatch(values); got != len(values) {
		t.Fatalf("PushBatch accepted %d of %d", got, len(values))
	}
	used := 0
	for i := range q.taskQueue.rings {
		if q.taskQueue.rings[i].enqueued() > 0 {
			used++
		}
	}
	if used < 2 {
		t.Fatalf("batch reached %d shards, want the key spread", used)
	}
	if err := q.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&ran); got != int64(len(values)) {
		t.Fatalf("%d of %d keyed batch values ran", got, len(values))
	}
}

// Once producers have spread, a batch lands whole on one shard and moves to
// the next shard each batch.
func TestPushBatchSpreadRotatesShards(t *testing.T) {
	var ran int64
	q := NewTask(func(int) { atomic.AddInt64(&ran, 1) }, WithConcurrency(64))
	tq := q.taskQueue
	atomic.StoreUint32(&tq.contended, 1)
	// A batch is one push of the rotation, so the next shard is reached
	// after pushRun of them.
	for b := 0; b < 2*pushRun; b++ {
		if got := q.PushBatch([]int{b}); got != 1 {
			t.Fatalf("PushBatch accepted %d of 1", got)
		}
	}
	used := 0
	for i := range tq.rings {
		if tq.rings[i].enqueued() > 0 {
			used++
		}
	}
	if used < 2 {
		t.Fatalf("four spread batches reached %d shards, want rotation", used)
	}
	if err := q.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&ran); got != int64(2*pushRun) {
		t.Fatalf("%d of %d spread batch values ran", got, 2*pushRun)
	}
}

// clearOrphans drops every mark, across all of the words a wide shard set
// spans, and keeps the orphan count in step.
func TestClearOrphansAcrossWords(t *testing.T) {
	q := NewTaskKeyed(func(int) {}, func(v int) uint32 { return uint32(v) }, 70,
		WithConcurrency(64))
	tq := q.taskQueue
	tq.markOrphan(0)
	tq.markOrphan(65)
	if got := atomic.LoadInt32(&tq.orphans); got != 2 {
		t.Fatalf("orphans = %d after two marks", got)
	}
	tq.clearOrphans()
	if got := atomic.LoadInt32(&tq.orphans); got != 0 {
		t.Fatalf("orphans = %d after clear, want 0", got)
	}
	for w, marks := range tq.orphaned {
		if marks != 0 {
			t.Fatalf("word %d kept marks %b after clear", w, marks)
		}
	}
	if err := q.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// shed parks a worker that may run while the target no longer wants it, and
// retires it at once when parking is off.
func TestShedParksOrRetires(t *testing.T) {
	parked := New(WithConcurrency(4), WithMaxIdle(time.Minute), withManualJanitor())
	parked.setBaseForTest(1)
	register := func() *worker[Job] {
		w := &worker[Job]{ch: make(chan bool, 1)}
		w.initBatch(parked.opt.maxBatchSize)
		parked.mu.Lock()
		parked.workers = append(parked.workers, w)
		parked.live++
		atomic.AddInt64(&parked.running, 1)
		parked.mu.Unlock()
		return w
	}
	// The target is met: shed declines, and the registered worker is taken
	// back out before the queue is stopped.
	first := register()
	if got := parked.shed(first); got != idleResume {
		t.Fatalf("shed with the target met = %d, want idleResume", got)
	}
	// One worker too many parks, and retires once dismissed off the idle
	// stack, as Stop or the janitor would dismiss it.
	w := register()
	go parked.shed(w) // blocks in the park
	waitFor(t, time.Second, func() bool { return parked.idleLen() == 1 })
	parked.mu.Lock()
	parked.idle = nil
	parked.mu.Unlock()
	w.ch <- true
	waitFor(t, time.Second, func() bool {
		parked.mu.Lock()
		defer parked.mu.Unlock()
		return parked.live == 1
	})
	parked.mu.Lock()
	atomic.AddInt64(&parked.running, -1)
	parked.live--
	parked.removeLocked(first)
	parked.mu.Unlock()
	if err := parked.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	retire := New(WithConcurrency(4))
	retire.setBaseForTest(1)
	w3 := &worker[Job]{ch: make(chan bool, 1)}
	w3.initBatch(retire.opt.maxBatchSize)
	retire.mu.Lock()
	retire.workers = append(retire.workers, w3)
	retire.live++
	atomic.AddInt64(&retire.running, 2)
	retire.mu.Unlock()
	if got := retire.shed(w3); got != idleExit {
		t.Fatalf("shed without parking = %d, want idleExit", got)
	}
	retire.mu.Lock()
	live := retire.live
	retire.mu.Unlock()
	if live != 0 {
		t.Fatalf("live = %d after an exit shed, want 0", live)
	}
	// The other running slot the shed measured against was this test's own:
	// give it back before the drain.
	atomic.AddInt64(&retire.running, -1)
	if err := retire.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// readyAfterPop looks past the shard a worker just served: a task waiting in
// the single queue, from before the producers spread, still fans a wake out.
func TestReadyAfterPopSeesSingleQueue(t *testing.T) {
	q := New(WithConcurrency(64))
	tq := q.taskQueue
	if tq.shards < 2 {
		t.Skipf("needs room for two shards, have %d", tq.shards)
	}
	atomic.StoreUint32(&tq.contended, 1)
	w := &worker[Job]{}
	atomic.StoreInt32(&w.ring, int32(tq.shards-1))
	if tq.readyAfterPop(w) {
		t.Fatal("readyAfterPop saw work on an empty queue")
	}
	tq.queue.push(taskItem[Job]{value: func() {}})
	if !tq.readyAfterPop(w) {
		t.Fatal("readyAfterPop missed a task in the single queue")
	}
	atomic.StoreInt32(&w.ring, -1)
	if !tq.readyAfterPop(w) {
		t.Fatal("readyAfterPop missed the single queue it serves")
	}
	// The item was injected past the pool's accounting: take it back rather
	// than stop on a task no worker will ever run.
	tq.queue.popBatch(make([]taskItem[Job], 1))
}

func TestSizerZeroStepKeepsTarget(t *testing.T) {
	s := newSizer(8, 100, 1000, 0, 0)
	if got := s.step(1000, 0, 0, 8, 4, 9); got != 9 {
		t.Fatalf("a zero-length step returned %d, want the target it was given", got)
	}
}

func TestSizerClampBounds(t *testing.T) {
	s := newSizer(8, 100, 0, 0, 0)
	if got := s.clamp(1); got != 8 {
		t.Fatalf("clamp(1) = %d, want the base 8", got)
	}
	if got := s.clamp(1000); got != 100 {
		t.Fatalf("clamp(1000) = %d, want the limit 100", got)
	}
}

// A halving that cost throughput is restored and the probe backs off; one
// that kept it stays, and the probe comes round again at its base pace.
func TestSizerJudgeSettlesAHalving(t *testing.T) {
	s := newSizer(8, 100, 0, 0, 0)
	s.probe, s.fromN, s.fromX = probeDown, 24, 1e-6
	next, _ := s.judge(int64(monitorTick), 0.5e-6, 12)
	if next != 24 {
		t.Fatalf("a halving that cost half the throughput returned %d, want 24 back", next)
	}
	if s.shrinkDur != 2*shrinkMin {
		t.Fatalf("shrinkDur = %v after a costly halving, want it doubled", s.shrinkDur)
	}
	s.probe, s.fromN, s.fromX = probeDown, 24, 1e-6
	next, _ = s.judge(2*int64(monitorTick), 1e-6, 12)
	if next != 12 {
		t.Fatalf("a halving that kept throughput returned %d, want it kept", next)
	}
	if s.shrinkDur != shrinkMin {
		t.Fatalf("shrinkDur = %v after a sound halving, want the base pace", s.shrinkDur)
	}
}

func TestAutoWorkerLimitClamped(t *testing.T) {
	old := runtime.GOMAXPROCS(4)
	defer runtime.GOMAXPROCS(old)
	if got := autoWorkerLimit(2); got != 2 {
		t.Fatalf("autoWorkerLimit(2) = %d under GOMAXPROCS 4", got)
	}
	if got := autoWorkerLimit(100); got != 4 {
		t.Fatalf("autoWorkerLimit(100) = %d under GOMAXPROCS 4, want 4", got)
	}
	runtime.GOMAXPROCS(1)
	if got := autoWorkerLimit(100); got != 1 {
		t.Fatalf("autoWorkerLimit(100) = %d under GOMAXPROCS 1, want 1", got)
	}
}

func TestNewTaskKeyedNilFnPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewTaskKeyed with a nil function did not panic")
		}
	}()
	_ = NewTaskKeyed(nil, func(i int) uint32 { return uint32(i) }, 4)
}
