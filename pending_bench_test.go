package taskgo

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"
)

const pendingBenchConcurrency = 10000

func BenchmarkPendingSubmit(b *testing.B) {
	const producers = 16
	for _, typed := range []bool{false, true} {
		name := "Queue"
		if typed {
			name = "Task"
		}
		b.Run(name, func(b *testing.B) {
			var done sync.WaitGroup
			done.Add(b.N)
			opts := []Option{WithConcurrency(pendingBenchConcurrency), WithMaxPending(b.N), WithMaxIdle(time.Second)}
			var submit func() bool
			var stop func()
			if typed {
				q := NewTask(func(int) { done.Done() }, opts...)
				submit = func() bool { return q.Submit(0) }
				stop = func() { _ = q.Stop(context.Background()) }
			} else {
				q := New(opts...)
				job := func() { done.Done() }
				submit = func() bool { return q.Submit(job) }
				stop = func() { _ = q.Stop(context.Background()) }
			}
			b.ResetTimer()
			var producersDone sync.WaitGroup
			producersDone.Add(producers)
			for p := 0; p < producers; p++ {
				go func(p int) {
					defer producersDone.Done()
					for i := p; i < b.N; i += producers {
						if !submit() {
							panic("unexpected rejection")
						}
					}
				}(p)
			}
			producersDone.Wait()
			done.Wait()
			b.StopTimer()
			stop()
		})
	}
}

func BenchmarkPendingSubmitBatch(b *testing.B) {
	const batchSize = 64
	for _, typed := range []bool{false, true} {
		name := "Queue"
		if typed {
			name = "Task"
		}
		b.Run(name, func(b *testing.B) {
			var done sync.WaitGroup
			done.Add(b.N)
			opts := []Option{WithConcurrency(pendingBenchConcurrency), WithMaxPending(b.N), WithMaxIdle(time.Second)}
			var submit func(int) int
			var stop func()
			if typed {
				q := NewTask(func(int) { done.Done() }, opts...)
				values := make([]int, batchSize)
				submit = func(n int) int { return q.SubmitBatch(values[:n]) }
				stop = func() { _ = q.Stop(context.Background()) }
			} else {
				q := New(opts...)
				jobs := make([]Job, batchSize)
				for i := range jobs {
					jobs[i] = func() { done.Done() }
				}
				submit = func(n int) int { return q.SubmitBatch(jobs[:n]) }
				stop = func() { _ = q.Stop(context.Background()) }
			}
			b.ResetTimer()
			for remaining := b.N; remaining > 0; {
				n := batchSize
				if remaining < n {
					n = remaining
				}
				if accepted := submit(n); accepted != n {
					b.Fatalf("accepted %d of %d", accepted, n)
				}
				remaining -= n
			}
			done.Wait()
			b.StopTimer()
			stop()
		})
	}
}

func BenchmarkShardedPush(b *testing.B) {
	const producers = 16
	for _, shards := range []int{1, 32, 64} {
		b.Run("shards="+strconv.Itoa(shards), func(b *testing.B) {
			q := New(WithConcurrency(10000), WithShards(shards), WithMaxIdle(time.Second))
			var done sync.WaitGroup
			done.Add(b.N)
			job := func() { done.Done() }
			b.ResetTimer()
			var producersDone sync.WaitGroup
			producersDone.Add(producers)
			for p := 0; p < producers; p++ {
				go func(p int) {
					defer producersDone.Done()
					for i := p; i < b.N; i += producers {
						q.Push(job)
					}
				}(p)
			}
			producersDone.Wait()
			done.Wait()
			b.StopTimer()
			_ = q.Stop(context.Background())
		})
	}
}

func BenchmarkShardedIngress(b *testing.B) {
	const producers = 256
	for _, shards := range []int{1, 32, 64} {
		b.Run("shards="+strconv.Itoa(shards), func(b *testing.B) {
			q := New(WithConcurrency(10000), WithShards(shards))
			var done sync.WaitGroup
			done.Add(b.N)
			release := make(chan struct{})
			job := func() {
				<-release
				done.Done()
			}
			b.ResetTimer()
			var producersDone sync.WaitGroup
			producersDone.Add(producers)
			for p := 0; p < producers; p++ {
				go func(p int) {
					defer producersDone.Done()
					for i := p; i < b.N; i += producers {
						q.Push(job)
					}
				}(p)
			}
			producersDone.Wait()
			b.StopTimer()
			close(release)
			done.Wait()
			_ = q.Stop(context.Background())
		})
	}
}

func BenchmarkShardedSubmit(b *testing.B) {
	const producers = 256
	for _, shards := range []int{1, 32, 64} {
		b.Run("shards="+strconv.Itoa(shards), func(b *testing.B) {
			q := New(WithConcurrency(10000), WithShards(shards), WithMaxPending(b.N))
			var done sync.WaitGroup
			done.Add(b.N)
			release := make(chan struct{})
			job := func() {
				<-release
				done.Done()
			}
			b.ResetTimer()
			var producersDone sync.WaitGroup
			producersDone.Add(producers)
			for p := 0; p < producers; p++ {
				go func(p int) {
					defer producersDone.Done()
					for i := p; i < b.N; i += producers {
						if !q.Submit(job) {
							panic("unexpected rejection")
						}
					}
				}(p)
			}
			producersDone.Wait()
			b.StopTimer()
			close(release)
			done.Wait()
			_ = q.Stop(context.Background())
		})
	}
}
