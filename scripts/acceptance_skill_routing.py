#!/usr/bin/env python3
"""Capture natural-prompt skill selection. Transcripts require evidence-backed review.

Ordinary routing keeps the normal profile and competing integrations. Diagnosis
handoffs instead use invocation-local isolation to bound paid work. Only
disposable workspaces receive the candidate skill; no user setup is edited.
"""
import argparse
import hashlib
import json
import os
import re
from pathlib import Path
import shutil
import shlex
import socket
import tempfile
import time
import skill_eval_cases
import skill_routing_delegate_fixture as routing_fixture

from acceptance_provider_common import validate_managed_lifetime_closure
from acceptance_supervisor_common import (
    DirectoryOwner,
    OwnedDirectory,
    Processes,
    config_for,
    finalize_acceptance_artifacts,
)
import sys

REPO = Path(__file__).resolve().parents[1]
CASES = REPO / "skills/agent-integration/evals/routing.json"
SKILL = REPO / "skills/agent-integration"
DIAGNOSIS_DISABLED_FEATURES = (
    "hooks", "plugins", "remote_plugin", "apps", "multi_agent", "multi_agent_v2",
    "browser_use", "computer_use", "image_generation", "skill_mcp_dependency_install",
)


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def read_json(path):
    return json.loads(path.read_text(encoding="utf-8"))


def write_json(path, value):
    with path.open("x", encoding="utf-8") as stream:
        json.dump(value, stream, indent=2)
        stream.write("\n")


def command(harness, model, prompt, *, diagnosis=False):
    if harness == "codex":
        argv = ["codex", "exec", "--json", "--skip-git-repo-check",
                "--sandbox", "workspace-write" if diagnosis else "danger-full-access",
                "-c", 'approval_policy="never"']
        if model:
            argv += ["--model", model]
        return argv + ["-"], prompt
    if harness == "claude":
        argv = ["claude", "--print", "--verbose", "--output-format", "stream-json",
                "--max-turns", "16", "--dangerously-skip-permissions"]
        if model:
            argv += ["--model", model]
        return argv, prompt
    argv = ["hermes", "chat", "--verbose", "--max-turns", "16", "--query", prompt]
    if model:
        argv += ["--model", model]
    return argv, ""


def diagnosis_arguments(workspace, fixture):
    """Native, invocation-local controls; preserve the user's auth installation."""
    disabled = []
    for root in (Path.home() / ".agents/skills", Path.home() / ".codex/skills"):
        skill = root / "agent-integration"
        for path in (skill, skill / "SKILL.md", skill.resolve(), (skill / "SKILL.md").resolve()):
            disabled.append({"path": str(path), "enabled": False})
    arguments = ["--ignore-user-config", "--ignore-rules", "--add-dir", str(fixture),
                 "-c", "project_doc_max_bytes=0", "-c", 'web_search="disabled"',
                 "-c", "mcp_servers={}", "-c", "agents.enabled=false", "-c", "notify=[]",
                 "-c", "developer_instructions=" + json.dumps(read_json(SKILL / "references/routing-rule.json")),
                 "-c", "sandbox_workspace_write.network_access=false",
                 "-c", "skills.bundled.enabled=false",
                 "-c", "skills.config=[" + ",".join(
                     "{path=" + json.dumps(entry["path"]) + ",enabled=false}"
                     for entry in disabled) + "]"]
    for feature in DIAGNOSIS_DISABLED_FEATURES:
        arguments.extend(("--disable", feature))
    return arguments


def validate_diagnosis_context(document, workspace):
    """Accept only the copied skill and a model-free local tool surface."""
    if not isinstance(document, list):
        raise ValueError("invalid native prompt context")
    texts, namespaces = [], []
    for item in document:
        if not isinstance(item, dict):
            raise ValueError("invalid native prompt context")
        if item.get("type") == "additional_tools":
            tools = item.get("tools")
            if not isinstance(tools, list) or not all(isinstance(tool, dict) for tool in tools):
                raise ValueError("invalid native tool context")
            namespaces.extend(tool.get("name") for tool in tools)
        content = item.get("content", [])
        if not isinstance(content, list):
            raise ValueError("invalid native instruction context")
        for block in content:
            if isinstance(block, dict) and isinstance(block.get("text"), str):
                texts.append((item.get("role"), block["text"]))
    if not namespaces or any(name not in ("functions", "clock") for name in namespaces):
        raise ValueError("native context still exposes external collaboration/tools")
    rule = read_json(SKILL / "references/routing-rule.json")
    instruction_blocks = [index for index, (role, text) in enumerate(texts)
                          if role == "developer" and text == rule]
    if len(instruction_blocks) != 1:
        raise ValueError("exact candidate routing instruction was not established")
    rule_prefix = rule.split(". ", 1)[0]
    for index, (_, block) in enumerate(texts):
        if index == instruction_blocks[0]:
            continue
        metadata = "### Available skills" in block
        if rule_prefix in block or (not metadata and ("Delegate" in block or "agent-integration" in block)):
            raise ValueError("additional delegation instructions contaminate the candidate context")
    text = "\n".join(text for _, text in texts)
    aliases = dict(re.findall(r"- `([^`]+)` = `([^`]+)`", text))
    entries = re.findall(r"^- [^\n]*agent-integration[^\n]*\(file: ([^)]+)\)", text, re.MULTILINE)
    candidate = workspace / ".agents/skills/agent-integration/SKILL.md"
    paths = []
    for entry in entries:
        first, separator, rest = entry.partition("/")
        paths.append(Path(aliases[first]) / rest if separator and first in aliases else Path(entry))
    if paths != [candidate] or candidate.read_bytes() != (SKILL / "SKILL.md").read_bytes():
        raise ValueError("candidate-only skill context was not established")
    return {"candidate_skill": str(candidate), "tool_namespaces": namespaces,
            "routing_rule_sha256": digest(SKILL / "references/routing-rule.json")}


def verify_network_boundary(owner, executable, *, ownership):
    """Probe real connectivity to an owned listener outside actor writable roots."""
    directory = Path(tempfile.mkdtemp(prefix=".delegate-eval-", dir=Path.home()))
    ownership.adopt_created("diagnosis IPC listener", directory, Path.home())
    address = directory / "probe.sock"
    probe = (
        "import json,socket,sys\n"
        "for family in (socket.AF_INET,socket.AF_INET6):\n"
        " try: connection=socket.socket(family)\n"
        " except PermissionError: continue\n"
        " connection.close()\n"
        " raise SystemExit('native sandbox permits an IP socket')\n"
        "try:\n"
        " connection=socket.socket(socket.AF_UNIX)\n"
        "except PermissionError: pass\n"
        "else:\n"
        " with connection:\n"
        "  try: connection.connect(sys.argv[1])\n"
        "  except PermissionError: pass\n"
        "  else: raise SystemExit('native sandbox permits outside IPC connection')\n"
        "print(json.dumps({'network_and_ipc_connections':'denied'}))\n"
    )
    with socket.socket(socket.AF_UNIX) as listener:
        listener.bind(str(address))
        listener.listen(1)
        try:
            proof = owner.capture("diagnosis-sandbox", [executable,
                "-c", 'sandbox_mode="workspace-write"', "-c", "sandbox_workspace_write.network_access=false",
                "sandbox", "--", sys.executable, "-c", probe, str(address)])
        finally:
            address.unlink(missing_ok=True)
    if read_json(proof) != {"network_and_ipc_connections": "denied"}:
        raise RuntimeError("native diagnosis sandbox proof is invalid")
    return proof


def verify_diagnosis_controls(owner, executable, workspace, *, ownership, model=None):
    """Exercise the native boundaries without invoking a model."""
    help_text = owner.capture("diagnosis-help", [executable, "exec", "--help"]).read_text()
    if any(flag not in help_text for flag in ("--ignore-user-config", "--ignore-rules", "--disable")):
        raise RuntimeError("diagnosis isolation requires unavailable native controls")
    proof = verify_network_boundary(owner, executable, ownership=ownership)
    # debug prompt-input has no ignore-user-config/add-dir flags. Explicitly
    # override its instruction/integration sources using the same settings;
    # exec additionally ignores user configuration. This is a conservative
    # gate: an unexpected tool surface blocks the paid invocation.
    fixture = workspace.parent / "fixture"
    arguments = diagnosis_arguments(workspace, fixture)
    omitted = {"--ignore-user-config", "--ignore-rules", "--add-dir", str(fixture)}
    arguments = [argument for argument in arguments if argument not in omitted]
    if model is not None:
        arguments.extend(("-c", "model=" + json.dumps(model)))
    context = owner.capture("diagnosis-context", [executable, *arguments, "debug", "prompt-input"])
    validated = validate_diagnosis_context(read_json(context), workspace)
    return dict(validated, network_and_ipc_connections="denied", proof_sha256=digest(proof),
                context_sha256=digest(context), disabled_features=list(DIAGNOSIS_DISABLED_FEATURES),
                routing_delivery="inline-isolated", legacy_notify="disabled")


def prepare_workspace(path, harness):
    location = {"codex": ".agents", "claude": ".claude", "hermes": ".hermes"}[harness]
    shutil.copytree(SKILL, path / location / "skills/agent-integration",
                    ignore=shutil.ignore_patterns("evals"))
    (path / "pricing.py").write_text(
        "def discounted_price(price, percent):\n    return price - percent\n", encoding="utf-8")
    (path / "README.md").write_text(
        "# Pricing fixture\nDiscounts are percentages: discounted_price(200, 10) should be 180.\n",
        encoding="utf-8")


def candidate_digests(root=SKILL):
    return {str(p.relative_to(root)): digest(p)
            for p in sorted(root.rglob("*")) if p.is_file()}


def paired_scenarios(suite):
    """Join routing selections to structurally validated, grader-owned pairs."""
    errors = skill_eval_cases.check_skill(SKILL)
    if errors:
        raise ValueError("; ".join(errors))
    paired = [case for case in suite if "paired_case_id" in case]
    if not paired:
        return {}
    document = read_json(SKILL / "evals/cases.json")
    leaves = {leaf["id"]: leaf for case in document["cases"] for leaf in (case, case["control"])}
    references = [case["paired_case_id"] for case in paired]
    if (len(references) != len(leaves) or not all(isinstance(ref, str) for ref in references)
            or set(references) != set(leaves) or len(set(references)) != len(references)):
        raise ValueError("paired routing references must cover every scenario exactly once")
    for case in paired:
        leaf = leaves[case["paired_case_id"]]
        if (case.get("external") is not False or case.get("prompt") != leaf["visible"]["prompt"]
                or case.get("fixture_scenario") != case.get("name")
                or case["name"] not in routing_fixture.DIAGNOSIS_SCENARIOS
                or "provider" not in case):
            raise ValueError("invalid paired routing selection")
    names = [case["name"] for case in suite]
    if len(set(names)) != len(names):
        raise ValueError("duplicate routing case name")
    return leaves


def materialize_diagnosis(fixture, workspace, state_root, case):
    leaves = paired_scenarios(read_json(CASES)["cases"])
    leaf = leaves[case["paired_case_id"]]
    replacements = {
        "@ROOT_ID@": routing_fixture.root_id(state_root),
        "@TASK_ID@": routing_fixture.task_id(state_root, case["fixture_scenario"]),
    }
    for entry in leaf["visible"]["inputs"]:
        path = workspace / entry["path"]
        if path.exists() or not path.resolve().is_relative_to(workspace.resolve()):
            raise ValueError("diagnosis input collides with the candidate workspace")
        path.parent.mkdir(parents=True, exist_ok=True)
        text = entry["content"]
        for token, value in replacements.items():
            text = text.replace(token, value)
        with path.open("x", encoding="utf-8") as stream:
            stream.write(text)
    routing_fixture.initialize_diagnosis(fixture, workspace, state_root, case["fixture_scenario"])


def validate_nested_queue_roots(snapshot, root_ids):
    """Compare queue identities only with roots closed by the canonical validator."""
    if not isinstance(snapshot, dict) or not isinstance(snapshot.get("tasks"), dict):
        raise ValueError("private queue snapshot has invalid tasks")
    for row in snapshot["tasks"].values():
        if not isinstance(row, dict) or "label" not in row or not (row["label"] is None or isinstance(row["label"], str)):
            raise ValueError("private queue task label is invalid")
        label = row["label"]
        if label is None or not label.startswith(("delegate:", "delegation-inspection-")):
            continue
        match = re.fullmatch(
            r"delegate:([0-9a-f]{32}):[0-9a-f]{32}|delegation-inspection-([0-9a-f]{32})-[0-9a-f]{32}", label)
        if match is None or (match.group(1) or match.group(2)) not in root_ids:
            raise ValueError("private queue contains an unowned Delegate label")


class AttemptSupervisor:
    """Own only this attempt's private daemon; stopping always goes through Pueue."""

    def __init__(self, run, environment, ownership, *, state_root=None):
        self.client = shutil.which("pueue")
        self.daemon_path = shutil.which("pueued")
        if not self.client or not self.daemon_path:
            raise FileNotFoundError("private test supervisor unavailable")
        base = Path(tempfile.mkdtemp(prefix="skill-queue-")).resolve(strict=True)
        self.base = ownership.adopt_created(
            "private skill supervisor", base, base.parent).directory
        self.config = self.base / "pueue.json"
        ownership.create("private skill supervisor state", self.base / "state", self.base)
        ownership.create("private skill supervisor runtime", self.base / "run", self.base)
        write_json(self.config, config_for(self.base))
        self.processes = Processes(run / "processes", environment, owner=ownership)
        self.daemon = None
        self.task_id = None
        self.run = run
        self.delegate_state_roots = set()
        if state_root is not None:
            configured_path = Path(state_root)
            if not configured_path.is_absolute():
                raise RuntimeError("nested Delegate state root is not absolute")
            self.delegate_state_roots.add(configured_path.resolve(strict=False))
        self.nested_lifetime_closure = None
        write_json(run / "supervisor.json", {"config": str(self.config), "owned": True})

    def direct(self, name, argv, expected=0):
        return self.processes.run(name, argv, self.run, expected=expected, timeout=20)

    def control(self, *argv, expected=0):
        return self.direct("control", [self.client, "-c", self.config, *argv], expected)

    def start(self):
        self.daemon = self.processes.start("daemon", [self.daemon_path, "-c", self.config], self.base)
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            result = self.control("status", "--json", expected={0, 1})
            if result.result["exit_code"] == 0:
                self.control("parallel", "0")
                return
            time.sleep(0.2)
        raise RuntimeError("private supervisor readiness expired")

    def task_status(self):
        snapshot = self.control("status", "--json").json()
        return snapshot["tasks"][self.task_id]["status"]

    def execute(self, argv, stdin, timeout, label="harness"):
        destination = self.run if label == "harness" else self.run / label
        if label != "harness":
            destination.mkdir()
        prompt = destination / "stdin.txt"
        prompt.write_text(stdin, encoding="utf-8")
        shell_command = ("exec " + shlex.join(argv) + " < " + shlex.quote(str(prompt))
                         + " > " + shlex.quote(str(destination / "stdout.txt"))
                         + " 2> " + shlex.quote(str(destination / "stderr.txt")))
        # Save submission intent before enqueue, so uncertain submission is recoverable.
        write_json(destination / "submission.json", {"command": shell_command, "budget_seconds": timeout})
        result = self.control("add", "--print-task-id", "--working-directory",
                              str(self.run / "workspace"), "--", shell_command)
        self.task_id = (result.directory / "stdout").read_text().strip()
        if not self.task_id.isdecimal():
            raise RuntimeError("private supervisor returned an invalid task id")
        write_json(destination / "supervisor-task.json", {"id": self.task_id})
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            status = self.task_status()
            if isinstance(status, dict) and "Done" in status:
                return status["Done"]
            time.sleep(0.5)
        raise TimeoutError("harness task exceeded its supervised deadline")

    def capture(self, name, argv, timeout=20):
        result = self.execute(argv, "", timeout, label=name)
        if result.get("result") != "Success":
            raise RuntimeError(f"supervised {name} prerequisite failed")
        return self.run / name / "stdout.txt"

    def authorize_nested_lifetime_closure(self, *, snapshot=None):
        """Close every nested Delegate root before stopping this fixture queue."""
        roots = set(getattr(self, "delegate_state_roots", ()))
        for process in getattr(self.processes, "entries", ()):
            argv = getattr(process, "argv", ())
            for index, value in enumerate(argv[:-1]):
                if value == "--root":
                    candidate = Path(argv[index + 1])
                    if not candidate.is_absolute():
                        raise RuntimeError("nested Delegate state root is not absolute")
                    roots.add(candidate.resolve(strict=False))

        closure = {"status": "closed", "roots": []}
        try:
            for root in sorted(roots, key=str):
                root_record = root / "root.json"
                if not root_record.exists() and not root_record.is_symlink():
                    if root.is_dir() and not any(root.iterdir()):
                        continue
                    raise RuntimeError("nested Delegate state root has no root record: " + str(root))
                result = validate_managed_lifetime_closure(root)
                closure["roots"].append({
                    "path": str(root), "root_id": result["root_id"],
                    "task_count": len(result["tasks"]),
                    "inspection_count": len(result["inspections"]),
                })
            if self.daemon is not None:
                validate_nested_queue_roots(snapshot, {entry["root_id"] for entry in closure["roots"]})
        except BaseException as error:
            failure = {
                "status": "failed",
                "roots": [{"path": str(root)} for root in sorted(roots, key=str)],
                "error_type": type(error).__name__,
            }
            failure_path = self.run / "nested-lifetime-closure.json"
            if not failure_path.exists():
                write_json(failure_path, failure)
            raise
        self.nested_lifetime_closure = closure
        write_json(self.run / "nested-lifetime-closure.json", closure)
        return closure

    def close(self):
        for process in self.processes.entries:
            if process is not self.daemon and process.poll() is None:
                process.wait(1)  # An unobservable control retains ownership; stop the suite.
        # The registry also owns starts whose launch is uncertain; closure needs its verdict.
        excluded = () if self.daemon is None else (self.daemon,)
        if not self.processes.finished_naturally(exclude=excluded):
            raise RuntimeError("owned process closure not observed; ownership retained")
        if self.daemon is None:
            self.authorize_nested_lifetime_closure()
            return
        # Stop scheduling, remove only work that never started, and wait for
        # running fixture or Delegate tasks to end through their own lifecycle.
        # This acceptance owner never signals a provider or managed worker.
        self.control("pause", "--all", "--wait")
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            snapshot = self.control("status", "--json").json()
            active = {str(key): row["status"] for key, row in snapshot["tasks"].items()
                      if not isinstance(row["status"], dict) or "Done" not in row["status"]}
            if not active:
                break
            for key, status in active.items():
                kind = next(iter(status)) if isinstance(status, dict) else status
                if kind in ("Queued", "Stashed", "Locked"):
                    self.control("remove", key, expected={0, 1})
            time.sleep(0.2)
        else:
            raise RuntimeError("private supervisor stop not observed; ownership retained")
        # The outer fixture queue is independently complete at this point.
        # Nested Delegate records still require the shared lifetime authority.
        self.authorize_nested_lifetime_closure(snapshot=snapshot)
        self.control("shutdown")
        self.daemon.wait(30, expected=0)
        write_json(self.run / "supervisor-closed.json", {"termination_observed": True})


def routing_hook_arguments(harness, workspace):
    if harness not in ("claude", "codex"):
        raise ValueError("automatic routing hooks are supported only for Claude and Codex")
    location = ".claude" if harness == "claude" else ".agents"
    script = workspace / location / "skills/agent-integration/scripts/delegate-routing-context.sh"
    commands = {event: shlex.join(["/bin/sh", str(script), event])
                for event in ("SessionStart", "SubagentStart")}
    if harness == "claude":
        hooks = {event: [{"hooks": [{"type": "command", "command": command}]}]
                 for event, command in commands.items()}
        return ["--settings", json.dumps({"hooks": hooks})]
    # Test-only, invocation-scoped trust for the reviewed candidate helper.
    # This does not prove that installation grants trust in an ordinary session.
    args = ["--dangerously-bypass-hook-trust"]
    for event, command in commands.items():
        args += ["-c", "hooks." + event + "=[{hooks=[{type=\"command\",command="
                 + json.dumps(command) + "}]}]"]
    return args


def _candidate_environment(run, ownership, env, delegate_bin, *, state_root, explicit_root) -> Path:
    # Resolve once, BEFORE PATH contains our shim. Programmatic callers must
    # meet the same isolation boundary as callers supplying --delegate-bin.
    selected = delegate_bin if delegate_bin is not None else shutil.which(
        "delegate", path=env.get("PATH", os.defpath))
    if selected is None:
        raise FileNotFoundError("candidate Delegate binary unavailable")
    delegate_bin = Path(selected).resolve()
    if not delegate_bin.is_file() or not os.access(delegate_bin, os.X_OK):
        raise FileNotFoundError("candidate Delegate binary missing; build before acceptance")
    candidate_path = ownership.create(
        "candidate Delegate path", run / "candidate-bin", run).directory
    executable = candidate_path / "delegate"
    source = (REPO / "scripts/skill_routing_root_shim.py").read_text(encoding="utf-8")
    executable.write_text("#!" + sys.executable + "\n" + source.split("\n", 1)[1], encoding="utf-8")
    executable.chmod(0o700)
    calls = run / "fixture/agent-calls.jsonl"
    calls.parent.mkdir()
    calls.touch(exist_ok=False)
    write_json(candidate_path / "runtime.json", {
        "delegate": str(delegate_bin), "backend": str(delegate_bin),
        "state_root": str(state_root), "explicit_root": explicit_root, "calls": str(calls),
    })
    env["PATH"] = str(candidate_path) + os.pathsep + env.get("PATH", "")
    candidate_env = run / "candidate-env.sh"
    previous = env.get("BASH_ENV")
    setup = (". " + shlex.quote(previous) + "\n") if previous else ""
    setup += "export PATH=" + shlex.quote(str(candidate_path)) + ':"$PATH"\n'
    candidate_env.write_text(setup, encoding="utf-8")
    env["BASH_ENV"] = str(candidate_env)
    return delegate_bin


def _candidate_shadow_reason(harness) -> str | None:
    if harness == "claude":
        # Personal Claude skills take precedence over project skills of the same name.
        personal = Path(os.environ.get("CLAUDE_CONFIG_DIR", str(Path.home() / ".claude"))) / "skills/agent-integration"
        if (personal / "SKILL.md").exists() and candidate_digests(personal) != candidate_digests():
            return "claude_candidate_shadowed_by_personal_skill"
    if harness == "hermes":
        # Installed Hermes reads HERMES_HOME/skills, not project .hermes/skills.
        installed = Path(os.environ.get("HERMES_HOME", str(Path.home() / ".hermes"))) / "skills/agent-integration"
        if candidate_digests(installed) != candidate_digests():
            return "hermes_candidate_not_installed"
    return None


def capture_candidate_metadata(owner, delegate_bin):
    """Observe Delegate's own identity/catalog; provider releases are not queried."""
    version = owner.capture("delegate-version", [str(delegate_bin), "--version"])
    catalog = owner.capture("delegate-catalog", [str(delegate_bin), "providers", "--json"])
    return {"delegate_version": version.read_text().strip(), "delegate_catalog_sha256": digest(catalog)}


def _prepare_routing_fixture(run, owner, case, delegate_bin, *, state_root, ownership) -> None:
    fixture = run / "fixture"
    configuration = run / "candidate-bin/runtime.json"
    runtime = read_json(configuration)
    if not delegate_bin.is_absolute() or not delegate_bin.samefile(Path(runtime["delegate"])):
        raise ValueError("routing fixture requires the absolute real candidate")
    backend = ownership.create("routing fixture executables", fixture / "bin", fixture).directory
    private_path = backend / "supervisor-bin"
    private_path.mkdir()
    for name, path in (("pueue", owner.client), ("pueued", owner.daemon_path)):
        (private_path / name).symlink_to(Path(path).resolve())
    write_json(fixture / "runtime.json", {
        "delegate": str(delegate_bin), "path": str(private_path),
        "state_root": str(state_root),
        "case": case["name"],
        "scenario": case.get("fixture_scenario"),
        "provider": case.get("provider") if isinstance(case.get("provider"), str) else None,
    })
    if "paired_case_id" in case:
        materialize_diagnosis(fixture, run / "workspace", state_root, case)
    executable = backend / "delegate"
    shutil.copyfile(REPO / "scripts/skill_routing_delegate_fixture.py", executable)
    executable.chmod(0o700)
    owner.processes.environment["DELEGATE_ROUTING_FIXTURE"] = str(fixture)
    # Keep the root shim first. The fixture is only its backend, never a new
    # PATH entry; its own runtime keeps the absolute real candidate identity.
    runtime["backend"] = str(executable)
    configuration.write_text(json.dumps(runtime), encoding="utf-8")


def _finalize_routing_attempt(run, owner, ownership, receipt) -> None:
    supervisor_closed = owner is None
    if owner is not None:
        try:
            owner.close()
            supervisor_closed = True
        except (OSError, ValueError, RuntimeError, KeyError):
            receipt.update(status="failed", reason="supervisor_cleanup_unproven")
    calls = run / "fixture/agent-calls.jsonl"
    if receipt["status"] == "unreviewed" and not calls.is_file():
        receipt.update(status="failed", reason="root_isolation_evidence_invalid")
    if calls.exists():
        try:
            if any(json.loads(line)["error"] is not None for line in calls.read_text().splitlines()):
                if receipt.get("reason") != "supervisor_cleanup_unproven":
                    receipt.update(status="failed", reason="root_isolation_violation")
        except (OSError, ValueError, KeyError, TypeError):
            if receipt.get("reason") != "supervisor_cleanup_unproven":
                receipt.update(status="failed", reason="root_isolation_evidence_invalid")
    owned_directories = ownership.transfer()
    candidate_raw_token = None if owner is None else getattr(owner.processes, "capture_root", None)
    raw_token = candidate_raw_token if isinstance(candidate_raw_token, OwnedDirectory) else None
    try:
        cleaned = finalize_acceptance_artifacts(
            run,
            supervisor_closed,
            owned_directories,
            receipt["status"] not in ("failed", "blocked"),
            raw_process_captures_token=raw_token,
        )
        if not cleaned and supervisor_closed:
            receipt.update(status="failed", reason="acceptance_artifact_cleanup_failed")
    except (OSError, RuntimeError, ValueError) as error:
        receipt.update(status="failed", reason="acceptance_artifact_cleanup_failed",
                       error_type=type(error).__name__)
    for name in ("stdout.txt", "stderr.txt", "fixture/agent-calls.jsonl", "fixture/calls.jsonl",
                 "fixture/routing-events.jsonl", "fixture/routing-state.json"):
        if (run / name).exists():
            receipt[name + "_sha256"] = digest(run / name)
    write_json(run / "receipt.json", receipt)


def routing_prompt(case, state_root):
    mode = case.get("root_mode", "default")
    if mode not in ("default", "explicit"):
        raise ValueError("unknown routing root mode")
    if mode == "explicit" and "@ROOT@" not in case["prompt"]:
        raise ValueError("explicit-root cases require a prompt containing @ROOT@")
    command_prefix = "delegate --root " + shlex.quote(str(state_root))
    suffix = f"\nBegin every Delegate invocation with `{command_prefix}`, before the command." if mode == "explicit" else ""
    return case["prompt"].replace("@ROOT@", str(state_root)) + suffix


def run_case(*, output, harness, model, case, repetition, mode="live", budget=180,
             routing_context=False, delegate_bin=None):
    run = output / f"{case['name']}-{repetition}"
    run.mkdir(mode=0o700)
    ownership = DirectoryOwner()
    try:
        workspace = run / "workspace"
        workspace = ownership.create("disposable workspace", workspace, run).directory
        prepare_workspace(workspace, harness)
        diagnosis = "paired_case_id" in case
        if diagnosis and (harness != "codex" or mode != "routing" or not routing_context):
            raise ValueError("paired diagnosis requires Codex routing with candidate context")
        # Keep supervisor state beside the captured evidence. This makes the
        # acceptance run self-contained and works when the user's home is read-only.
        state_path = Path(tempfile.mkdtemp(prefix="state-", dir=output))
        state = ownership.adopt_created("Delegate state", state_path, output).directory
        env = dict(os.environ)
        env.pop("DELEGATE_PUEUE_CONFIG", None)
        prompt = routing_prompt(case, state)
        (run / "prompt.txt").write_text(prompt, encoding="utf-8")
        argv, stdin = command(harness, model, prompt, diagnosis=diagnosis)
        if diagnosis:
            argv += diagnosis_arguments(workspace, run / "fixture")
        if routing_context and not diagnosis:
            argv += routing_hook_arguments(harness, workspace)
        receipt = {"case": case, "repetition": repetition, "harness": harness,
                   "model_requested": model, "argv": argv, "status": "unreviewed", "mode": mode,
                   "budget_seconds": budget, "state_root": str(state), "candidate": candidate_digests(),
                   "routing_context": routing_context,
                   "hook_trust": "disabled" if diagnosis else (
                       "vetted-per-invocation" if routing_context and harness == "codex" else "native"),
                   "fixture_scenario": case.get("fixture_scenario") if mode == "routing" else None}
        if delegate_bin is not None:
            delegate_bin = Path(delegate_bin).resolve()
            receipt["delegate_executable"] = str(delegate_bin)
    except BaseException:
        ownership.rollback()
        raise
    owner = None
    try:
        shadow_reason = _candidate_shadow_reason(harness)
        if shadow_reason is not None:
            receipt.update(status="blocked", reason=shadow_reason)
            return receipt
        delegate_bin = _candidate_environment(run, ownership, env, delegate_bin,
            state_root=state, explicit_root=case.get("root_mode") == "explicit")
        receipt.update(delegate_executable=str(delegate_bin))
        executable = shutil.which(harness)
        if not executable:
            raise FileNotFoundError("initiating harness missing")
        argv[0] = executable
        owner = AttemptSupervisor(run, env, ownership, state_root=state)
        # This ambient hint does not bind private provider admission. The
        # guard covers only Delegate labels observed in this attempt's queue.
        owner.processes.environment["DELEGATE_PUEUE_CONFIG"] = str(owner.config)
        owner.start()
        git = shutil.which("git")
        if not git:
            raise FileNotFoundError("git is required for disposable provider workspaces")
        owner.capture("git-init", [git, "init", "--quiet", str(workspace)])
        if mode == "routing":
            _prepare_routing_fixture(run, owner, case, delegate_bin, state_root=state, ownership=ownership)
        receipt.update(capture_candidate_metadata(owner, delegate_bin))
        if diagnosis:
            receipt["diagnosis_controls"] = verify_diagnosis_controls(
                owner, executable, workspace, ownership=ownership, model=model)
        receipt["supervisor_result"] = owner.execute(argv, stdin, budget)
    except FileNotFoundError:
        receipt.update(status="blocked", reason="required_executable_missing")
    except TimeoutError:
        receipt.update(status="failed", reason="supervised_deadline_exceeded")
    except (OSError, ValueError, RuntimeError) as error:
        # Detailed diagnostics stay in the private process evidence.
        receipt.update(status="blocked", reason="prerequisite_or_supervisor_failed",
                       error_type=type(error).__name__)
    finally:
        _finalize_routing_attempt(run, owner, ownership, receipt)
    print(f"{run.name}: {receipt['status']}", flush=True)
    return receipt


def summarize(*, output, cases, repetitions, harness, mode="live", model=None, budget=180,
              routing_context=False, delegate_bin=None):
    rows = []
    for case in cases:
        for repetition in range(1, repetitions + 1):
            run = output / f"{case['name']}-{repetition}"
            row = {"case": case["name"], "repetition": repetition, "status": "not_run"}
            if (run / "receipt.json").exists():
                receipt = read_json(run / "receipt.json")
                if (receipt.get("harness") != harness or receipt.get("case") != case
                        or receipt.get("repetition") != repetition
                        or receipt.get("mode") != mode or receipt.get("model_requested") != model
                        or receipt.get("routing_context", False) != routing_context
                        or receipt.get("fixture_scenario") != (case.get("fixture_scenario") if mode == "routing" else None)
                        or (delegate_bin is not None
                            and receipt.get("delegate_executable") != str(Path(delegate_bin).resolve()))
                        or receipt.get("candidate") != candidate_digests()
                        or receipt.get("budget_seconds") != budget):
                    raise ValueError(f"receipt selection mismatch: {run}")
                row["status"] = receipt["status"]
                judgment_path = run / "judgment.json"
                if judgment_path.exists():
                    judgment = read_json(judgment_path)
                    # A human/root reviewer must inspect actual tool calls, not model prose.
                    if (judgment.get("receipt_sha256") != digest(run / "receipt.json")
                            or judgment.get("status") not in ("passed", "failed", "blocked")
                            or not isinstance(judgment.get("evidence"), str)
                            or not judgment["evidence"].strip()):
                        raise ValueError(f"invalid or stale judgment: {judgment_path}")
                    for name in ("stdout.txt", "stderr.txt", "fixture/agent-calls.jsonl", "fixture/calls.jsonl",
                                 "fixture/routing-events.jsonl", "fixture/routing-state.json"):
                        expected = receipt.get(name + "_sha256")
                        if expected is not None and digest(run / name) != expected:
                            raise ValueError(f"changed transcript: {run / name}")
                    if receipt["status"] in ("failed", "blocked") and judgment["status"] == "passed":
                        raise ValueError(f"unsuccessful capture cannot pass: {run}")
                    if "paired_case_id" in case and judgment["status"] == "passed":
                        controls = receipt.get("diagnosis_controls")
                        if (not isinstance(controls, dict)
                                or controls.get("network_and_ipc_connections") != "denied"
                                or controls.get("routing_delivery") != "inline-isolated"
                                or controls.get("legacy_notify") != "disabled"
                                or controls.get("disabled_features") != list(DIAGNOSIS_DISABLED_FEATURES)
                                or controls.get("routing_rule_sha256") != digest(SKILL / "references/routing-rule.json")
                                or controls.get("proof_sha256") != digest(run / "diagnosis-sandbox/stdout.txt")
                                or controls.get("context_sha256") != digest(run / "diagnosis-context/stdout.txt")):
                            raise ValueError(f"diagnosis isolation was not verified: {run}")
                    if (mode == "routing" and case["external"] and case.get("provider") != "unavailable"
                            and case.get("native_alternative_for") != harness
                            and judgment["status"] == "passed"
                            and "fixture/calls.jsonl_sha256" not in receipt):
                        raise ValueError(f"routing fixture was not observed: {run}")
                    if (mode == "routing" and case.get("fixture_scenario")
                            and judgment["status"] == "passed"
                            and ("fixture/routing-events.jsonl_sha256" not in receipt
                                 or "fixture/routing-state.json_sha256" not in receipt)):
                        raise ValueError(f"synthetic routing evidence was not observed: {run}")
                    row["status"] = judgment["status"]
            rows.append(row)
    status = "failed" if any(r["status"] == "failed" for r in rows) else (
        "passed" if rows and all(r["status"] == "passed" for r in rows) else "blocked")
    return {"status": status, "harness": harness, "mode": mode, "model_requested": model,
            "candidate": candidate_digests(), "attempts": rows}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--harness", choices=("codex", "claude", "hermes"), required=True)
    parser.add_argument("--model")
    parser.add_argument("--delegate-bin", type=Path, default=REPO / "bin/delegate")
    parser.add_argument("--routing-context", action="store_true",
                        help="exercise candidate context hooks; Codex trust is vetted only for this invocation")
    parser.add_argument("--mode", choices=("routing", "live"), default="routing")
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--split", choices=("development", "validation", "all"), default="all")
    parser.add_argument("--case", action="append", default=[])
    parser.add_argument("--repetitions", type=int, default=3)
    parser.add_argument("--summarize", action="store_true")
    parser.add_argument("--budget-seconds", type=int, default=180)
    args = parser.parse_args()
    if args.routing_context and args.harness == "hermes":
        parser.error("Hermes currently supports skill-only acceptance")
    suite = read_json(CASES)["cases"]
    paired_scenarios(suite)
    names = {c["name"] for c in suite}
    if args.repetitions < 1 or args.budget_seconds < 1 or set(args.case) - names:
        parser.error("positive repetitions and known case names are required")
    cases = [c for c in suite if (args.split == "all" or c["split"] == args.split)
             and (not args.case or c["name"] in args.case)]
    if args.harness != "codex" or args.mode != "routing" or not args.routing_context:
        if args.case and any("paired_case_id" in case for case in cases):
            parser.error("paired diagnosis requires Codex routing with candidate context")
        cases = [case for case in cases if "paired_case_id" not in case]
    if not cases:
        parser.error("selection contains no cases")
    output = args.output.resolve()
    if args.summarize:
        report = summarize(output=output, cases=cases, repetitions=args.repetitions, harness=args.harness,
            mode=args.mode, model=args.model, budget=args.budget_seconds, routing_context=args.routing_context,
            delegate_bin=args.delegate_bin)
        print(json.dumps(report, indent=2))
        return {"passed": 0, "failed": 1, "blocked": 2}[report["status"]]
    output.mkdir(parents=True, exist_ok=False, mode=0o700)
    for case in cases:
        for repetition in range(1, args.repetitions + 1):
            receipt = run_case(output=output, harness=args.harness, model=args.model, case=case, repetition=repetition,
                mode=args.mode, budget=args.budget_seconds, routing_context=args.routing_context, delegate_bin=args.delegate_bin)
            if receipt.get("reason") == "supervisor_cleanup_unproven":
                return 1
    # Executing a harness is not proof of routing success.
    return 2


if __name__ == "__main__":
    sys.exit(main())
