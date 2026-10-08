package taskgo

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Compile with an initial worker batch of 8 to make the blocked-tail fixture
// deterministic. All later batch adaptation and monitor logic stay intact.
func BenchmarkBatchStall(b *testing.B) {
	for _, delay := range []time.Duration{time.Millisecond, 5 * time.Millisecond, 50 * time.Millisecond} {
		b.Run(fmt.Sprintf("block=%s", delay), func(b *testing.B) {
			b.StopTimer()
			rescues := 0
			for iteration := 0; iteration < b.N; iteration++ {
				started, release := make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				var slowReturned, tailRescued uint32
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				var fast sync.WaitGroup
				fast.Add(defaultMaxBatchSize - 1)
				q := NewTask(func(value int) {
					if value == 0 {
						close(started)
						<-release
						atomic.StoreUint32(&slowReturned, 1)
					} else {
						if atomic.LoadUint32(&slowReturned) == 0 {
							atomic.StoreUint32(&tailRescued, 1)
						}
						fast.Done()
					}
				}, WithConcurrency(2))
				q.setBaseForTest(1)
				// Start both variants' monitors at the same point in the timer.
				atomic.StoreUint32(&q.monitorOn, 1)
				q.queue.pushN(defaultMaxBatchSize, func(value int) taskItem[int] {
					return taskItem[int]{value: value}
				})
				q.mu.Lock()
				q.spawnLocked()
				atomic.AddInt64(&q.running, 1)
				q.mu.Unlock()
				<-started
				if q.queue.len() != 0 || q.Len() != defaultMaxBatchSize-1 {
					unblock()
					_ = q.Stop(context.Background())
					b.Skip("fixture requires compiling workers with an initial batch of 8")
				}
				b.StartTimer()
				timer := time.AfterFunc(delay, unblock)
				atomic.StoreUint32(&q.monitorOn, 0)
				q.startMonitor()
				fast.Wait()
				b.StopTimer()
				rescues += int(atomic.LoadUint32(&tailRescued))
				timer.Stop()
				unblock()
				// Let workers finish before Stop so its 100 ms polling interval
				// does not dominate the wall time of this untimed cleanup.
				for !q.finished() {
					runtime.Gosched()
				}
				if err := q.Stop(context.Background()); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(rescues)/float64(b.N), "rescues/op")
		})
	}
}
