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
	"testing"
	"time"
)

// These benchmarks synchronize after each task so queue growth and work
// backlog do not hide the allocation made by the submission path.
func BenchmarkPushClosure(b *testing.B) {
	done := make(chan struct{}, 1)
	q := New(WithConcurrency(1), WithMaxIdle(time.Minute))
	q.Push(func() { done <- struct{}{} })
	<-done

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		value := i
		q.Push(func() {
			if value == -1 {
				panic("unreachable")
			}
			done <- struct{}{}
		})
		<-done
	}
	b.StopTimer()
	_ = q.Stop(context.Background())
}

func BenchmarkPushTyped(b *testing.B) {
	done := make(chan struct{}, 1)
	q := NewTask(func(value int) {
		if value < 0 {
			panic("unreachable")
		}
		done <- struct{}{}
	}, WithConcurrency(1), WithMaxIdle(time.Minute))
	q.Push(0)
	<-done

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q.Push(i)
		<-done
	}
	b.StopTimer()
	_ = q.Stop(context.Background())
}
