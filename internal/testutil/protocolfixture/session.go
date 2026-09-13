package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func sessionError(err error) int {
	fmt.Fprintf(os.Stderr, "session: %v\n", err)
	if errors.Is(err, task.ErrSessionBusy) || errors.Is(err, task.ErrLockBusy) {
		return 20
	}
	return 1
}

func runSession(args []string) int {
	fs := flag.NewFlagSet("session", flag.ContinueOnError)
	root := fs.String("root", "", "store root")
	id := fs.String("task-id", "", "task ID")
	action := fs.String("action", "claim", "claim or release")
	provider := fs.String("provider", config.ProviderFixture, "provider identity")
	conversation := fs.String("conv-id", "conv-test", "explicit fake conversation")
	evidence := fs.String("evidence-digest", "", "verified terminal evidence digest")
	checkpoints := checkpointFlags(fs)
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *root == "" || *id == "" {
		return 2
	}
	s, err := taskdir.OpenStore(*root)
	if err != nil {
		return sessionError(err)
	}
	defer closeStore(s)
	s.SetFaultInjector(checkpoints)
	td, err := s.OpenTask(*id)
	if err != nil {
		return sessionError(err)
	}
	defer closeTask(td)
	if err = checkpoints.point("before-session-" + *action); err != nil {
		return sessionError(err)
	}
	switch *action {
	case "claim":
		err = td.ClaimSession(*provider, *conversation)
	case "release":
		err = td.ReleaseSession(*provider, *conversation, *evidence)
	default:
		return 2
	}
	if err != nil {
		return sessionError(err)
	}
	if err = checkpoints.point("after-session-" + *action); err != nil {
		return sessionError(err)
	}
	return printJSON(map[string]any{"status": "ok", "action": *action, "task_id": *id})
}
