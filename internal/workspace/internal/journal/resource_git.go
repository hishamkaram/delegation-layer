package journal

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace/contract"
)

const (
	resourceSourceName       = "source.json"
	resourceCheckoutName     = "checkout.json"
	resourceRegistrationName = "registration.json"
)

// RecordSource durably records the committed source candidate before any Git
// ref or checkout side effect. It stores observations only; it never invokes
// Git or checks the filesystem.
func (r *Resource) RecordSource(ctx context.Context, source contract.Source) error {
	if err := validateResourceSource(source); err != nil {
		return err
	}
	return r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		if err := crossCheckRegistrationSource(tx, source, r.request.WorkspaceID); err != nil {
			return classifyStoredResourceRecordError(err)
		}
		if checkout, present, err := readOptionalCheckoutTx(tx, r.request.WorkspaceID); err != nil {
			return err
		} else if present && checkout.Source != source {
			return task.ErrIdentityMismatch
		}
		if _, err := tx.Put(resourceSourceName, source); err != nil {
			return err
		}
		return tx.Verify(resourceSourceName, source)
	})
}

// Source returns the exact committed source candidate after local structural
// validation and canonical-byte verification. It does not call Git.
func (r *Resource) Source(ctx context.Context) (source contract.Source, resultErr error) {
	resultErr = r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		var readErr error
		source, readErr = readSourceTx(tx)
		if readErr != nil {
			return readErr
		}
		if err := crossCheckRegistrationSource(tx, source, r.request.WorkspaceID); err != nil {
			return classifyStoredResourceRecordError(err)
		}
		checkout, present, err := readOptionalCheckoutTx(tx, r.request.WorkspaceID)
		if err != nil {
			return err
		}
		if present && checkout.Source != source {
			return errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch)
		}
		return nil
	})
	return source, resultErr
}

// RecordCheckout commits the exact Git descriptor returned by provisioning.
// The embedded source is cross-checked against the canonical source record so
// it cannot become a second mutable source of truth.
func (r *Resource) RecordCheckout(ctx context.Context, checkout contract.Checkout) error {
	if err := validateResourceCheckout(checkout); err != nil {
		return err
	}
	return r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		source, err := readSourceTx(tx)
		if err != nil {
			if task.RecordAbsent(err) {
				return errors.Join(task.ErrEvidenceFault, err)
			}
			return err
		}
		if source != checkout.Source {
			return task.ErrIdentityMismatch
		}
		if err = crossCheckRegistrationCheckout(tx, checkout, r.request.WorkspaceID); err != nil {
			return err
		}
		if _, err = tx.Put(resourceCheckoutName, checkout); err != nil {
			return err
		}
		return tx.Verify(resourceCheckoutName, checkout)
	})
}

// Checkout returns the exact descriptor needed by the coordinator's
// already-authorized Git removal operation.
func (r *Resource) Checkout(ctx context.Context) (checkout contract.Checkout, resultErr error) {
	resultErr = r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		var err error
		checkout, err = readCheckoutTx(tx, r.request.WorkspaceID)
		if err != nil {
			return err
		}
		source, err := readSourceTx(tx)
		if err != nil {
			if task.RecordAbsent(err) {
				return errors.Join(task.ErrEvidenceFault, err)
			}
			return err
		}
		if checkout.Source != source {
			return errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch)
		}
		return classifyStoredResourceRecordError(crossCheckRegistrationCheckout(tx, checkout, r.request.WorkspaceID))
	})
	return checkout, resultErr
}

// RecordRegistration stores product-facing metadata while checking every
// overlapping field against the exact source and checkout descriptors when
// those records are present. Checkout remains the cleanup authority.
func (r *Resource) RecordRegistration(ctx context.Context, record contract.Registration) error {
	if err := validateResourceRegistration(record, r.request.WorkspaceID); err != nil {
		return err
	}
	return r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		if err := validateRegistrationSourceTx(tx, record, r.request.WorkspaceID); err != nil {
			return err
		}
		if checkout, present, err := readOptionalCheckoutTx(tx, r.request.WorkspaceID); err != nil {
			return err
		} else if present {
			if err = registrationMatchesCheckout(record, checkout); err != nil {
				return err
			}
		}
		if _, err := tx.Put(resourceRegistrationName, record); err != nil {
			return err
		}
		return tx.Verify(resourceRegistrationName, record)
	})
}

func (r *Resource) Registration(ctx context.Context) (record contract.Registration, resultErr error) {
	resultErr = r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		var err error
		record, err = readRegistrationTx(tx, r.request.WorkspaceID)
		if err != nil {
			return err
		}
		if err = validateRegistrationSourceTx(tx, record, r.request.WorkspaceID); err != nil {
			return err
		}
		checkout, present, err := readOptionalCheckoutTx(tx, r.request.WorkspaceID)
		if err != nil {
			return err
		}
		if present {
			return classifyStoredResourceRecordError(registrationMatchesCheckout(record, checkout))
		}
		return nil
	})
	return record, resultErr
}

func readSourceTx(tx *taskdir.ControlTransaction) (contract.Source, error) {
	var source contract.Source
	if err := tx.Read(resourceSourceName, &source); err != nil {
		return contract.Source{}, classifyResourceRecordReadError(err)
	}
	if err := validateResourceSource(source); err != nil {
		return contract.Source{}, classifyStoredResourceRecordError(err)
	}
	if err := tx.Verify(resourceSourceName, source); err != nil {
		return contract.Source{}, err
	}
	return source, nil
}

func readCheckoutTx(tx *taskdir.ControlTransaction, workspaceID string) (contract.Checkout, error) {
	var checkout contract.Checkout
	if err := tx.Read(resourceCheckoutName, &checkout); err != nil {
		return contract.Checkout{}, classifyResourceRecordReadError(err)
	}
	if err := validateResourceCheckout(checkout); err != nil {
		return contract.Checkout{}, classifyStoredResourceRecordError(err)
	}
	if err := task.ValidateTaskID(workspaceID); err != nil {
		return contract.Checkout{}, err
	}
	if err := tx.Verify(resourceCheckoutName, checkout); err != nil {
		return contract.Checkout{}, err
	}
	return checkout, nil
}

func readRegistrationTx(tx *taskdir.ControlTransaction, workspaceID string) (contract.Registration, error) {
	var record contract.Registration
	if err := tx.Read(resourceRegistrationName, &record); err != nil {
		return contract.Registration{}, classifyResourceRecordReadError(err)
	}
	if err := validateResourceRegistration(record, workspaceID); err != nil {
		return contract.Registration{}, classifyStoredResourceRecordError(err)
	}
	if err := tx.Verify(resourceRegistrationName, record); err != nil {
		return contract.Registration{}, err
	}
	return record, nil
}

func readOptionalCheckoutTx(tx *taskdir.ControlTransaction, workspaceID string) (contract.Checkout, bool, error) {
	checkout, err := readCheckoutTx(tx, workspaceID)
	if task.RecordAbsent(err) {
		return contract.Checkout{}, false, nil
	}
	if err != nil {
		return contract.Checkout{}, false, err
	}
	return checkout, true, nil
}

func readOptionalRegistrationTx(tx *taskdir.ControlTransaction, workspaceID string) (contract.Registration, bool, error) {
	record, err := readRegistrationTx(tx, workspaceID)
	if task.RecordAbsent(err) {
		return contract.Registration{}, false, nil
	}
	if err != nil {
		return contract.Registration{}, false, err
	}
	return record, true, nil
}

func crossCheckRegistrationSource(tx *taskdir.ControlTransaction, source contract.Source, workspaceID string) error {
	record, present, err := readOptionalRegistrationTx(tx, workspaceID)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	return registrationMatchesSource(record, source)
}

// validateRegistrationSourceTx compares registration metadata with the
// authoritative source.json already stored in this transaction. It never
// derives a source record from the registration being validated.
func validateRegistrationSourceTx(tx *taskdir.ControlTransaction, record contract.Registration, workspaceID string) error {
	source, err := readSourceTx(tx)
	if err != nil {
		if task.RecordAbsent(err) {
			return errors.Join(task.ErrEvidenceFault, err)
		}
		return err
	}
	return classifyStoredResourceRecordError(registrationMatchesSource(record, source))
}

func crossCheckRegistrationCheckout(tx *taskdir.ControlTransaction, checkout contract.Checkout, workspaceID string) error {
	record, present, err := readOptionalRegistrationTx(tx, workspaceID)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	return registrationMatchesCheckout(record, checkout)
}

func registrationMatchesSource(record contract.Registration, source contract.Source) error {
	if record.SourceRoot != source.Root || record.CommonDir != source.CommonDir || record.GitDir != source.GitDir || record.RelativeCwd != source.RelativeCwd || record.BaseOID != source.BaseOID || record.ObjectFormat != source.ObjectFormat || record.SourceChangesExcluded != source.Dirty {
		return task.ErrIdentityMismatch
	}
	return nil
}

func registrationMatchesCheckout(record contract.Registration, checkout contract.Checkout) error {
	if record.AdminDir != checkout.AdminDir || record.AdminBacklink != checkout.AdminBacklink || record.CheckoutPath != checkout.CheckoutPath || record.CanonicalCwd != checkout.Workdir {
		return task.ErrIdentityMismatch
	}
	return registrationMatchesSource(record, checkout.Source)
}

func validateResourceSource(source contract.Source) error {
	if err := contract.ValidateSourceShape(source); err != nil {
		return err
	}
	for _, path := range []string{source.Root, source.CommonDir, source.GitDir} {
		if err := contract.ValidateAbsoluteCleanPath(path); err != nil {
			return errors.Join(contract.ErrInvalidGitContext, err)
		}
	}
	return nil
}

func validateResourceCheckout(checkout contract.Checkout) error {
	if err := validateResourceSource(checkout.Source); err != nil {
		return err
	}
	for _, path := range []string{checkout.CheckoutPath, checkout.Workdir, checkout.AdminDir, checkout.AdminBacklink} {
		if err := contract.ValidateAbsoluteCleanPath(path); err != nil {
			return errors.Join(contract.ErrIdentityMismatch, err)
		}
	}
	if checkout.AdminDir != checkout.AdminBacklink || !contract.PathWithin(checkout.CheckoutPath, checkout.Workdir) {
		return contract.ErrIdentityMismatch
	}
	relative := filepath.FromSlash(checkout.Source.RelativeCwd)
	expectedWorkdir := filepath.Join(checkout.CheckoutPath, relative)
	if filepath.Clean(expectedWorkdir) != checkout.Workdir {
		return fmt.Errorf("%w: checkout workdir does not match source relative cwd", contract.ErrIdentityMismatch)
	}
	return nil
}

func validateResourceRegistration(record contract.Registration, workspaceID string) error {
	if record.SchemaVersion != task.SchemaVersion || record.WorkspaceID != workspaceID {
		return task.ErrIdentityMismatch
	}
	if err := task.ValidateTaskID(record.WorkspaceID); err != nil {
		return err
	}
	if err := validateRegistrationPaths(record); err != nil {
		return err
	}
	if err := validateRegistrationGit(record); err != nil {
		return err
	}
	return validateRegistrationCoordinates(record)
}

func validateRegistrationPaths(record contract.Registration) error {
	for _, path := range []string{record.SourceRoot, record.CommonDir, record.GitDir, record.AdminDir, record.AdminBacklink, record.CheckoutPath, record.CanonicalCwd} {
		if err := contract.ValidateAbsoluteCleanPath(path); err != nil {
			return err
		}
	}
	return contract.ValidateSourceRelativeCwd(record.RelativeCwd)
}

func validateRegistrationGit(record contract.Registration) error {
	if err := contract.ValidateObjectFormat(record.ObjectFormat); err != nil {
		return err
	}
	if err := contract.ValidateOID(record.BaseOID, record.ObjectFormat); err != nil {
		return err
	}
	if record.BaseRef == "" {
		return nil
	}
	return contract.ValidateRevision(record.BaseRef)
}

func validateRegistrationCoordinates(record contract.Registration) error {
	if record.AdminDir != record.AdminBacklink || !contract.PathWithin(record.CheckoutPath, record.CanonicalCwd) {
		return task.ErrIdentityMismatch
	}
	expectedCwd := filepath.Join(record.CheckoutPath, filepath.FromSlash(record.RelativeCwd))
	if filepath.Clean(expectedCwd) != record.CanonicalCwd {
		return task.ErrIdentityMismatch
	}
	if record.ContainerIdentity.Device == 0 || record.ContainerIdentity.Inode == 0 || record.CheckoutIdentity.Device == 0 || record.CheckoutIdentity.Inode == 0 {
		return task.ErrIdentityMismatch
	}
	return nil
}
