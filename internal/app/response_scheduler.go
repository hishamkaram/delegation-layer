package app

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SchedulerResponse is an observation of the host, separate from saved
// definitions and from the outcomes of tasks it admits.
type SchedulerResponse struct {
	State        string     `json:"state"`
	Epoch        string     `json:"epoch,omitempty"`
	Attempt      uint64     `json:"attempt,omitempty"`
	Ready        bool       `json:"ready"`
	Fault        bool       `json:"fault,omitempty"`
	NextRetryAt  *time.Time `json:"next_retry_at,omitempty"`
	LastError    string     `json:"last_error,omitempty"`
	NextAction   string     `json:"next_action,omitempty"`
	ServeCommand []string   `json:"serve_command,omitempty"`
	StopAccepted *bool      `json:"stop_accepted,omitempty"`
}

func writeHumanSchedulerResponse(output *strings.Builder, observed *SchedulerResponse) {
	if observed == nil {
		return
	}
	fmt.Fprintf(output, "scheduler state=%s ready=%t", observed.State, observed.Ready)
	if observed.NextAction != "" {
		fmt.Fprintf(output, " next_action=%s", observed.NextAction)
	}
	if observed.NextRetryAt != nil {
		fmt.Fprintf(output, " next_retry_at=%s", observed.NextRetryAt.Format(time.RFC3339Nano))
	}
	output.WriteByte('\n')
	if len(observed.ServeCommand) > 0 {
		// JSON argv preserves spaces and shell metacharacters without pretending
		// that an unquoted command string is safe to paste into a shell.
		command, err := json.Marshal(observed.ServeCommand)
		if err != nil {
			fmt.Fprintln(output, "start the scheduler from an independent terminal")
			return
		}
		fmt.Fprintf(output, "run independently (argv): %s\n", command)
	}
}
