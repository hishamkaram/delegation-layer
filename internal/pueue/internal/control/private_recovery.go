package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	privateRecoveryConfigName        = "legacy-pueue.yml"
	privateRecoveryRecordName        = "legacy-supervisor.json"
	privateRecoveryPendingConfigName = "legacy-pueue.pending"
	privateRecoveryPendingRecordName = "legacy-supervisor.pending.json"
)

// privateRecoveryBindingRecord preserves the runtime config needed by queued
// work while a legacy supervisor is deliberately left alive. The task record
// remains immutable; this root-scoped record is the bounded runtime alias that
// lets those workers reconnect to the exact legacy endpoint.
type privateRecoveryBindingRecord struct {
	SchemaVersion int                `json:"schema_version"`
	Supervisor    task.SupervisorRef `json:"supervisor"`
	ConfigPath    string             `json:"config_path"`
	ConfigSHA256  string             `json:"config_sha256"`
}

type privateRecoveryPublicationRecord struct {
	SchemaVersion int                          `json:"schema_version"`
	Binding       privateRecoveryBindingRecord `json:"binding"`
}

func privateRecoveryConfigPath(stateRoot string) string {
	return filepath.Join(stateRoot, privateSupervisorDirectory, privateRecoveryConfigName)
}

func privateRecoveryRecordPath(stateRoot string) string {
	return filepath.Join(stateRoot, privateSupervisorDirectory, privateRecoveryRecordName)
}

func privateRecoveryPendingConfigPath(stateRoot string) string {
	return filepath.Join(stateRoot, privateSupervisorDirectory, privateRecoveryPendingConfigName)
}

func privateRecoveryPendingRecordPath(stateRoot string) string {
	return filepath.Join(stateRoot, privateSupervisorDirectory, privateRecoveryPendingRecordName)
}

// PersistPrivateRecoveryBinding makes the currently used legacy config
// reusable by queued workers. The caller must hold the state-root bootstrap
// lock and must have already proven that the endpoint owns active work.
func (c *Client) PersistPrivateRecoveryBinding(stateRoot string) error {
	if c == nil {
		return ErrConfiguration
	}
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot || !IsPrivateConfig(stateRoot, c.binding.ConfigPath) {
		return ErrConfiguration
	}
	if err := validateSavedSupervisorRef(c.binding); err != nil {
		return fmt.Errorf("%w: preserve legacy supervisor binding: %w", ErrBinding, err)
	}
	if err := recoverPrivateRecoveryPublication(stateRoot); err != nil {
		return err
	}
	existing, found, err := readPrivateRecoveryBinding(stateRoot)
	if err != nil {
		return err
	}
	if found {
		if !task.SameSupervisorIdentity(existing.Supervisor, c.binding) {
			return fmt.Errorf("%w: another legacy supervisor recovery binding is active", ErrBinding)
		}
		return nil
	}
	data, err := readRegular(c.runtimeBinding.ConfigPath, MaxControlBytes)
	if err != nil {
		return fmt.Errorf("%w: read legacy recovery config: %w", ErrBinding, err)
	}
	return c.persistPrivateRecoveryData(stateRoot, data)
}

func (c *Client) persistPrivateRecoveryData(stateRoot string, data []byte) error {
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	options, err := optionsForSavedBinding(c.binding, c.options)
	if err != nil {
		return err
	}
	resolution, err := privateResolution(options)
	if err != nil {
		return err
	}
	config, err := ParseConfig(data)
	if err != nil {
		return err
	}
	resolved, err := ResolveConfig(config, resolution)
	if err != nil {
		return err
	}
	if err = validateResolvedPrivateMigrationConfig(resolved, base, c.binding.Endpoint); err != nil {
		return err
	}
	configPath := privateRecoveryConfigPath(stateRoot)
	record := privateRecoveryBindingRecord{
		SchemaVersion: task.SchemaVersion,
		Supervisor:    c.binding,
		ConfigPath:    configPath,
		ConfigSHA256:  task.ComputeSHA256(data),
	}
	pendingData, err := task.MarshalCanonical(privateRecoveryPublicationRecord{
		SchemaVersion: task.SchemaVersion,
		Binding:       record,
	})
	if err != nil {
		return fmt.Errorf("%w: marshal legacy recovery publication: %w", ErrConfiguration, err)
	}
	if err = publishPrivateRecord(base, privateRecoveryPendingConfigPath(stateRoot), data); err != nil {
		return err
	}
	if err = publishPrivateRecord(base, privateRecoveryPendingRecordPath(stateRoot), pendingData); err != nil {
		return err
	}
	return recoverPrivateRecoveryPublication(stateRoot)
}

// ClearPrivateRecoveryBinding removes a completed legacy runtime alias. It
// is idempotent and only removes a record that matches the requested legacy
// logical identity.
func ClearPrivateRecoveryBinding(stateRoot string, saved task.SupervisorRef) error {
	return clearPrivateRecoveryBinding(stateRoot, &saved)
}

// ClearPrivateRecoveryBindingIfPresent is idempotent after an authorized
// cleanup. If alias artifacts remain without their record, it refuses to
// remove ambiguous recovery data; RetirePrivateRecoveryBinding owns that
// cleanup after proving the completed successor and migration history.
func ClearPrivateRecoveryBindingIfPresent(stateRoot string) error {
	return clearPrivateRecoveryBinding(stateRoot, nil)
}

// RetirePrivateRecoveryBinding removes a legacy runtime alias after the caller
// has proved that the published successor is ready. The alias predecessor must
// resolve through the completed migration history to the exact supplied
// successor, including its full executable and resolver provenance. Production
// callers hold the state-root bootstrap lock while invoking this function.
func RetirePrivateRecoveryBinding(stateRoot string, successor task.SupervisorRef) error {
	return retirePrivateRecoveryBinding(context.Background(), stateRoot, successor, nil)
}

// RetirePrivateRecoveryBindingContext is the bounded form used by lifecycle
// owners that already carry a transition deadline.
func RetirePrivateRecoveryBindingContext(ctx context.Context, stateRoot string, successor task.SupervisorRef) error {
	return retirePrivateRecoveryBinding(ctx, stateRoot, successor, nil)
}

// Only same-generation explicit migration supplies authorizeQuiescent. The
// callback is used for a validated self-alias without completed history and
// runs under the caller's existing EX lease before the canonical removal.
func retirePrivateRecoveryBinding(ctx context.Context, stateRoot string, successor task.SupervisorRef, authorizeQuiescent func(context.Context) error) error {
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateSavedSupervisorRef(successor); err != nil {
		return err
	}
	if err := recoverPrivateRecoveryPublication(stateRoot); err != nil {
		return err
	}
	record, found, err := readPrivateRecoveryRecord(stateRoot)
	if err != nil {
		return err
	}
	if !found {
		artifacts, artifactErr := privateRecoveryAliasArtifactsPresent(stateRoot)
		if artifactErr != nil {
			return artifactErr
		}
		if !artifacts {
			return nil
		}
		if err = authorizePrivateRecoveryAliasRetirement(ctx, stateRoot, successor, nil, authorizeQuiescent); err != nil {
			return err
		}
		return clearPrivateRecoveryBindingAuthorized(stateRoot, nil, true)
	}
	if _, statErr := os.Stat(record.ConfigPath); statErr == nil {
		if err = validatePrivateRecoveryBinding(stateRoot, record); err != nil {
			return err
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("%w: inspect legacy recovery binding: %w", ErrBinding, statErr)
	}
	if err = authorizePrivateRecoveryAliasRetirement(ctx, stateRoot, successor, &record, authorizeQuiescent); err != nil {
		return err
	}
	return clearPrivateRecoveryBindingAuthorized(stateRoot, &record.Supervisor, false)
}

func authorizePrivateRecoveryAliasRetirement(ctx context.Context, stateRoot string, successor task.SupervisorRef, record *privateRecoveryBindingRecord, authorizeQuiescent func(context.Context) error) error {
	facts, err := loadPrivateGenerationFacts(stateRoot)
	if err != nil {
		return err
	}
	if facts.publishedErr != nil {
		return facts.publishedErr
	}
	current := facts.published
	if !facts.publishedFound || !privateFullGenerationMatches(current, successor) {
		return errors.Join(
			ErrBinding,
			fmt.Errorf("%w: recovery alias successor is not the completed private generation", ErrPrivateSupervisorMigrationRequired),
		)
	}
	complete, err := facts.complete(current, true)
	if err != nil {
		return err
	}
	if !complete {
		return errors.Join(
			ErrBinding,
			fmt.Errorf("%w: completed private generation is not ready to retire its recovery alias", ErrPrivateSupervisorMigrationRequired),
		)
	}
	historical, historyErr := privateRecoveryAliasHistoryAuthorized(ctx, stateRoot, current, record)
	if historyErr != nil {
		return historyErr
	}
	if historical {
		return nil
	}
	if authorizeQuiescent != nil && record != nil {
		phase, phaseErr := facts.phase(current)
		if phaseErr != nil {
			return phaseErr
		}
		if facts.aliasPresent && phase >= privateGenerationObservable {
			return authorizeQuiescent(ctx)
		}
	}
	return privateRecoveryAliasHistoryRefusal(record)
}

func privateRecoveryAliasHistoryAuthorized(ctx context.Context, stateRoot string, current task.SupervisorRef, record *privateRecoveryBindingRecord) (bool, error) {
	if record != nil {
		return privateSupervisorSuccessorConnected(ctx, stateRoot, record.Supervisor, current)
	}
	return privateCompletedSuccessorHistoryContains(ctx, stateRoot, current)
}

func privateRecoveryAliasHistoryRefusal(record *privateRecoveryBindingRecord) error {
	if record != nil {
		return errors.Join(
			ErrBinding,
			fmt.Errorf("%w: recovery alias predecessor is not connected to the completed successor", ErrPrivateSupervisorMigrationRequired),
		)
	}
	return errors.Join(
		ErrBinding,
		fmt.Errorf("%w: orphaned recovery alias has no completed successor history", ErrPrivateSupervisorMigrationRequired),
	)
}

// PrivateRecoveryBindingMatches compares a validated alias's logical identity
// with the supplied supervisor. It does not compare daemon provenance: a
// completed same-logical migration can leave a matching historical alias.
// This match alone does not authorize alias retirement.
func PrivateRecoveryBindingMatches(stateRoot string, supervisor task.SupervisorRef) (bool, error) {
	record, found, err := readPrivateRecoveryBinding(stateRoot)
	if err != nil || !found {
		return false, err
	}
	return task.SameSupervisorIdentity(record.Supervisor, supervisor), nil
}

// PrivateRecoveryBindingPresent reports whether a validated legacy recovery
// alias exists for the state root. Callers use this to avoid starting a
// successor merely to retire an already-absent alias.
func PrivateRecoveryBindingPresent(stateRoot string) (bool, error) {
	_, found, err := readPrivateRecoveryBinding(stateRoot)
	return found, err
}

// ReadPrivateRecoveryBinding returns the immediate legacy supervisor retained
// for queued work. Callers use this identity when a state root has gone through
// more than one migration generation; the durable alias is the authoritative
// predecessor for cleanup and continuation recovery.
func ReadPrivateRecoveryBinding(stateRoot string) (task.SupervisorRef, bool, error) {
	record, found, err := readPrivateRecoveryBinding(stateRoot)
	if err != nil || !found {
		return task.SupervisorRef{}, found, err
	}
	return record.Supervisor, true, nil
}

func privateRecoveryRuntimeMatchesPublished(stateRoot string, runtime, published task.SupervisorRef) (bool, error) {
	record, found, err := readPrivateRecoveryBinding(stateRoot)
	if err != nil || !found {
		return false, err
	}
	if runtime.ConfigPath != record.ConfigPath || runtime.ConfigDigest != record.ConfigSHA256 {
		return false, nil
	}
	// The validated alias is another path to the same retained config bytes.
	// Only that path changes in the detached comparison; daemon provenance
	// remains distinct from the logical endpoint used by compatible clients.
	runtime.ConfigPath = record.Supervisor.ConfigPath
	return privateBindingsMatch(record.Supervisor, published) && privateBindingsMatch(runtime, published), nil
}

func clearPrivateRecoveryBinding(stateRoot string, expected *task.SupervisorRef) error {
	return clearPrivateRecoveryBindingAuthorized(stateRoot, expected, false)
}

func clearPrivateRecoveryBindingAuthorized(stateRoot string, expected *task.SupervisorRef, allowOrphan bool) error {
	if err := recoverPrivateRecoveryPublication(stateRoot); err != nil {
		return err
	}
	record, found, err := readPrivateRecoveryRecord(stateRoot)
	if err != nil {
		return err
	}
	if !found {
		return clearPrivateRecoveryAliasWithoutRecord(stateRoot, allowOrphan)
	}
	if expected != nil && !task.SameSupervisorIdentity(record.Supervisor, *expected) {
		return fmt.Errorf("%w: legacy recovery binding identity changed", ErrBinding)
	}
	return removePrivateRecoveryBindingFiles(stateRoot, record)
}

func clearPrivateRecoveryAliasWithoutRecord(stateRoot string, allowOrphan bool) error {
	artifacts, err := privateRecoveryAliasArtifactsPresent(stateRoot)
	if err != nil {
		return err
	}
	if !artifacts {
		return nil
	}
	if !allowOrphan {
		return fmt.Errorf("%w: legacy recovery alias authority is unavailable", ErrPrivateSupervisorMigrationRequired)
	}
	for _, path := range []string{privateRecoveryConfigPath(stateRoot), privateRecoveryPendingConfigPath(stateRoot), privateRecoveryPendingRecordPath(stateRoot)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: remove orphaned legacy recovery file: %w", ErrConfiguration, err)
		}
	}
	return syncPrivateDirectory(filepath.Join(stateRoot, privateSupervisorDirectory))
}

func removePrivateRecoveryBindingFiles(stateRoot string, record privateRecoveryBindingRecord) error {
	for _, path := range []string{privateRecoveryRecordPath(stateRoot), record.ConfigPath, privateRecoveryPendingConfigPath(stateRoot), privateRecoveryPendingRecordPath(stateRoot)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: remove legacy recovery binding: %w", ErrConfiguration, err)
		}
	}
	return syncPrivateDirectory(filepath.Join(stateRoot, privateSupervisorDirectory))
}

func privateRecoveryAliasArtifactsPresent(stateRoot string) (bool, error) {
	for _, path := range []string{privateRecoveryConfigPath(stateRoot), privateRecoveryPendingConfigPath(stateRoot), privateRecoveryPendingRecordPath(stateRoot)} {
		if _, err := os.Lstat(path); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("%w: inspect legacy recovery file: %w", ErrConfiguration, err)
		}
	}
	return false, nil
}

func recoverPrivateRecoveryPublication(stateRoot string) error {
	pendingData, err := readRegular(privateRecoveryPendingRecordPath(stateRoot), MaxControlBytes)
	if errors.Is(err, os.ErrNotExist) {
		return removePrivateRecoveryStage(stateRoot)
	}
	if err != nil {
		return fmt.Errorf("%w: read legacy recovery publication: %w", ErrBinding, err)
	}
	record, err := decodePrivateRecoveryPublication(stateRoot, pendingData)
	if err != nil {
		return err
	}
	// The final config and record may already be durable when a crash occurs
	// before the staged files are cleaned up. Reconcile that committed pair
	// without requiring the staged config to still exist.
	if final, found, finalErr := readPrivateRecoveryBinding(stateRoot); finalErr != nil {
		return finalErr
	} else if found {
		if final != record {
			return fmt.Errorf("%w: final legacy recovery binding differs from pending publication", ErrBinding)
		}
		return removePrivateRecoveryStage(stateRoot)
	}
	return publishPrivateRecoveryPublication(stateRoot, record)
}

func removePrivateRecoveryStage(stateRoot string) error {
	for _, path := range []string{privateRecoveryPendingRecordPath(stateRoot), privateRecoveryPendingConfigPath(stateRoot)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: remove incomplete legacy recovery stage: %w", ErrConfiguration, err)
		}
	}
	return nil
}

func decodePrivateRecoveryPublication(stateRoot string, pendingData []byte) (privateRecoveryBindingRecord, error) {
	var pending privateRecoveryPublicationRecord
	if err := task.DecodeStrict(pendingData, &pending); err != nil {
		return privateRecoveryBindingRecord{}, fmt.Errorf("%w: decode legacy recovery publication: %w", ErrBinding, err)
	}
	if pending.SchemaVersion != task.SchemaVersion {
		return privateRecoveryBindingRecord{}, fmt.Errorf("%w: legacy recovery publication schema %d", task.ErrUnsupportedSchema, pending.SchemaVersion)
	}
	record := pending.Binding
	if err := validatePrivateRecoveryRecord(stateRoot, record); err != nil {
		return privateRecoveryBindingRecord{}, err
	}
	return record, nil
}

func publishPrivateRecoveryPublication(stateRoot string, record privateRecoveryBindingRecord) error {
	data, err := readRegular(privateRecoveryPendingConfigPath(stateRoot), MaxControlBytes)
	if err != nil {
		return fmt.Errorf("%w: read staged legacy recovery config: %w", ErrBinding, err)
	}
	if task.ComputeSHA256(data) != record.ConfigSHA256 {
		return fmt.Errorf("%w: staged legacy recovery config changed", ErrBinding)
	}
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err = publishPrivateRecord(base, record.ConfigPath, data); err != nil {
		return err
	}
	if err = validatePrivateRecoveryBinding(stateRoot, record); err != nil {
		return err
	}
	finalData, err := task.MarshalCanonical(record)
	if err != nil {
		return fmt.Errorf("%w: marshal legacy recovery binding: %w", ErrConfiguration, err)
	}
	if err = publishPrivateRecord(base, privateRecoveryRecordPath(stateRoot), finalData); err != nil {
		return err
	}
	// Remove the journal before its staged config. If cleanup is interrupted,
	// the durable final record lets the next bootstrap reconcile and remove the
	// remaining stage without needing to read it again.
	if err = os.Remove(privateRecoveryPendingRecordPath(stateRoot)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: clear legacy recovery publication journal: %w", ErrConfiguration, err)
	}
	if err = os.Remove(privateRecoveryPendingConfigPath(stateRoot)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: clear legacy recovery publication stage: %w", ErrConfiguration, err)
	}
	return syncPrivateDirectory(base)
}

func readPrivateRecoveryBinding(stateRoot string) (privateRecoveryBindingRecord, bool, error) {
	record, found, err := readPrivateRecoveryRecord(stateRoot)
	if err != nil || !found {
		return record, found, err
	}
	if err = validatePrivateRecoveryBinding(stateRoot, record); err != nil {
		return record, false, err
	}
	return record, true, nil
}

func readPrivateRecoveryRecord(stateRoot string) (privateRecoveryBindingRecord, bool, error) {
	var record privateRecoveryBindingRecord
	data, err := readRegular(privateRecoveryRecordPath(stateRoot), MaxControlBytes)
	if errors.Is(err, os.ErrNotExist) {
		return record, false, nil
	}
	if err != nil {
		return record, false, fmt.Errorf("%w: read legacy recovery binding: %w", ErrBinding, err)
	}
	if err = task.DecodeStrict(data, &record); err != nil {
		return record, false, fmt.Errorf("%w: decode legacy recovery binding: %w", ErrBinding, err)
	}
	if err = validatePrivateRecoveryRecord(stateRoot, record); err != nil {
		return record, false, err
	}
	return record, true, nil
}

func validatePrivateRecoveryRecord(stateRoot string, record privateRecoveryBindingRecord) error {
	if record.SchemaVersion != task.SchemaVersion {
		return fmt.Errorf("%w: legacy recovery binding schema %d", task.ErrUnsupportedSchema, record.SchemaVersion)
	}
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return ErrConfiguration
	}
	if err := validateSavedSupervisorRef(record.Supervisor); err != nil {
		return fmt.Errorf("%w: validate legacy recovery supervisor: %w", ErrBinding, err)
	}
	if !IsPrivateConfig(stateRoot, record.Supervisor.ConfigPath) || record.ConfigPath != privateRecoveryConfigPath(stateRoot) {
		return fmt.Errorf("%w: legacy recovery binding is outside its state root", ErrBinding)
	}
	if err := task.ValidateSHA256(record.ConfigSHA256); err != nil {
		return fmt.Errorf("%w: legacy recovery config digest: %w", ErrBinding, err)
	}
	return nil
}

func validatePrivateRecoveryBinding(stateRoot string, record privateRecoveryBindingRecord) error {
	if err := validatePrivateRecoveryRecord(stateRoot, record); err != nil {
		return err
	}
	data, err := readRegular(record.ConfigPath, MaxControlBytes)
	if err != nil {
		return err
	}
	if task.ComputeSHA256(data) != record.ConfigSHA256 {
		return fmt.Errorf("%w: legacy recovery config changed", ErrBinding)
	}
	options, err := optionsForSavedBinding(record.Supervisor, Options{})
	if err != nil {
		return err
	}
	resolution, err := privateResolution(options)
	if err != nil {
		return err
	}
	config, err := ParseConfig(data)
	if err != nil {
		return err
	}
	resolved, err := ResolveConfig(config, resolution)
	if err != nil {
		return err
	}
	return validateResolvedPrivateMigrationConfig(resolved, filepath.Dir(record.ConfigPath), record.Supervisor.Endpoint)
}

func recoverPrivateRecoveryBinding(ctx context.Context, stateRoot string, saved task.SupervisorRef, clientExecutable, daemonExecutable string, options Options) (*Client, bool, bool, error) {
	if err := recoverPrivateRecoveryPublication(stateRoot); err != nil {
		return nil, false, false, err
	}
	record, found, err := readPrivateRecoveryBinding(stateRoot)
	if err != nil || !found {
		return nil, false, found, err
	}
	if !task.SameSupervisorIdentity(record.Supervisor, saved) {
		return nil, false, true, fmt.Errorf("%w: another legacy supervisor recovery binding is active", ErrBinding)
	}
	options, err = optionsForSavedBinding(saved, options)
	if err != nil {
		return nil, false, true, err
	}
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	pending, pendingFound, err := loadPrivateMigrationRecord(base)
	if err != nil {
		return nil, false, true, fmt.Errorf("%w: read dispatched private migration journal: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	if pendingFound && pending.LegacyShutdownDispatched {
		return recoverDispatchedPrivateRecovery(ctx, base, record.Supervisor, pending, options)
	}
	if pendingFound && pending.LegacyShutdownRequested {
		return nil, false, true, fmt.Errorf("%w: predecessor shutdown is requested; resume private migration", ErrPrivateSupervisorMigrationRequired)
	}
	return bindPrivateRecoveryClient(ctx, record, saved, clientExecutable, daemonExecutable, options)
}

func bindPrivateRecoveryClient(ctx context.Context, record privateRecoveryBindingRecord, saved task.SupervisorRef, clientExecutable, daemonExecutable string, options Options) (*Client, bool, bool, error) {
	client, err := Bind(ctx, clientExecutable, record.ConfigPath, options)
	if err != nil {
		return nil, false, true, err
	}
	if client.Binding().Endpoint != saved.Endpoint {
		return nil, false, true, errors.Join(ErrBinding, client.Close())
	}
	if err = attachPrivateDaemon(client, daemonExecutable); err != nil {
		return nil, false, true, errors.Join(err, client.Close())
	}
	client.binding = saved
	ready, err := client.availableForBootstrap(ctx)
	if err != nil {
		return nil, false, true, errors.Join(err, client.CloseAfterPending(err))
	}
	if ready {
		return client, false, true, nil
	}
	if err = startDaemon(client.runtimeBinding.DaemonExecutable, client.runtimeBinding.DaemonSHA256, client.runtimeBinding.ConfigPath, filepath.Dir(client.runtimeBinding.ConfigPath), options); err != nil {
		return nil, false, true, errors.Join(err, client.Close())
	}
	if err = waitReady(ctx, client, options.ObservationTimeout); err != nil {
		// Readiness failure is inconclusive, so do not stop a recovery-owned
		// endpoint before a successful liveness observation proves it is safe.
		combinedErr := err
		return nil, true, true, errors.Join(combinedErr, client.CloseAfterPending(combinedErr))
	}
	return client, true, true, nil
}

func recoverDispatchedPrivateRecovery(ctx context.Context, base string, binding task.SupervisorRef, pending privateMigrationRecord, options Options) (*Client, bool, bool, error) {
	if !privateDaemonIdentityMatchesBinding(pending.Legacy, binding) {
		return nil, false, true, fmt.Errorf("%w: dispatched private migration journal does not match the recovery binding", ErrPrivateSupervisorMigrationRequired)
	}
	if _, err := observeDispatchedPrivateMigrationShutdown(ctx, base, binding, pending, options); err != nil {
		return nil, false, true, err
	}
	return nil, false, true, fmt.Errorf("%w: dispatched predecessor shutdown is recorded; resume private migration", ErrPrivateSupervisorMigrationRequired)
}
