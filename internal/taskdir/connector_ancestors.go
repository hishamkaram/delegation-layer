package taskdir

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

const namespaceMutationLockName = "task-namespace.lock"

// connectorAncestors owns one operation's maintenance reference and independent
// mutation descriptor. Descendants settle first, then mutation, then maintenance.
type connectorAncestors struct {
	store               *Store
	mutation            *LockFile
	mutationHeld        bool
	maintenanceReleased bool
}

func (a *connectorAncestors) release() error {
	if a.mutation != nil {
		if a.mutationHeld {
			if err := a.mutation.Unlock(); err != nil {
				return err
			}
			a.mutationHeld = false
		}
		if err := a.mutation.closeIdle(); err != nil {
			return err
		}
		a.mutation = nil
	}
	if !a.maintenanceReleased {
		if err := a.store.maintLock.Unlock(); err != nil {
			return err
		}
		a.maintenanceReleased = true
	}
	return nil
}

func (s *Store) beginConnectorMutationContext(ctx context.Context, exclusive bool) (result *connectorAncestors, resultErr error) {
	if err := s.maintLock.LockSHNonblockingContext(ctx); err != nil {
		return nil, err
	}
	ancestors := &connectorAncestors{store: s}
	defer func() {
		if resultErr != nil {
			s.finishConnectorAncestors(&resultErr, ancestors)
		}
	}()
	record, err := readStoreRootRecord(ctx, s)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(record.RequiredFeatures, task.NamespaceMutationFeature) {
		return ancestors, nil
	}
	ancestors.mutation, err = s.openExistingLock(filepath.Join(s.Root, namespaceMutationLockName), LockLevelNone)
	if err != nil {
		return nil, fmt.Errorf("%w: required namespace mutation lock: %w", task.ErrEvidenceFault, err)
	}
	lock := ancestors.mutation.LockSHNonblockingContext
	if exclusive {
		lock = ancestors.mutation.LockEXNonblockingContext
	}
	if err = lock(ctx); err != nil {
		return nil, err
	}
	ancestors.mutationHeld = true
	return ancestors, nil
}

func (s *Store) finishConnectorAncestors(resultErr *error, ancestors *connectorAncestors) {
	if CleanupRetainsStore(*resultErr, s) {
		var child *ConnectorTaskCleanupError
		if errors.As(*resultErr, &child) && child.store == s {
			child.ancestors = ancestors
			return
		}
		var namespace *NamespaceCleanupError
		if errors.As(*resultErr, &namespace) && namespace.store == s && !namespace.ancestorsTransferred {
			namespace.ancestors = ancestors
			return
		}
		var control *ControlObservationCleanupError
		if errors.As(*resultErr, &control) && control.RetainedStore() == s {
			control.retainAncestor(ancestors)
			return
		}
		return
	}
	if err := ancestors.release(); err != nil {
		*resultErr = errors.Join(*resultErr, &ConnectorMaintenanceCleanupError{store: s, ancestors: ancestors, cause: err})
	}
}

func (s *Store) finishCreationNamespace(resultErr *error, namespace *LockFile) {
	if namespace == nil {
		return
	}
	var child *ConnectorTaskCleanupError
	if errors.As(*resultErr, &child) && child.store == s {
		// Creation takes namespace before admission. An unresolved admission
		// cleanup therefore retains namespace as another ancestor.
		child.creationNamespace = namespace
		return
	}
	*resultErr = errors.Join(*resultErr, s.closeNamespace(namespace))
}

func (s *Store) verifyNamespaceMutationLock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Passive integrity checks never create an inode or issue sync barriers.
	file, err := s.openFile(filepath.Join(s.Root, namespaceMutationLockName), unix.O_RDONLY)
	if err != nil {
		return fmt.Errorf("%w: required namespace mutation lock: %w", task.ErrEvidenceFault, err)
	}
	return errors.Join(file.Close(), ctx.Err())
}

func (s *Store) closeFailedTaskCreation(td *TaskDir, cause error, admissionHeld bool) error {
	if !admissionHeld {
		if err := td.Close(); err == nil {
			return cause
		} else {
			cause = errors.Join(cause, err)
		}
	}
	return &ConnectorTaskCleanupError{store: s, task: td, admissionHeld: admissionHeld, ancestors: &connectorAncestors{store: s}, cause: cause}
}

// prepareConnectorFeaturesContext initializes durable authority before changing
// the in-memory root record. Only offline enable publishes that record.
func (s *Store) prepareConnectorFeaturesContext(ctx context.Context, record *task.RootRecord) (bool, error) {
	mutationEnabled := slices.Contains(record.RequiredFeatures, task.NamespaceMutationFeature)
	if mutationEnabled {
		if err := s.verifyNamespaceMutationLock(ctx); err != nil {
			return false, err
		}
	} else {
		lock, err := s.openLock(filepath.Join(s.Root, namespaceMutationLockName), LockLevelNone)
		if err != nil {
			return false, err
		}
		if err = errors.Join(lock.closeIdle(), s.barrierDir(s.Root)); err != nil {
			return false, err
		}
	}
	admissionEnabled := slices.Contains(record.RequiredFeatures, task.NamespaceAdmissionFeature)
	if !admissionEnabled {
		record.RequiredFeatures = append(record.RequiredFeatures, task.NamespaceAdmissionFeature)
	}
	if !mutationEnabled {
		record.RequiredFeatures = append(record.RequiredFeatures, task.NamespaceMutationFeature)
	}
	return !admissionEnabled || !mutationEnabled, nil
}
