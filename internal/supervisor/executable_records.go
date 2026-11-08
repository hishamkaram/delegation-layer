package supervisor

import (
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	runnerRepairReceiptRevision = "managed-runner-v1"
)

// runnerRepairReceipt records one fully completed queued-runner repair for a
// precise private supervisor generation and selected installed runner.
// Records are immutable: a changed daemon/configuration generation or runner
// gets a different receipt name and therefore requires a fresh repair.
type runnerRepairReceipt struct {
	SchemaVersion      int                     `json:"schema_version"`
	Revision           string                  `json:"revision"`
	Supervisor         task.SupervisorRef      `json:"supervisor"`
	RunnerExecutable   string                  `json:"runner_executable"`
	RunnerSHA256       string                  `json:"runner_sha256,omitempty"`
	ExecutableIdentity task.ExecutableIdentity `json:"executable_identity,omitempty"`
}

// RunnerRepairTarget is an immutable plan produced by PlanQueuedRunnerRepairTarget.
// Only this package selects the installed role; the value grants no launch authority.
type RunnerRepairTarget struct {
	executable string
}
