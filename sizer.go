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

import "math"

const (
	// stallTicks is how many consecutive ticks may pass without a task
	// starting, while tasks wait and workers run, before the workers count as
	// blocked: the target then doubles, bounded by the backlog.
	stallTicks = 2
	// minGain is the least a growth must pay: the tasks started per second
	// must rise by this fraction of the fraction of workers added, so
	// doubling the workers must start at least 10% more tasks, and by
	// minRise at least, above the noise of a window.
	minGain = 0.1
	minRise = 0.05
	// minStep is the least growth worth an experiment.
	minStep = 1.25
	// provenGain is the gain at which a growth counts as proven, letting the
	// next step grow by provenGrowth rather than probeGrowth.
	provenGain   = 0.5
	probeGrowth  = 2
	provenGrowth = 4
	// shrinkKeep is the share of the throughput a halving of the workers must
	// keep for the sizer to keep it.
	shrinkKeep = 0.9
	// drainNanos is the time in which the sizer asks the workers to drain the
	// backlog, on top of keeping up with arrivals.
	drainNanos = 4e6
	// The settle time after a resize is twice the mean task time, so that
	// the workers it added finish their first task before a window opens.
	settleMin = int64(monitorTick)
	settleMax = int64(50 * monitorTick)
	// A window closes once every worker has started a task on average, with
	// at least minWindowStarts starts over at least minWindowNanos, or after
	// maxWindowNanos, so that long tasks are still sized.
	minWindowStarts = 32
	minWindowNanos  = int64(2 * monitorTick)
	maxWindowNanos  = int64(100 * monitorTick)
	// A growth that did not pay is withdrawn, and the size it reached is not
	// tried again for holdMin, doubling with every further failure up to
	// holdMax, so that CPU-bound work keeps about one worker per P.
	holdMin = int64(100 * monitorTick)
	holdMax = int64(5000 * monitorTick)
	// A target above its base is halved, to see whether the workers still
	// pay, shrinkMin after it was last set, doubling up to shrinkMax with
	// every halving that cost throughput.
	shrinkMin = int64(200 * monitorTick)
	shrinkMax = int64(5000 * monitorTick)
	// lingerTicks is how long the target stays where the load put it once
	// no task is waiting, so that the next burst finds the workers it needs.
	lingerTicks = 100
)

// The sizer's experiments in flight: a resize whose window is yet to say
// whether it paid.
const (
	probeNone = iota
	probeUp
	probeConfirm // a growth that paid less than provenGain, under a second look
	probeDown
)

// sizer decides how many workers may run, from the queue's own counters
// alone: tasks started, tasks enqueued, the workers running and the backlog.
// It reads no scheduler or CPU metric.
//
// It climbs the throughput: over a window in which tasks wait, the tasks
// started per second are what the workers achieve, and every resize is an
// experiment the next window judges.
//
//   - Tasks wait and Little's law asks for more workers than run, to keep up
//     with arrivals and drain the backlog: the target grows, doubling at first
//     and quadrupling once growth has paid. The window after it must show
//     more tasks started, at least minGain of the share of workers added, or
//     the growth is withdrawn and not tried again for a while. Workers that
//     block let the added ones run, and raise throughput with their number;
//     workers that compute only queue for the CPUs, and CPU-bound work keeps
//     about one worker per P.
//   - A target above its base is halved from time to time, and restored if
//     the window after starts fewer tasks: work that no longer blocks sheds
//     the workers it needed.
//   - No task started for stallTicks ticks: every running worker is stuck in
//     a task, and the target doubles.
//
// Little's law gives the mean time a task holds its worker, S = N*T/starts,
// from which the workers needed follow. A window opens only once the workers
// a resize added have had time to finish a task, so that their first, still
// running tasks do not read as slow ones.
type sizer struct {
	base, limit int64

	lastNow     int64
	lastStarted uint64
	lastEnq     uint64
	stalled     int
	idle        int

	settleUntil int64
	winDT       int64
	winWork     float64 // worker-nanoseconds
	winStarts   uint64
	winArrivals uint64

	sLast float64 // the last window's mean task time, nanoseconds
	sBase float64 // the mean task time at the base target, where nothing inflates it
	xLast float64 // the last window's tasks started per nanosecond
	xRef  float64 // the most tasks per nanosecond a window saw since the last resize

	probe  int
	fromN  int64   // the target before the resize under test
	fromX  float64 // the throughput it achieved
	proven bool    // the last growth paid at least provenGain

	holdCap   int64 // growth to holdCap or past waits until holdUntil
	holdUntil int64
	holdDur   int64

	shrinkAt  int64 // when the target is next halved to test it
	shrinkDur int64
}

func newSizer(base, limit, now int64, started, enqueued uint64) sizer {
	return sizer{
		base: base, limit: limit,
		lastNow: now, lastStarted: started, lastEnq: enqueued,
		settleUntil: now,
		shrinkDur:   shrinkMin,
	}
}

// step takes one monitor tick's counters and returns the new target. started
// counts the tasks workers have started, and enqueued the tasks submitted;
// both only grow, and a sample below the last one counts as no change.
func (s *sizer) step(now int64, started, enqueued uint64, running, backlog, target int64) int64 {
	dt := now - s.lastNow
	var progress, arrivals uint64
	if started > s.lastStarted {
		progress, s.lastStarted = started-s.lastStarted, started
	}
	if enqueued > s.lastEnq {
		arrivals, s.lastEnq = enqueued-s.lastEnq, enqueued
	}
	s.lastNow = now
	if dt <= 0 {
		return target
	}

	if backlog == 0 {
		// Nothing waits, so no window can judge a resize: keep the size.
		s.stalled, s.probe = 0, probeNone
		s.resetWindow()
		if s.idle++; s.idle > lingerTicks && target > s.base {
			target -= (target-s.base)/4 + 1
			if target < s.base {
				target = s.base
			}
		}
		return target
	}
	s.idle = 0

	if progress == 0 && running > 0 {
		if s.stalled++; s.stalled >= stallTicks && s.stallLongEnough(dt, running) {
			s.stalled = 0
			grow := backlog
			if grow > target {
				grow = target
			}
			if next := s.clamp(target + grow); next > target {
				s.probe, s.fromN, s.fromX = probeUp, target, s.xLast
				s.resize(now, next)
				return next
			}
		}
	} else {
		s.stalled = 0
	}

	if now < s.settleUntil {
		return target
	}
	n := running
	if n < target {
		n = target
	}
	s.winDT += dt
	s.winWork += float64(n) * float64(dt)
	s.winStarts += progress
	s.winArrivals += arrivals
	if s.winStarts == 0 {
		if s.winDT >= maxWindowNanos {
			s.resetWindow()
		}
		return target
	}
	need := uint64(target)
	if need < minWindowStarts {
		need = minWindowStarts
	}
	if (s.winStarts < need || s.winDT < minWindowNanos) && s.winDT < maxWindowNanos {
		return target
	}

	x := float64(s.winStarts) / float64(s.winDT)
	mean := s.winWork / float64(s.winStarts)
	rate := float64(s.winArrivals) / float64(s.winDT)
	if target <= s.base {
		s.sBase = mean
	}
	s.sLast, s.xLast = mean, x
	next, pending := s.judge(now, x, target)
	if sizerDebug != nil {
		defer func() { sizerDebug(s, now, mean, x, rate, backlog, target, next) }()
	}
	if next != target || pending {
		s.resetWindow()
		return next
	}
	if x > s.xRef {
		s.xRef = x
	}

	want := (rate + float64(backlog)/drainNanos) * mean
	if target > s.base && now >= s.shrinkAt {
		// Little's law asks for the workers there are, and the window's
		// throughput is past what the base target delivers on its own: the
		// workers are keeping up with work only they can do, and halving them
		// would only queue tasks behind fewer. A pool whose tasks stopped
		// blocking delivers no more than the base does, however many workers
		// it runs, and keeps its probes.
		busy := float64(target) <= want
		if busy && s.sBase > 0 && x <= 1.2*float64(s.base)/s.sBase {
			busy = false
		}
		if busy {
			// Little's law asks for the workers there are: they are keeping
			// up, and halving them would only queue tasks behind fewer. The
			// probe waits for a window with slack.
			s.shrinkAt = now + s.shrinkDur
		} else {
			next = s.clamp(target / 2)
			s.probe, s.fromN, s.fromX = probeDown, target, s.xRef
			s.resize(now, next)
			return next
		}
	}
	if target < s.limit && want > float64(target) {
		ceiling := s.limit
		if now < s.holdUntil {
			// Short of the size that did not pay.
			ceiling = (target + s.holdCap) / 2
		}
		factor := int64(probeGrowth)
		if s.proven {
			factor = provenGrowth
		}
		next = target * factor
		if want < float64(next) {
			next = int64(math.Ceil(want))
		}
		if next > ceiling {
			next = ceiling
		}
		if next = s.clamp(next); float64(next) >= minStep*float64(target) {
			s.probe, s.fromN, s.fromX = probeUp, target, x
			s.resize(now, next)
			s.shrinkAt = now + s.shrinkDur
			return next
		}
	}
	s.resetWindow()
	return target
}

// judge settles the resize under test with the throughput x of the window
// after it. It returns the target to go back to if the resize did not pay,
// and pending when the resize needs another window to tell.
func (s *sizer) judge(now int64, x float64, target int64) (int64, bool) {
	probe := s.probe
	s.probe = probeNone
	switch probe {
	case probeUp, probeConfirm:
		added := float64(target)/float64(s.fromN) - 1
		if s.fromX <= 0 || added <= 0 {
			// A stall's growth: nothing ran before it to compare with.
			s.proven = true
			return target, false
		}
		rise := x/s.fromX - 1
		gain := rise / added
		if gain < minGain || rise < minRise {
			s.proven = false
			if s.holdDur *= 2; s.holdDur < holdMin {
				s.holdDur = holdMin
			} else if s.holdDur > holdMax {
				s.holdDur = holdMax
			}
			s.holdCap, s.holdUntil = target, now+s.holdDur
			s.resize(now, s.fromN)
			return s.fromN, false
		}
		if gain < provenGain && probe == probeUp {
			// A modest gain could be a noisy window: look again.
			s.probe = probeConfirm
			return target, true
		}
		s.proven = gain >= provenGain
		s.holdDur = 0
	case probeDown:
		if x < s.fromX*shrinkKeep {
			// The workers were busy: put them back, and test them again
			// later.
			if s.shrinkDur *= 2; s.shrinkDur > shrinkMax {
				s.shrinkDur = shrinkMax
			}
			s.shrinkAt = now + s.shrinkDur
			s.resize(now, s.fromN)
			return s.fromN, false
		}
		s.shrinkDur = shrinkMin
		s.shrinkAt = now + s.shrinkDur
		s.proven = false
		// The workers withdrawn did not pay: do not grow back for a while.
		if s.holdDur < holdMin {
			s.holdDur = holdMin
		}
		s.holdCap, s.holdUntil = target, now+s.holdDur
	}
	return target, false
}

// stallLongEnough reports whether a stretch of ticks without a task start
// is long for the tasks this queue runs: once windows have measured them, it
// must have been long enough for the running workers to start a few.
func (s *sizer) stallLongEnough(dt, running int64) bool {
	if s.sLast == 0 {
		return true
	}
	expected := float64(s.stalled) * float64(dt) * float64(running) / s.sLast
	return expected >= 3
}

func (s *sizer) clamp(n int64) int64 {
	if n > s.limit {
		n = s.limit
	}
	if n < s.base {
		n = s.base
	}
	return n
}

// resize waits for the workers of a new target to settle before the next
// window opens.
func (s *sizer) resize(now, target int64) {
	_ = target
	settle := int64(2 * s.sLast)
	if settle < settleMin {
		settle = settleMin
	} else if settle > settleMax {
		settle = settleMax
	}
	s.settleUntil = now + settle
	s.xRef = 0
	s.resetWindow()
}

func (s *sizer) resetWindow() {
	s.winDT, s.winWork, s.winStarts, s.winArrivals = 0, 0, 0, 0
}

var sizerDebug func(s *sizer, now int64, mean, x, rate float64, backlog, target, next int64)
