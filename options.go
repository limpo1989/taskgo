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

import "time"

const (
	defaultConcurrency = 8
	defaultMaxIdle     = 0 // 0 means no parking: a worker exits once the queue drains
	defaultMaxJobs     = 0 // 0 means no per-worker task limit
	defaultTimeout     = 30 * time.Second
)

type options struct {
	concurrency int           // maximum number of concurrent workers
	maxIdle     time.Duration // how long a worker may stay parked; <=0 disables parking
	maxJobs     int           // task count after which a worker is recycled; <=0 means no limit
	timeout     time.Duration // how long Stop waits for outstanding tasks
	panicFn     func(v any)   // task panic handler; when non-nil, panics are recovered

	// The following are unexported test hooks.
	nowFn         func() time.Time // time source, letting tests control janitor decisions
	manualJanitor bool             // when true, no background janitor runs; tests drive cleanExpired
}

// Option configures a Queue.
type Option func(o *options)

// newOptions applies opts on top of the defaults and normalizes invalid values.
func newOptions(opts []Option) *options {
	o := &options{
		concurrency: defaultConcurrency,
		maxIdle:     defaultMaxIdle,
		maxJobs:     defaultMaxJobs,
		timeout:     defaultTimeout,
	}
	for _, f := range opts {
		f(o)
	}
	if o.concurrency <= 0 {
		o.concurrency = defaultConcurrency
	}
	if o.timeout <= 0 {
		o.timeout = defaultTimeout
	}
	if o.maxIdle < 0 {
		o.maxIdle = 0
	}
	if o.maxJobs < 0 {
		o.maxJobs = 0
	}
	if o.nowFn == nil {
		o.nowFn = time.Now
	}
	return o
}

// WithConcurrency sets the maximum number of concurrent workers. The default
// is 8.
func WithConcurrency(n int) Option {
	return func(o *options) { o.concurrency = n }
}

// WithMaxIdle sets how long an idle worker stays parked before exiting.
//
// This is the key option for tasks with deep call chains. After a task runs,
// the worker does not exit even if the queue is empty; it stays parked for up
// to d, waiting for the next task. Later tasks then reuse its already grown
// goroutine stack (2k -> 4k -> 8k ...) instead of triggering stack growth
// (morestack) from 2k every time.
//
// The default is 0, which disables parking. With parking disabled the hot path
// carries no extra overhead.
func WithMaxIdle(d time.Duration) Option {
	return func(o *options) { o.maxIdle = d }
}

// WithMaxJobs sets the number of tasks a single worker may handle before it is
// recycled.
//
// It ensures workers are reclaimed periodically even under sustained load where
// they never go idle, preventing them from effectively becoming permanent. It
// works against stack reuse, since the replacement worker grows its stack again
// from 2k, so prefer a large value (for example 1000+) to amortize that cost
// over many tasks. The default is 0, which imposes no limit.
func WithMaxJobs(n int) Option {
	return func(o *options) { o.maxJobs = n }
}

// WithTimeout sets how long Stop waits for outstanding tasks. The default is 30s.
func WithTimeout(d time.Duration) Option {
	return func(o *options) { o.timeout = d }
}

// WithPanicHandler sets a handler invoked when a task panics. When set, a task
// panic is recovered and passed to fn, and the worker keeps running. When not
// set, a panic propagates and crashes the process.
func WithPanicHandler(fn func(v any)) Option {
	return func(o *options) { o.panicFn = fn }
}
