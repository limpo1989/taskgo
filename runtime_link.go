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

import _ "unsafe" // for go:linkname

// procPin pins the calling goroutine to its P and returns the P's id. The
// runtime keeps it reachable by linkname for the packages that use it.
//
//go:linkname procPin runtime.procPin
func procPin() int

//go:linkname procUnpin runtime.procUnpin
func procUnpin()

// nanotime reads the runtime's monotonic clock, without the wall clock that
// time.Now also reads.
//
//go:linkname nanotime runtime.nanotime
func nanotime() int64

// currentP returns the id of the P the caller runs on. The goroutine may move
// to another P right after; the id is a placement hint, not a guarantee.
func currentP() int {
	id := procPin()
	procUnpin()
	return id
}
