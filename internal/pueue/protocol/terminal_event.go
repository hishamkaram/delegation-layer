// Package protocol contains pure Pueue protocol values and parsers.
package protocol

// TerminalEvent is Pueue's compact, callback-originated wake-up hint for one
// classified queue row. Pueue supplies only a reusable numeric ID and an exit
// code here; Succeeded is diagnostic and never an admission proof. The
// application must reconcile the current labeled queue row before recording
// worker-exit authority.
type TerminalEvent struct {
	SchemaVersion int   `json:"schema_version"`
	NumericTaskID int64 `json:"numeric_task_id"`
	Succeeded     bool  `json:"succeeded"` // callback diagnostic; never admission authority
}
