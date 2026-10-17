package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// prepareStateTerminalEventRunner serializes stable callback-helper publication
// with the state-root transition and bootstrap owners. The durable record is
// the publication authority; the executable is its recoverable materialization.
func PrepareStateTerminalEventRunner(ctx context.Context, root, source string) (string, error) {
	return prepareStateTerminalEventRunnerRevision(ctx, root, source, stateTerminalEventRunnerRevision)
}

func prepareStateTerminalEventRunnerRevision(ctx context.Context, root, source string, revision uint64) (string, error) {
	return prepareStateTerminalEventRunnerRevisionWithSync(ctx, root, source, revision, syncRunnerDirectory)
}

func prepareStateTerminalEventRunnerRevisionWithSync(ctx context.Context, root, source string, revision uint64, syncDirectory func(string) error) (string, error) {
	if ctx == nil {
		return "", context.Canceled
	}
	if revision == 0 {
		return "", fmt.Errorf("%w: terminal event runner implementation revision is zero", pueue.ErrConfiguration)
	}
	if syncDirectory == nil {
		return "", fmt.Errorf("%w: terminal event runner directory sync is nil", pueue.ErrConfiguration)
	}
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		destination, ready, err := probeStateTerminalEventRunner(ctx, root, revision, syncDirectory)
		if err != nil || ready {
			return destination, err
		}
		destination = ""
		acquired, transitionErr := pueue.TryWithPrivateTransition(ctx, root, func() error {
			return pueue.WithBootstrapLockContext(ctx, root, func(lockedContext context.Context) error {
				var err error
				destination, err = installStateTerminalEventRunnerRevisionWithSync(lockedContext, root, source, revision, syncDirectory)
				return err
			})
		})
		if transitionErr != nil {
			return "", transitionErr
		}
		if acquired {
			return destination, nil
		}
		if err := pueue.WaitForPrivateTransition(ctx, root); err != nil {
			return "", err
		}
	}
}

// probeStateTerminalEventRunner recognizes a fully published compatible
// helper without entering the lifecycle-transition boundary. The record owns
// the desired semantic revision and build digest; the stable executable is its
// recoverable materialization. A publisher writes the record before replacing
// the executable, so any incomplete publication falls through to the exclusive
// recovery path below.
func probeStateTerminalEventRunner(ctx context.Context, root string, revision uint64, syncDirectory func(string) error) (destination string, ready bool, resultErr error) {
	if ctx == nil {
		return "", false, context.Canceled
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || revision == 0 || syncDirectory == nil {
		return "", false, pueue.ErrConfiguration
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	resultErr = pueue.WithSupervisorLeaseContext(ctx, root, func(lockedContext context.Context) error {
		base := filepath.Join(root, ".supervisor")
		record, found, err := readStateTerminalEventRunnerRecord(base)
		if err != nil || !found || record.ImplementationRevision < revision {
			return err
		}
		destination = pueue.PrivateTerminalEventRunnerPath(root)
		matched, err := stateExecutableMatches(destination, record.BuildSHA256)
		if err != nil || !matched {
			return err
		}
		if err = syncDirectory(base); err != nil {
			return err
		}
		if err = lockedContext.Err(); err != nil {
			return err
		}
		ready = true
		return nil
	})
	if errors.Is(resultErr, pueue.ErrPrivateSupervisorNotInitialized) {
		return "", false, nil
	}
	return destination, ready, resultErr
}

func installStateTerminalEventRunnerRevisionWithSync(ctx context.Context, root, source string, revision uint64, syncDirectory func(string) error) (string, error) {
	if ctx == nil {
		return "", context.Canceled
	}
	if revision == 0 || syncDirectory == nil {
		return "", pueue.ErrConfiguration
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	base, destination, digest, err := prepareStateTerminalEventRunnerInstallation(root, source)
	if err != nil {
		return "", err
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	record, ready, err := reconcileStateTerminalEventRunnerRecord(base, destination, digest, revision, syncDirectory)
	if err != nil {
		return "", err
	}
	if ready {
		return destination, nil
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	matched, err := stateExecutableMatches(destination, record.BuildSHA256)
	if err != nil {
		return "", err
	}
	if matched {
		return destination, syncDirectory(base)
	}
	if err = replaceStateTerminalEventRunnerWithSync(base, source, destination, record.BuildSHA256, syncDirectory); err != nil {
		return "", err
	}
	return destination, nil
}

func prepareStateTerminalEventRunnerInstallation(root, source string) (string, string, string, error) {
	base := filepath.Join(root, ".supervisor")
	if err := ensureSupervisorDirectory(root, base); err != nil {
		return "", "", "", err
	}
	destination := pueue.PrivateTerminalEventRunnerPath(root)
	if filepath.Base(destination) != stateTerminalEventRunnerName || filepath.Dir(destination) != base {
		return "", "", "", pueue.ErrConfiguration
	}
	digest, err := stateExecutableSourceDigest(source)
	if err != nil {
		return "", "", "", err
	}
	return base, destination, digest, nil
}

func reconcileStateTerminalEventRunnerRecord(base, destination, digest string, revision uint64, syncDirectory func(string) error) (stateTerminalEventRunnerRecord, bool, error) {
	record, found, err := readStateTerminalEventRunnerRecord(base)
	if err != nil {
		return stateTerminalEventRunnerRecord{}, false, err
	}
	if !found {
		record = newStateTerminalEventRunnerRecord(revision, digest)
		if err := publishStateTerminalEventRunnerRecord(base, record, syncDirectory); err != nil {
			return stateTerminalEventRunnerRecord{}, false, err
		}
		return record, false, nil
	}
	if revision < record.ImplementationRevision {
		ready, useErr := usePublishedStateTerminalEventRunner(base, destination, record.BuildSHA256, syncDirectory, fmt.Errorf("%w: source revision %d is older than desired revision %d", errStateTerminalEventRunnerRevisionDeferred, revision, record.ImplementationRevision))
		return record, ready, useErr
	}
	if revision == record.ImplementationRevision && digest != record.BuildSHA256 {
		// The revision is the semantic identity. A different build of the
		// same implementation must not replace the already-published helper.
		ready, useErr := usePublishedStateTerminalEventRunner(base, destination, record.BuildSHA256, syncDirectory, fmt.Errorf("%w: equal implementation revision has a different build digest", errStateTerminalEventRunnerRevisionDeferred))
		return record, ready, useErr
	}
	if revision > record.ImplementationRevision {
		record = newStateTerminalEventRunnerRecord(revision, digest)
		if err := publishStateTerminalEventRunnerRecord(base, record, syncDirectory); err != nil {
			return stateTerminalEventRunnerRecord{}, false, err
		}
	}
	return record, false, nil
}

func usePublishedStateTerminalEventRunner(base, destination, digest string, syncDirectory func(string) error, deferred error) (bool, error) {
	matched, err := stateExecutableMatches(destination, digest)
	if err != nil {
		return false, err
	}
	if matched {
		return true, syncDirectory(base)
	}
	return false, deferred
}

func replaceStateTerminalEventRunnerWithSync(base, source, destination, digest string, syncDirectory func(string) error) (resultErr error) {
	stagePath, err := prepareStateExecutableStage(base, source, digest)
	if err != nil {
		return err
	}
	defer func() {
		if stagePath != "" {
			resultErr = errors.Join(resultErr, removeStateExecutableStage(base, stagePath, syncDirectory))
		}
	}()
	if err = os.Rename(stagePath, destination); err != nil {
		return fmt.Errorf("replace terminal event runner: %w", err)
	}
	stagePath = ""
	return syncDirectory(base)
}

func readStateTerminalEventRunnerRecord(base string) (record stateTerminalEventRunnerRecord, found bool, resultErr error) {
	recordPath := filepath.Join(base, stateTerminalEventRunnerRecordName)
	info, err := os.Lstat(recordPath)
	if errors.Is(err, os.ErrNotExist) {
		return stateTerminalEventRunnerRecord{}, false, nil
	}
	if err != nil {
		return stateTerminalEventRunnerRecord{}, true, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return stateTerminalEventRunnerRecord{}, true, fmt.Errorf("%w: terminal event runner record is not protected", pueue.ErrConfiguration)
	}
	file, err := os.Open(recordPath)
	if err != nil {
		return stateTerminalEventRunnerRecord{}, true, err
	}
	data, readErr := task.ReadControlRecord(file)
	if resultErr = errors.Join(readErr, file.Close()); resultErr != nil {
		return stateTerminalEventRunnerRecord{}, true, resultErr
	}
	if err = task.DecodeStrict(data, &record); err != nil {
		return stateTerminalEventRunnerRecord{}, true, fmt.Errorf("%w: decode terminal event runner record: %w", pueue.ErrConfiguration, err)
	}
	if err = validateStateTerminalEventRunnerRecord(record); err != nil {
		return stateTerminalEventRunnerRecord{}, true, err
	}
	return record, true, nil
}

func publishStateTerminalEventRunnerRecord(base string, record stateTerminalEventRunnerRecord, syncDirectory func(string) error) (resultErr error) {
	if syncDirectory == nil {
		return pueue.ErrConfiguration
	}
	if err := validateStateTerminalEventRunnerRecord(record); err != nil {
		return err
	}
	data, err := task.MarshalCanonical(record)
	if err != nil {
		return fmt.Errorf("%w: encode terminal event runner record: %w", pueue.ErrConfiguration, err)
	}
	stagePath, err := prepareStateTerminalEventRunnerRecordStage(base, data)
	if err != nil {
		return err
	}
	defer func() {
		if stagePath != "" {
			resultErr = errors.Join(resultErr, os.Remove(stagePath))
		}
	}()
	if err = validateStateTerminalEventRunnerRecordDestination(base); err != nil {
		return err
	}
	destination := filepath.Join(base, stateTerminalEventRunnerRecordName)
	if err = os.Rename(stagePath, destination); err != nil {
		return err
	}
	stagePath = ""
	return syncDirectory(base)
}

func prepareStateTerminalEventRunnerRecordStage(base string, data []byte) (stagePath string, resultErr error) {
	file, err := os.CreateTemp(base, ".delegate-terminal-event-")
	if err != nil {
		return "", err
	}
	temporaryPath := file.Name()
	keepStage := true
	defer func() {
		if keepStage {
			resultErr = errors.Join(resultErr, os.Remove(temporaryPath))
		}
	}()
	if err = file.Chmod(0o600); err != nil {
		return "", errors.Join(err, file.Close())
	}
	written, writeErr := file.Write(data)
	if writeErr != nil || written != len(data) {
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		return "", errors.Join(writeErr, file.Close())
	}
	if err = taskdir.BarrierFile(file); err != nil {
		return "", errors.Join(err, file.Close())
	}
	if err = file.Close(); err != nil {
		return "", err
	}
	keepStage = false
	return temporaryPath, nil
}

func validateStateTerminalEventRunnerRecordDestination(base string) error {
	destination := filepath.Join(base, stateTerminalEventRunnerRecordName)
	info, err := os.Lstat(destination)
	if err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("%w: terminal event runner record is not a protected regular file", pueue.ErrConfiguration)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
