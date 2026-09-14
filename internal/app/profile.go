package app

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

var ErrProfileUnavailable = errors.New("provider permission profile is not available")

// PreparedProfile comes from one explicit compiled profile constructor. The
// launch plan is reconstructed and checked against saved metadata in each runner.
// It is never deserialized from a caller-supplied executable or argument list.
type PreparedProfile struct {
	Plan            execution.Plan
	ObservedVersion string
	Effective       task.EffectiveConfig
	Identity        func(task.SessionExpectation, func(task.SessionIdentity) error) (execution.IdentityObserver, error)
}

// PrepareProfile selects only the profiles compiled into a particular command.
// Production and acceptance mains pass different fixed constructors explicitly.
type PrepareProfile func(task.TaskRecord) (PreparedProfile, error)

// NativeProfile is the production selection point. Native implementations are
// introduced and certified in their adapter phases before the first release.
func NativeProfile(request task.TaskRecord) (PreparedProfile, error) {
	return PreparedProfile{}, fmt.Errorf("%w: %s/%s", ErrProfileUnavailable, request.Provider, request.Mode)
}

func (p PreparedProfile) Validate(request task.TaskRecord) error {
	if !filepath.IsAbs(p.Plan.Executable) || p.ObservedVersion == "" {
		return errors.New("profile must resolve an absolute executable and observed version")
	}
	if p.Plan.Directory != request.CanonicalCwd || p.Plan.Predicate.Adapter != request.Provider || p.Plan.Predicate.Mode != request.Mode {
		return task.ErrIdentityMismatch
	}
	if err := task.ValidatePredicateRef(p.Plan.Predicate); err != nil {
		return err
	}
	if p.Effective.Containment == "" || p.Effective.Approval == "" {
		return errors.New("profile must declare effective containment and approval")
	}
	return task.ValidateSHA256(p.Effective.Digest)
}

func (p PreparedProfile) Matches(request task.TaskRecord, meta task.MetaRecord) error {
	if err := p.Validate(request); err != nil {
		return err
	}
	if p.Plan.Executable != meta.ProviderExecutable || p.ObservedVersion != meta.ProviderVersion || p.Effective != meta.EffectiveConfig || !p.Plan.Predicate.Equal(meta.Predicate) {
		return task.ErrIdentityMismatch
	}
	return nil
}
