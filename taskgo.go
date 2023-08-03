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
	"sync/atomic"
	"time"
)

type TaskExecutor[T any] interface {
	Exec(task T)
	Cancel(timeout time.Duration)
}

func NewTaskExecutor[T any](parent context.Context, concurrency int32, executor func(ctx context.Context, task T)) TaskExecutor[T] {
	ctx, cancel := context.WithCancel(parent)
	token := make(chan struct{}, 1)
	token <- struct{}{}
	return &taskExecutor[T]{
		ctx:      ctx,
		cancel:   cancel,
		token:    token,
		window:   concurrency,
		executor: executor,
	}
}

func NewActionExecutor(parent context.Context, concurrency int32) TaskExecutor[func()] {
	var execAction = func(ctx context.Context, task func()) {
		task()
	}
	return NewTaskExecutor[func()](parent, concurrency, execAction)
}

type taskExecutor[T any] struct {
	ctx      context.Context
	cancel   context.CancelFunc
	token    chan struct{}
	window   int32
	executor func(ctx context.Context, task T)
}

func (te *taskExecutor[T]) Exec(task T) {
	// get token
	<-te.token

	// execute task in new goroutine
	go te.execTask(task)

	// flush token
	if wnd := atomic.AddInt32(&te.window, -1); wnd > 0 {
		te.token <- struct{}{}
	}
}

func (te *taskExecutor[T]) Cancel(timeout time.Duration) {
	te.cancel()
	<-time.After(timeout)
}

func (te *taskExecutor[T]) execTask(task T) {
	defer func() {
		if wnd := atomic.AddInt32(&te.window, 1); 1 == wnd {
			te.token <- struct{}{}
		}
	}()

	te.executor(te.ctx, task)
}
