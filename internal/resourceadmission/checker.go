package resourceadmission

import (
	"context"
	"time"
)

// Checker runs resource checks. It holds no per-check state and is safe for
// concurrent use.
type Checker struct {
	observer observer
	clock    clock
	budget   time.Duration
	policy   GuardPolicy
}

// NewChecker returns a Checker that reads the local operating system.
func NewChecker(policy GuardPolicy) *Checker {
	checker := newChecker(newNativeObserver().withPolicy(policy), systemClock{}, checkBudget)
	checker.policy = policy
	return checker
}

func newChecker(o observer, c clock, budget time.Duration) *Checker {
	return &Checker{observer: o, clock: c, budget: budget}
}

// Check samples the host and, where the platform has cgroups, the target
// resolved by resolver within a four-second sampling budget and classifies
// the result.
//
// Host memory is read first: a critical reading is refused at once, before the
// target is resolved or any other metric is read. Any other precheck result is
// discarded and the normal baseline decides.
//
// A nil resolver, an unresolvable target, a missing metric and the check's own
// four-second sampling budget (it bounds waits and tick boundaries; it does
// not preempt a blocked operating-system read) all yield an Assessment with unavailable metrics and a
// nil error; confirmed pressure on any other metric still wins. The returned
// error is only ever the caller's own context error.
func (c *Checker) Check(ctx context.Context, resolver TargetResolver) (Assessment, error) {
	if err := ctx.Err(); err != nil {
		return Assessment{}, err
	}
	if c.policy.DisableCPU && c.policy.DisableMemory {
		return c.assessment(Scope{}, Findings{}), nil
	}
	mon, cancel := context.WithTimeout(ctx, c.budget)
	defer cancel()
	r := &run{checker: c, caller: ctx, mon: mon, eval: evaluator{policy: c.policy}}
	return r.execute(resolver)
}

// Scope resolves the current target and its cgroup membership without
// sampling, so a caller can decide whether a previous Assessment is still
// Reusable. An unresolvable target yields the zero Scope.
func (c *Checker) Scope(ctx context.Context, resolver TargetResolver) (Scope, error) {
	if err := ctx.Err(); err != nil {
		return Scope{}, err
	}
	if c.policy.DisableCPU && c.policy.DisableMemory {
		return Scope{}, nil
	}
	mon, cancel := context.WithTimeout(ctx, c.budget)
	defer cancel()
	if !c.observer.usesTarget() {
		return Scope{}, nil
	}
	target, err := c.resolve(ctx, mon, resolver)
	if err != nil || target == nil {
		return Scope{}, err
	}
	cgroup := c.observer.membership(mon, *target)
	if err = ctx.Err(); err != nil {
		return Scope{}, err
	}
	return scopeOf(*target, cgroup.value), nil
}

func scopeOf(target Target, cgroup string) Scope {
	return Scope{TargetIdentity: target.Identity, Cgroup: cgroup}
}

// resolve returns the current target, or nil when it is unavailable. The
// error is the caller's context error only.
func (c *Checker) resolve(caller, mon context.Context, resolver TargetResolver) (*Target, error) {
	if resolver == nil {
		return nil, nil
	}
	target, resolveErr := resolver.ResolveTarget(mon)
	if err := caller.Err(); err != nil {
		return nil, err
	}
	if !usable(mon, target, resolveErr) {
		return nil, nil
	}
	return &target, nil
}

// usable reports whether a resolution produced a valid target within the
// check's own budget. A failed resolution makes the target unavailable; it is
// not a failed check.
func usable(mon context.Context, target Target, resolveErr error) bool {
	return resolveErr == nil && !ended(mon) && target.valid()
}

func ended(ctx context.Context) bool { return ctx.Err() != nil }

// run is the state of one Check.
type run struct {
	checker *Checker
	caller  context.Context
	mon     context.Context
	eval    evaluator
}

func (r *run) execute(resolver TargetResolver) (Assessment, error) {
	if critical, done, err := r.criticalPrecheck(); done || err != nil {
		return critical, err
	}
	var target *Target
	var err error
	if r.checker.observer.usesTarget() && !ended(r.mon) {
		if target, err = r.checker.resolve(r.caller, r.mon, resolver); err != nil {
			return Assessment{}, err
		}
	}
	if err = r.sample(target); err != nil {
		return Assessment{}, err
	}
	if target != nil {
		if err = r.revalidate(resolver, *target); err != nil {
			return Assessment{}, err
		}
	}
	return r.checker.assessment(r.scope(target), r.eval.findings()), nil
}

func (c *Checker) assessment(scope Scope, findings Findings) Assessment {
	assessment := NewAssessment(c.clock.Now(), scope, findings)
	assessment.policy = c.policy
	return assessment
}

// criticalPrecheck reads host memory alone. A critical reading is a confirmed
// host-memory pressure with every other metric skipped. Anything else is not
// a confirmation sample and is discarded. The error is the caller's.
func (r *run) criticalPrecheck() (Assessment, bool, error) {
	if r.checker.policy.DisableMemory {
		return Assessment{}, false, nil
	}
	memory := r.checker.observer.hostMemory(r.mon)
	if err := r.caller.Err(); err != nil {
		return Assessment{}, false, err
	}
	outcome := hostMemoryOutcome(memory)
	if outcome.state != outcomeCritical {
		return Assessment{}, false, nil
	}
	r.eval.hostMemory.record(outcome)
	return r.checker.assessment(Scope{}, r.eval.findings()), true, nil
}

func (r *run) scope(target *Target) Scope {
	if target == nil {
		return Scope{}
	}
	return scopeOf(*target, r.eval.cgroupPath)
}

// sample takes the baseline and up to requiredStreak interval samples. It
// stops at the first confirmed pressure and, after any positive interval, as
// soon as no hot metric is still waiting for confirmation. The error is the
// caller's.
func (r *run) sample(target *Target) error {
	r.eval.expect(r.unsampled())
	var prev *snapshot
	for tick := 0; tick <= requiredStreak; tick++ {
		if prev == nil {
			if done, err := r.stop(); done {
				return err
			}
		} else if done, err := r.pause(prev.at); done {
			return err
		}
		cur := r.checker.observer.observe(r.mon, target)
		cur.at = r.checker.clock.Now()
		if done, err := r.stop(); done {
			return err
		}
		r.eval.step(prev, cur)
		if r.eval.confirmed() || (prev != nil && !r.eval.pending()) {
			return nil
		}
		prev = &cur
	}
	return nil
}

// unsampled names every metric the platform provides, none of them observed,
// so a budget that ends before the baseline reports each one unavailable
// instead of clear. The baseline replaces it with what the first tick saw.
func (r *run) unsampled() snapshot {
	cgroup := notApplicable[cgroupSample]()
	if r.checker.observer.usesTarget() {
		cgroup = unobserved[cgroupSample](ReasonDeadline)
	}
	first := snapshot{
		cpu:    unobserved[cpuCounters](ReasonDeadline),
		memory: unobserved[hostMemory](ReasonDeadline),
		cgroup: cgroup,
	}
	if r.checker.policy.DisableCPU {
		first.cpu = notApplicable[cpuCounters]()
	}
	if r.checker.policy.DisableMemory {
		first.memory = notApplicable[hostMemory]()
	}
	return first
}

// pause waits until one interval after prevAt.
func (r *run) pause(prevAt time.Time) (bool, error) {
	wait := prevAt.Add(sampleInterval).Sub(r.checker.clock.Now())
	if wait > 0 {
		if err := r.checker.clock.Sleep(r.mon, wait); err != nil {
			if done, stopErr := r.stop(); done {
				return true, stopErr
			}
			return true, err
		}
	}
	return r.stop()
}

// stop reports whether sampling must end. A caller cancellation or deadline
// is returned as the caller's error; the check's own budget marks unfinished
// metrics unavailable instead.
func (r *run) stop() (bool, error) {
	if err := r.caller.Err(); err != nil {
		return true, err
	}
	if ended(r.mon) {
		r.eval.expire(ReasonDeadline)
		return true, nil
	}
	return false, nil
}

// revalidate resolves the target again after the interval. A changed or
// unreachable target invalidates the target-specific metrics.
func (r *run) revalidate(resolver TargetResolver, before Target) error {
	if ended(r.mon) {
		r.eval.invalidateTarget(ReasonDeadline)
		return r.caller.Err()
	}
	after, err := r.checker.resolve(r.caller, r.mon, resolver)
	switch {
	case err != nil:
		return err
	case ended(r.mon):
		r.eval.invalidateTarget(ReasonDeadline)
	case after == nil:
		r.eval.invalidateTarget(ReasonTargetUnavailable)
	case *after != before:
		r.eval.invalidateTarget(ReasonTargetChanged)
	}
	return nil
}
