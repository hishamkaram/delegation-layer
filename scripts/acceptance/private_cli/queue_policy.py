"""Private CLI queue policy boundary."""

from typing import NotRequired, TypedDict


class QueueWaitProjection(TypedDict):
    kind: str
    state: NotRequired[str]
    queue_id: NotRequired[int]


def queue_wait_projection(snapshot: object, label: str) -> QueueWaitProjection:
    """Project one exact label without retaining commands, environments or output."""
    if not isinstance(snapshot, dict) or not isinstance(snapshot.get("tasks"), dict):
        raise ValueError("queue observation has no task map")
    selected = None
    matches = 0
    for row in snapshot["tasks"].values():
        if isinstance(row, dict) and row.get("label") == label:
            selected = row
            matches += 1
    if matches == 0:
        return {"kind": "missing"}
    if matches > 1:
        return {"kind": "ambiguous"}
    if selected is None:
        raise ValueError("queue observation omitted its selected row")
    state = pueue_state(selected)
    projection: QueueWaitProjection = {
        "kind": "exact-label",
        "state": state if state in {"Queued", "Running", "Paused", "Stashed", "Done", "Locked"} else "unknown",
    }
    queue_id = selected.get("id")
    if type(queue_id) is int and 0 <= queue_id < 2**64:
        projection["queue_id"] = queue_id
    return projection


def task_rows_by_label(snapshot, expected_labels):
    rows_by_label = {}
    duplicate_labels = set()
    for row in snapshot.get("tasks", {}).values():
        label = row.get("label")
        if not isinstance(label, str) or label not in expected_labels:
            continue
        if label in rows_by_label:
            duplicate_labels.add(label)
        else:
            rows_by_label[label] = row
    if duplicate_labels:
        label = sorted(duplicate_labels)[0]
        raise RuntimeError("queue snapshot contains duplicate task label: " + label)
    return rows_by_label


def task_row_by_label(snapshot, label):
    return task_rows_by_label(snapshot, {label}).get(label)


def pueue_state(row):
    return next(iter(row.get("status", {})), "unknown")
