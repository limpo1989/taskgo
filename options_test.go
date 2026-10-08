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
	"testing"
	"time"
)

func TestOptionDefaults(t *testing.T) {
	o := newOptions(nil)
	if o.concurrency != defaultConcurrency || o.timeout != defaultTimeout ||
		o.maxIdle != 0 || o.maxJobs != 0 || o.maxPending != defaultMaxPending ||
		o.maxBatchSize != defaultMaxBatchSize || o.nowFn == nil {
		t.Fatalf("unexpected defaults: %+v", o)
	}
}

func TestOptionNegativeNormalization(t *testing.T) {
	o := newOptions([]Option{
		WithConcurrency(-1),
		WithTimeout(-time.Second),
		WithMaxIdle(-time.Second),
		WithMaxJobs(-5),
		WithMaxPending(-7),
		WithMaxBatchSize(-3),
	})
	if o.concurrency != defaultConcurrency {
		t.Fatalf("concurrency = %d", o.concurrency)
	}
	if o.timeout != defaultTimeout {
		t.Fatalf("timeout = %v", o.timeout)
	}
	if o.maxIdle != 0 {
		t.Fatalf("maxIdle = %v", o.maxIdle)
	}
	if o.maxJobs != 0 {
		t.Fatalf("maxJobs = %d", o.maxJobs)
	}
	if o.maxPending != 0 {
		t.Fatalf("maxPending = %d", o.maxPending)
	}
	if o.maxBatchSize != defaultMaxBatchSize {
		t.Fatalf("maxBatchSize = %d", o.maxBatchSize)
	}
}

func TestOptionSetters(t *testing.T) {
	o := newOptions([]Option{
		WithConcurrency(3),
		WithMaxIdle(5 * time.Second),
		WithMaxJobs(100),
		WithMaxPending(200),
		WithMaxBatchSize(16),
		WithTimeout(2 * time.Second),
		WithPanicHandler(func(any) {}),
	})
	if o.concurrency != 3 || o.maxIdle != 5*time.Second || o.maxJobs != 100 || o.maxPending != 200 ||
		o.maxBatchSize != 16 || o.timeout != 2*time.Second || o.panicFn == nil {
		t.Fatalf("unexpected: %+v", o)
	}
}

func TestMaxBatchSizeNormalization(t *testing.T) {
	cases := []struct{ value, want int }{
		{0, defaultMaxBatchSize},
		{-1, defaultMaxBatchSize},
		{1, 1},
		{3, 3},
		{16, 16},
		{maxSupportedBatchSize, maxSupportedBatchSize},
	}
	if maxInt := int(^uint(0) >> 1); maxInt > maxSupportedBatchSize {
		cases = append(cases, struct{ value, want int }{maxInt, defaultMaxBatchSize})
	}
	for _, tc := range cases {
		if got := newOptions([]Option{WithMaxBatchSize(tc.value)}).maxBatchSize; got != tc.want {
			t.Errorf("WithMaxBatchSize(%d): got %d, want %d", tc.value, got, tc.want)
		}
	}
}

func TestBacklogCapacityHintIsBounded(t *testing.T) {
	tests := []struct {
		workers int
		want    int
	}{
		{workers: 1, want: minBacklogCapacity},
		{workers: minBacklogCapacity, want: minBacklogCapacity},
		{workers: 32, want: 32},
		{workers: maxBacklogCapacity, want: maxBacklogCapacity},
		{workers: 10000, want: maxBacklogCapacity},
	}
	for _, tt := range tests {
		if got := backlogCapacityHint(tt.workers); got != tt.want {
			t.Errorf("backlogCapacityHint(%d) = %d, want %d", tt.workers, got, tt.want)
		}
	}
}
