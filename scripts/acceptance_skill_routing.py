#!/usr/bin/env python3
"""Capture natural-prompt skill selection. Transcripts require evidence-backed review.

Run with the normal harness profile so competing skills/plugins remain visible.
Only disposable workspaces receive the candidate skill; no user setup is edited.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import shlex
import tempfile
import time

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


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def read_json(path):
    return json.loads(path.read_text(encoding="utf-8"))


def write_json(path, value):
    with path.open("x", encoding="utf-8") as stream:
        json.dump(value, stream, indent=2)
        stream.write("\n")


def command(harness, model, prompt):
    if harness == "codex":
        argv = ["codex", "exec", "--json", "--skip-git-repo-check",
                "--sandbox", "danger-full-access", "-c", 'approval_policy="never"']
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


def prepare_workspace(path, harness):
    location = {"codex": ".agents", "claude": ".claude", "hermes": ".hermes"}[harness]
    shutil.copytree(SKILL, path / location / "skills/agent-integration")
    (path / "pricing.py").write_text(
        "def discounted_price(price, percent):\n    return price - percent\n", encoding="utf-8")
    (path / "README.md").write_text(
        "# Pricing fixture\nDiscounts are percentages: discounted_price(200, 10) should be 180.\n",
        encoding="utf-8")


def candidate_digests(root=SKILL):
    return {str(p.relative_to(root)): digest(p)
            for p in sorted(root.rglob("*")) if p.is_file()}


class AttemptSupervisor:
    """Own only this attempt's private daemon; stopping always goes through Pueue."""

    def __init__(self, run, environment, ownership):
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
        configured_root = self.processes.environment.get("DELEGATE_ROOT")
        if configured_root:
            configured_path = Path(configured_root)
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

    def authorize_nested_lifetime_closure(self):
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
                    "path": str(root),
                    "task_count": len(result["tasks"]),
                    "inspection_count": len(result["inspections"]),
                })
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
        self.authorize_nested_lifetime_closure()
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


def executable_digest(path):
    try:
        return digest(path) if path.is_file() else None
    except OSError:
        return None


def run_case(output, harness, model, case, repetition, mode="live", budget=180,
             routing_context=False, delegate_bin=None):
    run = output / f"{case['name']}-{repetition}"
    run.mkdir(mode=0o700)
    ownership = DirectoryOwner()
    workspace = run / "workspace"
    workspace = ownership.create("disposable workspace", workspace, run).directory
    prepare_workspace(workspace, harness)
    prompt = case["prompt"]
    (run / "prompt.txt").write_text(prompt, encoding="utf-8")
    argv, stdin = command(harness, model, prompt)
    if routing_context:
        argv += routing_hook_arguments(harness, workspace)
    # Keep supervisor state beside the captured evidence. This makes the
    # acceptance run self-contained and works when the user's home is read-only.
    state_path = Path(tempfile.mkdtemp(prefix="state-", dir=output))
    state = ownership.adopt_created("Delegate state", state_path, output).directory
    env = dict(os.environ, DELEGATE_ROOT=str(state))
    env.pop("DELEGATE_PUEUE_CONFIG", None)
    candidate_path = None
    if delegate_bin is not None:
        delegate_bin = Path(delegate_bin).resolve()
        candidate_path = run / "candidate-bin"
        candidate_path = ownership.create(
            "candidate Delegate path", candidate_path, run).directory
        (candidate_path / "delegate").symlink_to(delegate_bin)
        env["PATH"] = str(candidate_path) + os.pathsep + env.get("PATH", "")
        candidate_env = run / "candidate-env.sh"
        previous = env.get("BASH_ENV")
        setup = (". " + shlex.quote(previous) + "\n") if previous else ""
        setup += "export PATH=" + shlex.quote(str(candidate_path)) + ':"$PATH"\n'
        candidate_env.write_text(setup, encoding="utf-8")
        env["BASH_ENV"] = str(candidate_env)
    receipt = {"case": case, "repetition": repetition, "harness": harness,
               "model_requested": model, "argv": argv, "status": "unreviewed", "mode": mode,
               "budget_seconds": budget, "state_root": str(state), "candidate": candidate_digests(),
               "routing_context": routing_context,
               "hook_trust": "vetted-per-invocation" if routing_context and harness == "codex" else "native",
               "fixture_scenario": case.get("fixture_scenario") if mode == "routing" else None}
    if delegate_bin is not None:
        receipt["delegate_executable"] = str(delegate_bin)
        receipt["delegate_sha256"] = executable_digest(delegate_bin)
    owner = None
    try:
        if harness == "claude":
            # Personal Claude skills take precedence over project skills of the same name.
            personal = Path(os.environ.get("CLAUDE_CONFIG_DIR", str(Path.home() / ".claude"))) / "skills/agent-integration"
            if (personal / "SKILL.md").exists() and candidate_digests(personal) != candidate_digests():
                receipt.update(status="blocked", reason="claude_candidate_shadowed_by_personal_skill")
                return receipt
        if harness == "hermes":
            # Installed Hermes reads HERMES_HOME/skills, not project .hermes/skills.
            installed = Path(os.environ.get("HERMES_HOME", str(Path.home() / ".hermes"))) / "skills/agent-integration"
            if candidate_digests(installed) != candidate_digests():
                receipt.update(status="blocked", reason="hermes_candidate_not_installed")
                return receipt
        if delegate_bin is not None:
            if not delegate_bin.is_file() or not os.access(delegate_bin, os.X_OK):
                raise FileNotFoundError("candidate Delegate binary missing; build before acceptance")
        executable = shutil.which(harness)
        if not executable:
            raise FileNotFoundError("initiating harness missing")
        argv[0] = executable
        owner = AttemptSupervisor(run, env, ownership)
        # Nested Delegate tasks share only this attempt's queue. This both avoids
        # caller-owned queues and makes outer cleanup observe paid child work.
        owner.processes.environment["DELEGATE_PUEUE_CONFIG"] = str(owner.config)
        owner.start()
        version = owner.capture("version", [executable, "--version"])
        receipt["harness_version"] = version.read_text().strip()
        git = shutil.which("git")
        if not git:
            raise FileNotFoundError("git is required for disposable provider workspaces")
        owner.capture("git-init", [git, "init", "--quiet", str(workspace)])
        if mode == "routing":
            fixture = run / "fixture"
            fixture.mkdir()
            delegate = str(delegate_bin) if delegate_bin is not None else shutil.which("delegate")
            if not delegate:
                raise FileNotFoundError("Delegate catalog executable missing")
            private_path = fixture / "supervisor-bin"
            private_path.mkdir()
            for name, path in (("pueue", owner.client), ("pueued", owner.daemon_path)):
                (private_path / name).symlink_to(Path(path).resolve())
            write_json(fixture / "runtime.json", {
                "delegate": str(Path(delegate).resolve()), "path": str(private_path),
                "delegate_sha256": digest(Path(delegate)),
                "case": case["name"],
                "scenario": case.get("fixture_scenario"),
                "provider": case.get("provider") if isinstance(case.get("provider"), str) else None,
            })
            executable = fixture / "delegate"
            shutil.copyfile(REPO / "scripts/skill_routing_delegate_fixture.py", executable)
            executable.chmod(0o700)
            owner.processes.environment["PATH"] = str(fixture) + os.pathsep + env.get("PATH", "")
            owner.processes.environment["DELEGATE_ROUTING_FIXTURE"] = str(fixture)
            # Bash login profiles can replace PATH. Preserve the caller's BASH_ENV
            # behavior, then restore only this disposable command-recording fixture.
            bash_env = fixture / "bash-env.sh"
            previous = env.get("BASH_ENV")
            setup = (". " + shlex.quote(previous) + "\n") if previous else ""
            setup += "export PATH=" + shlex.quote(str(fixture)) + ':"$PATH"\n'
            bash_env.write_text(setup, encoding="utf-8")
            owner.processes.environment["BASH_ENV"] = str(bash_env)
        if delegate_bin is not None:
            receipt["delegate_version"] = owner.capture("delegate-version", [str(delegate_bin), "--version"]).read_text().strip()
            catalog = owner.capture("delegate-catalog", [str(delegate_bin), "providers", "--json"])
            receipt["delegate_catalog_sha256"] = digest(catalog)
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
        supervisor_closed = owner is None
        if owner is not None:
            try:
                owner.close()
                supervisor_closed = True
            except (OSError, ValueError, RuntimeError, KeyError):
                receipt.update(status="failed", reason="supervisor_cleanup_unproven")
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
        for name in ("stdout.txt", "stderr.txt", "fixture/calls.jsonl",
                     "fixture/routing-events.jsonl", "fixture/routing-state.json"):
            if (run / name).exists():
                receipt[name + "_sha256"] = digest(run / name)
        write_json(run / "receipt.json", receipt)
    print(f"{run.name}: {receipt['status']}", flush=True)
    return receipt


def summarize(output, cases, repetitions, harness, mode="live", model=None, budget=180,
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
                        or (delegate_bin is not None and (receipt.get("delegate_executable") != str(Path(delegate_bin).resolve())
                            or receipt.get("delegate_sha256") != executable_digest(Path(delegate_bin))))
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
                    for name in ("stdout.txt", "stderr.txt", "fixture/calls.jsonl",
                                 "fixture/routing-events.jsonl", "fixture/routing-state.json"):
                        expected = receipt.get(name + "_sha256")
                        if expected is not None and digest(run / name) != expected:
                            raise ValueError(f"changed transcript: {run / name}")
                    if receipt["status"] in ("failed", "blocked") and judgment["status"] == "passed":
                        raise ValueError(f"unsuccessful capture cannot pass: {run}")
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
    names = {c["name"] for c in suite}
    if args.repetitions < 1 or args.budget_seconds < 1 or set(args.case) - names:
        parser.error("positive repetitions and known case names are required")
    cases = [c for c in suite if (args.split == "all" or c["split"] == args.split)
             and (not args.case or c["name"] in args.case)]
    if not cases:
        parser.error("selection contains no cases")
    output = args.output.resolve()
    if args.summarize:
        report = summarize(output, cases, args.repetitions, args.harness, args.mode, args.model, args.budget_seconds, args.routing_context, args.delegate_bin)
        print(json.dumps(report, indent=2))
        return {"passed": 0, "failed": 1, "blocked": 2}[report["status"]]
    output.mkdir(parents=True, exist_ok=False, mode=0o700)
    for case in cases:
        for repetition in range(1, args.repetitions + 1):
            receipt = run_case(output, args.harness, args.model, case, repetition, args.mode, args.budget_seconds, args.routing_context, args.delegate_bin)
            if receipt.get("reason") == "supervisor_cleanup_unproven":
                return 1
    # Executing a harness is not proof of routing success.
    return 2


if __name__ == "__main__":
    sys.exit(main())
