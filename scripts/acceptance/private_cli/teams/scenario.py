"""Real source-built CLI Team admission, artifact and native-closure proof."""

import json
import re
import uuid

from scripts.acceptance.private_cli.admission import observe_created_admission
from scripts.acceptance.private_cli.commands import run_json
from scripts.acceptance.private_cli.configuration import COLLECT_TIMEOUT_SECONDS, COLLECT_WATCH, TASK_BUDGET
from scripts.acceptance.private_cli.disposal_observer import public_status_observer
from scripts.acceptance.private_cli.supervisor import shutdown_terminal_private_supervisor


def run_teams_scenario(lifetime, fixture, response_directory, run_receipt):
    """The CLI owns catalog/task writes; the existing lifetime owner owns disposal."""
    delegate = fixture.upgraded_install / "delegate"
    context = ["--cwd", str(fixture.workspace), "--project-root", str(fixture.workspace)]

    def command(name, arguments, **options):
        return run_json(delegate, lifetime.env, arguments,
                        receipt=response_directory / (name + ".json"),
                        capture_root=lifetime.capture_token, capture_response=True, **options)

    run_receipt.phase("teams_registration")
    definition = {
        "schema_version": 1, "id": "private-write", "name": "Private write team",
        "purpose": "Prove explicit Team Member admission and owned workspace writes.",
        "roles": [{"id": "implementer", "name": "Implementer", "responsibility": "Create the nonce proof artifacts."}],
        "members": [{"id": "writer", "name": "Writer", "role_ids": ["implementer"],
                     "execution_defaults": {"provider": "pi:json", "model": "openai/gpt-4.1", "effort": "low"}}],
    }
    definition_path = lifetime.base / "team-definition.json"
    definition_path.write_text(json.dumps(definition) + "\n")
    command("team-register", ["teams", "register", "--scope", "project", *context,
                              "--definition", str(definition_path), "--json"])
    shown = command("team-show", ["teams", "show", "--scope", "project", *context,
                                   "--team", "private-write", "--json"])
    reference = {"scope": "project", "team_id": "private-write",
                 "project": {"kind": "directory", "path": str(fixture.workspace)}}
    record = {"reference": reference, "revision": 1, "definition": definition}
    catalog_id = shown.get("catalog_id")
    if (shown.get("status") != "ok" or shown.get("team") != record
            or not isinstance(catalog_id, str) or re.fullmatch(r"[0-9a-f]{32}", catalog_id) is None):
        raise RuntimeError("Team show did not preserve the registered definition and catalog identity")
    command("team-default-set", ["teams", "default", "set", *context,
                                 "--team-scope", "project", "--team", "private-write", "--json"])
    default = command("team-default-show", ["teams", "default", "show", *context, "--json"])
    listed = command("team-list", ["teams", "list", *context, "--json"])
    if (default.get("status") != "ok"
            or default.get("default") != {"state": "resolved", "reference": reference, "team": record}
            or listed.get("catalog_id") != catalog_id or listed.get("status") != "ok"
            or listed.get("next_cursor") or listed.get("diagnostics")
            or listed.get("teams") != [{"reference": reference, "revision": 1, "name": definition["name"],
                                       "purpose": definition["purpose"], "role_count": 1, "member_count": 1}]):
        raise RuntimeError("isolated Team discovery did not return the saved default and scoped summary")

    run_receipt.phase("teams_dispatch")
    expected_team = {
        "schema_version": 1, "target": {"team": reference, "member_id": "writer", "catalog_id": catalog_id},
        "team_revision": 1, "team_name": definition["name"], "team_purpose": definition["purpose"],
        "member_name": "Writer", "roles": definition["roles"],
        "intent": {"config": {"permission": "workspace-write", "budget": TASK_BUDGET}},
    }
    fixture.brief.write_text("DELEGATE_TEAMS_WRITE:" + fixture.lease_nonce + "\n")
    task_id = uuid.uuid4().hex
    # Register intent before dispatch: uncertain creation is never an excuse to lose cleanup ownership.
    lifetime.attempted_task_ids.append(task_id)
    lifetime.primary_observer = public_status_observer(
        delegate, fixture.state, lifetime.env, capture_root=lifetime.capture_token,
    )
    response = command("team-dispatch", ["--root", str(fixture.state), "dispatch", *context,
                                         "--brief", str(fixture.brief), "--id", task_id,
                                         "--team-scope", "project", "--team", "private-write", "--member", "writer",
                                         "--permission", "workspace-write", "--budget", TASK_BUDGET, "--json"],
                       expected=(0, 1))
    observe_created_admission(delegate, fixture.state, lifetime.env, response,
                             response_directory / "team-admission.json", capture_root=lifetime.capture_token)
    require_public_team(response, task_id, expected_team, "dispatch")
    run_receipt.update_counts(dispatch_completed_count=1, admitted_task_count=1)
    status = command("team-status", ["--root", str(fixture.state), "status", task_id, "--json"])
    require_public_team(status, task_id, expected_team, "status")
    collected = command("team-collect", ["--root", str(fixture.state), "collect", task_id,
                                         "--watch", COLLECT_WATCH, "--json"], timeout=COLLECT_TIMEOUT_SECONDS)
    require_public_team(collected, task_id, expected_team, "collect")
    if (collected.get("task_id") != task_id or collected.get("publication") != "committed"
            or collected.get("outcome", {}).get("verdict") != "committed"):
        raise RuntimeError("Team workspace-write task did not commit its provider result")

    task = json.loads((fixture.state / "tasks" / task_id / "task.json").read_text())
    expected_config = {"model": "openai/gpt-4.1", "effort": "low", "permission": "workspace-write", "budget": "2m0s"}
    if (task.get("task_id") != task_id or task.get("provider") != "pi:json"
            or task.get("mode") != "workspace-write" or task.get("canonical_cwd") != str(fixture.workspace)
            or task.get("budget_nanos") != 120 * 10**9 or task.get("requested_config") != expected_config
            or task.get("team") != expected_team):
        raise RuntimeError("immutable task did not preserve exact admitted Team and explicit caller authority")
    nonce = fixture.lease_nonce
    artifact = fixture.workspace / ("team-write-" + nonce + ".txt")
    command_artifact = fixture.workspace / ("team-command-" + nonce + ".txt")
    if (artifact.read_bytes() != ("created:" + nonce + "\nedited:" + nonce + "\n").encode()
            or command_artifact.read_bytes() != ("command:" + nonce + "\n").encode()):
        raise RuntimeError("admitted Team Member did not create, edit and run the exact nonce proof")
    run_receipt.update_counts(collected_task_count=1)
    closure = shutdown_terminal_private_supervisor(
        delegate, lifetime.upgraded_libexec / "pueue", lifetime.private_config, lifetime.env,
        state=fixture.state, task_ids=(task_id,), capture_root=lifetime.capture_token,
        supervisor_cleanups=lifetime.supervisor_cleanups, disposal=lifetime.disposal,
        reader=lifetime.closure_reader, observe=lifetime.primary_observer, closed_roots=lifetime.closed_roots,
    )
    run_receipt.add_details(teams={"catalog_id": catalog_id, "task_id": task_id, "admitted_team": task["team"],
                                   "provider": task["provider"], "requested_config": task["requested_config"],
                                   "workspace_artifacts": [artifact.name, command_artifact.name],
                                   "native_closure": closure})
    run_receipt.stage("teams-admission-write-and-native-closure-proved")


def require_public_team(response, task_id, expected_team, phase):
    """Public views must preserve the same exact admitted context as task.json."""
    if response.get("task_id") != task_id or response.get("team") != expected_team:
        raise RuntimeError("Team %s did not preserve the admitted task and Team context" % phase)
