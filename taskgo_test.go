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
	"testing"
)

func fib(n int) int {
	switch n {
	case 0, 1:
		return n
	default:
		return fib(n-1) + fib(n-2)
	}
}

func TestTaskExecutor(t *testing.T) {

	var wg sync.WaitGroup

	var executor = func(ctx context.Context, task int) {
		fib(task)
		wg.Done()
	}

	te := NewTaskExecutor[int](context.Background(), 10, executor)

	for i := 0; i < 1000000; i++ {
		wg.Add(1)
		te.Exec(13)
	}

	wg.Wait()
	te.Cancel(0)
}

const (
	Concurrency = 48
	M           = 10000
	N           = 13
)

func Benchmark_Fib(b *testing.B) {
	for i := 0; i < b.N; i++ {
		fib(N)
	}
}

func Benchmark_StdGo(b *testing.B) {
	wg := &sync.WaitGroup{}
	wg.Add(M * b.N)

	for i := 0; i < b.N; i++ {
		for j := 0; j < M; j++ {
			go func() {
				fib(N)
				wg.Done()
			}()
		}
	}
	wg.Wait()
}

func Benchmark_TaskGo(b *testing.B) {
	wg := &sync.WaitGroup{}
	wg.Add(M * b.N)

	ae := NewActionExecutor(context.Background(), Concurrency)
	for i := 0; i < b.N; i++ {
		for j := 0; j < M; j++ {
			ae.Exec(func() {
				fib(N)
				wg.Done()
			})
		}
	}
	wg.Wait()
}

func Benchmark_TaskGoFunc(b *testing.B) {

	wg := &sync.WaitGroup{}
	wg.Add(M * b.N)

	ae := NewTaskExecutor[int](context.Background(), Concurrency, func(ctx context.Context, task int) {
		fib(task)
		wg.Done()
	})

	for i := 0; i < b.N; i++ {
		for j := 0; j < M; j++ {
			ae.Exec(N)
		}
	}

	wg.Wait()
}
