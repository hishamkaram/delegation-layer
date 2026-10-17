package migration

import (
	"errors"
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type ViolationCode string

const (
	ViolationRecordDecode      ViolationCode = "record_decode"
	ViolationRecordSchema      ViolationCode = "record_schema"
	ViolationLegacyIdentity    ViolationCode = "legacy_identity"
	ViolationSuccessorIdentity ViolationCode = "successor_identity"
	ViolationIdenticalIdentity ViolationCode = "identical_identity"
	ViolationProcessState      ViolationCode = "process_state"
	ViolationRecordEncode      ViolationCode = "record_encode"
	ViolationHistoryDecode     ViolationCode = "history_decode"
	ViolationHistorySchema     ViolationCode = "history_schema"
	ViolationHistoryUnproven   ViolationCode = "history_unproven"
	ViolationHistoryEncode     ViolationCode = "history_encode"
	ViolationExecutablePath    ViolationCode = "executable_path"
	ViolationExecutableDigest  ViolationCode = "executable_digest"
	ViolationLogicalIdentity   ViolationCode = "logical_identity"
	ViolationConfigPath        ViolationCode = "config_path"
	ViolationConfigDigest      ViolationCode = "config_digest"
	ViolationResolvedDigest    ViolationCode = "resolved_digest"
	ViolationEndpointScheme    ViolationCode = "endpoint_scheme"
	ViolationEndpointPath      ViolationCode = "endpoint_path"
)

type ViolationError struct {
	Code          ViolationCode
	SchemaVersion int
	Cause         error
}

func (v *ViolationError) Error() string {
	if v == nil {
		return "migration violation"
	}
	if v.Cause != nil {
		return v.Cause.Error()
	}
	return string(v.Code)
}

func (v *ViolationError) Unwrap() error {
	if v == nil {
		return nil
	}
	return v.Cause
}

func violation(code ViolationCode, cause error) *ViolationError {
	return &ViolationError{Code: code, Cause: cause}
}

func ValidateDaemonIdentity(identity DaemonIdentity) *ViolationError {
	if !filepath.IsAbs(identity.Executable) || filepath.Clean(identity.Executable) != identity.Executable || strings.ContainsRune(identity.Executable, 0) {
		return violation(ViolationExecutablePath, nil)
	}
	if err := task.ValidateSHA256(identity.SHA256); err != nil {
		return violation(ViolationExecutableDigest, err)
	}
	if !LogicalIdentityPresent(identity) {
		return nil
	}
	if identity.ConfigPath == "" || identity.Endpoint == "" {
		return violation(ViolationLogicalIdentity, nil)
	}
	if !filepath.IsAbs(identity.ConfigPath) || filepath.Clean(identity.ConfigPath) != identity.ConfigPath || strings.ContainsRune(identity.ConfigPath, 0) {
		return violation(ViolationConfigPath, nil)
	}
	if err := task.ValidateSHA256(identity.ConfigDigest); err != nil {
		return violation(ViolationConfigDigest, err)
	}
	if err := task.ValidateSHA256(identity.ResolvedConfigSHA256); err != nil {
		return violation(ViolationResolvedDigest, err)
	}
	_, endpointViolation := UnixSocketPath(identity.Endpoint)
	return endpointViolation
}

func LogicalIdentityPresent(identity DaemonIdentity) bool {
	return identity.ConfigPath != "" || identity.ConfigDigest != "" || identity.ResolvedConfigSHA256 != "" || identity.Endpoint != ""
}

func UnixSocketPath(endpoint string) (string, *ViolationError) {
	const prefix = "unix:"
	if !strings.HasPrefix(endpoint, prefix) {
		return "", violation(ViolationEndpointScheme, nil)
	}
	path := strings.TrimPrefix(endpoint, prefix)
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) {
		return "", violation(ViolationEndpointPath, nil)
	}
	return path, nil
}

func ValidateProcessState(record Record) *ViolationError {
	switch {
	case record.LegacyPID < 0:
		return violation(ViolationProcessState, errors.New("negative predecessor PID"))
	case record.LegacyProcessStart != "" && record.LegacyPID <= 0:
		return violation(ViolationProcessState, errors.New("process start token lacks predecessor PID"))
	case record.LegacyShutdownRequested && (record.LegacyPID <= 0 || record.LegacyProcessStart == ""):
		return violation(ViolationProcessState, errors.New("shutdown request lacks predecessor process identity"))
	case record.LegacyShutdownDispatched && record.LegacyPID <= 0:
		return violation(ViolationProcessState, errors.New("dispatched shutdown lacks predecessor PID"))
	case record.LegacyStopped && record.LegacyPID <= 0:
		return violation(ViolationProcessState, errors.New("stopped predecessor lacks PID"))
	default:
		return nil
	}
}

func ValidateHistoryRecord(record Record) *ViolationError {
	if !record.LegacyStopped || record.LegacyPID <= 0 {
		return violation(ViolationHistoryUnproven, nil)
	}
	return nil
}
