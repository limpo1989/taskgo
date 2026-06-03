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

import "testing"

func TestRingFIFOAndGrow(t *testing.T) {
	r := newRing(2)
	if _, ok := r.pop(); ok {
		t.Fatal("pop on empty ring should fail")
	}
	// Push past the initial capacity to trigger grow, including the wrap-around
	// copy when head != 0.
	var order []int
	mk := func(i int) Job { return func() { order = append(order, i) } }
	r.push(mk(1))
	r.push(mk(2))
	first, _ := r.pop() // make head=1 so a later push wraps around
	first()
	r.push(mk(3))
	r.push(mk(4)) // length is now 3 > cap 2, so grow
	r.push(mk(5))
	if r.len() != 4 {
		t.Fatalf("len = %d, want 4", r.len())
	}
	for r.len() > 0 {
		j, _ := r.pop()
		j()
	}
	want := []int{1, 2, 3, 4, 5}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestNewRingMinCapacity(t *testing.T) {
	r := newRing(0)
	if len(r.buf) != 1 {
		t.Fatalf("cap = %d, want 1", len(r.buf))
	}
}
