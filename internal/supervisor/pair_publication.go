package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// prepareStateSupervisorPair verifies an already-published pair under a shared
// lease. Only installation or a changed pair needs exclusive publication.
func PrepareStateSupervisorPair(ctx context.Context, root, clientExecutable, daemonExecutable string) error {
	expected, err := stateSupervisorPairIdentity(ctx, clientExecutable, daemonExecutable)
	if err != nil {
		return err
	}
	for {
		ready, probeErr := probeStateSupervisorPairWithLease(ctx, root, &expected)
		if probeErr != nil || ready {
			return probeErr
		}
		acquired, transitionErr := pueue.TryWithPrivateTransition(ctx, root, func() error {
			return pueue.WithBootstrapLockContext(ctx, root, func(lockedContext context.Context) error {
				// A competing owner may have published the exact pair between the
				// shared probe and this transition. Recheck while exclusive before
				// copying or replacing any state-root executable.
				current, currentErr := probeStateSupervisorPair(lockedContext, root, &expected)
				if currentErr != nil && !errors.Is(currentErr, os.ErrNotExist) {
					return currentErr
				}
				if current {
					return nil
				}
				return installStateSupervisorPair(lockedContext, root, clientExecutable, daemonExecutable)
			})
		})
		if transitionErr != nil {
			return transitionErr
		}
		if acquired {
			return nil
		}
		if transitionErr = pueue.WaitForPrivateTransition(ctx, root); transitionErr != nil {
			return transitionErr
		}
		// Another process owns the publication. Wait for it to finish before
		// the next shared probe, rather than starving its bootstrap lock.
	}
}

func probeStateSupervisorPairWithLease(ctx context.Context, root string, expected *stateSupervisorPairRecord) (ready bool, resultErr error) {
	resultErr = pueue.WithSupervisorLeaseContext(ctx, root, func(lockedContext context.Context) error {
		var probeErr error
		ready, probeErr = probeStateSupervisorPair(lockedContext, root, expected)
		if errors.Is(probeErr, os.ErrNotExist) {
			return nil
		}
		return probeErr
	})
	if errors.Is(resultErr, pueue.ErrPrivateSupervisorNotInitialized) {
		return false, nil
	}
	return ready, resultErr
}

func probeStateSupervisorPair(ctx context.Context, root string, expected *stateSupervisorPairRecord) (bool, error) {
	base := filepath.Join(root, ".supervisor")
	saved, found, err := readStateSupervisorPairRecord(base)
	if err != nil || !found || (expected != nil && saved != *expected) {
		return false, err
	}
	_, _, found, err = ResolveStateSupervisorPair(base)
	if err != nil || !found {
		return false, err
	}
	if err = ctx.Err(); err != nil {
		return false, err
	}
	// A prior publisher may have exited after rename but before its final
	// directory barrier. Reestablish it without creating or replacing records.
	// The child barrier also completes a missing-member repair interrupted
	// after linking the executable but before syncing the pair directory.
	pairDirectory := filepath.Join(base, stateSupervisorPairDirectoryPrefix+saved.PairSHA256)
	if err = syncRunnerDirectory(pairDirectory); err != nil {
		return false, err
	}
	if err = syncRunnerDirectory(base); err != nil {
		return false, err
	}
	return true, ctx.Err()
}

// installStateSupervisorPair requires exclusive supervisor ownership in
// production. Migration already holds it; other callers use preparation above.
func installStateSupervisorPair(ctx context.Context, root, clientExecutable, daemonExecutable string) error {
	_, _, err := InstallStateSupervisorPairPaths(ctx, root, clientExecutable, daemonExecutable)
	return err
}

// installStateSupervisorPairPaths returns the immutable pair just installed,
// without rereading the mutable inventory pointer used to stage later upgrades.
func InstallStateSupervisorPairPaths(ctx context.Context, root, clientExecutable, daemonExecutable string) (string, string, error) {
	if ctx == nil {
		return "", "", context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	base := filepath.Join(root, ".supervisor")
	if err := ensureSupervisorDirectory(root, base); err != nil {
		return "", "", err
	}
	record, err := stateSupervisorPairIdentity(ctx, clientExecutable, daemonExecutable)
	if err != nil {
		return "", "", err
	}
	if err = installStateSupervisorPairContentAddressed(ctx, base, clientExecutable, daemonExecutable, record); err != nil {
		return "", "", err
	}
	pairDirectory := filepath.Join(base, stateSupervisorPairDirectoryPrefix+record.PairSHA256)
	return filepath.Join(pairDirectory, "pueue"), filepath.Join(pairDirectory, "pueued"), nil
}

func installStateSupervisorPairContentAddressed(ctx context.Context, base, clientExecutable, daemonExecutable string, record stateSupervisorPairRecord) error {
	pairDirectory := filepath.Join(base, stateSupervisorPairDirectoryPrefix+record.PairSHA256)
	if err := ensureStateSupervisorPairDirectory(base, pairDirectory); err != nil {
		return err
	}
	for _, executable := range []struct {
		source string
		name   string
		digest string
	}{{source: clientExecutable, name: "pueue", digest: record.ClientSHA256}, {source: daemonExecutable, name: "pueued", digest: record.DaemonSHA256}} {
		if err := ctx.Err(); err != nil {
			return err
		}
		destination := filepath.Join(pairDirectory, executable.name)
		if err := installImmutableStateExecutable(pairDirectory, executable.source, destination, executable.digest); err != nil {
			return err
		}
		installedDigest, digestErr := protectedStateExecutableDigest(destination)
		if digestErr != nil || installedDigest != executable.digest {
			return errors.Join(fmt.Errorf("%w: state-root supervisor pair changed while being installed", pueue.ErrConfiguration), digestErr)
		}
	}
	if err := syncRunnerDirectory(pairDirectory); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return publishStateSupervisorPairRecord(base, record)
}

func stateSupervisorPairIdentity(ctx context.Context, clientExecutable, daemonExecutable string) (stateSupervisorPairRecord, error) {
	if ctx == nil {
		return stateSupervisorPairRecord{}, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return stateSupervisorPairRecord{}, err
	}
	clientDigest, err := stateExecutableSourceDigest(clientExecutable)
	if err != nil {
		return stateSupervisorPairRecord{}, err
	}
	if err = ctx.Err(); err != nil {
		return stateSupervisorPairRecord{}, err
	}
	daemonDigest, err := stateExecutableSourceDigest(daemonExecutable)
	if err != nil {
		return stateSupervisorPairRecord{}, err
	}
	return stateSupervisorPairRecord{
		SchemaVersion: 1,
		PairSHA256:    stateSupervisorPairDigest(clientDigest, daemonDigest),
		ClientSHA256:  clientDigest,
		DaemonSHA256:  daemonDigest,
	}, ctx.Err()
}

func ensureStateSupervisorPairDirectory(base, pairDirectory string) error {
	if filepath.Dir(pairDirectory) != base || !strings.HasPrefix(filepath.Base(pairDirectory), stateSupervisorPairDirectoryPrefix) {
		return fmt.Errorf("%w: invalid state-root supervisor pair directory", pueue.ErrConfiguration)
	}
	if err := os.Mkdir(pairDirectory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	} else if err == nil {
		if err = syncRunnerDirectory(base); err != nil {
			return err
		}
	}
	return validateStateSupervisorPairDirectory(base, pairDirectory)
}

func validateStateSupervisorPairDirectory(base, pairDirectory string) error {
	if filepath.Dir(pairDirectory) != base || !strings.HasPrefix(filepath.Base(pairDirectory), stateSupervisorPairDirectoryPrefix) {
		return fmt.Errorf("%w: invalid state-root supervisor pair directory", pueue.ErrConfiguration)
	}
	info, err := os.Lstat(pairDirectory)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: state-root supervisor pair directory is not protected", pueue.ErrConfiguration)
	}
	return nil
}

func ResolveStateSupervisorPair(directory string) (client, daemon string, found bool, resultErr error) {
	record, found, err := readStateSupervisorPairRecord(directory)
	if err != nil || !found {
		return "", "", found, err
	}
	pairDirectory := filepath.Join(directory, stateSupervisorPairDirectoryPrefix+record.PairSHA256)
	if err = validateStateSupervisorPairDirectory(directory, pairDirectory); err != nil {
		return "", "", found, err
	}
	client, err = resolveStateSupervisorPairExecutable(pairDirectory, "pueue", record.ClientSHA256)
	if err != nil {
		return "", "", found, err
	}
	daemon, err = resolveStateSupervisorPairExecutable(pairDirectory, "pueued", record.DaemonSHA256)
	if err != nil {
		return "", "", found, err
	}
	return client, daemon, found, nil
}

func ValidateSavedStateSupervisorPair(root string, saved task.SupervisorRef) error {
	base := filepath.Join(root, ".supervisor")
	clientDirectory, clientManaged := stateSupervisorPairDirectoryForExecutable(base, saved.ClientExecutable)
	daemonDirectory, daemonManaged := stateSupervisorPairDirectoryForExecutable(base, saved.DaemonExecutable)
	if !clientManaged && !daemonManaged {
		return nil
	}
	if !clientManaged || !daemonManaged || clientDirectory != daemonDirectory {
		return pueue.ErrBinding
	}
	return validateSavedStateSupervisorPairContents(base, clientDirectory, saved)
}

func validateSavedStateSupervisorPairContents(base, pairDirectory string, saved task.SupervisorRef) error {
	if filepath.Base(saved.ClientExecutable) != "pueue" || filepath.Base(saved.DaemonExecutable) != "pueued" ||
		task.ValidateSHA256(saved.ClientSHA256) != nil || task.ValidateSHA256(saved.DaemonSHA256) != nil {
		return pueue.ErrBinding
	}
	if err := validateStateSupervisorDirectory(base); err != nil {
		return err
	}
	expectedDirectory := filepath.Join(base, stateSupervisorPairDirectoryPrefix+stateSupervisorPairDigest(saved.ClientSHA256, saved.DaemonSHA256))
	if pairDirectory != expectedDirectory {
		return pueue.ErrBinding
	}
	client, err := resolveStateSupervisorPairExecutable(pairDirectory, "pueue", saved.ClientSHA256)
	if err != nil {
		return err
	}
	daemon, err := resolveStateSupervisorPairExecutable(pairDirectory, "pueued", saved.DaemonSHA256)
	if err != nil {
		return err
	}
	if client != filepath.Clean(saved.ClientExecutable) || daemon != filepath.Clean(saved.DaemonExecutable) {
		return pueue.ErrBinding
	}
	return nil
}

func stateSupervisorPairDirectoryForExecutable(base, executable string) (string, bool) {
	if executable == "" {
		return "", false
	}
	directory := filepath.Dir(filepath.Clean(executable))
	if filepath.Dir(directory) != base || !strings.HasPrefix(filepath.Base(directory), stateSupervisorPairDirectoryPrefix) {
		return "", false
	}
	return directory, true
}

func readStateSupervisorPairRecord(directory string) (record stateSupervisorPairRecord, found bool, resultErr error) {
	recordPath := filepath.Join(directory, stateSupervisorPairRecordName)
	if _, err := os.Lstat(recordPath); errors.Is(err, os.ErrNotExist) {
		return stateSupervisorPairRecord{}, false, nil
	} else if err != nil {
		return stateSupervisorPairRecord{}, true, err
	}
	found = true
	data, err := readProtectedStateSupervisorPairRecord(recordPath)
	if err != nil {
		return stateSupervisorPairRecord{}, found, err
	}
	if err = task.DecodeStrict(data, &record); err != nil {
		return stateSupervisorPairRecord{}, found, fmt.Errorf("%w: decode state-root supervisor pair record: %w", pueue.ErrConfiguration, err)
	}
	if err = validateStateSupervisorPairRecord(record); err != nil {
		return stateSupervisorPairRecord{}, found, err
	}
	if err = validateStateSupervisorDirectory(directory); err != nil {
		return stateSupervisorPairRecord{}, found, err
	}
	return record, found, nil
}

func readProtectedStateSupervisorPairRecord(path string) (_ []byte, resultErr error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%w: state-root supervisor pair record is not protected", pueue.ErrConfiguration)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, readErr := task.ReadControlRecord(file)
	if resultErr = errors.Join(readErr, file.Close()); resultErr != nil {
		return nil, resultErr
	}
	return data, nil
}

func validateStateSupervisorDirectory(directory string) error {
	baseInfo, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if !baseInfo.IsDir() || baseInfo.Mode()&os.ModeSymlink != 0 || baseInfo.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: state-root supervisor directory is not protected", pueue.ErrConfiguration)
	}
	return nil
}

func resolveStateSupervisorPairExecutable(directory, name, expectedDigest string) (string, error) {
	candidate := filepath.Join(directory, name)
	info, err := os.Lstat(candidate)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return "", fmt.Errorf("%w: state-root %s is not a protected executable", pueue.ErrConfiguration, name)
	}
	resolved, err := resolveSupervisorExecutable(candidate, name)
	if err != nil {
		return "", err
	}
	if resolved != candidate {
		return "", fmt.Errorf("%w: state-root %s path changed during resolution", pueue.ErrConfiguration, name)
	}
	digest, err := stateExecutableSourceDigest(resolved)
	if err != nil {
		return "", err
	}
	if digest != expectedDigest {
		return "", fmt.Errorf("%w: state-root %s digest does not match its pair record", pueue.ErrConfiguration, name)
	}
	return resolved, nil
}

func publishStateSupervisorPairRecord(base string, record stateSupervisorPairRecord) (resultErr error) {
	data, err := task.MarshalCanonical(record)
	if err != nil {
		return fmt.Errorf("%w: encode state-root supervisor pair record: %w", pueue.ErrConfiguration, err)
	}
	file, err := os.CreateTemp(base, ".pueue-pair-")
	if err != nil {
		return err
	}
	stagePath := file.Name()
	defer func() {
		if stagePath != "" {
			resultErr = errors.Join(resultErr, os.Remove(stagePath))
		}
	}()
	if err = file.Chmod(0o600); err != nil {
		return errors.Join(err, file.Close())
	}
	written, writeErr := file.Write(data)
	if writeErr != nil || written != len(data) {
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		return errors.Join(writeErr, file.Close())
	}
	if err = taskdir.BarrierFile(file); err != nil {
		return errors.Join(err, file.Close())
	}
	if err = file.Close(); err != nil {
		return err
	}
	destination := filepath.Join(base, stateSupervisorPairRecordName)
	if info, statErr := os.Lstat(destination); statErr == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("%w: state-root supervisor pair record is not a protected regular file", pueue.ErrConfiguration)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if err = os.Rename(stagePath, destination); err != nil {
		return err
	}
	stagePath = ""
	return syncRunnerDirectory(base)
}
