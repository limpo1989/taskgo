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
	"runtime"
	"runtime/metrics"
	"sync"
	"time"
)

// Go 1.26 reports how many goroutines wait for a P and how many run on one.
// Older runtimes fall back to the monitor's own wake-up lateness.
const (
	runnableMetric = "/sched/goroutines/runnable:goroutines"
	runningMetric  = "/sched/goroutines/running:goroutines"
)

var (
	schedMetricsOnce      sync.Once
	schedMetricsSupported bool
)

// schedLoad classifies how busy the Go scheduler is.
type schedLoad int

const (
	loadIdle   schedLoad = iota // some P has nothing to run
	loadNormal                  // every P is busy, the run queues are short
	loadBusy                    // the run queues are long
)

// idleLateness and busyLateness classify the monitor's wake-up delay when the
// scheduler metrics are unavailable. Timer slack alone stays below
// idleLateness on current platforms; a delay past a whole tick means the
// monitor waited for a P behind other goroutines.
const (
	idleLateness = monitorTick / 5
	busyLateness = monitorTick
)

// loadProbe tells the monitor whether the Go scheduler has spare capacity.
// More running workers only help when it does: blocked workers leave Ps idle,
// while a worker added to busy Ps would just queue for one.
type loadProbe struct {
	samples []metrics.Sample
}

func (p *loadProbe) load(lateness time.Duration) schedLoad {
	schedMetricsOnce.Do(func() {
		found := 0
		for _, d := range metrics.All() {
			if d.Name == runnableMetric || d.Name == runningMetric {
				found++
			}
		}
		schedMetricsSupported = found == 2
	})
	if schedMetricsSupported {
		if p.samples == nil {
			p.samples = []metrics.Sample{{Name: runnableMetric}, {Name: runningMetric}}
		}
		metrics.Read(p.samples)
		runnable, running := p.samples[0].Value, p.samples[1].Value
		if runnable.Kind() == metrics.KindUint64 && running.Kind() == metrics.KindUint64 {
			procs := uint64(runtime.GOMAXPROCS(0))
			switch {
			case runnable.Uint64() > procs/2:
				return loadBusy
			case runnable.Uint64() == 0 && running.Uint64() < procs:
				return loadIdle
			default:
				return loadNormal
			}
		}
	}
	switch {
	case lateness > busyLateness:
		return loadBusy
	case lateness < idleLateness:
		return loadIdle
	default:
		return loadNormal
	}
}
