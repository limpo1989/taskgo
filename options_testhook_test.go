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

// The options in this file are for testing only. They live in a _test.go file
// so they are excluded from production builds, and they are unexported so they
// never appear in the public API. Same-package tests can still use them.

// withManualJanitor disables the background janitor so that tests can drive
// cleanExpired directly. This makes reclamation assertions deterministic and
// independent of wall-clock time and scheduler jitter.
func withManualJanitor() Option {
	return func(o *options) { o.manualJanitor = true }
}

// withNowFunc injects a time source so tests can control the janitor's
// expiration decisions precisely.
func withNowFunc(fn func() time.Time) Option {
	return func(o *options) { o.nowFn = fn }
}
