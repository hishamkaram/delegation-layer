package control

const (
	privateTerminalEventRunnerName = "delegate-terminal-event-v1"
	privateTerminalEventLockName   = "terminal-events.lock"
	privateTerminalEventTicketDir  = "terminal-event-tickets"
	privateTerminalEventRecordDir  = "terminal-events"
)

// terminalEventTicket classifies one live numeric ID for callback publication.
// Cleanup clears it only after removing the authorized terminal row. Every
// new submission retains the ticket and resets the reused ID's event before
// enqueue because the callback surface has no per-submission token.
type terminalEventTicket struct {
	SchemaVersion int   `json:"schema_version"`
	NumericTaskID int64 `json:"numeric_task_id"`
}
