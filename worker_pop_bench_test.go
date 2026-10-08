package taskgo

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

// BenchmarkPopBatchQueue isolates dequeue contention with a prefilled queue.
// One iteration drains 65536 tasks; setup is excluded from the timer.
func BenchmarkPopBatchQueue(b *testing.B) {
	const tasks = 1 << 16
	for _, consumers := range []int{1, runtime.GOMAXPROCS(0)} {
		for _, batch := range []int{1, 8} {
			b.Run(fmt.Sprintf("consumers=%d/batch=%d", consumers, batch), func(b *testing.B) {
				b.StopTimer()
				var elapsed time.Duration
				for i := 0; i < b.N; i++ {
					var q lfQueue[taskItem[int]]
					q.init(tasks)
					q.pushN(tasks, func(value int) taskItem[int] { return taskItem[int]{value: value} })
					start := make(chan struct{})
					counts := make([]int, consumers)
					sums := make([]int64, consumers)
					var done sync.WaitGroup
					done.Add(consumers)
					for c := 0; c < consumers; c++ {
						go func(c int) {
							defer done.Done()
							var buf [8]taskItem[int]
							count, sum := 0, int64(0)
							<-start
							for {
								n, _ := q.popBatch(buf[:batch])
								if n == 0 {
									break
								}
								count += n
								for j := 0; j < n; j++ {
									sum += int64(buf[j].value)
								}
							}
							counts[c], sums[c] = count, sum
						}(c)
					}
					b.StartTimer()
					startTime := time.Now()
					close(start)
					done.Wait()
					elapsed += time.Since(startTime)
					b.StopTimer()
					count, sum := 0, int64(0)
					for c := range counts {
						count += counts[c]
						sum += sums[c]
					}
					if count != tasks || sum != int64(tasks)*(tasks-1)/2 || q.len() != 0 {
						b.Fatalf("incomplete drain: count=%d sum=%d remaining=%d", count, sum, q.len())
					}
				}
				total := float64(b.N) * tasks
				b.ReportMetric(elapsed.Seconds()*1e9/total, "ns/task")
				b.ReportMetric(total/elapsed.Seconds(), "tasks/s")
			})
		}
	}
}

// BenchmarkWorkerPop measures single-value submission through the real worker
// loop. Use compile-time overlays to compare worker dequeue policies without
// changing the production source or adding a branch to its hot path.
func BenchmarkWorkerPop(b *testing.B) {
	workloads := []struct {
		name string
		run  func(int)
	}{
		{"Noop", func(int) {}},
		{"Fib10", func(int) {
			if workerPopFib(10) != 55 {
				panic("incorrect Fibonacci result")
			}
		}},
		{"CPU25us", func(int) { busyFor(25 * time.Microsecond) }},
		{"Jitter5ms", func(value int) {
			if value%1024 == 0 {
				time.Sleep(5 * time.Millisecond)
			}
		}},
	}
	for _, workload := range workloads {
		for _, producers := range []int{1, 16} {
			b.Run(fmt.Sprintf("%s/producers=%d", workload.name, producers), func(b *testing.B) {
				var done sync.WaitGroup
				done.Add(b.N)
				q := NewTask(func(value int) {
					workload.run(value)
					done.Done()
				}, WithConcurrency(10000))
				start := make(chan struct{})
				var submitted sync.WaitGroup
				submitted.Add(producers)
				for p := 0; p < producers; p++ {
					go func(p int) {
						defer submitted.Done()
						<-start
						for value := p; value < b.N; value += producers {
							q.Push(value)
						}
					}(p)
				}
				b.ResetTimer()
				startTime := time.Now()
				close(start)
				submitted.Wait()
				done.Wait()
				elapsed := time.Since(startTime)
				b.StopTimer()
				b.ReportMetric(float64(b.N)/elapsed.Seconds(), "tasks/s")
				if err := q.Stop(context.Background()); err != nil {
					b.Fatal(err)
				}
			})
		}
	}
}

func workerPopFib(n int) int {
	if n < 2 {
		return n
	}
	return workerPopFib(n-1) + workerPopFib(n-2)
}
