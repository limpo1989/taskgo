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

// sizerSim drives a sizer with synthetic counters: tasks arrive at
// arrivalsPerMs, and running workers, up to the target, start tasks at the
// rate the caller gives.
type sizerSim struct {
	s         sizer
	now       int64
	started   uint64
	enqueued  uint64
	backlog   int64
	target    int64
	rate      uint64 // arrivals per tick
	startRate func(running int64) uint64
}

func newSizerSim(base, limit, now int64) *sizerSim {
	return &sizerSim{s: newSizer(base, limit, now, 0, 0), now: now, target: base, startRate: func(int64) uint64 { return 0 }}
}

// tick advances one monitor period: arrivals join the backlog, and the
// running workers start tasks from it.
func (m *sizerSim) tick(running int64) int64 {
	m.now += int64(monitorTick)
	m.enqueued += m.rate
	m.backlog += int64(m.rate)
	start := m.startRate(running)
	if start > uint64(m.backlog) {
		start = uint64(m.backlog)
	}
	m.started += start
	m.backlog -= int64(start)
	m.target = m.s.step(m.now, m.started, m.enqueued, running, m.backlog, m.target)
	return m.target
}

func TestSizerGrowsWhenWorkersBlock(t *testing.T) {
	m := newSizerSim(8, 1000, 0)
	// Tasks take 1ms each with no queueing: a saturated pool starts ~8 tasks
	// a tick per 8 workers, and more workers start more.
	m.rate = 100
	m.startRate = func(running int64) uint64 { return uint64(running) * 2 }
	target := m.target
	for i := 0; i < 200 && target < 200; i++ {
		target = m.tick(target)
	}
	if target < 32 {
		t.Fatalf("target = %d after blocking load, want growth past the base", target)
	}
}

func TestSizerWithdrawsWhenGrowthDoesNotPay(t *testing.T) {
	m := newSizerSim(8, 1000, 0)
	// CPU-bound work: throughput stays at 10 tasks a tick whatever runs.
	m.rate = 100
	m.startRate = func(int64) uint64 { return 10 }
	target := m.target
	for i := 0; i < 400; i++ {
		target = m.tick(target)
	}
	if target > 16 {
		t.Fatalf("target = %d for CPU-bound work, want about the base", target)
	}
}

func TestSizerHoldsWhenKeepingUp(t *testing.T) {
	m := newSizerSim(8, 1000, 0)
	// Blocking tasks: each worker starts one task a tick, so throughput is
	// the worker count.
	m.rate = 100
	m.startRate = func(running int64) uint64 { return uint64(running) }
	target := m.target
	for i := 0; i < 100; i++ {
		target = m.tick(target)
		m.rate = uint64(target)
	}
	held := target
	if held < 16 {
		t.Fatalf("target = %d after blocking ramp, want growth", held)
	}
	// The backlog is drained and tasks keep arriving at what the workers
	// start: the size holds through the shrink probes.
	m.rate = uint64(held)
	m.backlog = 0
	for i := 0; i < 300; i++ {
		target = m.tick(target)
	}
	if target < held*3/4 {
		t.Fatalf("target = %d after 300 busy ticks, want about %d held", target, held)
	}
}

func TestSizerStallDoublesSmalls(t *testing.T) {
	m := newSizerSim(8, 1000, 0)
	m.backlog = 4
	target := m.target
	m.rate, m.startRate = 0, func(int64) uint64 { return 0 }
	for i := 0; i < stallTicks; i++ {
		target = m.tick(target)
	}
	if target != 12 {
		t.Fatalf("target = %d after a stall with 4 queued, want 12", target)
	}
}

func TestSizerLingersThenSheds(t *testing.T) {
	m := newSizerSim(8, 1000, 0)
	m.rate = 100
	m.startRate = func(running int64) uint64 { return uint64(running) * 2 }
	target := m.target
	for i := 0; i < 60; i++ {
		target = m.tick(target)
	}
	if target <= 8 {
		t.Fatalf("target = %d under blocking load, want growth past the base", target)
	}
	// The load stops: no arrivals, everything already queued drains, and
	// the target comes back to the base after the linger.
	m.rate, m.backlog = 0, 0
	m.startRate = func(int64) uint64 { return 0 }
	for i := 0; i < lingerTicks+400; i++ {
		target = m.tick(8)
	}
	if target != 8 {
		t.Fatalf("target = %d after the load stopped, want the base 8", target)
	}
}

func assertWithin(t *testing.T, got, lo, hi int64, what string) {
	t.Helper()
	if got < lo || got > hi {
		t.Fatalf("target = %d %s, want %d..%d", got, what, lo, hi)
	}
}
