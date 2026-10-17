package migration

import (
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
	if record.Legacy == record.Successor {
		return Record{}, violation(ViolationIdenticalIdentity, nil)
	}
	if invalid := ValidateProcessState(record); invalid != nil {
		return Record{}, violation(ViolationProcessState, invalid)
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
	if record.Legacy == record.Successor {
		return nil, violation(ViolationIdenticalIdentity, nil)
	}
	if invalid := ValidateProcessState(record); invalid != nil {
		return nil, violation(ViolationProcessState, invalid)
	}
	data, err := task.MarshalCanonical(record)
	if err != nil {
		return nil, violation(ViolationRecordEncode, err)
	}
	return data, nil
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
	}
	data, err := task.MarshalCanonical(history)
	if err != nil {
		return nil, violation(ViolationHistoryEncode, err)
	}
	return data, nil
}
