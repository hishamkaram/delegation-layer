package control

import (
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// privateTestObservationTimeout keeps normal success-path fixtures independent
// from race-instrumented process scheduling. Tests that prove a bounded timeout
// set their own smaller observation window explicitly.
const privateTestObservationTimeout = 10 * time.Second

type semanticallyChangedPrivateMigrationFixture struct {
	stateRoot    string
	configPath   string
	identityPath string
	legacy       *Client
	current      *Client
}

type completedPrivateSuccessorTestFixture struct {
	base       string
	stateRoot  string
	clientPath string
	daemonPath string
	saved      task.SupervisorRef
	successor  task.SupervisorRef
	options    Options
}

type completedPrivateLegacyTestFixture struct {
	base       string
	stateRoot  string
	clientPath string
	daemonPath string
	saved      task.SupervisorRef
	options    Options
}

type privateSupervisorTestFixture struct {
	stateRoot  string
	clientPath string
	daemonPath string
}

const privateClientFixture = `#!/bin/sh
set -eu
config=
while [ "$#" -gt 0 ]; do
	if [ "${PRIVATE_CONTROL_MARKER:-}" != "" ]; then printf '%s\n' "$1" >> "$PRIVATE_CONTROL_MARKER"; fi
  case "$1" in
    -c) config=$2; shift 2 ;;
	    --version)
	      if [ "${PRIVATE_VERSION_MARKER:-}" != "" ]; then printf '%s\n' started >> "$PRIVATE_VERSION_MARKER"; fi
	      if [ "${PRIVATE_VERSION_DELAY:-}" != "" ]; then sleep "$PRIVATE_VERSION_DELAY"; elif [ "${PRIVATE_DELAY_VERSION:-}" = "1" ]; then sleep 0.08; fi
	      if [ "${PRIVATE_VERSION_HOLD:-}" != "" ]; then while [ ! -f "$PRIVATE_VERSION_HOLD" ]; do sleep 0.01; done; fi
	      case "$config" in
	        */.migration-pueue-*) if [ "${PRIVATE_VERSION_RELEASE:-}" != "" ]; then while [ ! -f "$PRIVATE_VERSION_RELEASE" ]; do sleep 0.01; done; fi ;;
	      esac
	      if [ "${PRIVATE_VERSION_MARKER:-}" != "" ]; then printf '%s\n' completed >> "$PRIVATE_VERSION_MARKER"; fi
	      if [ "${PRIVATE_VERSION_FILE:-}" != "" ]; then cat "$PRIVATE_VERSION_FILE"; exit 0; fi
      printf '%s\n' 'pueue 99.7.3'; exit 0 ;;
    parallel)
      [ "$2" = "0" ] || exit 64
      touch "$(dirname "$config")/parallel-unlimited"
      exit 0 ;;
	    group)
	      marker="$(dirname "$config")/ready"
	      [ -f "$marker" ] || exit 1
	      parallel=1
	      [ -f "$(dirname "$config")/parallel-unlimited" ] && parallel=0
	      printf '{"default":{"status":"Running","parallel_tasks":%s}}\n' "$parallel"
	      exit 0 ;;
    shutdown)
      rm -f "$(dirname "$config")/ready"
      rm -f "$(dirname "$config")/run/pueue.pid"
      if [ "${PRIVATE_SHUTDOWN_MARKER:-}" != "" ]; then : > "$PRIVATE_SHUTDOWN_MARKER"; fi
      exit 0 ;;
	clean)
	      [ "$2" = "--successful-only" ] || exit 64
	      if [ "${PRIVATE_CLEAN_MARKER:-}" != "" ]; then : > "$PRIVATE_CLEAN_MARKER"; fi
	      if [ "${PRIVATE_CLEAN_ARGS_MARKER:-}" != "" ]; then printf '%s\n' "$@" | tail -n +2 > "$PRIVATE_CLEAN_ARGS_MARKER"; fi
      rm -f "$(dirname "$config")/test-status.json"
      exit 0 ;;
    status)
      if [ "${PRIVATE_STATUS_DELAY:-}" != "" ]; then sleep "$PRIVATE_STATUS_DELAY"; fi
      if [ "${PRIVATE_STATUS_STARTED:-}" != "" ]; then
        : > "$PRIVATE_STATUS_STARTED"
        while [ ! -f "$PRIVATE_STATUS_RELEASE" ]; do sleep 0.01; done
      fi
      marker="$(dirname "$config")/ready"
      [ -f "$marker" ] || exit 1
      status_file="$(dirname "$config")/test-status.json"
      if [ -f "$status_file" ]; then
        cat "$status_file"
        exit 0
      fi
      if [ "${PRIVATE_INVALID_STATUS:-}" = "1" ]; then
        printf '%s\n' '{"tasks":{}}'
        exit 0
      fi
      printf '%s\n' '{"tasks":{},"groups":{"default":{"status":"Running","parallel_tasks":0}}}'
      exit 0
      ;;
    kill|remove)
      if [ "$1" = "remove" ] && [ "${PRIVATE_REMOVE_STARTED:-}" != "" ]; then
        : > "$PRIVATE_REMOVE_STARTED"
        while [ ! -f "$PRIVATE_REMOVE_RELEASE" ]; do sleep 0.01; done
      fi
      if [ -f "$(dirname "$config")/test-record-kill" ]; then shift; printf '%s\n' "$@" >> "$(dirname "$config")/test-kill-task-id"; fi
      exit 0 ;;
    *) exit 64 ;;
  esac
done
exit 64
`

const privateDaemonFixture = `#!/bin/sh
set -eu
config=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -c) config=$2; shift 2 ;;
    *) shift ;;
  esac
done
identity="$(dirname "$config")/daemon.identity.json"
if [ -f "$identity" ]; then printf '%s\n' present; else printf '%s\n' absent; fi > "$(dirname "$config")/daemon-identity-at-start"
printf '%s\n' started >> "$(dirname "$config")/daemon-started"
mkdir -p "$(dirname "$config")/run"
printf '%s\n' "$$" > "$(dirname "$config")/run/pueue.pid"
touch "$(dirname "$config")/ready"
`
