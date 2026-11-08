"""Private CLI queue policy boundary."""


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
