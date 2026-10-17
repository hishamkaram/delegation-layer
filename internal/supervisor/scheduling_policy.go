package supervisor

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func decodePrivateOrdinaryScheduling(data []byte, binding task.SupervisorRef) (pueue.SubmissionAuthorization, bool, error) {
	var fields map[string]json.RawMessage
	if err := task.DecodeStrict(data, &fields); err != nil {
		return pueue.SubmissionAuthorization{}, false, err
	}
	if err := validatePrivateOrdinarySchedulingFields(fields); err != nil {
		return pueue.SubmissionAuthorization{}, false, err
	}
	var schemaVersion int
	if err := json.Unmarshal(fields["schema_version"], &schemaVersion); err != nil {
		return pueue.SubmissionAuthorization{}, false, err
	}
	if schemaVersion != task.SchemaVersion {
		return pueue.SubmissionAuthorization{}, false, task.ErrUnsupportedSchema
	}
	var revision string
	if err := json.Unmarshal(fields["revision"], &revision); err != nil {
		return pueue.SubmissionAuthorization{}, false, err
	}
	switch revision {
	case privateOrdinarySchedulingLegacyRevision:
		recorded, err := decodeLegacyPrivateOrdinaryScheduling(fields)
		return pueue.SubmissionAuthorization{}, recorded, err
	case privateOrdinarySchedulingDaemonRevision:
		recorded, err := decodeDaemonPrivateOrdinaryScheduling(fields)
		return pueue.SubmissionAuthorization{}, recorded, err
	case privateOrdinarySchedulingCapabilitylessRevision:
		recorded, err := decodeCapabilitylessPrivateOrdinaryScheduling(fields, binding)
		return pueue.SubmissionAuthorization{}, recorded, err
	case privateOrdinarySchedulingRevision:
		return decodeCurrentPrivateOrdinaryScheduling(fields, binding)
	default:
		return pueue.SubmissionAuthorization{}, false, fmt.Errorf("unsupported private ordinary scheduling revision %q", revision)
	}
}

func validatePrivateOrdinarySchedulingFields(fields map[string]json.RawMessage) error {
	for field := range fields {
		switch field {
		case "schema_version", "revision", "supervisor", "config_digest", "staged_launch":
		default:
			return fmt.Errorf("unknown private ordinary scheduling field %q", field)
		}
	}
	if len(fields["schema_version"]) == 0 || len(fields["revision"]) == 0 {
		return task.ErrEvidenceFault
	}
	return nil
}

func decodeLegacyPrivateOrdinaryScheduling(fields map[string]json.RawMessage) (bool, error) {
	configDigest, err := privateOrdinarySchedulingString(fields, "config_digest")
	if err != nil || len(fields["supervisor"]) != 0 || len(fields["staged_launch"]) != 0 || task.ValidateSHA256(configDigest) != nil {
		return false, task.ErrEvidenceFault
	}
	return false, nil
}

func decodeDaemonPrivateOrdinaryScheduling(fields map[string]json.RawMessage) (bool, error) {
	configDigest, err := privateOrdinarySchedulingString(fields, "config_digest")
	if err != nil || configDigest != "" || len(fields["supervisor"]) == 0 || len(fields["staged_launch"]) != 0 {
		return false, task.ErrEvidenceFault
	}
	var daemon privateOrdinarySchedulingDaemon
	if err := task.DecodeStrict(fields["supervisor"], &daemon); err != nil {
		return false, err
	}
	if err := validatePrivateOrdinarySchedulingDaemon(daemon); err != nil {
		return false, err
	}
	// v2 did not bind the client provenance, so even a complete daemon record
	// is stale for the full-generation contract.
	return false, nil
}

func privateOrdinarySchedulingString(fields map[string]json.RawMessage, field string) (string, error) {
	data := fields[field]
	if len(data) == 0 {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return "", err
	}
	return value, nil
}

func validatePrivateOrdinarySchedulingDaemon(daemon privateOrdinarySchedulingDaemon) error {
	if !canonicalAbsolutePath(daemon.Executable) || task.ValidateSHA256(daemon.SHA256) != nil {
		return task.ErrEvidenceFault
	}
	logical := []string{daemon.ConfigPath, daemon.ConfigDigest, daemon.ResolvedConfigSHA256, daemon.Endpoint}
	present := 0
	for _, value := range logical {
		if value != "" {
			present++
		}
	}
	if present == 0 {
		return nil
	}
	if present != len(logical) || !canonicalAbsolutePath(daemon.ConfigPath) {
		return task.ErrEvidenceFault
	}
	if task.ValidateSHA256(daemon.ConfigDigest) != nil || task.ValidateSHA256(daemon.ResolvedConfigSHA256) != nil {
		return task.ErrEvidenceFault
	}
	if !strings.HasPrefix(daemon.Endpoint, "unix:") || !canonicalAbsolutePath(strings.TrimPrefix(daemon.Endpoint, "unix:")) {
		return task.ErrEvidenceFault
	}
	return nil
}

func canonicalAbsolutePath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsRune(path, 0)
}

func decodeCapabilitylessPrivateOrdinaryScheduling(fields map[string]json.RawMessage, binding task.SupervisorRef) (bool, error) {
	_, err := decodePrivateOrdinarySchedulingBinding(fields, binding)
	if err != nil || len(fields["staged_launch"]) != 0 {
		if err == nil {
			err = task.ErrEvidenceFault
		}
		return false, err
	}
	// v3 proves the exact generation and scheduling policy but contains no
	// staged command-surface evidence, so it is a valid stale receipt.
	return false, nil
}

func decodeCurrentPrivateOrdinaryScheduling(fields map[string]json.RawMessage, binding task.SupervisorRef) (pueue.SubmissionAuthorization, bool, error) {
	recorded, err := decodePrivateOrdinarySchedulingBinding(fields, binding)
	if err != nil || !recorded {
		return pueue.SubmissionAuthorization{}, recorded, err
	}
	if len(fields["staged_launch"]) == 0 {
		return pueue.SubmissionAuthorization{}, false, task.ErrEvidenceFault
	}
	var capability pueue.StagedLaunchCapability
	if err := task.DecodeStrict(fields["staged_launch"], &capability); err != nil {
		return pueue.SubmissionAuthorization{}, false, err
	}
	if !capability.AddStashed || !capability.EnqueueByID {
		return pueue.SubmissionAuthorization{}, false, task.ErrEvidenceFault
	}
	return pueue.SubmissionAuthorization{StagedLaunchCapability: &capability}, true, nil
}

func decodePrivateOrdinarySchedulingBinding(fields map[string]json.RawMessage, binding task.SupervisorRef) (bool, error) {
	configDigest, err := privateOrdinarySchedulingString(fields, "config_digest")
	if err != nil || configDigest != "" || len(fields["supervisor"]) == 0 {
		return false, task.ErrEvidenceFault
	}
	var recorded task.SupervisorRef
	if err := task.DecodeStrict(fields["supervisor"], &recorded); err != nil {
		return false, err
	}
	if err := validatePrivateOrdinarySchedulingBinding(recorded); err != nil {
		return false, err
	}
	return recorded == binding, nil
}

func validatePrivateOrdinarySchedulingBinding(binding task.SupervisorRef) error {
	if err := task.ValidateFreshSupervisorRef(binding); err != nil {
		return err
	}
	if !canonicalAbsolutePath(binding.DaemonExecutable) {
		return task.ErrEvidenceFault
	}
	return task.ValidateSHA256(binding.DaemonSHA256)
}
