package control

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/pueue/internal/migration"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func privateMigrationRecordPath(base string) string {
	return filepath.Join(base, privateMigrationRecordName)
}

func privateBootstrapRecordPath(stateRoot string) string {
	return filepath.Join(stateRoot, privateSupervisorDirectory, privateBootstrapRecordName)
}

// PrivateBootstrapPending reports whether a private bootstrap has durable
// intent without a published daemon identity. It distinguishes a retryable
// Delegate-owned startup interrupted before publication from an unrelated
// pre-existing private config whose owner cannot be proven.
func PrivateBootstrapPending(stateRoot string) (bool, error) {
	_, found, err := readPrivateBootstrapRecord(stateRoot)
	return found, err
}

func readPrivateBootstrapRecord(stateRoot string) (privateBootstrapRecord, bool, error) {
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return privateBootstrapRecord{}, false, ErrConfiguration
	}
	data, err := readRegular(privateBootstrapRecordPath(stateRoot), MaxControlBytes)
	if errors.Is(err, os.ErrNotExist) {
		return privateBootstrapRecord{}, false, nil
	}
	if err != nil {
		return privateBootstrapRecord{}, false, err
	}
	var record privateBootstrapRecord
	if err = task.DecodeStrict(data, &record); err != nil {
		return privateBootstrapRecord{}, false, errors.Join(ErrBinding, err)
	}
	if record.SchemaVersion != task.SchemaVersion {
		return privateBootstrapRecord{}, false, fmt.Errorf("%w: unsupported private bootstrap journal schema %d", task.ErrUnsupportedSchema, record.SchemaVersion)
	}
	if record.Restart != nil {
		if err = validatePrivatePairRestartIntent(stateRoot, *record.Restart); err != nil {
			return privateBootstrapRecord{}, false, err
		}
	}
	return record, true, nil
}

func markPrivateBootstrapPending(stateRoot string) error {
	data, err := task.MarshalCanonical(privateBootstrapRecord{SchemaVersion: task.SchemaVersion})
	if err != nil {
		return fmt.Errorf("%w: marshal private bootstrap journal: %w", ErrConfiguration, err)
	}
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	return publishPrivateRecord(base, privateBootstrapRecordPath(stateRoot), data)
}

func clearPrivateBootstrapPending(stateRoot string) error {
	pending, err := PrivateBootstrapPending(stateRoot)
	if err != nil || !pending {
		return err
	}
	if err = os.Remove(privateBootstrapRecordPath(stateRoot)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: remove private bootstrap journal: %w", ErrConfiguration, err)
	}
	return syncPrivateDirectory(filepath.Join(stateRoot, privateSupervisorDirectory))
}

func loadPrivateMigrationRecord(base string) (record privateMigrationRecord, found bool, resultErr error) {
	data, err := readRegular(privateMigrationRecordPath(base), MaxControlBytes)
	if errors.Is(err, os.ErrNotExist) {
		return privateMigrationRecord{}, false, nil
	}
	if err != nil {
		return privateMigrationRecord{}, false, errors.Join(ErrBinding, err)
	}
	record, invalid := migration.DecodeRecord(data)
	if invalid != nil {
		return privateMigrationRecord{}, false, privateMigrationDecodeError(invalid)
	}
	return record, true, nil
}

func publishPrivateMigrationRecord(base string, legacy, successor privateDaemonIdentity) error {
	record := privateMigrationRecord{SchemaVersion: task.SchemaVersion, Legacy: legacy, Successor: successor}
	return publishPrivateMigrationState(base, record)
}

// publishPrivateMigrationState preserves progress when resuming a journal.
// Other publishers retain their create-once equality checks.
func publishPrivateMigrationState(base string, record privateMigrationRecord) error {
	data, err := marshalPrivateMigrationRecord(record)
	if err != nil {
		return err
	}
	return publishPrivateRecord(base, privateMigrationRecordPath(base), data)
}

func clearPrivateMigrationRecord(base string, expected privateMigrationRecord) error {
	record, found, err := loadPrivateMigrationRecord(base)
	if err != nil || !found {
		return err
	}
	if record != expected {
		return fmt.Errorf("%w: private migration journal changed", ErrBinding)
	}
	if err = os.Remove(privateMigrationRecordPath(base)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: remove private migration journal: %w", ErrConfiguration, err)
	}
	return syncPrivateDirectory(base)
}

func replacePrivateMigrationRecord(base string, expected, replacement privateMigrationRecord) error {
	current, found, err := loadPrivateMigrationRecord(base)
	if err != nil {
		return err
	}
	if !found || current != expected {
		return fmt.Errorf("%w: private migration journal changed", ErrBinding)
	}
	data, err := marshalPrivateMigrationRecord(replacement)
	if err != nil {
		return err
	}
	return replacePrivateRecord(base, privateMigrationRecordPath(base), data)
}

func privateMigrationHistoryPath(base string) string {
	return filepath.Join(base, privateMigrationHistoryRecordName)
}

func loadPrivateMigrationHistory(base string) (history privateMigrationHistory, found bool, resultErr error) {
	data, err := readRegular(privateMigrationHistoryPath(base), MaxControlBytes)
	if errors.Is(err, os.ErrNotExist) {
		return privateMigrationHistory{}, false, nil
	}
	if err != nil {
		return privateMigrationHistory{}, false, errors.Join(ErrBinding, err)
	}
	history, invalid := migration.DecodeHistory(data)
	if invalid != nil {
		return privateMigrationHistory{}, false, privateMigrationHistoryDecodeError(invalid)
	}
	return history, true, nil
}

func appendPrivateMigrationHistory(base string, record privateMigrationRecord) error {
	if _, err := marshalPrivateMigrationRecord(record); err != nil {
		return err
	}
	if err := validatePrivateMigrationHistoryRecord(record); err != nil {
		return err
	}
	history, found, err := loadPrivateMigrationHistory(base)
	if err != nil {
		return err
	}
	if !found {
		history = privateMigrationHistory{SchemaVersion: task.SchemaVersion}
	}
	for _, existing := range history.Migrations {
		if existing == record {
			return nil
		}
	}
	history.Migrations = append(history.Migrations, record)
	data, invalid := migration.EncodeHistory(history)
	if invalid != nil {
		return privateMigrationHistoryEncodeError(invalid)
	}
	return replacePrivateRecord(base, privateMigrationHistoryPath(base), data)
}

func privateLogicalIdentityMatchesBinding(identity privateDaemonIdentity, binding task.SupervisorRef) bool {
	if !privateLogicalIdentityPresent(identity) {
		return false
	}
	current := privateDaemonIdentityForBinding(binding)
	return identity.ConfigPath == current.ConfigPath &&
		identity.ConfigDigest == current.ConfigDigest &&
		identity.ResolvedConfigSHA256 == current.ResolvedConfigSHA256 &&
		identity.Endpoint == current.Endpoint
}

// PrivateSupervisorGenerationMatches reports whether a task or inspection
// receipt belongs to the current private supervisor generation or a published
// predecessor retained in the migration history. Terminal queue rows remain
// auditable after a later logical migration without accepting another root's
// supervisor identity.
func PrivateSupervisorGenerationMatches(stateRoot string, binding task.SupervisorRef) (bool, error) {
	if !IsPrivateConfig(stateRoot, binding.ConfigPath) {
		return false, ErrConfiguration
	}
	if current, found, err := loadPrivateSupervisorBinding(stateRoot); err != nil {
		return false, err
	} else if found && task.SameSupervisorIdentity(current, binding) {
		return true, nil
	}
	history, found, err := loadPrivateMigrationHistory(filepath.Join(stateRoot, privateSupervisorDirectory))
	if err != nil || !found {
		return false, err
	}
	for _, migration := range history.Migrations {
		if privateLogicalIdentityMatchesBinding(migration.Legacy, binding) || privateLogicalIdentityMatchesBinding(migration.Successor, binding) {
			return true, nil
		}
	}
	return false, nil
}

func marshalPrivateMigrationRecord(record privateMigrationRecord) ([]byte, error) {
	data, invalid := migration.EncodeRecord(record)
	if invalid != nil {
		return nil, privateMigrationEncodeError(invalid)
	}
	return data, nil
}

func validatePrivateMigrationHistoryRecord(record privateMigrationRecord) error {
	return privateMigrationHistoryDecodeError(migration.ValidateHistoryRecord(record))
}

func privateMigrationProcess(record privateMigrationRecord) (privateProcessIdentity, error) {
	if record.LegacyPID <= 0 || record.LegacyProcessStart == "" {
		return privateProcessIdentity{}, fmt.Errorf("%w: private migration journal lacks a predecessor process identity", ErrPrivateSupervisorMigrationRequired)
	}
	return privateProcessIdentity{PID: record.LegacyPID, StartToken: record.LegacyProcessStart}, nil
}

func replacePrivateRecord(base, destination string, data []byte) error {
	return replacePrivateRecordAtomic(base, destination, ".private-migration-", "private migration journal", data, false)
}

// replacePrivateRecordAtomic stages one existing private record and atomically
// replaces it. The caller supplies the stage prefix and record context so the
// error identifies the record being changed. Binding publication requests a
// directory barrier before rename as well as the common barrier afterward.
func replacePrivateRecordAtomic(base, destination, stagePrefix, recordContext string, data []byte, syncBeforeRename bool) (resultErr error) {
	file, stagePath, err := createPrivateStage(base, stagePrefix)
	if err != nil {
		return fmt.Errorf("%w: stage %s replacement: %w", ErrConfiguration, recordContext, err)
	}
	defer func() {
		if stagePath != "" {
			resultErr = errors.Join(resultErr, os.Remove(stagePath))
		}
	}()
	written, writeErr := file.Write(data)
	if writeErr != nil || written != len(data) {
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		return errors.Join(fmt.Errorf("%w: write %s: %w", ErrConfiguration, recordContext, writeErr), file.Close())
	}
	if err = privateBarrierFile(file); err != nil {
		return errors.Join(fmt.Errorf("%w: sync %s: %w", ErrConfiguration, recordContext, err), file.Close())
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("%w: close %s: %w", ErrConfiguration, recordContext, err)
	}
	if syncBeforeRename {
		if err = syncPrivateDirectory(base); err != nil {
			return fmt.Errorf("%w: sync %s directory before replacement: %w", ErrConfiguration, recordContext, err)
		}
	}
	if err = os.Rename(stagePath, destination); err != nil {
		return fmt.Errorf("%w: atomically replace %s: %w", ErrConfiguration, recordContext, err)
	}
	stagePath = ""
	if err = syncPrivateDirectory(base); err != nil {
		return fmt.Errorf("%w: sync %s directory after replacement: %w", ErrConfiguration, recordContext, err)
	}
	return nil
}

func privateDaemonIdentityPath(base string) string {
	return filepath.Join(base, "daemon.identity.json")
}

func recoverPrivateIdentityPublication(stateRoot string) error {
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return ErrConfiguration
	}
	pending, found, err := readPrivateIdentityPublication(stateRoot)
	if err != nil || !found {
		return err
	}
	return publishPrivateIdentityPublication(stateRoot, pending)
}

func readPrivateIdentityPublication(stateRoot string) (privateIdentityPublicationRecord, bool, error) {
	var pending privateIdentityPublicationRecord
	pendingPath := privateIdentityPublicationPath(stateRoot)
	data, err := readRegular(pendingPath, MaxControlBytes)
	if errors.Is(err, os.ErrNotExist) {
		return pending, false, nil
	}
	if err != nil {
		return pending, false, fmt.Errorf("%w: read private identity publication: %w", ErrBinding, err)
	}
	if err = task.DecodeStrict(data, &pending); err != nil {
		return pending, false, fmt.Errorf("%w: decode private identity publication: %w", ErrBinding, err)
	}
	if pending.SchemaVersion != task.SchemaVersion {
		return pending, false, fmt.Errorf("%w: unsupported private identity publication schema %d", task.ErrUnsupportedSchema, pending.SchemaVersion)
	}
	if !IsPrivateConfig(stateRoot, pending.Binding.ConfigPath) {
		return pending, false, fmt.Errorf("%w: private identity publication is outside its state root", ErrBinding)
	}
	if err = validatePrivateSupervisorRef(pending.Binding); err != nil {
		return pending, false, fmt.Errorf("%w: validate private identity publication: %w", ErrBinding, err)
	}
	identity := privateDaemonIdentityForBinding(pending.Binding)
	if err = validatePrivateDaemonIdentity(identity); err != nil {
		return pending, false, fmt.Errorf("%w: validate private identity publication daemon: %w", ErrBinding, err)
	}
	return pending, true, nil
}

func publishPrivateIdentityPublication(stateRoot string, pending privateIdentityPublicationRecord) error {
	pendingPath := privateIdentityPublicationPath(stateRoot)
	bindingData, err := task.MarshalCanonical(privateSupervisorBindingRecord{
		SchemaVersion: task.SchemaVersion,
		Binding:       pending.Binding,
	})
	if err != nil {
		return fmt.Errorf("%w: marshal private identity publication binding: %w", ErrConfiguration, err)
	}
	daemonData, err := task.MarshalCanonical(privateDaemonIdentityForBinding(pending.Binding))
	if err != nil {
		return fmt.Errorf("%w: marshal private identity publication daemon: %w", ErrConfiguration, err)
	}
	if err = replacePrivateSupervisorBindingRecord(stateRoot, bindingData); err != nil {
		return err
	}
	if err = replacePrivateDaemonIdentityRecord(filepath.Join(stateRoot, privateSupervisorDirectory), daemonData); err != nil {
		return err
	}
	if err = os.Remove(pendingPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: clear private identity publication: %w", ErrConfiguration, err)
	}
	return syncPrivateDirectory(filepath.Join(stateRoot, privateSupervisorDirectory))
}

func publishPrivateDaemonIdentity(base string, binding task.SupervisorRef) error {
	identity := privateDaemonIdentityForBinding(binding)
	if err := validatePrivateDaemonIdentity(identity); err != nil {
		return fmt.Errorf("%w: validate private supervisor identity: %w", ErrConfiguration, err)
	}
	if binding.ConfigPath == "" {
		return publishDaemonOnlyIdentity(base, identity)
	}
	if err := task.ValidateFreshSupervisorRef(binding); err != nil {
		return fmt.Errorf("%w: validate private supervisor binding: %w", ErrConfiguration, err)
	}
	stateRoot, private := privateStateRoot(binding.ConfigPath)
	if !private {
		return publishDaemonOnlyIdentity(base, identity)
	}
	data, err := task.MarshalCanonical(identity)
	if err != nil {
		return fmt.Errorf("%w: marshal private daemon identity: %w", ErrConfiguration, err)
	}
	if err = recoverPrivateIdentityPublication(stateRoot); err != nil {
		return err
	}
	bindingData, err := task.MarshalCanonical(privateSupervisorBindingRecord{SchemaVersion: task.SchemaVersion, Binding: binding})
	if err != nil {
		return fmt.Errorf("%w: marshal private supervisor binding: %w", ErrConfiguration, err)
	}
	pendingData, err := task.MarshalCanonical(privateIdentityPublicationRecord{
		SchemaVersion: task.SchemaVersion,
		Binding:       binding,
	})
	if err != nil {
		return fmt.Errorf("%w: marshal private identity publication: %w", ErrConfiguration, err)
	}
	if err = publishPrivateRecord(filepath.Join(stateRoot, privateSupervisorDirectory), privateIdentityPublicationPath(stateRoot), pendingData); err != nil {
		return err
	}
	if err = replacePrivateSupervisorBindingRecord(filepath.Dir(base), bindingData); err != nil {
		return err
	}
	if err = replacePrivateDaemonIdentityRecord(base, data); err != nil {
		return err
	}
	if err = os.Remove(privateIdentityPublicationPath(stateRoot)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: clear private identity publication: %w", ErrConfiguration, err)
	}
	return syncPrivateDirectory(filepath.Join(stateRoot, privateSupervisorDirectory))
}

func publishDaemonOnlyIdentity(base string, identity privateDaemonIdentity) error {
	data, err := task.MarshalCanonical(privateDaemonIdentity{Executable: identity.Executable, SHA256: identity.SHA256})
	if err != nil {
		return fmt.Errorf("%w: marshal private daemon identity: %w", ErrConfiguration, err)
	}
	return replacePrivateDaemonIdentityRecord(base, data)
}

func replacePrivateSupervisorBindingRecord(stateRoot string, data []byte) error {
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	destination := privateSupervisorBindingPath(stateRoot)
	return replacePrivateRecordAtomic(base, destination, ".private-binding-", "private supervisor binding", data, true)
}

// replacePrivateDaemonIdentityRecord atomically replaces launch provenance
// after the supervisor it describes has passed readiness. The create-once
// publisher below remains strict for all other private records.
func replacePrivateDaemonIdentityRecord(base string, data []byte) error {
	destination := privateDaemonIdentityPath(base)
	return replacePrivateRecordAtomic(base, destination, privateRecordStagePrefix, "private daemon identity", data, false)
}

func publishPrivateRecord(base, destination string, data []byte) (resultErr error) {
	file, stagePath, err := createPrivateStage(base, privateRecordStagePrefix)
	if err != nil {
		return err
	}
	defer func() {
		if stagePath != "" {
			resultErr = errors.Join(resultErr, os.Remove(stagePath))
		}
	}()
	written, writeErr := file.Write(data)
	if writeErr != nil || written != len(data) {
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		return errors.Join(fmt.Errorf("%w: write private record: %w", ErrConfiguration, writeErr), file.Close())
	}
	if err = privateBarrierFile(file); err != nil {
		return errors.Join(fmt.Errorf("%w: sync private record: %w", ErrConfiguration, err), file.Close())
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("%w: close private record: %w", ErrConfiguration, err)
	}
	if err = os.Link(stagePath, destination); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: publish private record: %w", ErrConfiguration, err)
		}
		existing, readErr := readRegular(destination, MaxControlBytes)
		if readErr != nil {
			return errors.Join(ErrBinding, readErr)
		}
		if !bytes.Equal(existing, data) {
			return ErrBinding
		}
		if removeErr := os.Remove(stagePath); removeErr != nil {
			return fmt.Errorf("%w: discard private record stage: %w", ErrConfiguration, removeErr)
		}
		stagePath = ""
		return syncPrivateDirectory(base)
	}
	if err = syncPrivateDirectory(base); err != nil {
		return err
	}
	if err = os.Remove(stagePath); err != nil {
		return fmt.Errorf("%w: remove private record stage: %w", ErrConfiguration, err)
	}
	stagePath = ""
	return syncPrivateDirectory(base)
}
