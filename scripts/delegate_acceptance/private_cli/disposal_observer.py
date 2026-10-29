"""Public status observation of explicit created tasks before supervisor disposal.

The observation gives the canonical recovery path its documented next-observer
turn. It is never cleanup authority: callers reread the canonical closure and
recorded pool identity after it, and its exit status proves nothing.
"""

from scripts.delegate_acceptance.private_cli.commands import run_json


DISPOSAL_OBSERVATION_PHASE = "managed-runner-observation"


def public_status_observer(delegate, state, env, *, capture_root):
    """Bind the selected Delegate CLI, owned root, environment and capture owner to one per-task observer.

    The returned callback runs one ``status --root STATE TASK --json`` bounded
    by the caller's absolute monotonic ``deadline``, which the capture owner
    applies to its launch lock, launch and wait. It returns the root identity
    the command reported. A failed command, a different task, or a missing
    root identity raises.
    """

    def observe(task_id, deadline):
        response = run_json(
            delegate,
            env,
            ["--root", str(state), "status", task_id, "--json"],
            timeout=None,
            phase=DISPOSAL_OBSERVATION_PHASE,
            capture_root=capture_root,
            deadline=deadline,
        )
        if response.get("task_id") != task_id:
            raise RuntimeError("disposal status observation returned a different task")
        root_id = response.get("root_id")
        if not isinstance(root_id, str) or not root_id:
            raise RuntimeError("disposal status observation omitted its root identity")
        return root_id

    return observe
