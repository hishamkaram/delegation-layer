package control

import (
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type inspectionTestPermit struct {
	calls                int
	err                  error
	groupRoot            string
	groupSupervisor      task.SupervisorRef
	submissionRoot       string
	submissionTask       string
	submissionSupervisor task.SupervisorRef
	stopRoot             string
	stopTask             string
	stopTarget           *int64
	stopSupervisor       task.SupervisorRef
}

type concurrentInspectionTestPermit struct {
	*inspectionTestPermit
	queueGroup string
}

type inspectionTestSubmissionPermit interface {
	InspectionSubmissionPermit
	inspectionCallCount() int
}

type inspectionStopCase struct {
	name         string
	mode         string
	state        State
	action       string
	acknowledged *bool
}

const inspectionSupervisorScript = `#!/bin/sh
set -eu
{
  printf 'argc=%s\n' "$#"
  for arg in "$@"; do printf 'arg=%s\n' "$arg"; done
  printf '%s\n' '---'
} >> "$FAKE_LOG"
case "${3-}" in
  --version)
    printf '%s\n' "${FAKE_VERSION:-pueue 4.0.4}"
    ;;
  status)
    if [ "${5-}" != "" ] && [ "${FAKE_FILTERED_STATUS:-}" != "" ]; then
      cat "$FAKE_FILTERED_STATUS"
    else
      cat "$FAKE_STATUS"
    fi
    ;;
  group)
    [ "${4-}" = add ] && [ "${5-}" = --parallel ] && [ "${6-}" = 1 ]
    ;;
  add)
    if [ "${4-}" = "--help" ]; then
      printf '%s\n' 'Usage: pueue add [OPTIONS] <COMMAND>...'
      printf '%s\n' '  -s, --stashed    Create the task in Stashed state.'
      printf '%s\n' '  -p, --print-task-id    Only return the task id.'
    else
      printf '%s\n' "${FAKE_ADD_ID:-9}"
    fi
    ;;
  enqueue)
    if [ "${4-}" = "--help" ]; then
      printf '%s\n' 'Usage: pueue enqueue <TASK_IDS>...'
    else
	  if [ "${FAKE_ENQUEUE_DELAY:-}" != "" ]; then sleep "$FAKE_ENQUEUE_DELAY"; elif [ "${FAKE_MODE:-ok}" = "delay-enqueue" ]; then sleep 0.5; fi
      case "${FAKE_MODE:-ok}" in
        enqueue-refuse) printf '%s\n' 'refused' >&2; exit 7 ;;
        *) exit 0 ;;
      esac
    fi
    ;;
  remove|kill)
    case "${FAKE_MODE:-ok}" in
      stop-refuse) printf '%s\n' 'refused' >&2; exit 7 ;;
      stop-overflow) dd if=/dev/zero bs=1048577 count=1 2>/dev/null ;;
      *) exit 0 ;;
    esac
    ;;
  *)
    printf '%s\n' 'unexpected command' >&2
    exit 64
    ;;
esac
`
