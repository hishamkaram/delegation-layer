package migration

import (
	"errors"
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/pueue/protocol"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func DecodeRecord(data []byte) (Record, *ViolationError) {
	var record Record
	if len(data) > protocol.MaxControlBytes {
		return Record{}, violation(ViolationRecordDecode, protocol.ErrControlLimit)
	}
	if err := task.DecodeStrict(data, &record); err != nil {
		return Record{}, violation(ViolationRecordDecode, err)
	}
	if record.SchemaVersion != task.SchemaVersion {
		return Record{}, &ViolationError{Code: ViolationRecordSchema, SchemaVersion: record.SchemaVersion}
	}
	if invalid := ValidateDaemonIdentity(record.Legacy); invalid != nil {
		return Record{}, violation(ViolationLegacyIdentity, invalid)
	}
	if invalid := ValidateDaemonIdentity(record.Successor); invalid != nil {
		return Record{}, violation(ViolationSuccessorIdentity, invalid)
	}
	if record.Legacy == record.Successor && record.UpgradeGeneration == (UpgradeGeneration{}) {
		return Record{}, violation(ViolationIdenticalIdentity, nil)
	}
	if invalid := ValidateProcessState(record); invalid != nil {
		return Record{}, violation(ViolationProcessState, invalid)
	}
	if invalid := validateUpgradeGeneration(record); invalid != nil {
		return Record{}, invalid
	}
	return record, nil
}

func EncodeRecord(record Record) ([]byte, *ViolationError) {
	if record.SchemaVersion != task.SchemaVersion {
		return nil, violation(ViolationRecordSchema, nil)
	}
	if invalid := ValidateDaemonIdentity(record.Legacy); invalid != nil {
		return nil, violation(ViolationLegacyIdentity, invalid)
	}
	if invalid := ValidateDaemonIdentity(record.Successor); invalid != nil {
		return nil, violation(ViolationSuccessorIdentity, invalid)
	}
	if record.Legacy == record.Successor && record.UpgradeGeneration == (UpgradeGeneration{}) {
		return nil, violation(ViolationIdenticalIdentity, nil)
	}
	if invalid := ValidateProcessState(record); invalid != nil {
		return nil, violation(ViolationProcessState, invalid)
	}
	if invalid := validateUpgradeGeneration(record); invalid != nil {
		return nil, invalid
	}
	data, err := task.MarshalCanonical(record)
	if err != nil {
		return nil, violation(ViolationRecordEncode, err)
	}
	return data, nil
}

// ValidateUpgradeSubject guards the frozen upgrade-v1 correlation encoding.
// It does no I/O and grants no lifecycle authority.
func ValidateUpgradeSubject(root UpgradeRoot, predecessor task.SupervisorRef, op UpgradeOperationID, mode UpgradeMode) error {
	const prefix = "upgrade-v1:"
	if !strings.HasPrefix(string(op), prefix) || task.ValidateTaskID(strings.TrimPrefix(string(op), prefix)) != nil {
		return errors.New("invalid upgrade operation identity")
	}
	if !filepath.IsAbs(root.Path) || filepath.Clean(root.Path) != root.Path || root.Inode == 0 || task.ValidateTaskID(root.ID) != nil {
		return errors.New("invalid upgrade root identity")
	}
	if mode != UpgradeReactive && mode != UpgradeOperationRequired {
		return errors.New("invalid upgrade mode")
	}
	if err := task.ValidateFreshSupervisorRef(predecessor); err != nil {
		return err
	}
	if predecessor.ConfigPath != filepath.Join(root.Path, ".supervisor", "pueue.yml") {
		return errors.New("upgrade predecessor is outside its root")
	}
	return nil
}

func validateUpgradeGeneration(record Record) *ViolationError {
	u := record.UpgradeGeneration
	if u == (UpgradeGeneration{}) {
		return nil
	}
	if err := ValidateUpgradeSubject(u.Root, u.Predecessor, u.OperationID, u.Mode); err != nil {
		return violation(ViolationProcessState, err)
	}
	if err := task.ValidateFreshSupervisorRef(u.Target); err != nil {
		return violation(ViolationProcessState, err)
	}
	if u.Target.ExecutableIdentity != task.ExecutableIdentityInstalledPathV1 || u.Target.ConfigPath != u.Predecessor.ConfigPath || !upgradeBindingMatches(u.Predecessor, record.Legacy) || !upgradeBindingMatches(u.Target, record.Successor) || lifetime.CompareBootIDs(u.LegacyBootID, u.LegacyBootID) != lifetime.BootSame {
		return violation(ViolationProcessState, errors.New("upgrade migration identities disagree"))
	}
	if u.AcceptedStart && !record.LegacyStopped {
		return violation(ViolationProcessState, errors.New("upgrade Start precedes predecessor closure"))
	}
	if record.LegacyStopped && !record.LegacyShutdownRequested {
		return violation(ViolationProcessState, errors.New("upgrade closure lacks predecessor shutdown intent"))
	}
	return validateUpgradeBirth(record)
}

func validateUpgradeBirth(record Record) *ViolationError {
	u := record.UpgradeGeneration
	if u.SuccessorPID == 0 && u.SuccessorProcessStart == "" && u.SuccessorBootID == "" {
		if u.Completed {
			return violation(ViolationProcessState, errors.New("completed upgrade lacks successor birth"))
		}
		return nil
	}
	if !u.AcceptedStart || lifetime.CompareBootIDs(u.SuccessorBootID, u.SuccessorBootID) != lifetime.BootSame {
		return violation(ViolationProcessState, errors.New("upgrade birth lacks accepted Start"))
	}
	newProcess, err := lifetime.NewProcessRef(u.SuccessorPID, u.SuccessorProcessStart)
	if err != nil {
		return violation(ViolationProcessState, err)
	}
	oldProcess, err := lifetime.NewProcessRef(record.LegacyPID, record.LegacyProcessStart)
	if err != nil {
		return violation(ViolationProcessState, err)
	}
	same, err := lifetime.SameProcess(newProcess, oldProcess)
	if err != nil || same && lifetime.CompareBootIDs(u.LegacyBootID, u.SuccessorBootID) != lifetime.BootDifferent {
		return violation(ViolationProcessState, errors.Join(errors.New("upgrade successor is its predecessor"), err))
	}
	return nil
}

func upgradeBindingMatches(binding task.SupervisorRef, identity DaemonIdentity) bool {
	return binding.DaemonExecutable == identity.Executable && binding.DaemonSHA256 == identity.SHA256 && binding.ExecutableIdentity == identity.ExecutableIdentity &&
		binding.ConfigPath == identity.ConfigPath && binding.ConfigDigest == identity.ConfigDigest && binding.ResolvedConfigSHA256 == identity.ResolvedConfigSHA256 && binding.Endpoint == identity.Endpoint
}

func DecodeHistory(data []byte) (History, *ViolationError) {
	var history History
	if len(data) > protocol.MaxControlBytes {
		return History{}, violation(ViolationHistoryDecode, protocol.ErrControlLimit)
	}
	if err := task.DecodeStrict(data, &history); err != nil {
		return History{}, violation(ViolationHistoryDecode, err)
	}
	if history.SchemaVersion != task.SchemaVersion {
		return History{}, &ViolationError{Code: ViolationHistorySchema, SchemaVersion: history.SchemaVersion}
	}
	for _, record := range history.Migrations {
		if _, invalid := EncodeRecord(record); invalid != nil {
			return History{}, invalid
		}
		if invalid := ValidateHistoryRecord(record); invalid != nil {
			return History{}, invalid
		}
		if record.UpgradeGeneration.OperationID != "" && !record.UpgradeGeneration.Completed {
			return History{}, violation(ViolationHistoryUnproven, nil)
		}
	}
	if invalid := validateUpgradeHistoryWinners(history); invalid != nil {
		return History{}, invalid
	}
	return history, nil
}

func EncodeHistory(history History) ([]byte, *ViolationError) {
	if history.SchemaVersion != task.SchemaVersion {
		return nil, violation(ViolationHistorySchema, nil)
	}
	for _, record := range history.Migrations {
		if _, invalid := EncodeRecord(record); invalid != nil {
			return nil, invalid
		}
		if invalid := ValidateHistoryRecord(record); invalid != nil {
			return nil, invalid
		}
		if record.UpgradeGeneration.OperationID != "" && !record.UpgradeGeneration.Completed {
			return nil, violation(ViolationHistoryUnproven, nil)
		}
	}
	if invalid := validateUpgradeHistoryWinners(history); invalid != nil {
		return nil, invalid
	}
	data, err := task.MarshalCanonical(history)
	if err != nil {
		return nil, violation(ViolationHistoryEncode, err)
	}
	return data, nil
}

func validateUpgradeHistoryWinners(history History) *ViolationError {
	seen := make(map[UpgradeOperationID]struct{})
	for _, record := range history.Migrations {
		op := record.UpgradeGeneration.OperationID
		if op == "" {
			continue
		}
		if _, exists := seen[op]; exists {
			return violation(ViolationHistoryUnproven, errors.New("duplicate upgrade operation history"))
		}
		seen[op] = struct{}{}
	}
	return nil
}
