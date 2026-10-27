package resourceadmission

import (
	"context"
	"errors"
	"testing"
	"time"
)

const gib = uint64(1) << 30

var errResolver = errors.New("resolver failed")

// fakeClock advances only when Sleep is called, so tests control time.
type fakeClock struct {
	now     time.Time
	sleeps  []time.Duration
	block   bool
	onSleep func()
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_000, 0)} }

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.sleeps = append(c.sleeps, d)
	if c.onSleep != nil {
		c.onSleep()
	}
	if c.block {
		<-ctx.Done()
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.now = c.now.Add(d)
	return nil
}

// fakeObserver replays scripted snapshots; the last one repeats.
type fakeObserver struct {
	ticks      []snapshot
	targets    []*Target
	path       reading[string]
	untargeted bool
	// precheck overrides the memory-only precheck reading; by default it
	// returns the first scripted memory sample.
	precheck        *reading[hostMemory]
	onMemory        func()
	memoryCalls     int
	membershipCalls int
}

func (f *fakeObserver) hostMemory(context.Context) reading[hostMemory] {
	f.memoryCalls++
	if f.onMemory != nil {
		f.onMemory()
	}
	if f.precheck != nil {
		return *f.precheck
	}
	return f.ticks[0].memory
}

func (f *fakeObserver) usesTarget() bool { return !f.untargeted }

func (f *fakeObserver) observe(_ context.Context, target *Target) snapshot {
	f.targets = append(f.targets, target)
	return f.ticks[min(len(f.targets)-1, len(f.ticks)-1)]
}

func (f *fakeObserver) membership(context.Context, Target) reading[string] {
	f.membershipCalls++
	return f.path
}

type resolverFunc func(ctx context.Context) (Target, error)

func (f resolverFunc) ResolveTarget(ctx context.Context) (Target, error) { return f(ctx) }

// sequenceResolver returns the scripted targets in order; the last repeats.
type sequenceResolver struct {
	targets []Target
	errs    []error
	calls   int
}

func (s *sequenceResolver) ResolveTarget(context.Context) (Target, error) {
	i := min(s.calls, len(s.targets)-1)
	s.calls++
	var err error
	if i < len(s.errs) {
		err = s.errs[i]
	}
	return s.targets[i], err
}

var testTarget = Target{Identity: "daemon-1:start-7", PID: 4242}

func steadyResolver() *sequenceResolver {
	return &sequenceResolver{targets: []Target{testTarget}}
}

// cpuSeries builds cumulative CPU readings: a zero baseline then one reading
// per busy share, each advancing the total by 100 seconds.
func cpuSeries(shares ...float64) []reading[cpuCounters] {
	out := []reading[cpuCounters]{observed(cpuCounters{})}
	var total, busy float64
	for _, share := range shares {
		total += 100
		busy += 100 * share
		out = append(out, observed(cpuCounters{busy: busy, total: total}))
	}
	return out
}

func availableMemory(available uint64) reading[hostMemory] {
	return observed(hostMemory{source: memorySourceAvailable, total: 16 * gib, available: available})
}

func vmMemory(level vmPressure) reading[hostMemory] {
	return observed(hostMemory{source: memorySourceVMPressure, vm: level})
}

// healthySnapshots returns n snapshots with idle CPU and ample memory and no
// cgroup on the platform.
func healthySnapshots(n int) []snapshot {
	cpus := cpuSeries(make([]float64, n-1)...)
	ticks := make([]snapshot, n)
	for i := range ticks {
		ticks[i] = snapshot{
			cpu:    cpus[i],
			memory: availableMemory(8 * gib),
			cgroup: notApplicable[cgroupSample](),
		}
	}
	return ticks
}

func newTestChecker(t *testing.T, ticks []snapshot) (*Checker, *fakeObserver, *fakeClock) {
	t.Helper()
	obs := &fakeObserver{ticks: ticks, path: observed("")}
	clk := newFakeClock()
	return newChecker(obs, clk, checkBudget), obs, clk
}

func pressuresOf(a Assessment) map[Metric]Basis {
	out := map[Metric]Basis{}
	for _, p := range a.Pressures() {
		out[p.Metric] = p.Basis
	}
	return out
}

func unavailableOf(a Assessment) map[Metric]Reason {
	out := map[Metric]Reason{}
	for _, u := range a.Unavailable() {
		out[u.Metric] = u.Reason
	}
	return out
}
