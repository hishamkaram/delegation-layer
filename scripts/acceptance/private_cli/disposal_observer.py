"""Public status observation of explicit created tasks before supervisor disposal.

The observation gives the canonical recovery path its documented next-observer
turn. It is never cleanup authority: callers reread the canonical closure and
recorded pool identity after it, and its exit status proves nothing.
"""

import subprocess
import uuid

from scripts.acceptance.evidence.validation import _task_identity
from scripts.acceptance.private_cli.commands import record_invocation_error
from scripts.acceptance.private_cli.commands import run_json


DISPOSAL_OBSERVATION_PHASE = "managed-runner-observation"


def public_status_observer(delegate, state, env, *, capture_root):
    """Bind the selected Delegate CLI, owned root, environment and capture owner to one per-task observer.

    The returned callback runs one ``status --root STATE TASK --json`` bounded
    by the caller's absolute monotonic ``deadline``, which the capture owner
    applies to its launch lock, launch and wait. It returns the root identity
    the command reported. A failed command, a different task, or a missing
    root identity raises. A timed-out command re-raises its own timeout after
    a failure-only receipt beside its captures maps the task and argv to the
    direct CLI PID and capture files.
    """

    def observe(task_id, deadline):
        args = ["--root", str(state), "status", task_id, "--json"]
        try:
            response = run_json(
                delegate,
                env,
                args,
                timeout=None,
                phase=DISPOSAL_OBSERVATION_PHASE,
                capture_root=capture_root,
                deadline=deadline,
            )
        except subprocess.TimeoutExpired as timeout:
            _record_timeout(capture_root, task_id, args, timeout)
            raise
        if response.get("task_id") != task_id:
            raise RuntimeError("disposal status observation returned a different task")
        root_id = response.get("root_id")
        if not isinstance(root_id, str) or not root_id:
            raise RuntimeError("disposal status observation omitted its root identity")
        return root_id

    return observe


def _record_timeout(capture_root, task_id, args, timeout):
    """Write a unique timeout receipt for task_id in the owned capture root and note it on timeout.

    A receipt that cannot be named or written is noted with its cause, and
    with its destination once one was allocated; the original timeout is what
    the caller re-raises either way.
    """
    receipt = None
    try:
        if not _task_identity(task_id):
            raise ValueError("task id is not 32 lowercase hex characters")
        receipt = capture_root.directory / ("disposal-status-%s-%s.json" % (task_id, uuid.uuid4().hex))
        capture_root.assert_live()
        record_invocation_error(receipt, args, timeout, None)
    except Exception as failure:
        destination = " for task %r" % (task_id,) if receipt is None else " to %s" % receipt
        timeout.add_note(
            "disposal status timeout receipt was not written%s: %s: %s" % (destination, type(failure).__name__, failure)
        )
        return
    timeout.add_note("disposal status timeout receipt: %s" % receipt)
