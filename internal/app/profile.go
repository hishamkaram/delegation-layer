package app

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/provider/antigravity"
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
	// WritableRoots includes provider runtime, temporary, and cache roots.
	// Native resolvers must include this canonical list in Effective.Digest.
	WritableRoots []string
}

// ValidateStatePlacement rejects state that overlaps any provider-writable
// tree. It runs before admission and again in the runner after policy refresh.
// The workspace is always checked, including for hermetic fixture profiles.
func (p PreparedProfile) ValidateStatePlacement(root string) error {
	canonicalRoot, err := config.CanonicalizePath(root)
	if err != nil || canonicalRoot != root {
		return fmt.Errorf("%w: state root is not canonical", ErrProfileUnavailable)
	}
	roots := append([]string{p.Plan.Directory}, p.WritableRoots...)
	for _, writable := range roots {
		canonical, pathErr := config.CanonicalizePath(writable)
		if pathErr != nil || canonical != writable {
			return fmt.Errorf("%w: provider writable root is not canonical: %s", ErrProfileUnavailable, writable)
		}
		if pathContains(root, writable) || pathContains(writable, root) {
			return fmt.Errorf("%w: state root overlaps provider-writable tree %s", ErrProfileUnavailable, writable)
		}
	}
	return nil
}

func pathContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// PrepareProfile selects only the profiles compiled into a particular command.
// Production and acceptance mains pass different fixed constructors explicitly.
type PrepareProfile func(task.TaskRecord) (PreparedProfile, error)

// NativeProfile is the production selection point. Native implementations are
// introduced and certified in their adapter phases before the first release.
func NativeProfile(request task.TaskRecord) (PreparedProfile, error) {
	if request.Provider == antigravity.Provider {
		candidate, err := antigravity.PrepareCertified(request)
		if err != nil {
			return PreparedProfile{}, fmt.Errorf("%w: %w", ErrProfileUnavailable, err)
		}
		return PreparedProfile{
			Plan: candidate.Plan, ObservedVersion: candidate.ObservedVersion, Effective: candidate.Effective, WritableRoots: candidate.WritableRoots,
			Identity: func(expected task.SessionExpectation, record func(task.SessionIdentity) error) (execution.IdentityObserver, error) {
				return antigravity.NewIdentityObserver(request.TaskID, expected, record)
			},
		}, nil
	}
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
	if err := task.ValidateEffectiveConfig(p.Effective); err != nil {
		return err
	}
	if p.Effective.Policy != nil && (p.Effective.Policy.Workspace != request.CanonicalCwd || !slices.Equal(p.WritableRoots, p.Effective.Policy.WritableRoots)) {
		return task.ErrIdentityMismatch
	}
	return nil
}

func (p PreparedProfile) Matches(request task.TaskRecord, meta task.MetaRecord) error {
	if err := p.Validate(request); err != nil {
		return err
	}
	if p.Plan.Executable != meta.ProviderExecutable || p.ObservedVersion != meta.ProviderVersion || !task.CompareEffectiveConfigs(p.Effective, meta.EffectiveConfig) || !p.Plan.Predicate.Equal(meta.Predicate) {
		return task.ErrIdentityMismatch
	}
	return nil
}
