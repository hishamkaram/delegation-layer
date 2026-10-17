// Package protocol contains pure Pueue protocol values and parsers.
package protocol

// CommandEvent reports entry, successful Start, or actual completion. Entry
// does not establish Start success; a completed negative exit remains unknown.
type CommandEvent struct {
	CommandID string   `json:"command_id"`
	Stage     string   `json:"stage"`
	Argv      []string `json:"argv"`
	PID       int      `json:"pid"`
	ExitCode  int      `json:"exit_code"`
}

// Identity supplies immutable task identity and an optional already-bound job ID.
type Identity struct {
	RootID        string
	TaskID        string
	SpecSHA256    string
	MetaSHA256    string
	NumericTaskID *int64
}

func (i Identity) Label() string { return "delegate:" + i.RootID + ":" + i.TaskID }

// Launch transports identity only. The brief never appears in supervisor argv.
type Launch struct {
	RunnerExecutable   string
	RootPath           string
	Supervised         bool
	WorkspaceOperation bool
}
type State string

const (
	StateUnknown State = "unknown"
	StateQueued  State = "queued"
	StateStashed State = "stashed"
	StateRunning State = "running"
	StateEnded   State = "ended"
)
