"""Pure pre-admission refusal and empty-queue decisions."""

from __future__ import annotations



INSPECTION_FAILURE_RESULTS = {"Killed", "Errored", "DependencyFailed", "Failed", "FailedToSpawn"}


STATIC_REFUSAL_ERROR = "unsupported-effective-config"


AUTHENTICATION_REFUSAL_ERROR = "provider authentication unavailable"


_STATIC_REFUSAL_RESPONSE_KEYS = {
    "schema_version", "command", "root_id", "task_id", "admission",
    "liveness", "publication", "error",
}


def empty_private_queue(status: dict[str, object]):
    tasks = status.get("tasks")
    groups = status.get("groups")
    if not isinstance(tasks, dict) or not isinstance(groups, dict) or tasks:
        return False
    if not groups:
        return True
    # Pueue may expose its implicit default group even when no task has
    # ever been admitted. Any named group is durable supervisor work and
    # therefore makes this static-refusal oracle inapplicable.
    if set(groups) != {"default"}:
        return False
    default = groups["default"]
    return (isinstance(default, dict) and default.get("status") == "Running" and
            type(default.get("parallel_tasks")) is int and
            default.get("parallel_tasks") == 1)


def is_static_refusal_observation(observation: dict[str, object], root: str, task: str):
    if (observation.get("receipt_written") is not True or
            observation.get("task_id") != task or
            observation.get("exit_code") != 2 or
            observation.get("natural_wait") is not True):
        return False
    response = observation.get("response")
    if not isinstance(response, dict):
        return False
    error = response.get("error")
    if not isinstance(error, str) or not error:
        return False
    static_refusal = (set(response) == _STATIC_REFUSAL_RESPONSE_KEYS and
                      (error == STATIC_REFUSAL_ERROR or
                       error.startswith(STATIC_REFUSAL_ERROR + ":")))
    auth_refusal = (set(response) == _STATIC_REFUSAL_RESPONSE_KEYS | {"status"} and
                    response.get("status") == "blocked" and
                    error.startswith(AUTHENTICATION_REFUSAL_ERROR + ": " +
                                     STATIC_REFUSAL_ERROR + ":"))
    return ((static_refusal or auth_refusal) and
            type(response.get("schema_version")) is int and
            response.get("schema_version") == 1 and response.get("command") == "dispatch" and
            response.get("root_id") == root and response.get("task_id") == task and
            response.get("admission") == "unknown" and
            response.get("liveness") == "undetermined" and
            response.get("publication") == "unknown")
