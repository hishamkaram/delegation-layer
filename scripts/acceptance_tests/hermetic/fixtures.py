"""Finite retry observations for the exact natural-wait ordering test."""

class RetryProcess:
    def __init__(self, response, events):
        self.events = events
        self.response = response

    def wait(self, *_args, **_kwargs):
        self.events.append("initial-dispatch-wait")

    def json(self):
        return self.response


class RetryCase:
    def __init__(self, root, events, task_id, supervisor_records, add, first_enqueue, retry_enqueue):
        self.events = events
        self.add = add
        self.first_enqueue = first_enqueue
        self.retry_enqueue = retry_enqueue
        self.base = root
        self.root = root
        self.case_id = "A06"
        self.task_id = task_id
        self.root_id = None
        self.supervisor_records = supervisor_records
        self.enqueue_release = root / "enqueue.release"

    def update_fake(self, verb, **_changes):
        self.updated_verb = verb

    def start_dispatch(self):
        return RetryProcess({
            "failure": {
                "code": "task_launch_uncertain",
                "stage": "enqueue-task",
                "next_action": "check_status",
            }
        }, self.events)

    def wait_entry(self, verb):
        return self.add if verb == "add" else self.first_enqueue

    def set_status(self, state):
        self.events.append("status:" + state)

    def release_add(self):
        self.events.append("release-add")

    def wait_completion(self, entry):
        self.events.append("helper:" + entry["invocation_id"])
        return {"exit_code": 1 if entry is self.first_enqueue else 0}

    def observe_helper_completion_if_present(self, entry, timeout=1.0):
        _ = timeout
        completion = self.wait_completion(entry)
        self.wait_supervisor_completion(entry)
        return completion

    def wait_supervisor_completion(self, entry):
        self.events.append("observer:" + entry["invocation_id"])

    def release(self, _path):
        self.events.append("release-enqueue")

    def retry_dispatch(self, **_kwargs):
        self.events.append("retry-dispatch")
        return RetryProcess({"admission": "admitted"}, self.events), {"admission": "admitted"}

    def wait_entry_after(self, verb, previous_invocation_ids):
        self.assertion = (verb, set(previous_invocation_ids))
        self.events.append("retry-entry")
        return self.retry_enqueue

    def counts(self):
        self.events.append("counts")
        return {"A": 1, "Q": 2, "S": 0, "E": 0, "K": 0, "M": 0}


def retry_case(root):
    task_id = "b" * 32
    root_id = "a" * 32
    supervisor_records = root / "supervisor-records"
    supervisor_records.mkdir()
    task_dir = root / "tasks" / task_id
    task_dir.mkdir(parents=True)
    (task_dir / "supervisor.ref.json").write_text("{}\n")
    events = []
    add = {
        "verb": "add",
        "invocation_id": "add-invocation",
        "pid": 701,
        "argv": ["--label", "delegate:" + root_id + ":" + task_id],
    }
    first_enqueue = {
        "verb": "enqueue",
        "invocation_id": "enqueue-first",
        "pid": 702,
        "argv": ["-c", "config", "enqueue", "7"],
        "executable": "/pueue",
    }
    retry_enqueue = {
        "verb": "enqueue",
        "invocation_id": "enqueue-retry",
        "pid": 703,
        "argv": ["-c", "config", "enqueue", "7"],
        "executable": "/pueue",
    }
    case = RetryCase(root, events, task_id, supervisor_records, add, first_enqueue, retry_enqueue)
    return case, events
