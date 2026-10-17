package supervisor

import (
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	privateOrdinarySchedulingRevision               = "unlimited-v4"
	privateOrdinarySchedulingCapabilitylessRevision = "unlimited-v3"
	privateOrdinarySchedulingDaemonRevision         = "unlimited-v2"
	privateOrdinarySchedulingLegacyRevision         = "unlimited-v1"
)

type privateOrdinarySchedulingRecord struct {
	SchemaVersion int                          `json:"schema_version"`
	Revision      string                       `json:"revision"`
	Supervisor    task.SupervisorRef           `json:"supervisor"`
	StagedLaunch  pueue.StagedLaunchCapability `json:"staged_launch"`
}

type privateOrdinarySchedulingDaemon struct {
	Executable           string `json:"executable"`
	SHA256               string `json:"sha256"`
	ConfigPath           string `json:"config_path,omitempty"`
	ConfigDigest         string `json:"config_digest,omitempty"`
	ResolvedConfigSHA256 string `json:"resolved_config_sha256,omitempty"`
	Endpoint             string `json:"endpoint,omitempty"`
}
