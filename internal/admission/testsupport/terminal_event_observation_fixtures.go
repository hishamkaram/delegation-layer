package testsupport

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TerminalMaintenanceLabelFilter(argument string) bool {
	return strings.HasPrefix(argument, "label=") || strings.HasPrefix(argument, "label%=")
}

func WaitForTestFileContent(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.NewTimer(pueue.DefaultObservationTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	last := ""
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			last = string(data)
			if last == want {
				return
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s content: got=%q want=%q", path, last, want)
		case <-ticker.C:
		}
	}
}

func WriteTerminalEventInspectionSupervisorExecutables(t *testing.T, clientPath, daemonPath string) {
	t.Helper()
	supervisorfixture.WriteExecutable(t, clientPath, `#!/bin/sh
set -eu
[ "${1-}" = -c ]
base=${2%/*}
case "${3-}" in
  --version) printf '%s\n' 'pueue 4.0.4' ;;
  status)
    if [ "${FAKE_BLOCK_CLEANUP_STATUS:-}" != "" ] && [ -f "$FAKE_BLOCK_CLEANUP_STATUS" ]; then
      : > "$base/terminal-event-status-blocked"
      while [ ! -f "${FAKE_RELEASE_CLEANUP_STATUS:?}" ]; do sleep 0.01; done
    fi
    if [ -f "$base/terminal-event-status.json" ]; then
      cat "$base/terminal-event-status.json"
      exit 0
    fi
    parallel=1
    [ -f "$base/parallel-unlimited" ] && parallel=0
    printf '{"tasks":{},"groups":{"default":{"status":"Running","parallel_tasks":%s}}}\n' "$parallel"
    ;;
  group)
    parallel=1
    [ -f "$base/parallel-unlimited" ] && parallel=0
    printf '{"default":{"status":"Running","parallel_tasks":%s}}\n' "$parallel"
    ;;
  parallel) : > "$base/parallel-unlimited" ;;
  remove)
	if [ -f "$base/block-maintenance-remove" ]; then
	  : > "$base/maintenance-remove-blocked"
	  while [ ! -f "$base/release-maintenance-remove" ]; do sleep 0.01; done
	fi
    if [ "${FAKE_BLOCK_CLEANUP_REMOVE:-}" != "" ] && [ -f "$FAKE_BLOCK_CLEANUP_REMOVE" ]; then
      : > "$base/terminal-event-cleanup-blocked"
      while [ ! -f "${FAKE_RELEASE_CLEANUP_REMOVE:?}" ]; do sleep 0.01; done
    fi
	if [ "${FAKE_FAIL_CLEANUP_REMOVE_ONCE:-}" != "" ] && [ -f "$FAKE_FAIL_CLEANUP_REMOVE_ONCE" ]; then
	  rm -f "$FAKE_FAIL_CLEANUP_REMOVE_ONCE"
	  exit 75
	fi
    shift 3
    printf '%s\n' "$*" >> "$base/terminal-event-removals"
    ;;
  add)
    if [ "${4-}" = --help ]; then printf '%s\n' '--stashed --print-task-id'; else printf '%s\n' '9'; fi
    ;;
  enqueue)
    if [ "${4-}" = --help ]; then printf '%s\n' 'Usage: pueue enqueue <TASK_IDS>...'; fi
    ;;
  *) exit 64 ;;
esac
`)
	supervisorfixture.WriteExecutable(t, daemonPath, `#!/bin/sh
set -eu
[ "${1-}" = -c ]
base=${2%/*}
mkdir -p "$base/run"
printf '%s\n' "$$" > "$base/run/pueue.pid"
`)
}

func WriteTerminalEventStatus(t *testing.T, store *taskdir.Store, job pueue.Job, state map[string]any) {
	t.Helper()
	if job.Label == nil {
		t.Fatal("terminal-event status requires a job label")
	}
	parallelTasks := uint64(1)
	if job.Group == "default" {
		parallelTasks = 0
	}
	status, err := json.Marshal(map[string]any{
		"tasks": map[string]any{strconv.FormatInt(job.ID, 10): map[string]any{
			"id": job.ID, "created_at": "2026-10-10T00:00:00Z", "original_command": "inspection-worker", "command": "inspection-worker",
			"path": "/tmp", "envs": map[string]string{}, "group": job.Group, "dependencies": []int64{}, "priority": 0,
			"label": *job.Label, "status": state,
		}},
		"groups": map[string]any{job.Group: map[string]any{"status": "Running", "parallel_tasks": parallelTasks}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(store.Root, ".supervisor", "terminal-event-status.json"), status, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TerminalEventDoneState(succeeded bool) map[string]any {
	result := "Killed"
	if succeeded {
		result = "Success"
	}
	return map[string]any{"Done": map[string]any{
		"enqueued_at": "2026-10-10T00:00:00Z", "start": "2026-10-10T00:00:00Z", "end": "2026-10-10T00:00:00Z", "result": result,
	}}
}
