package resourceadmission

import "time"

// reading is one metric reading: a value, an unavailability reason, or
// not-applicable on this platform. The zero value is unavailable, never a
// healthy zero.
type reading[T any] struct {
	value  T
	reason Reason
	ok     bool
}

// reasonNotApplicable marks a metric the platform does not have at all. It is
// never reported.
const reasonNotApplicable Reason = "not_applicable"

func observed[T any](value T) reading[T] { return reading[T]{value: value, ok: true} }

func unobserved[T any](reason Reason) reading[T] { return reading[T]{reason: reason} }

func notApplicable[T any]() reading[T] { return reading[T]{reason: reasonNotApplicable} }

func (r reading[T]) skipped() bool { return !r.ok && r.reason == reasonNotApplicable }

// failure is the reason of an unavailable reading.
func (r reading[T]) failure() Reason {
	if r.reason == "" {
		return ReasonUnreadable
	}
	return r.reason
}

// snapshot is everything one sampling tick observed.
type snapshot struct {
	at     time.Time
	cpu    reading[cpuCounters]
	memory reading[hostMemory]
	cgroup reading[cgroupSample]
}

// cpuCounters are cumulative busy and total CPU time in one platform unit:
// clock ticks (jiffies) on Linux, seconds elsewhere. Only their ratio over an
// interval is used.
type cpuCounters struct {
	busy  float64
	total float64
}

type memorySource int

const (
	memorySourceAvailable memorySource = iota
	memorySourceVMPressure
)

// vmPressure is the native macOS VM pressure flag.
type vmPressure uint32

const (
	vmPressureNormal   vmPressure = 1
	vmPressureWarning  vmPressure = 2
	vmPressureCritical vmPressure = 4
)

// hostMemory is one host memory observation; source selects the fields that
// are meaningful.
type hostMemory struct {
	source    memorySource
	total     uint64
	available uint64
	vm        vmPressure
}

// limit is an optional byte limit; a zero value is "not set", not zero bytes.
type limit struct {
	bytes uint64
	set   bool
}

// cgroupSample is the target cgroup and its ancestor chain at one tick.
type cgroupSample struct {
	path string
	// psiStallUS is the cumulative memory PSI "some" total of the target
	// cgroup itself, in microseconds.
	psiStallUS reading[uint64]
	// levels runs from the target cgroup up to and including the cgroup
	// root visible in this namespace.
	levels []cgroupLevel
	// hiddenAncestors is set when limits above the observed levels cannot be
	// seen: the visible mount starts below the namespace root, or the chain
	// was cut at maxObservedAncestors.
	hiddenAncestors bool
}

type cgroupLevel struct {
	path   string
	memory reading[levelMemory]
	cpu    reading[levelCPU]
}

// levelMemory holds one cgroup level's memory limits and, when a limit is
// set, a working-set estimate. The estimate is memory.current minus
// inactive_file; it is not guaranteed reclaimable or free.
type levelMemory struct {
	max        limit
	high       limit
	workingSet uint64
}

// levelCPU holds one cgroup level's CPU capacity and cumulative usage. The
// usage counts every descendant, including siblings of the target. usageUS is
// read only when the level has a quota or a cpuset.
type levelCPU struct {
	quotaCPUs  float64
	hasQuota   bool
	cpusetCPUs int
	usageUS    uint64
}
