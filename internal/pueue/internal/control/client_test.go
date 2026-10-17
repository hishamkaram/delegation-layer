package control

import (
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type fakeSupervisor struct {
	client         *Client
	executable     string
	configPath     string
	statusPath     string
	logPath        string
	environment    []string
	cleanupPending *Pending
}

type submitStartFailure struct {
	executable string
	hidden     string
	moved      bool
	failed     bool
	err        error
}

const fakeSupervisorScript = `#!/bin/sh
set -eu
{
  printf 'argc=%s\n' "$#"
  for arg in "$@"; do printf 'arg=%s\n' "$arg"; done
  printf '%s\n' '---'
} >> "$FAKE_LOG"
mode=${FAKE_MODE:-ok}
case "${3-}" in
  --version)
    if [ "${FAKE_VERSION_DELAY:-}" != "" ]; then sleep "$FAKE_VERSION_DELAY"; fi
    printf '%s\n' "${FAKE_VERSION:-pueue 4.0.4}"
    ;;
  status)
    if [ "${FAKE_STATUS_GATE:-}" != "" ]; then while [ ! -f "$FAKE_STATUS_GATE" ]; do sleep 0.01; done; elif [ "${FAKE_STATUS_DELAY:-}" != "" ]; then sleep "$FAKE_STATUS_DELAY"; elif [ "$mode" = "delay-status" ]; then sleep 0.08; elif [ "$mode" = "delay-status-fail" ]; then sleep 0.08; [ -f "${FAKE_READY:?}" ] || exit 1; fi
    if [ "${FAKE_READY:-}" != "" ] && [ ! -f "$FAKE_READY" ]; then exit 1; fi
    if [ "${FAKE_SHUTDOWN_MARKER:-}" != "" ] && [ -f "$FAKE_SHUTDOWN_MARKER" ]; then exit 1; fi
    if [ "${5-}" != "" ] && [ "${FAKE_FILTERED_STATUS:-}" != "" ]; then cat "$FAKE_FILTERED_STATUS"; else cat "$FAKE_STATUS"; fi
    ;;
  group)
    printf '%s\n' '{"default":{"status":"Running","parallel_tasks":0}}'
    ;;
  add)
    if [ "${4-}" = "--help" ]; then
      printf '%s\n' 'Usage: pueue add [OPTIONS] <COMMAND>...'
      printf '%s\n' '  -s, --stashed    Create the task in Stashed state.'
      printf '%s\n' '  -p, --print-task-id    Only return the task id.'
    else
      if [ "${FAKE_ADD_DELAY:-}" != "" ]; then sleep "$FAKE_ADD_DELAY"; fi
      printf '%s\n' "${FAKE_ADD_ID:-7}"
    fi
    ;;
  enqueue)
    if [ "${4-}" = "--help" ]; then
      printf '%s\n' 'Usage: pueue enqueue <TASK_IDS>...'
    else
      if [ "${FAKE_ENQUEUE_DELAY:-}" != "" ]; then sleep "$FAKE_ENQUEUE_DELAY"; fi
      case "$mode" in
        enqueue-refuse) printf '%s\n' 'refused' >&2; exit 7 ;;
        *) exit 0 ;;
      esac
    fi
    ;;
  kill|remove)
    case "$mode" in
      stop-refuse) printf '%s\n' 'refused' >&2; exit 7 ;;
      stop-overflow) dd if=/dev/zero bs=1048577 count=1 2>/dev/null ;;
      *) exit 0 ;;
    esac
    ;;
  shutdown)
    if [ "${FAKE_SHUTDOWN_MARKER:-}" != "" ]; then touch "$FAKE_SHUTDOWN_MARKER"; fi
    ;;
  *)
    printf '%s\n' 'unexpected command' >&2
    exit 64
    ;;
esac
`

type taskFixture struct {
	store   *taskdir.Store
	taskDir *taskdir.TaskDir
}

type targetedStopCase struct {
	name      string
	mode      string
	state     State
	action    string
	ack       *bool
	wantError error
}
