package migration

import "github.com/hishamkaram/delegation-layer/internal/task"

// DaemonIdentity is the immutable executable and logical endpoint identity
// saved while a private supervisor generation changes.
type DaemonIdentity struct {
	Executable           string                  `json:"executable"`
	SHA256               string                  `json:"sha256,omitempty"`
	ExecutableIdentity   task.ExecutableIdentity `json:"executable_identity,omitempty"`
	ConfigPath           string                  `json:"config_path,omitempty"`
	ConfigDigest         string                  `json:"config_digest,omitempty"`
	ResolvedConfigSHA256 string                  `json:"resolved_config_sha256,omitempty"`
	Endpoint             string                  `json:"endpoint,omitempty"`
}

type Record struct {
	SchemaVersion            int            `json:"schema_version"`
	Legacy                   DaemonIdentity `json:"legacy"`
	Successor                DaemonIdentity `json:"successor"`
	LegacyPID                int            `json:"legacy_pid,omitempty"`
	LegacyProcessStart       string         `json:"legacy_process_start,omitempty"`
	LegacyShutdownRequested  bool           `json:"legacy_shutdown_requested,omitempty"`
	LegacyShutdownDispatched bool           `json:"legacy_shutdown_dispatched,omitempty"`
	// Acknowledgement proves clean shutdown command completion, never daemon closure.
	LegacyShutdownAcknowledged bool              `json:"legacy_shutdown_acknowledged,omitempty"`
	LegacyStopped              bool              `json:"legacy_stopped,omitempty"`
	UpgradeGeneration          UpgradeGeneration `json:"upgrade_generation,omitzero"`
}

// UpgradeRoot is the immutable root observation bound by the bootstrap owner.
// It is correlation data; control revalidates it against the native root.
type UpgradeRoot struct {
	Path   string `json:"path"`
	ID     string `json:"id"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

type UpgradeOperationID string

type UpgradeMode string

const (
	UpgradeReactive          UpgradeMode = "reactive"
	UpgradeOperationRequired UpgradeMode = "operation_required"
)

// UpgradeGeneration links progress to one authorized bootstrap drain. Target
// is the config projection of Drain's pinned pair, never a second selection.
// AcceptedStart survives an unknown Start/birth result and forbids relaunch.
type UpgradeGeneration struct {
	OperationID           UpgradeOperationID `json:"operation_id"`
	Root                  UpgradeRoot        `json:"root"`
	Predecessor           task.SupervisorRef `json:"predecessor"`
	Mode                  UpgradeMode        `json:"mode"`
	Target                task.SupervisorRef `json:"target"`
	LegacyBootID          string             `json:"legacy_boot_id"`
	AcceptedStart         bool               `json:"accepted_start,omitempty"`
	SuccessorBootID       string             `json:"successor_boot_id,omitempty"`
	SuccessorPID          int                `json:"successor_pid,omitempty"`
	SuccessorProcessStart string             `json:"successor_process_start,omitempty"`
	Completed             bool               `json:"completed,omitempty"`
}

type History struct {
	SchemaVersion int      `json:"schema_version"`
	Migrations    []Record `json:"migrations"`
}
