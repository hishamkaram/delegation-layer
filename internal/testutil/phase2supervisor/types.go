// Package phase2supervisor contains the finite compiled supervisor used by
// Phase 2 acceptance tests. It records each invocation and never starts or
// signals another process.
package phase2supervisor

import (
	"errors"
	"time"
)

const (
	SchemaVersion  = 1
	DefaultVersion = "4.0.4"
	ConfigName     = "phase2-supervisor.json"

	MaxConfigBytes  = 64 * 1024
	MaxOutputBytes  = 32 * 1024 * 1024
	MaxReceiptBytes = 1024 * 1024
	MaxRunDuration  = 10 * time.Second
)

var (
	ErrConfigTooBig     = errors.New("phase2 supervisor configuration exceeds 64 KiB")
	ErrInvalidConfig    = errors.New("invalid phase2 supervisor configuration")
	ErrInvalidArguments = errors.New("invalid phase2 supervisor arguments")
	ErrOutputTooBig     = errors.New("phase2 supervisor output exceeds 32 MiB")
	ErrReleaseTimeout   = errors.New("phase2 supervisor release rendezvous timed out")
)

// Config is the complete explicit control surface for one finite fake client.
// ConfigPath names the canonical phase2-supervisor.json beside the client;
// ExpectedConfigPath names the separate production pueue config supplied to
// every client invocation.
type Config struct {
	SchemaVersion      int        `json:"schema_version"`
	ConfigPath         string     `json:"config_path"`
	ExpectedConfigPath string     `json:"expected_config_path"`
	ArtifactDir        string     `json:"artifact_dir"`
	StatusPath         string     `json:"status_path"`
	Version            string     `json:"version"`
	AddID              int64      `json:"add_id"`
	Status             VerbConfig `json:"status"`
	Add                VerbConfig `json:"add"`
	Kill               VerbConfig `json:"kill"`
	Remove             VerbConfig `json:"remove"`
}

// VerbConfig controls one finite command invocation. Delay is applied before
// the optional release rendezvous. An absent release file causes cooperative
// self-exit at MaxRunDuration; it is never signalled or killed by this helper.
type VerbConfig struct {
	Delay       time.Duration `json:"delay"`
	ReleasePath string        `json:"release_path,omitempty"`
	ExitCode    int           `json:"exit_code"`
	StdoutPath  string        `json:"stdout_path,omitempty"`
	StderrPath  string        `json:"stderr_path,omitempty"`
}

// EntryReceipt records the immutable invocation identity before command
// validation or execution.
type EntryReceipt struct {
	SchemaVersion     int      `json:"schema_version"`
	InvocationID      string   `json:"invocation_id"`
	PID               int      `json:"pid"`
	Executable        string   `json:"executable"`
	FixtureConfigPath string   `json:"fixture_config_path"`
	ConfigPath        string   `json:"config_path"`
	Argv              []string `json:"argv"`
	Verb              string   `json:"verb"`
	StartedAt         string   `json:"started_at"`
}

// CompletionReceipt records the result after natural finite completion. Output
// bytes are represented by exact lengths and SHA-256 digests; raw output stays
// on the command streams or in the explicitly configured input files.
type CompletionReceipt struct {
	SchemaVersion     int      `json:"schema_version"`
	InvocationID      string   `json:"invocation_id"`
	PID               int      `json:"pid"`
	Executable        string   `json:"executable"`
	FixtureConfigPath string   `json:"fixture_config_path"`
	ConfigPath        string   `json:"config_path"`
	Argv              []string `json:"argv"`
	Verb              string   `json:"verb"`
	CompletedAt       string   `json:"completed_at"`
	ExitCode          int      `json:"exit_code"`
	StdoutBytes       int64    `json:"stdout_bytes"`
	StdoutSHA256      string   `json:"stdout_sha256"`
	StderrBytes       int64    `json:"stderr_bytes"`
	StderrSHA256      string   `json:"stderr_sha256"`
	Error             string   `json:"error,omitempty"`
}

// StatusJob is the small valid 4.0.4 fixture input supported by
// BuildStatusFixture. It deliberately contains no production parser logic.
type StatusJob struct {
	ID    int64
	Label string
	State string
}
