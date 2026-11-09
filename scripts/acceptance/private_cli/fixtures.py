"""Private CLI fixtures boundary."""

import shlex
import sys
from pathlib import Path


PI_FIXTURE = r"""#!/bin/sh
set -eu

gate_path=__GATE_PATH_JSON__
waiter_directory=__WAITER_DIRECTORY_JSON__
lease_nonce=__LEASE_NONCE_JSON__
if [ "${1-}" = "--list-models" ]; then
  printf '%s\n' "provider model context max-out thinking images" "openai gpt-4.1 128000 8192 enabled enabled"
  exit 0
fi
if [ "${1-}" = "--version" ]; then
  printf '%s\n' "pi fixture 1.0.0"
  exit 0
fi
help_only=1
for argument in "$@"; do
  if [ "$argument" = "--help" ]; then
    printf '%s\n' "Options: --mode --tools --model --thinking --session --session-dir --no-extensions --list-models"
    exit 0
  fi
  case "$argument" in
    --no-extensions|--offline) ;;
    *) help_only=0 ;;
  esac
done
if [ "$help_only" = 1 ]; then
  printf '%s\n' "Options: --mode --tools --model --thinking --session --session-dir --no-extensions --list-models"
  exit 0
fi

prompt=$(/bin/cat)
case "$prompt" in
  *DELEGATE_NESTED_SURVIVAL*)
    exec __PYTHON_EXECUTABLE__ -c __NESTED_PROGRAM__ "$prompt"
    ;;
  *DELEGATE_SCHEDULER_CONTROLLED_FAILURE*)
    if [ -f "$gate_path.scheduler-fail" ]; then
      printf '%s\n' 'controlled scheduler fixture failure' >&2
      exit 17
    fi
    ;;
  *DELEGATE_PRIVATE_SILENCE_WAIT*)
    exec __PYTHON_EXECUTABLE__ -c __PI_WAIT_PROGRAM__ \
      "$gate_path" "$waiter_directory" "$lease_nonce" session
    ;;
  *DELEGATE_PRIVATE_UPGRADE_WAIT*)
    exec __PYTHON_EXECUTABLE__ -c __PI_WAIT_PROGRAM__ \
      "$gate_path" "$waiter_directory" "$lease_nonce"
    ;;
esac
/bin/cat <<'EOF'
__SESSION_EVENT__
{"type":"agent_start"}
{"type":"turn_start"}
{"type":"message_start","message":{"role":"assistant","content":[]}}
{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"private supervisor acceptance passed"}],"stopReason":"stop"}}
{"type":"turn_end","message":{"role":"assistant","content":[{"type":"text","text":"private supervisor acceptance passed"}],"stopReason":"stop"},"toolResults":[]}
{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"private supervisor acceptance passed"}],"stopReason":"stop"}]}
EOF
"""


PI_WAIT_PROGRAM = r"""import fcntl
import os
import signal
import stat
import sys

gate_path, waiter_directory, lease_nonce = sys.argv[1:4]
include_session = __INCLUDE_SESSION__
emit_session = sys.argv[4:] == ["session"]
lease_fifo = os.path.join(waiter_directory, lease_nonce + "-" + str(os.getpid()) + ".fifo")
descriptor = -1
gate_descriptor = -1

def stop(signum, _frame):
    raise SystemExit(128 + signum)

for handled in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM):
    signal.signal(handled, stop)

try:
    os.mkfifo(lease_fifo, 0o600)
    # Linux and Darwin both allow one process to keep a FIFO's read and write
    # ends open. This preserves the live-writer lease while also letting failed
    # orchestration release the fixture before a lease reader has attached.
    flags = os.O_RDWR | os.O_NONBLOCK
    flags |= getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    descriptor = os.open(lease_fifo, flags)
    handshake = lease_nonce + "\t" + str(os.getpid()) + "\n"
    os.write(descriptor, handshake.encode())
    if include_session and emit_session:
        sys.stdout.write('{"type":"session","id":"00000000-0000-4000-8000-000000000001"}\n')
        sys.stdout.flush()
    gate_flags = os.O_RDONLY
    gate_flags |= getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    gate_descriptor = os.open(gate_path, gate_flags)
    gate_identity = os.fstat(gate_descriptor)
    if not stat.S_ISREG(gate_identity.st_mode) or gate_identity.st_uid != os.getuid():
        raise RuntimeError("capacity fixture gate is unsafe")
    fcntl.flock(gate_descriptor, fcntl.LOCK_SH)
    fcntl.flock(gate_descriptor, fcntl.LOCK_UN)
    if include_session and not emit_session:
        sys.stdout.write('{"type":"session","id":"00000000-0000-4000-8000-000000000001"}\n')
    sys.stdout.write('''{"type":"agent_start"}\n{"type":"turn_start"}\n{"type":"message_start","message":{"role":"assistant","content":[]}}\n{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"private supervisor acceptance passed"}],"stopReason":"stop"}}\n{"type":"turn_end","message":{"role":"assistant","content":[{"type":"text","text":"private supervisor acceptance passed"}],"stopReason":"stop"},"toolResults":[]}\n{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"private supervisor acceptance passed"}],"stopReason":"stop"}]}\n''')
finally:
    if gate_descriptor >= 0:
        os.close(gate_descriptor)
    if descriptor >= 0:
        os.close(descriptor)
    try:
        os.unlink(lease_fifo)
    except FileNotFoundError:
        pass
"""


def install_pi_fixture(install_dir, gate_file, waiter_directory, lease_nonce, *, emit_session: bool = True):
    pi = install_dir / "pi"
    script = PI_FIXTURE.replace("__GATE_PATH_JSON__", shlex.quote(str(gate_file)))
    script = script.replace("__WAITER_DIRECTORY_JSON__", shlex.quote(str(waiter_directory)))
    script = script.replace("__LEASE_NONCE_JSON__", shlex.quote(lease_nonce))
    script = script.replace("__PYTHON_EXECUTABLE__", shlex.quote(sys.executable))
    # Workspace fixtures own their early session header and suppress ordinary/wait
    # headers through this boundary, rather than rewriting generated program text.
    session_event = '{"type":"session","id":"00000000-0000-4000-8000-000000000001"}\n' if emit_session else ""
    script = script.replace("__SESSION_EVENT__\n", session_event)
    wait_program = PI_WAIT_PROGRAM.replace("__INCLUDE_SESSION__", repr(emit_session))
    script = script.replace("__PI_WAIT_PROGRAM__", shlex.quote(wait_program))
    repository = str(Path(__file__).resolve().parents[3])
    nested_program = ("import sys; sys.path.insert(0, " + repr(repository)
                      + "); from scripts.acceptance.private_cli.nested.fixture import main; main(sys.argv[1])")
    script = script.replace("__NESTED_PROGRAM__", shlex.quote(nested_program))
    pi.write_text(script)
    pi.chmod(0o700)
