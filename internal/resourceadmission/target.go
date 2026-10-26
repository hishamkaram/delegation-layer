package resourceadmission

import "context"

// Target is the execution target a check is measured for. The resolver owns
// its identity; this package only compares and reads it.
type Target struct {
	// Identity is the stable identity of the target, including any start
	// token that makes a reused PID distinguishable. It is compared before
	// and after the sampled interval.
	Identity string
	// PID selects the target's cgroup membership and takes precedence over
	// CgroupPath.
	PID int
	// CgroupPath is a cgroup v2 path such as "/user.slice/app.scope", used
	// only when PID is zero.
	CgroupPath string
}

func (t Target) valid() bool {
	return t.Identity != "" && (t.PID > 0 || t.CgroupPath != "")
}

// TargetResolver yields the current execution target. A Check calls it before
// and after the sampled interval so a PID or cgroup change invalidates the
// target-specific metrics. Any non-nil error, or an invalid Target, marks the
// target unavailable; only the caller's own context error aborts a check.
type TargetResolver interface {
	ResolveTarget(ctx context.Context) (Target, error)
}
