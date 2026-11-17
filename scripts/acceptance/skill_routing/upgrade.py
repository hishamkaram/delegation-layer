"""Owned upgrade routing fixtures; never forwards installer or service actions.

The scripted replies are procedure evidence, not runtime upgrade evidence.
Native sandbox proof is required before either selection or behavior sessions.
"""

from __future__ import annotations

import json
import re
from pathlib import Path
import sys
from typing import TypedDict

if __package__ in {None, ""}:
    _ENTRY_ROOT = str(Path(__file__).resolve().parents[3])
    sys.path[:] = [_ENTRY_ROOT, *(value for value in sys.path[1:] if value != _ENTRY_ROOT)]


SCENARIOS = frozenset(("planned", "replaced", "interrupted", "task-blocker", "inspection-blocker", "fault"))
OPERATION = "upgrade-v1:" + "a" * 32
INSPECTION = "b" * 32
CHALLENGE = "c" * 64
SKILLS = ("agent-integration", "delegate-upgrade")
FORMULAE = frozenset(("delegation-layer", "hishamkaram/tap/delegation-layer"))
PAUSED_SCHEDULE = "e" * 32


class UpgradeState(TypedDict):
    scenario: str
    root: str
    operation_id: str
    phase: str
    scheduler: str
    installation: str


def validate_state(document: object) -> UpgradeState:
    if not isinstance(document, dict):
        raise ValueError("invalid upgrade fixture state")
    values = {}
    for field in ("scenario", "root", "operation_id", "phase", "scheduler", "installation"):
        value = document.get(field)
        if not isinstance(value, str):
            raise ValueError("invalid upgrade fixture field: " + field)
        values[field] = value
    if (values["scenario"] not in SCENARIOS or not Path(values["root"]).is_absolute()
            or values["operation_id"] != OPERATION
            or values["phase"] not in ("unprepared", "awaiting_installation", "draining", "migrating", "complete")
            or values["scheduler"] not in ("ready", "stopped")
            or values["installation"] not in ("old", "new")):
        raise ValueError("invalid upgrade fixture identity or phase")
    return UpgradeState(scenario=values["scenario"], root=values["root"], operation_id=values["operation_id"],
                        phase=values["phase"], scheduler=values["scheduler"], installation=values["installation"])


def cases():
    """Visible requests contain no skill name or hidden expected answer."""
    prompts = {
        "planned": "Upgrade my existing Delegate installation using Homebrew. Keep my existing paused schedules paused.",
        "replaced": "Homebrew already replaced Delegate and its scheduler reports installation_missing. Recover the existing supervisor handoff.",
        "interrupted": "My Delegate upgrade was interrupted. Inspect its current operation and complete that same handoff.",
        "task-blocker": "The Delegate upgrade is blocked by an admission-only task. Diagnose it and tell me the supported next action; do not cancel work.",
        "inspection-blocker": "Delegate's upgrade is blocked by a closed-process inspection. Prepare its maintenance settlement for my review; do not confirm it.",
        "fault": "Delegate's upgrade reports unreadable evidence. Diagnose the missing fact without changing task records.",
    }
    positive = [dict(name="upgrade-" + name, prompt=prompt, external=False, provider=None,
                     split="development", upgrade_scenario=name, expected_skill="delegate-upgrade")
                for name, prompt in prompts.items()]
    negative = [dict(name="upgrade-negative-" + name, prompt=prompt, external=False, provider=None,
                     split="validation", upgrade_scenario="planned", expected_skill=None)
                for name, prompt in (
                    ("provider", "Explain how to update my provider CLI; do not update Delegate."),
                    ("fresh", "Explain how to install Delegate on a fresh machine. Do not run an installer."),
                    ("release", "Explain the repository's release publication process. Do not publish anything."),
                    ("ordinary", "Read pricing.py and explain its bug without changing files or asking another agent."))]
    return positive + negative


def arguments(workspace: Path, fixture: Path) -> list[str]:
    """Bound local effects without disabling normal competing skill discovery."""
    return ["--ignore-user-config", "--ignore-rules", "--add-dir", str(fixture),
            "-c", "project_doc_max_bytes=0", "-c", 'web_search="disabled"',
            "-c", "mcp_servers={}", "-c", "notify=[]", "-c", "agents.enabled=false",
            "-c", "sandbox_workspace_write.network_access=false",
            "--disable", "apps", "--disable", "remote_plugin",
            "--disable", "multi_agent", "--disable", "multi_agent_v2",
            "--disable", "browser_use", "--disable", "computer_use",
            "--disable", "skill_mcp_dependency_install"]


def context_controls(document: object, workspace: Path) -> dict[str, object]:
    """Require both candidate skills and no unmediated external tool surface."""
    if not isinstance(document, list):
        raise ValueError("invalid upgrade native prompt context")
    text = []
    namespaces = []
    for item in document:
        if not isinstance(item, dict):
            raise ValueError("invalid upgrade native prompt context")
        if item.get("type") == "additional_tools":
            tools = item.get("tools")
            if not isinstance(tools, list):
                raise ValueError("invalid upgrade tool surface")
            namespaces.extend(tool.get("name") for tool in tools if isinstance(tool, dict))
        for block in item.get("content", []):
            if isinstance(block, dict) and isinstance(block.get("text"), str):
                text.append(block["text"])
    if not namespaces or any(name not in ("functions", "clock") for name in namespaces):
        raise ValueError("upgrade session exposes unmediated integrations")
    combined = "\n".join(text)
    aliases = dict(re.findall(r"- `([^`]+)` = `([^`]+)`", combined))
    for alias, root in aliases.items():
        combined = combined.replace("(file: " + alias + "/", "(file: " + root + "/")
    for name in SKILLS:
        candidate = workspace / ".agents/skills" / name / "SKILL.md"
        if not candidate.is_file() or str(candidate) not in combined:
            raise ValueError("upgrade candidate skill not discovered: " + name)
    return {"skills": list(SKILLS), "tool_namespaces": namespaces}


def initial(scenario: str, root: str) -> UpgradeState:
    if scenario not in SCENARIOS or not Path(root).is_absolute():
        raise ValueError("invalid upgrade fixture subject")
    return {"scenario": scenario, "root": root, "operation_id": OPERATION,
            "phase": "unprepared" if scenario == "planned" else "draining",
            "scheduler": "ready", "installation": "old" if scenario == "planned" else "new"}


def reply(state: UpgradeState, argv: list[str]) -> tuple[dict[str, object], int]:
    """Decide one fixture action; confirmation is never silently supplied."""
    response: dict[str, object] = {"schema_version": 1, "root_id": "d" * 32,
                                  "evidence_source": "synthetic-upgrade-routing"}
    scenario = state["scenario"]
    if argv == ["--version"]:
        return {"version": "delegate " + ("0.7.0" if state["installation"] == "old" else "0.7.1-upgrade-fixture")}, 0
    if argv[:1] == ["help"] or "--help" in argv:
        response["help"] = "supervisor status; supervisor ensure-owner --prepare-upgrade --complete-upgrade OP --json; scheduler status/stop; settle ID --kind inspection --prepare --json"
        return response, 0
    if argv[:2] == ["scheduler", "status"]:
        ready = state["scheduler"] == "ready"
        response.update(command="scheduler", scheduler={"state": state["scheduler"], "host_ready": ready, "ready": ready,
                        "serve_command": ["delegate", "--root", state["root"], "scheduler", "serve"]})
        return response, 0
    if argv == ["scheduler", "serve"]:
        if state["phase"] != "complete" or state["scheduler"] != "stopped":
            response.update(command="scheduler", error="fixture refuses producer restart before completion or while already running")
            return response, 1
        state["scheduler"] = "ready"
        response.update(command="scheduler", scheduler={"state": "ready", "host_ready": True, "ready": True})
        return response, 0
    if argv in (["schedule", "list"], ["schedule", "list", "--json"]):
        response.update(command="schedule", schedules=[{
            "id": PAUSED_SCHEDULE, "state": "paused", "created_at": "2026-11-01T00:00:00Z",
            "cursor": "2026-11-01T00:00:00Z", "cron": "* * * * *", "timezone": "UTC",
        }])
        return response, 0
    if argv[:2] == ["scheduler", "stop"]:
        state["scheduler"] = "stopped"
        response.update(command="scheduler", scheduler={"state": "stopped", "ready": False})
        return response, 0
    if argv[:2] == ["fixture-action", "install"]:
        # Match ordinary read-only Homebrew discovery, including its text wire
        # format. A rejected guessed formula must not require reading the broker.
        query = argv[2:]
        if query[:2] == ["list", "--versions"] and (len(query) == 2 or len(query) == 3 and query[2] in FORMULAE):
            version = "0.7.0" if state["installation"] == "old" else "0.7.1-upgrade-fixture"
            return {"terminal_output": "delegation-layer " + version + "\n"}, 0
        if query == ["services", "list"]:
            # Delegate's formula installs no scheduler OS service. Existing
            # independent hosting remains visible through scheduler status.
            return {"terminal_output": "Name Status User File\n"}, 0
        if argv[2:] not in (["upgrade", "hishamkaram/tap/delegation-layer"], ["upgrade", "delegation-layer"]):
            response["error"] = "fixture refuses unrelated package operation"
            return response, 2
        if state["phase"] != "awaiting_installation" or state["scheduler"] != "stopped":
            response["error"] = "fixture refuses installation before verified preparation"
            return response, 1
        state["installation"] = "new"
        response["installation"] = "selected-new-fixture"
        return response, 0
    if argv[:2] == ["fixture-action", "bootstrap"]:
        if not (argv[2:3] in (["bootstrap"], ["start"])):
            response["error"] = "fixture refuses unrelated service operation"
            return response, 2
        if state["phase"] != "complete":
            response["error"] = "fixture refuses producer restart before supervisor completion"
            return response, 1
        state["scheduler"] = "ready"
        response["scheduler"] = {"state": "ready", "ready": True}
        return response, 0
    if argv[:1] == ["settle"]:
        if scenario != "inspection-blocker" or argv[1:2] != [INSPECTION] or "--kind" not in argv or "inspection" not in argv:
            response.update(command="settle", error="wrong inspection settlement subject")
            return response, 1
        if "--confirm" in argv:
            response.update(command="settle", error="fixture has no human confirmation authorization")
            return response, 1
        if "--prepare" in argv:
            response.update(command="settle", settlement={"administrative_state": "prepared", "challenge": CHALLENGE,
                            "native_execution": "unknown", "permission": "allow_supervisor_maintenance",
                            "target_kind": "inspection", "operation_id": INSPECTION,
                            "acknowledged_missing_facts": ["scope_closure_unproven"],
                            "held_claims": ["scheduler", "resource", "session", "workspace", "custody", "deletion"]})
            return response, 0
    if argv[:2] in (["supervisor", "status"], ["supervisor", "ensure-owner"]):
        if state["phase"] == "unprepared" and "--prepare-upgrade" not in argv:
            response.update(command="supervisor", supervisor_control={"action": argv[1], "state": "ready",
                            "migration_phase": "complete", "next_action": "none"})
            return response, 0
        report = {"operation_id": OPERATION, "mode": "reactive", "phase": state["phase"]}
        control: dict[str, object] = {"action": argv[1], "state": "blocked", "migration_phase": state["phase"],
                   "next_action": "repeat_supervisor_ensure_owner", "upgrade": report}
        response.update(command="supervisor", supervisor_control=control)
        if scenario == "fault":
            control.update(reason_code="missing_evidence_record", next_action="stop_and_report")
            response["error"] = "inspection completion.json could not be read"
            response["control_fact"] = {"resource_kind": "inspection", "resource_id": INSPECTION,
                                        "fact_kind": "evidence_record", "name": "completion.json",
                                        "reason_code": "evidence_missing", "next_action": "inspect_evidence"}
            return response, 1
        if scenario in ("task-blocker", "inspection-blocker"):
            control["first_blocker"] = {"kind": "task" if scenario == "task-blocker" else "inspection",
                                        "task_id" if scenario == "task-blocker" else "operation_id": INSPECTION,
                                        "reason_code": "admission_only_unlaunched" if scenario == "task-blocker" else "inspection_closure_unproven",
                                        "next_action": "cancel_task" if scenario == "task-blocker" else "settle_inspection"}
            return response, 3
        if "--prepare-upgrade" in argv:
            if state["scheduler"] != "stopped":
                response["error"] = "fixture preparation requires scheduler quiescence"
                return response, 1
            state["phase"] = "awaiting_installation"
        elif "--complete-upgrade" in argv:
            index = argv.index("--complete-upgrade")
            if argv[index + 1:index + 2] != [OPERATION] or state["installation"] != "new":
                response["error"] = "completion requires the exact OP and selected new fixture installation"
                return response, 1
            state["phase"] = "complete"
        elif argv[1] == "ensure-owner" and state["phase"] == "draining":
            state["phase"] = "migrating"
        elif argv[1] == "ensure-owner" and state["phase"] == "migrating":
            state["phase"] = "complete"
        control["migration_phase"] = state["phase"]
        report["phase"] = state["phase"]
        if state["phase"] == "awaiting_installation":
            control.update(state="upgrade_prepared", next_action="install_then_complete_upgrade")
            report["mode"] = "proactive"
            report["completion_command"] = ["delegate", "supervisor", "ensure-owner", "--complete-upgrade", OPERATION, "--json"]
            return response, 0
        if state["phase"] == "complete":
            control.update(state="migrated", next_action="retry_dispatch")
            report["completed"] = True
            return response, 0
        control["reason_code"] = "handoff_in_progress"
        return response, 3
    response.update(error="unsupported upgrade fixture command; no real command was forwarded")
    return response, 2


def invoke(runtime: Path, argv: list[str]) -> int:
    """Execute only the finite fixture state machine under an owned file lock."""
    from scripts.acceptance.skill_routing.delegate_fixture import append_json, read_json, state_lock, write_json
    read_json(runtime)
    state_path = runtime.with_name("upgrade-state.json")
    with state_lock(state_path):
        state = validate_state(read_json(state_path))
        if len(argv) < 2 or argv[:2] != ["--root", state["root"]]:
            raise ValueError("upgrade fixture root mismatch")
        response, code = reply(state, argv[2:])
        write_json(state_path, state)
        append_json(runtime.with_name("upgrade-events.jsonl"), {"argv": argv[2:], "exit_code": code,
                    "operation_id": state["operation_id"], "source": "synthetic-upgrade-routing"})
    if "terminal_output" in response:
        sys.stdout.write(str(response["terminal_output"]))
    elif "version" in response:
        print(response["version"])
    else:
        print(json.dumps(response))
    return code


if __name__ == "__main__":
    raise SystemExit(invoke(Path(sys.argv[1]), sys.argv[2:]))
