package supervisor

import (
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	stateRunnerName                           = "delegate-run"
	stateTerminalEventRunnerName              = "delegate-terminal-event-v1"
	stateTerminalEventRunnerRecordName        = stateTerminalEventRunnerName + ".json"
	stateTerminalEventRunnerRevision   uint64 = 2
	managedRunnerMain                         = "github.com/hishamkaram/delegation-layer/cmd/delegate-run"
	stateSupervisorPairRecordName             = "pueue-pair.json"
	stateSupervisorPairDirectoryPrefix        = "pueue-pair-"
	runnerRepairReceiptRevision               = "managed-runner-v1"
)

type stateSupervisorPairRecord struct {
	SchemaVersion int    `json:"schema_version"`
	PairSHA256    string `json:"pair_sha256"`
	ClientSHA256  string `json:"client_sha256"`
	DaemonSHA256  string `json:"daemon_sha256"`
}

type stateTerminalEventRunnerRecord struct {
	SchemaVersion          int    `json:"schema_version"`
	ProtocolPath           string `json:"protocol_path"`
	ImplementationRevision uint64 `json:"implementation_revision"`
	BuildSHA256            string `json:"build_sha256"`
}

// runnerRepairReceipt records one fully completed queued-runner repair for a
// precise private supervisor generation and content-addressed target runner.
// Records are immutable: a changed daemon/configuration generation or runner
// gets a different receipt name and therefore requires a fresh repair.
type runnerRepairReceipt struct {
	SchemaVersion    int                `json:"schema_version"`
	Revision         string             `json:"revision"`
	Supervisor       task.SupervisorRef `json:"supervisor"`
	RunnerExecutable string             `json:"runner_executable"`
	RunnerSHA256     string             `json:"runner_sha256"`
}

// RunnerRepairTarget is an immutable plan produced by PlanQueuedRunnerRepairTarget.
// Only this package interprets its source, content address and digest.
type RunnerRepairTarget struct {
	source     string
	executable string
	sha256     string
}
