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
	"sync/atomic"
	"testing"
	"time"
)

var _ interface{ Push(int) } = (*Task[int])(nil)
var _ interface{ Submit(int) bool } = (*Task[int])(nil)

func TestTaskPushPassesValues(t *testing.T) {
	const total = 1000
	var got int64
	var wg sync.WaitGroup
	wg.Add(total)
	q := NewTask(func(value int) {
		atomic.AddInt64(&got, int64(value))
		wg.Done()
	}, WithConcurrency(4))

	for i := 1; i <= total; i++ {
		q.Push(i)
	}
	wg.Wait()
	if want := int64(total * (total + 1) / 2); got != want {
		t.Fatalf("sum = %d, want %d", got, want)
	}
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func TestTaskConcurrentProducers(t *testing.T) {
	const producers = 8
	const perProducer = 1000
	var completed int64
	q := NewTask(func(int) { atomic.AddInt64(&completed, 1) }, WithConcurrency(4))
	var wg sync.WaitGroup
	wg.Add(producers)
	for producer := 0; producer < producers; producer++ {
		go func(producer int) {
			defer wg.Done()
			for i := 0; i < perProducer; i++ {
				q.Push(producer*perProducer + i)
			}
		}(producer)
	}
	wg.Wait()
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := atomic.LoadInt64(&completed); got != producers*perProducer {
		t.Fatalf("completed = %d, want %d", got, producers*perProducer)
	}
}

func TestTaskPushAcceptsZeroValue(t *testing.T) {
	done := make(chan struct{})
	q := NewTask(func(value *int) {
		if value != nil {
			t.Errorf("value = %v, want nil", value)
		}
		close(done)
	})
	q.Push(nil)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("typed nil value was not executed")
	}
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func TestTaskSubmitUsesPendingLimit(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	q := NewTask(func(value int) {
		if value == 1 {
			close(started)
			<-release
		}
	}, WithConcurrency(1), WithMaxPending(2))

	if !q.Submit(1) || !q.Submit(2) {
		t.Fatal("initial typed Submit rejected")
	}
	<-started
	if q.Submit(3) {
		t.Fatal("typed Submit accepted past pending limit")
	}
	close(release)
	waitFor(t, time.Second, func() bool { return q.Submit(3) })
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func TestTaskSubmitBatchAcceptsPrefixInOrder(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	got := make(chan int, 3)
	q := NewTask(func(value int) {
		if value == 0 {
			close(started)
			<-release
		}
		got <- value
	}, WithConcurrency(1), WithMaxPending(3))

	if !q.Submit(0) {
		t.Fatal("initial Submit rejected")
	}
	<-started
	if accepted := q.SubmitBatch([]int{1, 2, 3, 4}); accepted != 2 {
		t.Fatalf("accepted = %d, want 2", accepted)
	}
	close(release)
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}

	want := []int{0, 1, 2}
	for i, value := range want {
		if gotValue := <-got; gotValue != value {
			t.Fatalf("got[%d] = %d, want %d", i, gotValue, value)
		}
	}
}

func TestTaskTrySubmitBatchIsAtomic(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	q := NewTask(func(value int) {
		if value == 0 {
			close(started)
			<-release
		}
	}, WithConcurrency(1), WithMaxPending(2))

	if !q.Submit(0) {
		t.Fatal("initial Submit rejected")
	}
	<-started
	if q.TrySubmitBatch([]int{1, 2}) {
		t.Fatal("oversized batch was accepted")
	}
	if q.Len() != 0 {
		t.Fatalf("Len = %d after rejected batch, want 0", q.Len())
	}
	close(release)
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func TestTaskTrySubmitBatchRejectsAfterStop(t *testing.T) {
	q := NewTask(func(int) {})
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if q.TrySubmitBatch(nil) {
		t.Fatal("empty batch accepted after Stop")
	}
}

func TestNewTaskRejectsNilFunction(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewTask(nil) did not panic")
		}
	}()
	NewTask[int](nil)
}
