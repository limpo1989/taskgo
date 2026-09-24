package taskgo

import (
	"sync"
	"sync/atomic"
)

type jobShard struct {
	mu      sync.Mutex
	backlog *ring
	queued  int64
}

func newJobShards(n int) []jobShard {
	shards := make([]jobShard, n)
	for i := range shards {
		shards[i].backlog = newRing(8)
	}
	return shards
}

func (s *jobShard) push(job Job) bool {
	s.mu.Lock()
	wasEmpty := s.backlog.len() == 0
	s.backlog.push(job)
	atomic.AddInt64(&s.queued, 1)
	s.mu.Unlock()
	return wasEmpty
}

func (s *jobShard) pop() (Job, bool) {
	if atomic.LoadInt64(&s.queued) == 0 {
		return nil, false
	}
	s.mu.Lock()
	job, ok := s.backlog.pop()
	if ok {
		atomic.AddInt64(&s.queued, -1)
	}
	s.mu.Unlock()
	return job, ok
}

func (s *jobShard) len() int {
	return int(atomic.LoadInt64(&s.queued))
}

type taskShard[T any] struct {
	mu      sync.Mutex
	backlog *taskRing[T]
	queued  int64
}

func newTaskShards[T any](n int) []taskShard[T] {
	shards := make([]taskShard[T], n)
	for i := range shards {
		shards[i].backlog = newTaskRing[T](8)
	}
	return shards
}

func (s *taskShard[T]) push(item taskItem[T]) bool {
	s.mu.Lock()
	wasEmpty := s.backlog.len() == 0
	s.backlog.push(item)
	atomic.AddInt64(&s.queued, 1)
	s.mu.Unlock()
	return wasEmpty
}

func (s *taskShard[T]) pop() (taskItem[T], bool) {
	if atomic.LoadInt64(&s.queued) == 0 {
		return taskItem[T]{}, false
	}
	s.mu.Lock()
	item, ok := s.backlog.pop()
	if ok {
		atomic.AddInt64(&s.queued, -1)
	}
	s.mu.Unlock()
	return item, ok
}

func (s *taskShard[T]) len() int {
	return int(atomic.LoadInt64(&s.queued))
}
