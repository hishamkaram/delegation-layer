"""Ordinary CLI compatibility while scheduler state is absent, invalid, or held.

The scheduler fixture is the scheduler facade's own test binary. In each mode
it takes the real owner lease, prepares one scheduler database state, and
reports ready only once that authority is held. Its stdin barrier stays open
until this case waits for it, so its single Wait is also its release. An older
task is collected before any scheduler state exists; its immutable tree must
stay byte-identical, and every ordinary command must keep working.
"""

from pathlib import Path
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.evidence.files import write_json
from scripts.delegate_acceptance.hermetic.chain import configure_chain_task
from scripts.delegate_acceptance.hermetic.chain import dispatch_chain_task
from scripts.delegate_acceptance.hermetic.chain import finish_runner_for
from scripts.delegate_acceptance.hermetic.chain import require_chain_terminal
from scripts.delegate_acceptance.hermetic.chain import run_command_for
from scripts.delegate_acceptance.hermetic.chain import snapshot_chain_provider
from scripts.delegate_acceptance.hermetic.chain import start_runner_for
from scripts.delegate_acceptance.hermetic.chain import wait_provider_entry_for_task
from scripts.delegate_acceptance.hermetic.contract import CASE_TIMEOUT
from scripts.delegate_acceptance.hermetic.contract import require_true
from scripts.delegate_acceptance.hermetic.evidence import require_publication
from scripts.delegate_acceptance.hermetic.files import canonical_executable
from scripts.delegate_acceptance.processes.observation import wait_until
import stat

FIXTURE_FLAG = "--scheduler-facade-helper"
# state -> (fixture mode, store class while held, store class after release)
SCHEDULER_STATES = (
    ("missing", "cli-fixture-missing", "missing", "missing"),
    ("corrupt", "cli-fixture-corrupt", "unsupported", "unsupported"),
    ("newer", "cli-fixture-newer", "unsupported", "unsupported"),
    ("locked", "cli-fixture-locked", "busy", "open"),
)
COLLECT_FIELDS = ("task_id", "publication", "outcome", "payload", "evidence_sha256")
STATUS_FIELDS = ("task_id", "publication", "outcome", "payload")


def run_s01_scheduler_compatibility(case):
    fixture = canonical_executable(case.scheduler_fixture)
    old = configure_chain_task(case=case, label="old", answer="older task answer\n",
                               session_id="scheduler-old-" + case.task_id,
                               task_value=case.task_id, brief=case.brief)
    _complete_task(case, old, first=True)
    baseline = _ordinary_reads(case, old, "baseline")
    old_tree = tree_snapshot(case.root / "tasks" / old["task_id"])
    help_text = _help(case, "baseline")
    expected = {"reads": baseline, "tree": old_tree, "help": help_text}
    receipts = [_run_state(case=case, fixture=fixture, old=old, expected=expected, spec=spec)
                for spec in SCHEDULER_STATES]
    require_true(_provider_invocations(case) == 1 + len(SCHEDULER_STATES),
                 "ordinary tasks did not each invoke the provider exactly once")
    require_true(_fixture_line(case, fixture, "lease-final", "probe") == "acquired",
                 "owner lease stayed held after every fixture released")
    destination = case.suite.output / "cases" / case.case_id
    destination.mkdir(mode=0o700, parents=True, exist_ok=True)
    write_json(destination / "scheduler-compatibility.json", {
        "schema_version": 1,
        "fixture_executable": str(fixture),
        "old_task": old["task_id"],
        "old_tree": old_tree,
        "old_reads": baseline,
        "help_sha256": sha(help_text),
        "provider_invocations": 1 + len(SCHEDULER_STATES),
        "states": receipts,
        "signals_sent": 0,
    })


def _run_state(*, case, fixture, old, expected, spec):
    state, mode, held_class, released_class = spec
    baseline, old_tree, help_text = expected["reads"], expected["tree"], expected["help"]
    database = case.root / ".scheduler" / "schedules.sqlite"
    holder = case.processes.start("scheduler-fixture-" + state,
                                  [str(fixture), FIXTURE_FLAG, mode, str(case.root)],
                                  case.base, stdin=b"")
    wait_until(lambda: holder.stdout_path.read_bytes() == b"ready\n",
               timeout=CASE_TIMEOUT, description="scheduler fixture " + state + " ready")
    held_bytes = _database_bytes(database)
    require_true(_fixture_line(case, fixture, "lease-" + state, "probe") == "busy",
                 "positive control: owner lease was not held for " + state)
    require_true(_fixture_line(case, fixture, "store-" + state, "cli-probe-store") == held_class,
                 "positive control: scheduler store class changed for " + state)
    require_true(_help(case, state) == help_text, "help changed with scheduler state " + state)
    reads = _ordinary_reads(case, old, state)
    require_true(reads == baseline, "older task reads changed with scheduler state " + state)
    record = configure_chain_task(case=case, label=state, answer="answer with scheduler " + state + "\n",
                                  session_id="scheduler-" + state + "-" + case.task_id)
    before = _provider_invocations(case)
    response = _complete_task(case, record, first=False)
    require_true(_provider_invocations(case) == before + 1,
                 "new task did not invoke the provider exactly once with " + state)
    new_reads = _ordinary_reads(case, record, state + "-new")
    require_true(new_reads["collect"]["outcome"] == response["outcome"],
                 "new task collection lost its committed outcome with " + state)
    require_true(_fixture_line(case, fixture, "lease-after-" + state, "probe") == "busy",
                 "owner lease was not held through the whole ordinary round for " + state)
    holder.wait(CASE_TIMEOUT, expected=0)
    require_true(holder.stdout_path.read_bytes() == b"ready\n",
                 "scheduler fixture reported more than readiness for " + state)
    require_true(_database_bytes(database) == held_bytes,
                 "ordinary commands changed the scheduler database for " + state)
    require_true(_fixture_line(case, fixture, "store-after-" + state, "cli-probe-store") == released_class,
                 "scheduler database changed class after release for " + state)
    require_true(tree_snapshot(case.root / "tasks" / old["task_id"]) == old_tree,
                 "older task tree changed with scheduler state " + state)
    _remove_fixture_database(database)
    return {
        "state": state,
        "fixture_mode": mode,
        "database_sha256": None if held_bytes is None else sha(held_bytes),
        "store_class_held": held_class,
        "store_class_after_release": released_class,
        "new_task": record["task_id"],
        "holder": dict(holder.result),
    }


def _complete_task(case, record, *, first):
    if first:
        case.dispatch_dynamic(status="queued")
    else:
        dispatch_chain_task(case, record)
    label = "delegate:" + case.root_id + ":" + record["task_id"]
    case.set_status("running", label=label)
    runner = start_runner_for(case, record)
    response = finish_runner_for(case, record, runner)
    require_publication(response, "committed")
    case.set_status("ended", label=label)
    wait_provider_entry_for_task(case, record)
    snapshot_chain_provider(case, record)
    require_chain_terminal(case, record, response)
    return response


def _ordinary_reads(case, record, name):
    """Run status and repeated collect; keep only their defined fields."""
    _, status = run_command_for(case, record, "status-" + name + "-" + record["label"], "status")
    _, collected = run_command_for(case, record, "collect-" + name + "-" + record["label"], "collect")
    _, again = run_command_for(case, record, "collect-again-" + name + "-" + record["label"], "collect")
    require_publication(collected, "committed")
    collect = {key: collected.get(key) for key in COLLECT_FIELDS}
    require_true(collect == {key: again.get(key) for key in COLLECT_FIELDS},
                 "repeated collection changed for " + record["task_id"])
    return {
        "status": {key: status.get(key) for key in STATUS_FIELDS},
        "collect": collect,
    }


def _help(case, name):
    process = case.processes.run("help-" + name, [str(case.delegate), "help"], case.base,
                                 expected=0, timeout=CASE_TIMEOUT)
    text = process.stdout_path.read_bytes()
    require_true(b"dispatch" in text and b"collect" in text, "help omitted ordinary commands")
    return text


def _fixture_line(case, fixture, name, mode):
    """Run one finite fixture probe and return its decision line."""
    process = case.processes.run(name, [str(fixture), FIXTURE_FLAG, mode, str(case.root)],
                                 case.base, expected=0, timeout=CASE_TIMEOUT, stdin=b"")
    lines = process.stdout_path.read_text().splitlines()
    if mode == "probe":
        require_true(len(lines) == 2 and lines[0] == "ready", "lease probe output changed")
    else:
        require_true(len(lines) == 1, "store probe output changed")
    return lines[-1]


def _provider_invocations(case):
    invocations = case.provider_records / "invocations"
    if not invocations.is_dir():
        return 0
    return sum(1 for directory in invocations.iterdir()
               if (directory / "provider.entry.json").is_file())


def _database_bytes(path):
    if not path.exists() and not path.is_symlink():
        return None
    require_true(stat.S_ISREG(path.lstat().st_mode) and stat.S_IMODE(path.lstat().st_mode) == 0o600,
                 "scheduler database is not a private regular file")
    return path.read_bytes()


def _remove_fixture_database(path):
    """Remove the database this state's fixture created, so the next state
    starts from a root with only the owner lease file."""
    journal = path.with_name(path.name + "-journal")
    require_true(not journal.exists() and not journal.is_symlink(),
                 "scheduler fixture left a rollback journal")
    if path.exists() or path.is_symlink():
        path.unlink()


def tree_snapshot(top):
    """Inventory every entry below top: type, mode, size, and content digest."""
    top = Path(top)
    entries = {}
    for path in sorted([top, *top.rglob("*")]):
        info = path.lstat()
        relative = str(path.relative_to(top))
        fact = {"type": stat.S_IFMT(info.st_mode), "mode": stat.S_IMODE(info.st_mode)}
        if stat.S_ISREG(info.st_mode):
            fact["size"] = info.st_size
            fact["sha256"] = sha(path.read_bytes())
        elif stat.S_ISLNK(info.st_mode):
            fact["target"] = str(path.readlink())
        entries[relative] = fact
    return entries
