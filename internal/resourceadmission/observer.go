package resourceadmission

import "context"

// observer is the I/O port of the checker. Native implementations read the
// operating system; tests supply fixed snapshots. The checker stamps the
// snapshot time itself.
type observer interface {
	// usesTarget reports whether this platform has target-specific metrics.
	// When false the checker never calls the target resolver.
	usesTarget() bool
	// observe reads one tick. A nil target means none is available.
	observe(ctx context.Context, target *Target) snapshot
	// hostMemory reads only host memory, for the critical precheck that runs
	// before any other work.
	hostMemory(ctx context.Context) reading[hostMemory]
	// membership reads the target's current cgroup path. It is empty and
	// observed on platforms without cgroups.
	membership(ctx context.Context, target Target) reading[string]
}

// composedObserver assembles an observer from per-metric readers. The native
// platform files choose the readers; no reader owns policy.
type composedObserver struct {
	targeted   bool
	cpu        func(ctx context.Context) reading[cpuCounters]
	memory     func(ctx context.Context) reading[hostMemory]
	cgroup     func(target *Target) reading[cgroupSample]
	cgroupPath func(target Target) reading[string]
}

func (o composedObserver) usesTarget() bool { return o.targeted }

func (o composedObserver) observe(ctx context.Context, target *Target) snapshot {
	return snapshot{
		cpu:    o.cpu(ctx),
		memory: o.memory(ctx),
		cgroup: o.cgroup(target),
	}
}

func (o composedObserver) hostMemory(ctx context.Context) reading[hostMemory] { return o.memory(ctx) }

func (o composedObserver) membership(_ context.Context, target Target) reading[string] {
	return o.cgroupPath(target)
}
