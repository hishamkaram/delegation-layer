package migration

// DaemonIdentity is the immutable executable and logical endpoint identity
// saved while a private supervisor generation changes.
type DaemonIdentity struct {
	Executable           string `json:"executable"`
	SHA256               string `json:"sha256"`
	ConfigPath           string `json:"config_path,omitempty"`
	ConfigDigest         string `json:"config_digest,omitempty"`
	ResolvedConfigSHA256 string `json:"resolved_config_sha256,omitempty"`
	Endpoint             string `json:"endpoint,omitempty"`
}

type Record struct {
	SchemaVersion            int            `json:"schema_version"`
	Legacy                   DaemonIdentity `json:"legacy"`
	Successor                DaemonIdentity `json:"successor"`
	LegacyPID                int            `json:"legacy_pid,omitempty"`
	LegacyProcessStart       string         `json:"legacy_process_start,omitempty"`
	LegacyShutdownRequested  bool           `json:"legacy_shutdown_requested,omitempty"`
	LegacyShutdownDispatched bool           `json:"legacy_shutdown_dispatched,omitempty"`
	LegacyStopped            bool           `json:"legacy_stopped,omitempty"`
}

type History struct {
	SchemaVersion int      `json:"schema_version"`
	Migrations    []Record `json:"migrations"`
}
