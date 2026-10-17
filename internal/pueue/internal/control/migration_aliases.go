package control

import (
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/pueue/internal/migration"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type (
	privateDaemonIdentity   = migration.DaemonIdentity
	privateMigrationRecord  = migration.Record
	privateMigrationHistory = migration.History
)

func privateDaemonIdentityError(invalid *migration.ViolationError) error {
	if invalid == nil {
		return nil
	}
	switch invalid.Code {
	case migration.ViolationExecutableDigest, migration.ViolationConfigDigest, migration.ViolationResolvedDigest:
		return invalid.Cause
	case migration.ViolationEndpointScheme:
		return fmt.Errorf("%w: private supervisor endpoint is not a Unix socket", ErrBinding)
	case migration.ViolationEndpointPath:
		return fmt.Errorf("%w: private supervisor endpoint is invalid", ErrBinding)
	case migration.ViolationRecordDecode,
		migration.ViolationRecordSchema,
		migration.ViolationLegacyIdentity,
		migration.ViolationSuccessorIdentity,
		migration.ViolationIdenticalIdentity,
		migration.ViolationProcessState,
		migration.ViolationRecordEncode,
		migration.ViolationHistoryDecode,
		migration.ViolationHistorySchema,
		migration.ViolationHistoryUnproven,
		migration.ViolationHistoryEncode,
		migration.ViolationExecutablePath,
		migration.ViolationLogicalIdentity,
		migration.ViolationConfigPath:
		return ErrBinding
	}
	return ErrBinding
}

func privateMigrationDecodeError(invalid *migration.ViolationError) error {
	if invalid == nil {
		return nil
	}
	switch invalid.Code {
	case migration.ViolationRecordDecode:
		return errors.Join(ErrBinding, invalid.Cause)
	case migration.ViolationRecordSchema:
		return fmt.Errorf("%w: unsupported private migration journal schema %d", task.ErrUnsupportedSchema, invalid.SchemaVersion)
	case migration.ViolationLegacyIdentity:
		return fmt.Errorf("%w: invalid private migration legacy identity: %w", ErrBinding, privateDaemonIdentityError(asMigrationViolation(invalid.Cause)))
	case migration.ViolationSuccessorIdentity:
		return fmt.Errorf("%w: invalid private migration successor identity: %w", ErrBinding, privateDaemonIdentityError(asMigrationViolation(invalid.Cause)))
	case migration.ViolationIdenticalIdentity:
		return fmt.Errorf("%w: private migration journal has identical identities", ErrBinding)
	case migration.ViolationProcessState:
		return fmt.Errorf("%w: invalid private migration predecessor state", ErrBinding)
	case migration.ViolationRecordEncode,
		migration.ViolationHistoryDecode,
		migration.ViolationHistorySchema,
		migration.ViolationHistoryUnproven,
		migration.ViolationHistoryEncode,
		migration.ViolationExecutablePath,
		migration.ViolationExecutableDigest,
		migration.ViolationLogicalIdentity,
		migration.ViolationConfigPath,
		migration.ViolationConfigDigest,
		migration.ViolationResolvedDigest,
		migration.ViolationEndpointScheme,
		migration.ViolationEndpointPath:
		return ErrBinding
	}
	return ErrBinding
}

func privateMigrationEncodeError(invalid *migration.ViolationError) error {
	if invalid == nil {
		return nil
	}
	switch invalid.Code {
	case migration.ViolationRecordSchema:
		return fmt.Errorf("%w: invalid private migration journal schema", ErrConfiguration)
	case migration.ViolationLegacyIdentity:
		return fmt.Errorf("%w: invalid private migration legacy identity: %w", ErrConfiguration, privateDaemonIdentityError(asMigrationViolation(invalid.Cause)))
	case migration.ViolationSuccessorIdentity:
		return fmt.Errorf("%w: invalid private migration successor identity: %w", ErrConfiguration, privateDaemonIdentityError(asMigrationViolation(invalid.Cause)))
	case migration.ViolationIdenticalIdentity:
		return fmt.Errorf("%w: private migration identities are identical", ErrConfiguration)
	case migration.ViolationProcessState:
		return fmt.Errorf("%w: invalid private migration predecessor state", ErrConfiguration)
	case migration.ViolationRecordEncode:
		return fmt.Errorf("%w: marshal replacement private migration journal: %w", ErrConfiguration, invalid.Cause)
	case migration.ViolationRecordDecode,
		migration.ViolationHistoryDecode,
		migration.ViolationHistorySchema,
		migration.ViolationHistoryUnproven,
		migration.ViolationHistoryEncode,
		migration.ViolationExecutablePath,
		migration.ViolationExecutableDigest,
		migration.ViolationLogicalIdentity,
		migration.ViolationConfigPath,
		migration.ViolationConfigDigest,
		migration.ViolationResolvedDigest,
		migration.ViolationEndpointScheme,
		migration.ViolationEndpointPath:
		return ErrConfiguration
	}
	return ErrConfiguration
}

func privateMigrationHistoryDecodeError(invalid *migration.ViolationError) error {
	if invalid == nil {
		return nil
	}
	switch invalid.Code {
	case migration.ViolationHistoryDecode:
		return errors.Join(ErrBinding, invalid.Cause)
	case migration.ViolationHistorySchema:
		return fmt.Errorf("%w: unsupported private migration history schema %d", task.ErrUnsupportedSchema, invalid.SchemaVersion)
	case migration.ViolationHistoryUnproven:
		return fmt.Errorf("%w: private migration history does not prove predecessor shutdown", ErrBinding)
	case migration.ViolationRecordDecode,
		migration.ViolationRecordSchema,
		migration.ViolationLegacyIdentity,
		migration.ViolationSuccessorIdentity,
		migration.ViolationIdenticalIdentity,
		migration.ViolationProcessState,
		migration.ViolationRecordEncode,
		migration.ViolationHistoryEncode,
		migration.ViolationExecutablePath,
		migration.ViolationExecutableDigest,
		migration.ViolationLogicalIdentity,
		migration.ViolationConfigPath,
		migration.ViolationConfigDigest,
		migration.ViolationResolvedDigest,
		migration.ViolationEndpointScheme,
		migration.ViolationEndpointPath:
		return privateMigrationEncodeError(invalid)
	}
	return privateMigrationEncodeError(invalid)
}

func privateMigrationHistoryEncodeError(invalid *migration.ViolationError) error {
	if invalid == nil {
		return nil
	}
	if invalid.Code == migration.ViolationHistoryEncode {
		return fmt.Errorf("%w: marshal private migration history: %w", ErrConfiguration, invalid.Cause)
	}
	if invalid.Code == migration.ViolationHistoryUnproven {
		return fmt.Errorf("%w: private migration history does not prove predecessor shutdown", ErrBinding)
	}
	return privateMigrationEncodeError(invalid)
}

func asMigrationViolation(err error) *migration.ViolationError {
	var invalid *migration.ViolationError
	if errors.As(err, &invalid) {
		return invalid
	}
	return nil
}
