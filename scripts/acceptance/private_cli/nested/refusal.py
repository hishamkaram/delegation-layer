"""Real warm-supervisor cleanup after one finite injected leader refusal."""

from pathlib import Path
import sys
import time
import uuid

from scripts.acceptance.directories.operations import DirectoryOwner
from scripts.acceptance.evidence.records import _read_control_record
from scripts.acceptance.evidence.validation import AcceptanceFailure
from scripts.acceptance.evidence.validation import require
from scripts.acceptance.private_cli.commands import run_json
from scripts.acceptance.private_cli.finalization import finalize_fixture
from scripts.acceptance.private_cli.fixture_lifetime import FixtureLifetime
from scripts.acceptance.private_cli.fixture_setup import prepare_fixture
from scripts.acceptance.private_cli.nested.workflow import bootstrap
from scripts.acceptance.private_cli.nested.case import NestedCase
from scripts.acceptance.private_cli.nested.observation import supervisor_continuity
from scripts.acceptance.private_cli.nested.policy import require_backend
from scripts.acceptance.private_cli.receipt import AcceptanceReceipt
from scripts.acceptance.private_cli.root_cleanup import remove_private_acceptance_root
from scripts.acceptance.private_cli.supervisor import selected_owner_subject


class RefusedLeaderCase(NestedCase):
    """Only leader admission is synthetic; all other controls keep their real owner."""

    def __init__(self, args, lifetime, fixture, responses, receipt):
        super().__init__(args, lifetime, fixture, responses, receipt)
        self.commands = []
        self.injected = False

    def command(self, state, arguments, name, *, expected=(0,), deadline=None):
        self.commands.append({"state": str(state), "arguments": list(arguments), "name": name})
        if name == "leader":
            require(not self.injected and state == self.a and arguments[0] == "dispatch"
                    and arguments[arguments.index("--id") + 1] == self.configuration["leader"],
                    "refusal fixture may replace only the original leader dispatch")
            self.injected = True
            root = _read_control_record(state / "root.json", "warmed root")["root_id"]
            return run_json(
                Path(sys.executable), self.lifetime.env,
                [str(Path(__file__).parent.parent / "refusal_fixture.py"), root, self.configuration["leader"]],
                expected=(1,), timeout=10, receipt=self.receipts / "leader.json", capture_response=True,
                capture_root=self.lifetime.capture_token,
            )
        return super().command(state, arguments, name, expected=expected, deadline=deadline)


def require_injected_refusal(case):
    failure = case.evidence.get("failed_admission", {})
    resource = failure.get("resource_admission", {})
    diagnostics = resource.get("diagnostics", {})
    cpu = diagnostics.get("host_cpu", {})
    intervals = cpu.get("intervals", [])
    require(case.injected and failure.get("name") == "leader"
            and failure.get("task_id") == case.configuration["leader"]
            and failure.get("requested_task_id") == case.configuration["leader"]
            and failure.get("task_record") == "unknown" and failure.get("admission") == "unknown"
            and failure.get("failure") == {"code": "resource_pressure", "stage": "check-resources",
                                            "reason_code": "host_cpu_sustained", "next_action": "stop_and_report"}
            and resource.get("status") == "pressure" and cpu.get("counter_unit") == (
                "ticks" if sys.platform == "linux" else "seconds")
            and cpu.get("busy_threshold") == 0.95 and cpu.get("required_streak") == 3
            and len(intervals) == 3 and all(interval.get("classification") == "hot" for interval in intervals),
            "expected only the injected explicit-ID leader resource refusal")


def observe_live_supervisor(case):
    continuity = supervisor_continuity(case.a, case.configuration, case.lifetime.capture_token, "")
    daemon = continuity["process"]
    owner = case.native(daemon["ppid"])
    require(owner["present"] is True and owner["pid"] != daemon["pid"],
            "warm daemon lacks its real live resident parent")
    token = case.lifetime.state_token
    require(token is not None, "warm root lost its original directory owner")
    token.assert_live()
    return {"root_device": token.identity[0], "root_inode": token.identity[1],
            "owner": owner, "daemon": daemon, "continuity": continuity}


def require_inner_failure(case, inner):
    require_injected_refusal(case)
    captured = _read_control_record(case.receipts / "leader.json", "finite refusal capture")
    response = captured.get("response")
    if not isinstance(response, dict):
        raise AcceptanceFailure("finite refusal capture omitted its response")
    require(captured.get("exit_code") == 1
            and case.evidence["failed_admission"]["resource_admission"] == response.get("resource_admission"),
            "finite refusal CPU diagnostics changed in the allowlisted admission receipt")
    require(inner.get("result") == "FAIL"
            and inner.get("failure") == {"code": "nested-setup-failed", "exception_type": "AcceptanceFailure",
                                         "phase": "nested_setup"}
            and inner["cleanup"]["status"] == "retained"
            and inner["cleanup"]["pending_supervisor_count"] == 0
            and inner["nested_survival"]["failed_admission"] == case.evidence["failed_admission"]
            and inner["nested_survival"]["cleanup"]["status"] == "canonical-closure-proven",
            "injected refusal did not retain the original failure with real canonical cleanup")
    leader = case.configuration["leader"]
    commands = case.commands
    require(sum(command["arguments"][0] == "dispatch" and "--id" in command["arguments"]
                and command["arguments"][command["arguments"].index("--id") + 1] == leader
                for command in commands) == 1,
            "refused leader was dispatched more than once")
    require(not any(command["arguments"][0] in {"status", "cancel", "collect"}
                    and leader in command["arguments"] for command in commands),
            "absent leader reached a task observation or cancellation")


def dispose_retained_root(case, before):
    """The outer invocation rechecks closure after the failed inner finalizer released its leases."""
    lifetime = case.lifetime
    require(lifetime.base is not None and lifetime.base_token is not None,
            "outer cleanup lacks its exact original root owner")
    case.cleanup_deadline = time.monotonic() + 45
    closure = case.read_closure(case.a, tuple(lifetime.attempted_task_ids))
    require(set(closure["tasks"]) == set(lifetime.attempted_task_ids) - {case.configuration["leader"]},
            "fresh outer closure changed the retained warm task history")
    observations = {}
    for role in ("owner", "daemon"):
        original = before[role]
        observed = case.native(original["pid"], expected_start=original["process_start"])
        require(not observed["present"] or observed["identity_matches"] is False,
                "original warm " + role + " incarnation remained live")
        observations[role] = observed
    stopped = _read_control_record(case.receipts / ("cleanup-owner-" + case.a.name + ".json"), "owner stop capture")
    selected = selected_owner_subject(stopped["response"], closure["root_id"], lifetime.private_config)
    require(selected is not None and selected["process"]["pid"] == before["owner"]["pid"]
            and case.tokens_match(before["owner"]["process_start"], selected["process"]["start_token"]),
            "cleanup did not stop the original warm resident incarnation")
    owned = lifetime.ownership.transfer()
    entries = (lifetime.base_token, *lifetime.old_layout_owned,
               *(entry for entry in owned if entry is not lifetime.base_token))
    try:
        remove_private_acceptance_root(lifetime.base_token, entries,
                                       managed_state_roots=tuple(lifetime.managed_state_roots),
                                       disposal=lifetime.disposal, deadline=case.cleanup_deadline,
                                       reader=case.read_closure)
    finally:
        lifetime.disposal.release_all()
    return {"status": "passed", "private_root": {"status": "removed", "verified": True},
            "fresh_closure": closure, "selected_owner": selected, "native_absence": observations}


def run_refused_leader(args, responses, receipt):
    lifetime = FixtureLifetime(DirectoryOwner(), DirectoryOwner())
    inner_receipt = AcceptanceReceipt(args.output.with_suffix(".inner.json"), 3, 1,
                                      {"pueue": args.pueue, "pueued": args.pueued,
                                       "native-fixture": args.native_fixture})
    case, before, failure, expected = None, None, None, False
    finalization_failure = None
    try:
        fixture = prepare_fixture(args, uuid.uuid4().hex, False, lifetime, responses, inner_receipt)
        case = RefusedLeaderCase(args, lifetime, fixture, responses, inner_receipt)
        inner_receipt.phase("nested_setup")
        bootstrap(case, case.a, "warm-a")
        before = observe_live_supervisor(case)
        case.evidence["warm_before_refusal"] = before
        warm = case.responses["warm-a"]
        intent = _read_control_record(case.a / "tasks" / warm["task_id"] / "lifetime" / "intent.json", "warm task intent")
        require_backend(intent, args.expected_backend)
        try:
            case.dispatch(case.a, case.configuration["leader"], case.configuration["leader_brief"], "3m", "leader")
        except AcceptanceFailure:
            require_injected_refusal(case)
            expected = True
            raise
        raise AcceptanceFailure("leader refusal fixture unexpectedly admitted work")
    except BaseException as error:
        failure = error
        inner_receipt.add_details(failure_phase=inner_receipt.current_phase)
    finally:
        try:
            finalize_fixture(args, failure, lifetime, inner_receipt, False)
        except BaseException as cleanup_error:
            finalization_failure = cleanup_error
            expected = False
    inner = {}
    cleanup = {"status": "retained", "private_root": {"status": "retained", "verified": False}}
    try:
        inner = _read_control_record(inner_receipt.output, "inner failed acceptance receipt")
        if case is not None and before is not None and case.a in lifetime.closed_roots:
            cleanup = dispose_retained_root(case, before)
        if case is None or before is None or not expected:
            raise AcceptanceFailure("real warm setup or finalization failed before expected refusal proof") from (
                finalization_failure or failure)
        require_inner_failure(case, inner)
        require(cleanup["status"] == "passed", "outer owner did not freshly prove and dispose the retained root")
        receipt.add_details(refused_leader={"inner": inner, "commands": case.commands,
                                            "warm_task_ids": lifetime.attempted_task_ids[:-1]})
        receipt.write("PASS", cleanup)
        print("PASS real refused-leader cleanup: " + str(args.output))
    except BaseException as error:
        context = {"phase": inner_receipt.details.get("failure_phase", inner_receipt.current_phase)}
        if failure is not None:
            context["primary"] = type(failure).__name__
        if finalization_failure is not None:
            context["finalization"] = type(finalization_failure).__name__
        receipt.add_details(refused_leader={"inner": inner, "failure_context": context}, retained_root=str(lifetime.base))
        receipt.write("FAIL", cleanup, error)
        if not inner and failure is not None:
            raise error from failure
        raise
