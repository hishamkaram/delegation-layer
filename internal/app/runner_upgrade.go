package app

import (
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

const (
	stateRunnerName                           = "delegate-run"
	stateTerminalEventRunnerName              = "delegate-terminal-event-v1"
	stateTerminalEventRunnerRecordName        = stateTerminalEventRunnerName + ".json"
	stateTerminalEventRunnerRevision   uint64 = 2
	managedRunnerMain                         = "github.com/hishamkaram/delegation-layer/cmd/delegate-run"
	stateSupervisorPairRecordName             = "pueue-pair.json"
	stateSupervisorPairDirectoryPrefix        = "pueue-pair-"
	runnerRepairReceiptRevision               = "managed-runner-v1"
)

var errStateTerminalEventRunnerRevisionDeferred = errors.New("terminal event runner publication deferred to a newer implementation")

type stateSupervisorPairRecord struct {
	SchemaVersion int    `json:"schema_version"`
	PairSHA256    string `json:"pair_sha256"`
	ClientSHA256  string `json:"client_sha256"`
	DaemonSHA256  string `json:"daemon_sha256"`
}

type stateTerminalEventRunnerRecord struct {
	SchemaVersion          int    `json:"schema_version"`
	ProtocolPath           string `json:"protocol_path"`
	ImplementationRevision uint64 `json:"implementation_revision"`
	BuildSHA256            string `json:"build_sha256"`
}

// runnerRepairReceipt records one fully completed queued-runner repair for a
// precise private supervisor generation and content-addressed target runner.
// Records are immutable: a changed daemon/configuration generation or runner
// gets a different receipt name and therefore requires a fresh repair.
type runnerRepairReceipt struct {
	SchemaVersion    int                `json:"schema_version"`
	Revision         string             `json:"revision"`
	Supervisor       task.SupervisorRef `json:"supervisor"`
	RunnerExecutable string             `json:"runner_executable"`
	RunnerSHA256     string             `json:"runner_sha256"`
}

type queuedRunnerRepairTarget struct {
	source     string
	executable string
	sha256     string
}

func installStateRunner(root, source string) (_ string, resultErr error) {
	base := filepath.Join(root, ".supervisor")
	if err := ensureSupervisorDirectory(root, base); err != nil {
		return "", err
	}
	digest, err := stateExecutableSourceDigest(source)
	if err != nil {
		return "", err
	}
	name := stateRunnerName + "-" + digest
	destination := filepath.Join(base, name)
	if filepath.Clean(source) == destination {
		if _, err = protectedStateExecutableDigest(destination); err != nil {
			return "", err
		}
		return destination, nil
	}
	if err = installImmutableStateExecutable(base, source, destination, digest); err != nil {
		return "", err
	}
	return destination, nil
}

// prepareStateTerminalEventRunner serializes stable callback-helper publication
// with the state-root transition and bootstrap owners. The durable record is
// the publication authority; the executable is its recoverable materialization.
func prepareStateTerminalEventRunner(ctx context.Context, root, source string) (string, error) {
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

// installStateTerminalEventRunner is retained as a compatibility entry point
// for callers that do not yet pass their admission context. New call sites
// should use prepareStateTerminalEventRunner so publication shares that
// caller's deadline.
func installStateTerminalEventRunner(root, source string) (_ string, resultErr error) {
	return prepareStateTerminalEventRunner(context.Background(), root, source)
}

func installStateTerminalEventRunnerRevision(ctx context.Context, root, source string, revision uint64) (string, error) {
	return installStateTerminalEventRunnerRevisionWithSync(ctx, root, source, revision, syncRunnerDirectory)
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

func newStateTerminalEventRunnerRecord(revision uint64, digest string) stateTerminalEventRunnerRecord {
	return stateTerminalEventRunnerRecord{
		SchemaVersion:          task.SchemaVersion,
		ProtocolPath:           stateTerminalEventRunnerName,
		ImplementationRevision: revision,
		BuildSHA256:            digest,
	}
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

func replaceStateTerminalEventRunner(base, source, destination, digest string) (resultErr error) {
	return replaceStateTerminalEventRunnerWithSync(base, source, destination, digest, syncRunnerDirectory)
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

func validateStateTerminalEventRunnerRecord(record stateTerminalEventRunnerRecord) error {
	if record.SchemaVersion != task.SchemaVersion || record.ProtocolPath != stateTerminalEventRunnerName || record.ImplementationRevision == 0 || task.ValidateSHA256(record.BuildSHA256) != nil {
		return fmt.Errorf("%w: invalid terminal event runner record identity", pueue.ErrConfiguration)
	}
	return nil
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

func installImmutableStateExecutable(base, source, destination, digest string) (resultErr error) {
	matched, err := stateExecutableMatches(destination, digest)
	if err != nil {
		return err
	}
	if matched {
		return nil
	}
	stagePath, err := prepareStateExecutableStage(base, source, digest)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, removeStateExecutableStage(base, stagePath, syncRunnerDirectory))
	}()
	if err = publishStateExecutableStage(stagePath, destination, digest); err != nil {
		return err
	}
	return syncRunnerDirectory(base)
}

func removeStateExecutableStage(base, stagePath string, syncDirectory func(string) error) error {
	if err := os.Remove(stagePath); err != nil {
		return err
	}
	return syncDirectory(base)
}

func prepareStateExecutableStage(base, source, digest string) (stagePath string, resultErr error) {
	input, err := os.Open(source)
	if err != nil {
		return "", err
	}
	stage, err := os.CreateTemp(base, ".delegate-run-")
	if err != nil {
		return "", errors.Join(err, input.Close())
	}
	tempPath := stage.Name()
	stageOpen := true
	defer func() {
		if stageOpen {
			resultErr = errors.Join(resultErr, stage.Close())
		}
		resultErr = errors.Join(resultErr, input.Close())
		if resultErr != nil {
			resultErr = errors.Join(resultErr, os.Remove(tempPath))
			stagePath = ""
		}
	}()
	if _, err = io.Copy(stage, input); err != nil {
		return "", err
	}
	if err = stage.Chmod(0o700); err != nil {
		return "", err
	}
	if err = taskdir.BarrierFile(stage); err != nil {
		return "", err
	}
	if err = stage.Close(); err != nil {
		stageOpen = false
		return "", err
	}
	stageOpen = false
	stageDigest, err := protectedStateExecutableDigest(tempPath)
	if err != nil || stageDigest != digest {
		return "", errors.Join(fmt.Errorf("%w: runner changed while being installed", pueue.ErrConfiguration), err)
	}
	return tempPath, nil
}

func publishStateExecutableStage(stagePath, destination, digest string) error {
	if err := os.Link(stagePath, destination); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		if matched, matchErr := stateExecutableMatches(destination, digest); matchErr != nil {
			return matchErr
		} else if !matched {
			return fmt.Errorf("%w: immutable state runner path contains different executable bytes", pueue.ErrConfiguration)
		}
	}
	return nil
}

func isStateRunnerPath(root, path string) bool {
	base := filepath.Join(root, ".supervisor")
	if filepath.Dir(path) != base {
		return false
	}
	name := filepath.Base(path)
	if name == stateRunnerName {
		return true
	}
	prefix := stateRunnerName + "-"
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	digest := strings.TrimPrefix(name, prefix)
	if len(digest) != 64 {
		return false
	}
	for _, value := range digest {
		if (value < '0' || value > '9') && (value < 'a' || value > 'f') {
			return false
		}
	}
	return true
}

func managedRunnerExecutable(root, path, ownership string) bool {
	switch ownership {
	case task.RunnerOwnershipManaged:
		return true
	case task.RunnerOwnershipCustom:
		return false
	}
	if isStateRunnerPath(root, filepath.Clean(path)) {
		return true
	}
	info, err := buildinfo.ReadFile(path)
	return err == nil && managedRunnerBuildInfo(info)
}

func managedInspectionWorkerExecutable(root string, binding inspection.Binding) bool {
	if binding.RunnerOwnership != task.RunnerOwnershipManaged {
		return false
	}
	path := filepath.Clean(binding.WorkerExecutable)
	if !isStateRunnerPath(root, path) {
		return managedRunnerExecutable(root, path, "")
	}
	if filepath.Base(path) == stateRunnerName {
		digest, err := protectedStateExecutableDigest(path)
		return err == nil && digest == binding.WorkerSHA256
	}
	return currentManagedRunnerExecutable(root, path) && strings.TrimPrefix(filepath.Base(path), stateRunnerName+"-") == binding.WorkerSHA256
}

func persistedRunnerOwnership(root string, meta *task.MetaRecord) string {
	if meta == nil {
		return ""
	}
	if meta.RunnerOwnership == task.RunnerOwnershipManaged || meta.RunnerOwnership == task.RunnerOwnershipCustom {
		return meta.RunnerOwnership
	}
	if managedRunnerExecutable(root, meta.RunnerExecutable, "") {
		return task.RunnerOwnershipManaged
	}
	return task.RunnerOwnershipCustom
}

func runnerOwnershipForTask(root string, td *taskdir.TaskDir, meta *task.MetaRecord) (string, error) {
	if meta == nil || meta.RunnerOwnership != "" || td == nil {
		return persistedRunnerOwnership(root, meta), nil
	}
	migrated, err := td.HasManagedRunnerMigration()
	if err != nil {
		return "", err
	}
	if migrated {
		return task.RunnerOwnershipManaged, nil
	}
	return persistedRunnerOwnership(root, meta), nil
}

func currentManagedRunnerExecutable(root, path string) bool {
	if path == "" {
		return false
	}
	if isStateRunnerPath(root, filepath.Clean(path)) {
		name := filepath.Base(path)
		digest := strings.TrimPrefix(name, stateRunnerName+"-")
		if digest == name {
			return false
		}
		actualDigest, err := protectedStateExecutableDigest(path)
		return err == nil && actualDigest == digest
	}
	info, err := buildinfo.ReadFile(path)
	return err == nil && managedRunnerBuildInfo(info)
}

func refreshManagedRetryRunner(a Arguments, deps Dependencies, root string, store *taskdir.Store, td *taskdir.TaskDir, meta *task.MetaRecord) (Arguments, bool, error) {
	ownership, err := runnerOwnershipForTask(root, td, meta)
	if err != nil {
		return a, false, err
	}
	if meta == nil || ownership != task.RunnerOwnershipManaged {
		return a, false, nil
	}
	if a.Runner != "" {
		if a.Runner != meta.RunnerExecutable {
			return a, false, task.ErrIdentityMismatch
		}
		return a, false, nil
	}
	pinnedRunner, pinned, err := legacyInspectionRetryRunner(store, td, meta)
	if err != nil {
		return a, false, err
	}
	if pinned {
		a.Runner = pinnedRunner
		return a, false, nil
	}
	runner, err := resolveRunner("", deps)
	if err != nil {
		return a, false, fmt.Errorf("resolve current managed task runner: %w", err)
	}
	runner, err = installStateRunner(root, runner)
	if err != nil {
		return a, false, fmt.Errorf("install current managed task runner: %w", err)
	}
	if meta.RunnerOwnership == "" {
		if err = recordManagedRunnerMigration(context.Background(), root, td, meta); err != nil {
			return a, false, fmt.Errorf("record managed task runner migration: %w", err)
		}
	}
	a.Runner = runner
	a.runnerOwnership = task.RunnerOwnershipManaged
	return a, true, nil
}

func legacyInspectionRetryRunner(store *taskdir.Store, td *taskdir.TaskDir, meta *task.MetaRecord) (string, bool, error) {
	if meta == nil || meta.RunnerOwnership != "" {
		return "", false, nil
	}
	if store == nil || td == nil {
		return "", false, task.ErrInvalidPermit
	}
	inspected, err := taskHasInspectionOperation(store, meta.TaskID)
	if err != nil || !inspected {
		return "", false, err
	}
	if meta.RunnerExecutable == "" {
		return "", false, task.ErrEvidenceFault
	}
	return meta.RunnerExecutable, true, nil
}

func taskHasInspectionOperation(store *taskdir.Store, taskID string) (bool, error) {
	if store == nil {
		return false, task.ErrInvalidPermit
	}
	operation, err := inspection.LoadOperation(store, taskID)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, operation.Close()
}

func managedRunnerBuildInfo(info *debug.BuildInfo) bool {
	return info != nil && info.Path == managedRunnerMain
}

// prepareStateSupervisorPair verifies an already-published pair under a shared
// lease. Only installation or a changed pair needs exclusive publication.
func prepareStateSupervisorPair(ctx context.Context, root, clientExecutable, daemonExecutable string) error {
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
	_, _, found, err = resolveStateSupervisorPair(base)
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
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	base := filepath.Join(root, ".supervisor")
	if err := ensureSupervisorDirectory(root, base); err != nil {
		return err
	}
	return installStateSupervisorPairContentAddressed(ctx, base, clientExecutable, daemonExecutable)
}

func installStateSupervisorPairContentAddressed(ctx context.Context, base, clientExecutable, daemonExecutable string) error {
	record, err := stateSupervisorPairIdentity(ctx, clientExecutable, daemonExecutable)
	if err != nil {
		return err
	}
	pairDirectory := filepath.Join(base, stateSupervisorPairDirectoryPrefix+record.PairSHA256)
	if err = ensureStateSupervisorPairDirectory(base, pairDirectory); err != nil {
		return err
	}
	for _, executable := range []struct {
		source string
		name   string
		digest string
	}{{source: clientExecutable, name: "pueue", digest: record.ClientSHA256}, {source: daemonExecutable, name: "pueued", digest: record.DaemonSHA256}} {
		if err = ctx.Err(); err != nil {
			return err
		}
		destination := filepath.Join(pairDirectory, executable.name)
		if err = installImmutableStateExecutable(pairDirectory, executable.source, destination, executable.digest); err != nil {
			return err
		}
		installedDigest, digestErr := protectedStateExecutableDigest(destination)
		if digestErr != nil || installedDigest != executable.digest {
			return errors.Join(fmt.Errorf("%w: state-root supervisor pair changed while being installed", pueue.ErrConfiguration), digestErr)
		}
	}
	if err = syncRunnerDirectory(pairDirectory); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
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

func stateSupervisorPairDigest(clientDigest, daemonDigest string) string {
	return task.ComputeSHA256([]byte(clientDigest + "\n" + daemonDigest))
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

func resolveStateSupervisorPair(directory string) (client, daemon string, found bool, resultErr error) {
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

func validateSavedStateSupervisorPair(root string, saved task.SupervisorRef) error {
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

func validateStateSupervisorPairRecord(record stateSupervisorPairRecord) error {
	if record.SchemaVersion != 1 || task.ValidateSHA256(record.PairSHA256) != nil || task.ValidateSHA256(record.ClientSHA256) != nil || task.ValidateSHA256(record.DaemonSHA256) != nil || record.PairSHA256 != stateSupervisorPairDigest(record.ClientSHA256, record.DaemonSHA256) {
		return fmt.Errorf("%w: invalid state-root supervisor pair identity", pueue.ErrConfiguration)
	}
	return nil
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

func installStateExecutable(base, source, name string) (_ string, resultErr error) {
	if err := validateStateExecutableName(name); err != nil {
		return "", err
	}
	destination := filepath.Join(base, name)
	if source == destination {
		return destination, nil
	}
	sourceDigest, err := stateExecutableSourceDigest(source)
	if err != nil {
		return "", err
	}
	matched, err := stateExecutableMatches(destination, sourceDigest)
	if err != nil {
		return "", err
	}
	if matched {
		return destination, nil
	}
	if err = copyStateExecutable(base, source, destination); err != nil {
		return "", err
	}
	return destination, nil
}

func validateStateExecutableName(name string) error {
	if name == "" || filepath.Base(name) != name || strings.IndexByte(name, 0) >= 0 {
		return fmt.Errorf("%w: invalid state-root executable name", pueue.ErrConfiguration)
	}
	return nil
}

func stateExecutableSourceDigest(source string) (string, error) {
	info, err := os.Lstat(source)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%w: state-root executable source must be an executable regular file", pueue.ErrConfiguration)
	}
	return commonprovider.FingerprintExecutable(source)
}

func protectedStateExecutableDigest(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o700 {
		return "", fmt.Errorf("%w: state-root executable must be a protected regular file with mode 0700", pueue.ErrConfiguration)
	}
	return commonprovider.FingerprintExecutable(path)
}

func stateExecutableMatches(destination, sourceDigest string) (bool, error) {
	destinationDigest, err := protectedStateExecutableDigest(destination)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return destinationDigest == sourceDigest, nil
}

func copyStateExecutable(base, source, destination string) (resultErr error) {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, input.Close()) }()
	stage, err := os.CreateTemp(base, ".delegate-run-")
	if err != nil {
		return err
	}
	stagePath := stage.Name()
	defer func() {
		if stagePath != "" {
			resultErr = errors.Join(resultErr, os.Remove(stagePath))
		}
	}()
	if _, err = io.Copy(stage, input); err != nil {
		return errors.Join(err, stage.Close())
	}
	if err = stage.Chmod(0o700); err != nil {
		return errors.Join(err, stage.Close())
	}
	if err = taskdir.BarrierFile(stage); err != nil {
		return errors.Join(err, stage.Close())
	}
	if err = stage.Close(); err != nil {
		return err
	}
	if err = os.Rename(stagePath, destination); err != nil {
		return err
	}
	stagePath = ""
	return syncRunnerDirectory(base)
}

func ensureSupervisorDirectory(root, base string) error {
	info, err := os.Lstat(base)
	if errors.Is(err, os.ErrNotExist) {
		if err = os.Mkdir(base, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		if err = syncRunnerDirectory(root); err != nil {
			return err
		}
		info, err = os.Lstat(base)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: private supervisor directory is not a protected directory", pueue.ErrConfiguration)
	}
	return nil
}

func syncRunnerDirectory(path string) (resultErr error) {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	return unix.Fsync(int(directory.Fd()))
}

func repairQueuedRunnerCommandsToTarget(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, targetRunner string) error {
	replacements, err := collectQueuedRunnerCommandReplacements(ctx, store, supervisor, targetRunner)
	if err != nil || len(replacements) == 0 {
		return err
	}
	return supervisor.ReplaceQueuedRunnerCommands(ctx, replacements)
}

func prepareQueuedRunnerRepairTarget(store *taskdir.Store, runnerExecutable string) (string, error) {
	target, err := planQueuedRunnerRepairTarget(store, runnerExecutable)
	if err != nil {
		return "", err
	}
	installed, err := installStateRunner(store.Root, target.source)
	if err != nil {
		return "", err
	}
	if installed != target.executable {
		return "", fmt.Errorf("%w: runner changed while repair target was installed", pueue.ErrConfiguration)
	}
	return installed, nil
}

func planQueuedRunnerRepairTarget(store *taskdir.Store, runnerExecutable string) (queuedRunnerRepairTarget, error) {
	if store == nil {
		return queuedRunnerRepairTarget{}, task.ErrInvalidPermit
	}
	runner, err := resolveRunner(runnerExecutable, Dependencies{})
	if err != nil {
		return queuedRunnerRepairTarget{}, err
	}
	digest, err := stateExecutableSourceDigest(runner)
	if err != nil {
		return queuedRunnerRepairTarget{}, err
	}
	return queuedRunnerRepairTarget{
		source:     runner,
		executable: filepath.Join(store.Root, ".supervisor", stateRunnerName+"-"+digest),
		sha256:     digest,
	}, nil
}

func collectQueuedRunnerCommandReplacements(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, targetRunner string) ([]pueue.RunnerCommandReplacement, error) {
	snapshot, err := supervisor.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	prefix := "delegate:" + store.RootID + ":"
	replacements := make([]pueue.RunnerCommandReplacement, 0)
	for _, job := range snapshot.Jobs {
		if job.State != pueue.StateQueued && job.State != pueue.StateStashed {
			continue
		}
		replacement, eligible, prepareErr := prepareQueuedRunnerCommandForJob(ctx, store, supervisor, job, prefix)
		if prepareErr != nil {
			return nil, prepareErr
		}
		if !eligible {
			continue
		}
		replacements = append(replacements, replacement)
	}
	if len(replacements) == 0 {
		return nil, nil
	}
	previousRunners, err := managedStateRunnerPaths(store.Root)
	if err != nil {
		return nil, err
	}
	for i := range replacements {
		replacements[i].NewRunner = targetRunner
		replacements[i].PreviousManagedRunners = previousRunners
	}
	if err = authorizeQueuedInspectionRunnerUpgrades(ctx, store, replacements); err != nil {
		return nil, err
	}
	if err = recordQueuedManagedRunnerMigrations(ctx, store, replacements); err != nil {
		return nil, err
	}
	return replacements, nil
}

func recordQueuedManagedRunnerMigrations(ctx context.Context, store *taskdir.Store, replacements []pueue.RunnerCommandReplacement) error {
	if store == nil {
		return task.ErrInvalidPermit
	}
	if ctx == nil {
		return context.Canceled
	}
	for _, replacement := range replacements {
		if replacement.Inspection {
			continue
		}
		td, err := store.OpenTask(replacement.TaskID)
		if err != nil {
			return err
		}
		_, meta, readErr := td.PreparedRecords()
		if readErr == nil && meta == nil {
			readErr = task.ErrEvidenceFault
		}
		if readErr == nil && meta.RunnerOwnership == "" {
			if meta.RunnerExecutable != replacement.OldRunner {
				readErr = task.ErrIdentityMismatch
			} else {
				readErr = recordManagedRunnerMigration(ctx, store.Root, td, meta)
			}
		}
		readErr = errors.Join(readErr, td.Close())
		if readErr != nil {
			return readErr
		}
	}
	return nil
}

func recordManagedRunnerMigration(ctx context.Context, root string, td *taskdir.TaskDir, meta *task.MetaRecord) error {
	if td == nil {
		return task.ErrInvalidPermit
	}
	if ctx == nil {
		return context.Canceled
	}
	if meta == nil || meta.RunnerOwnership != "" || meta.RunnerExecutable == "" {
		return nil
	}
	migrated, err := td.HasManagedRunnerMigration()
	if err != nil || migrated {
		return err
	}
	if !managedRunnerExecutable(root, meta.RunnerExecutable, "") {
		return task.ErrIdentityMismatch
	}
	digest, err := commonprovider.FingerprintExecutable(meta.RunnerExecutable)
	if err != nil {
		return err
	}
	return td.RecordManagedRunnerMigrationContext(ctx, digest)
}

func authorizeQueuedInspectionRunnerUpgrades(ctx context.Context, store *taskdir.Store, replacements []pueue.RunnerCommandReplacement) error {
	for _, replacement := range replacements {
		if !replacement.Inspection {
			continue
		}
		if err := authorizeQueuedInspectionRunnerUpgrade(ctx, store, replacement); err != nil {
			return err
		}
	}
	return nil
}

func prepareQueuedRunnerCommandForJob(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, job pueue.Job, taskPrefix string) (pueue.RunnerCommandReplacement, bool, error) {
	if job.Label == nil {
		return pueue.RunnerCommandReplacement{}, false, nil
	}
	taskID, inspectionWorker, matched := queuedRunnerCommandIdentity(store.RootID, *job.Label, taskPrefix)
	if !matched {
		return pueue.RunnerCommandReplacement{}, false, nil
	}
	if inspectionWorker {
		return prepareQueuedInspectionRunnerCommand(ctx, store, supervisor, job, taskID)
	}
	return prepareQueuedRunnerCommand(store, supervisor, job, taskID)
}

func queuedRunnerCommandIdentity(rootID, label, taskPrefix string) (taskID string, inspectionWorker, matched bool) {
	if strings.HasPrefix(label, taskPrefix) {
		return strings.TrimPrefix(label, taskPrefix), false, true
	}
	inspectionPrefix := (pueue.InspectionIdentity{RootID: rootID}).Group() + "-"
	if strings.HasPrefix(label, inspectionPrefix) {
		return strings.TrimPrefix(label, inspectionPrefix), true, true
	}
	return "", false, false
}

func managedStateRunnerPaths(root string) ([]string, error) {
	base := filepath.Join(root, ".supervisor")
	entries, err := os.ReadDir(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		path := filepath.Join(base, entry.Name())
		if !isStateRunnerPath(root, path) {
			continue
		}
		want := strings.TrimPrefix(entry.Name(), stateRunnerName+"-")
		got, digestErr := protectedStateExecutableDigest(path)
		if errors.Is(digestErr, os.ErrNotExist) {
			continue
		}
		if digestErr != nil {
			return nil, digestErr
		}
		if got == want {
			paths = append(paths, path)
		}
	}
	return paths, nil
}

// ensurePrivateQueuedRunnerRepair performs the queue-wide migration at most
// once for one supervisor generation and target managed runner. New tasks use
// the target directly, while this transition repairs rows admitted by an
// earlier Delegate build. The receipt is published only after the exact Pueue
// replacement operation succeeds; an interruption retries safely.
func ensurePrivateQueuedRunnerRepair(store *taskdir.Store, supervisor *pueue.Client, runnerExecutable string) error {
	if store == nil || supervisor == nil || !pueue.IsPrivateConfig(store.Root, supervisor.Binding().ConfigPath) {
		return nil
	}
	target, err := planQueuedRunnerRepairTarget(store, runnerExecutable)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), pueue.RunnerCommandUpgradeTimeout)
	defer cancel()
	return ensurePrivateQueuedRunnerRepairContext(ctx, store, supervisor, target)
}

func ensurePrivateQueuedRunnerRepairContext(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, target queuedRunnerRepairTarget) error {
	if ctx == nil {
		return context.Canceled
	}
	receipt, name, err := newRunnerRepairReceipt(store, supervisor, target)
	if err != nil {
		return err
	}
	for {
		complete, receiptErr := runnerRepairReceiptCurrent(ctx, store, name, receipt)
		if receiptErr != nil {
			return receiptErr
		}
		if complete {
			return nil
		}
		acquired, transitionErr := pueue.TryWithPrivateTransition(ctx, store.Root, func() error {
			return repairPrivateQueuedRunnerTransition(ctx, store, supervisor, target, name, receipt)
		})
		if transitionErr != nil {
			return transitionErr
		}
		if acquired {
			return nil
		}
		if transitionErr = pueue.WaitForPrivateTransition(ctx, store.Root); transitionErr != nil {
			return transitionErr
		}
	}
}

func repairPrivateQueuedRunnerTransition(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, target queuedRunnerRepairTarget, name string, receipt runnerRepairReceipt) error {
	// Another elected owner may have completed the same transition before
	// this caller acquired transition.lock. Recheck the immutable receipt
	// before reading or changing any queue row.
	complete, err := runnerRepairReceiptCurrent(ctx, store, name, receipt)
	if err != nil || complete {
		return err
	}
	installed, err := prepareQueuedRunnerRepairTarget(store, target.source)
	if err != nil {
		return err
	}
	if installed != target.executable {
		return fmt.Errorf("%w: runner changed while repair target was installed", pueue.ErrConfiguration)
	}
	if err = repairQueuedRunnerCommandsToTarget(ctx, store, supervisor, target.executable); err != nil {
		return err
	}
	return publishRunnerRepairReceipt(ctx, store, name, receipt)
}

func newRunnerRepairReceipt(store *taskdir.Store, supervisor *pueue.Client, target queuedRunnerRepairTarget) (runnerRepairReceipt, string, error) {
	if store == nil || supervisor == nil || !pueue.IsPrivateConfig(store.Root, supervisor.Binding().ConfigPath) {
		return runnerRepairReceipt{}, "", task.ErrInvalidPermit
	}
	if target.source == "" || !filepath.IsAbs(target.executable) || filepath.Clean(target.executable) != target.executable ||
		!isStateRunnerPath(store.Root, target.executable) || task.ValidateSHA256(target.sha256) != nil {
		return runnerRepairReceipt{}, "", pueue.ErrConfiguration
	}
	receipt := runnerRepairReceipt{
		SchemaVersion:    task.SchemaVersion,
		Revision:         runnerRepairReceiptRevision,
		Supervisor:       supervisor.RuntimeBinding(),
		RunnerExecutable: target.executable,
		RunnerSHA256:     target.sha256,
	}
	if err := validateRunnerRepairReceiptShape(store.Root, receipt); err != nil {
		return runnerRepairReceipt{}, "", err
	}
	data, err := task.MarshalCanonical(receipt)
	if err != nil {
		return runnerRepairReceipt{}, "", err
	}
	return receipt, "r-" + task.ComputeSHA256(data)[:48] + ".json", nil
}

func validateRunnerRepairReceipt(root string, receipt runnerRepairReceipt) error {
	if err := validateRunnerRepairReceiptShape(root, receipt); err != nil {
		return err
	}
	digest, err := protectedStateExecutableDigest(receipt.RunnerExecutable)
	if err != nil {
		return err
	}
	if digest != receipt.RunnerSHA256 {
		return task.ErrEvidenceFault
	}
	return nil
}

func validateRunnerRepairReceiptShape(root string, receipt runnerRepairReceipt) error {
	if receipt.SchemaVersion != task.SchemaVersion || receipt.Revision != runnerRepairReceiptRevision ||
		task.ValidateFreshSupervisorRef(receipt.Supervisor) != nil ||
		!pueue.IsPrivateConfig(root, receipt.Supervisor.ConfigPath) ||
		!filepath.IsAbs(receipt.RunnerExecutable) || filepath.Clean(receipt.RunnerExecutable) != receipt.RunnerExecutable ||
		!isStateRunnerPath(root, receipt.RunnerExecutable) || task.ValidateSHA256(receipt.RunnerSHA256) != nil {
		return task.ErrEvidenceFault
	}
	return nil
}

func runnerRepairReceiptCurrent(ctx context.Context, store *taskdir.Store, name string, want runnerRepairReceipt) (current bool, resultErr error) {
	if store == nil {
		return false, task.ErrInvalidPermit
	}
	if err := validateRunnerRepairReceiptShape(store.Root, want); err != nil {
		return false, err
	}
	directory, err := store.OpenRunnerRepairContext(ctx, true)
	if err != nil {
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	var got runnerRepairReceipt
	if err = directory.Read(name, &got); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if err = validateRunnerRepairReceipt(store.Root, got); err != nil {
		return false, err
	}
	if got != want {
		return false, task.ErrEvidenceFault
	}
	return true, nil
}

func publishRunnerRepairReceipt(ctx context.Context, store *taskdir.Store, name string, receipt runnerRepairReceipt) (resultErr error) {
	if store == nil {
		return task.ErrInvalidPermit
	}
	if err := validateRunnerRepairReceipt(store.Root, receipt); err != nil {
		return err
	}
	directory, err := store.OpenRunnerRepairContext(ctx, true)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	_, err = directory.PutContext(ctx, name, receipt)
	return err
}

func preparePrivateTaskSupervisor(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, meta *task.MetaRecord, deps Dependencies) (*pueue.Client, error) {
	if store == nil || meta == nil || !pueue.IsPrivateConfig(store.Root, binding.ConfigPath) {
		return nil, nil
	}
	if ctx == nil {
		return nil, context.Canceled
	}
	ctx, cancel := context.WithTimeout(ctx, pueue.RunnerCommandUpgradeTimeout)
	defer cancel()
	options := supervisorOptionsForMeta(deps.SupervisorOptions, *meta)
	client, err := preparePrivateAdmissionSupervisorContext(ctx, deps, store, options)
	if err != nil {
		return nil, err
	}
	target, err := planQueuedRunnerRepairTarget(store, deps.RunnerExecutable)
	if err != nil {
		return nil, closePrivateAdmissionTransition(client, err)
	}
	if err = ensurePrivateQueuedRunnerRepairContext(ctx, store, client, target); err != nil {
		return nil, closePrivateAdmissionTransition(client, err)
	}
	if err = client.ScopePrivateTask(ctx, store.Root, binding); err != nil {
		return nil, closePrivateAdmissionTransition(client, err)
	}
	return client, nil
}

func repairPrivateQueueForTask(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, meta *task.MetaRecord, deps Dependencies) (resultErr error) {
	client, err := preparePrivateTaskSupervisor(ctx, store, binding, meta, deps)
	if err != nil || client == nil {
		return err
	}
	defer func() { closeSupervisorMaintenance(client, resultErr) }()
	return nil
}

func installStateSupervisorPairForRecovery(ctx context.Context, root, initialSupervisorExecutable string, binding task.SupervisorRef) error {
	client, daemon, err := resolveBundledExecutables(Dependencies{InitialSupervisorExecutable: initialSupervisorExecutable})
	if err == nil {
		return prepareStateSupervisorPair(ctx, root, client, daemon)
	}
	found := false
	stateErr := pueue.WithSupervisorLeaseContext(ctx, root, func(lockedContext context.Context) error {
		var probeErr error
		found, probeErr = probeStateSupervisorPair(lockedContext, root, nil)
		return probeErr
	})
	if errors.Is(stateErr, pueue.ErrPrivateSupervisorNotInitialized) {
		stateErr = pueue.WithBootstrapLockContext(ctx, root, func(lockedContext context.Context) error {
			var probeErr error
			found, probeErr = probeStateSupervisorPair(lockedContext, root, nil)
			if errors.Is(probeErr, os.ErrNotExist) {
				return nil
			}
			return probeErr
		})
	}
	if stateErr != nil {
		return errors.Join(err, stateErr)
	}
	if found {
		return nil
	}
	if !privateSupervisorExecutablesAvailable(binding.ClientExecutable, binding.DaemonExecutable) {
		return err
	}
	clientDigest, clientErr := stateExecutableSourceDigest(binding.ClientExecutable)
	daemonDigest, daemonErr := stateExecutableSourceDigest(binding.DaemonExecutable)
	if clientErr != nil || daemonErr != nil || clientDigest != binding.ClientSHA256 || daemonDigest != binding.DaemonSHA256 {
		return errors.Join(err, clientErr, daemonErr, pueue.ErrBinding)
	}
	return prepareStateSupervisorPair(ctx, root, binding.ClientExecutable, binding.DaemonExecutable)
}

func prepareQueuedRunnerCommand(store *taskdir.Store, supervisor *pueue.Client, job pueue.Job, taskID string) (_ pueue.RunnerCommandReplacement, eligible bool, resultErr error) {
	if err := task.ValidateTaskID(taskID); err != nil {
		return pueue.RunnerCommandReplacement{}, false, task.ErrIdentityMismatch
	}
	td, err := store.OpenTask(taskID)
	if err != nil {
		return pueue.RunnerCommandReplacement{}, false, err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	return queuedRunnerCommandReplacement(store, supervisor, job, taskID, td)
}

func prepareQueuedInspectionRunnerCommand(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, job pueue.Job, taskID string) (_ pueue.RunnerCommandReplacement, eligible bool, resultErr error) {
	if err := task.ValidateTaskID(taskID); err != nil {
		return pueue.RunnerCommandReplacement{}, false, task.ErrIdentityMismatch
	}
	operation, err := inspection.LoadOperationContext(ctx, store, taskID)
	if err != nil {
		return pueue.RunnerCommandReplacement{}, false, err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	request := operation.Request()
	identity := request.SupervisorIdentity()
	if err = validateQueuedInspectionIdentity(store, supervisor, job, request, identity); err != nil {
		return pueue.RunnerCommandReplacement{}, false, err
	}
	eligible, err = queuedInspectionWorkerUpgradeEligible(ctx, store.Root, operation, request.Binding)
	if err != nil {
		return pueue.RunnerCommandReplacement{}, false, err
	}
	if !eligible {
		return pueue.RunnerCommandReplacement{}, false, nil
	}
	return pueue.RunnerCommandReplacement{
		NumericID:       job.ID,
		RootID:          store.RootID,
		TaskID:          taskID,
		Label:           identity.Label(),
		RootPath:        store.Root,
		OldRunner:       request.Binding.WorkerExecutable,
		Inspection:      true,
		InspectionGroup: request.Group,
	}, true, nil
}

func validateQueuedInspectionIdentity(store *taskdir.Store, supervisor *pueue.Client, job pueue.Job, request inspection.RequestRecord, identity pueue.InspectionIdentity) error {
	if request.RootID != store.RootID || request.TaskID != identity.TaskID || request.Task.RootID != store.RootID || request.Task.TaskID != identity.TaskID || request.Group != identity.Group() || request.Label != identity.Label() {
		return task.ErrIdentityMismatch
	}
	if job.Label == nil || *job.Label != identity.Label() || job.Group != identity.Group() || !task.SameSupervisorIdentity(request.Binding.Supervisor, supervisor.Binding()) {
		return pueue.ErrBinding
	}
	return nil
}

func queuedInspectionWorkerUpgradeEligible(ctx context.Context, root string, operation *inspection.Operation, binding inspection.Binding) (bool, error) {
	if binding.RunnerOwnership != task.RunnerOwnershipManaged {
		return false, nil
	}
	authorized, err := operation.ManagedWorkerUpgradeAuthorizedContext(ctx)
	if err != nil || authorized {
		return authorized, err
	}
	if !managedInspectionWorkerExecutable(root, binding) {
		return false, nil
	}
	if err = validateInspectionRunner(operation, binding.WorkerExecutable); err != nil {
		return false, err
	}
	return true, nil
}

func authorizeQueuedInspectionRunnerUpgrade(ctx context.Context, store *taskdir.Store, replacement pueue.RunnerCommandReplacement) (resultErr error) {
	if store == nil || !replacement.Inspection || replacement.RootID != store.RootID || replacement.RootPath != store.Root {
		return task.ErrIdentityMismatch
	}
	if !currentManagedRunnerExecutable(store.Root, replacement.NewRunner) {
		return task.ErrEvidenceFault
	}
	operation, err := inspection.LoadOperationContext(ctx, store, replacement.TaskID)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	request := operation.Request()
	identity := request.SupervisorIdentity()
	if err = validateInspectionReplacementIdentity(store, replacement, request, identity); err != nil {
		return err
	}
	return authorizeInspectionManagedWorker(ctx, store.Root, operation, request.Binding)
}

func validateInspectionReplacementIdentity(store *taskdir.Store, replacement pueue.RunnerCommandReplacement, request inspection.RequestRecord, identity pueue.InspectionIdentity) error {
	if (pueue.InspectionIdentity{RootID: replacement.RootID, QueueGroup: replacement.InspectionGroup}).Group() != request.Group {
		return task.ErrIdentityMismatch
	}
	if request.RootID != store.RootID || request.TaskID != replacement.TaskID || request.Task.RootID != store.RootID || request.Task.TaskID != replacement.TaskID || request.Group != identity.Group() || request.Label != identity.Label() || replacement.Label != identity.Label() || request.Binding.WorkerExecutable != replacement.OldRunner {
		return task.ErrIdentityMismatch
	}
	return nil
}

func authorizeInspectionManagedWorker(ctx context.Context, root string, operation *inspection.Operation, binding inspection.Binding) error {
	if binding.RunnerOwnership != task.RunnerOwnershipManaged {
		return task.ErrEvidenceFault
	}
	authorized, err := operation.ManagedWorkerUpgradeAuthorizedContext(ctx)
	if err != nil {
		return err
	}
	if authorized {
		return nil
	}
	if !managedInspectionWorkerExecutable(root, binding) {
		return task.ErrEvidenceFault
	}
	if err = validateInspectionRunner(operation, binding.WorkerExecutable); err != nil {
		return err
	}
	return operation.AuthorizeManagedWorkerUpgradeContext(ctx)
}

func queuedRunnerCommandReplacement(store *taskdir.Store, supervisor *pueue.Client, job pueue.Job, taskID string, td *taskdir.TaskDir) (pueue.RunnerCommandReplacement, bool, error) {
	req, meta, err := td.PreparedRecords()
	if err != nil {
		return pueue.RunnerCommandReplacement{}, false, err
	}
	submission, err := td.ReadSubmission()
	if err != nil {
		return pueue.RunnerCommandReplacement{}, false, err
	}
	label := "delegate:" + store.RootID + ":" + taskID
	if req.RootID != store.RootID || req.TaskID != taskID || submission.RootID != store.RootID || submission.TaskID != taskID || submission.Label != label {
		return pueue.RunnerCommandReplacement{}, false, task.ErrIdentityMismatch
	}
	if job.Label == nil || *job.Label != label || !task.SameSupervisorIdentity(submission.Supervisor, meta.SupervisorConfig) || !task.SameSupervisorIdentity(supervisor.Binding(), submission.Supervisor) {
		return pueue.RunnerCommandReplacement{}, false, pueue.ErrBinding
	}
	if meta.RunnerExecutable == "" {
		return pueue.RunnerCommandReplacement{}, false, task.ErrEvidenceFault
	}
	ownership, err := runnerOwnershipForTask(store.Root, td, meta)
	if err != nil {
		return pueue.RunnerCommandReplacement{}, false, err
	}
	if ownership != task.RunnerOwnershipManaged {
		return pueue.RunnerCommandReplacement{}, false, nil
	}
	return pueue.RunnerCommandReplacement{
		NumericID: job.ID,
		RootID:    store.RootID,
		TaskID:    taskID,
		Label:     label,
		RootPath:  store.Root,
		OldRunner: meta.RunnerExecutable,
	}, true, nil
}
