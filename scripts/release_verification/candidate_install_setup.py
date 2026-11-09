"""Packed onboarding observations inside the caller-owned candidate workspace.

ConsumerPorts owns each foreground child; the workspace owner retains all files on
failure. The existing bootstrap probe remains the sole owner of task and supervisor
closure. Only distribution archives, installer sources and skill files are hashed.
"""

from __future__ import annotations

from dataclasses import dataclass
import json
import os
import shlex
from pathlib import Path

from scripts.acceptance.evidence.source import VerificationError
from scripts.release_verification.candidate_install_contract import CandidateInstallRequest
from scripts.release_verification.candidate_install_contract import package_command_args
from scripts.release_verification.published_contract import npm_environment
from scripts.release_verification.published_files import observe_installed_skill
from scripts.release_verification.published_files import require_installed_skill
from scripts.release_verification.published_ports import ConsumerLayout
from scripts.release_verification.published_ports import ConsumerPorts
from scripts.release_verification.published_ports import tool_path


STAGE = "candidate-setup"


@dataclass(frozen=True)
class CandidateSetup:
    """One channel's isolated setup session, with a package bound by the caller."""

    request: CandidateInstallRequest
    ports: ConsumerPorts
    receipt: dict
    layout: ConsumerLayout
    channel: str
    base: Path
    command_directory: Path
    extra_environment: dict[str, str]
    select_directory: bool = True

    @property
    def home(self) -> Path:
        return self.base / "setup-home"

    def environment(self) -> dict[str, str]:
        env = npm_environment(
            path=os.pathsep.join((str(self.command_directory), tool_path(self.ports, ("npm", "node")))),
            home=str(self.home), cache=str(self.layout.cache), user_npmrc=str(self.layout.user_npmrc),
            global_npmrc=str(self.layout.global_npmrc), inherited=self.ports.inherited)
        return {**env, **self.extra_environment, "npm_config_offline": "true",
                # Exact package metadata must beat an inherited release selection.
                "DELEGATION_LAYER_VERSION": "v0.0.0"}

    def setup(self, label: str, provider_arguments: tuple[str, ...] = ()) -> bytes:
        npm = self.ports.which("npm")
        package = self.request.files.package
        if npm is None or package is None:
            raise VerificationError("candidate_setup_prerequisite_missing", STAGE)
        args = ["setup", "--yes", "--scope", "global", "--harness", "claude", "--routing", "skill-only"]
        if self.select_directory:
            args += ["--install-dir", str(self.command_directory)]
        args += provider_arguments
        env = self.environment()
        if provider_arguments:
            env["PATH"] = os.pathsep.join((str(self.base / "setup-providers"), env["PATH"]))
        return self.ports.run(
            self.receipt, f"candidate-{self.channel}-setup-{label}",
            [npm, *package_command_args(package, args)], env,
            capture=True, timeout=600, cwd=self.layout.cwd)

    def fresh(self) -> None:
        self.home.mkdir(mode=0o700)
        self._require_output(self.setup("fresh"))

    def verify_repeat(self, delegate: Path) -> dict[str, object]:
        skill = self.home / ".claude" / "skills" / "agent-integration"
        before = observe_installed_skill(skill)
        require_installed_skill(self.request.skill_digests(), before, "candidate_setup_skill_mismatch", STAGE)
        output = self.setup("repeat")
        self._require_output(output)
        require_installed_skill(self.request.skill_digests(), observe_installed_skill(skill),
                                "candidate_setup_repeat_changed_skill", STAGE)
        # No provider was selected: the printed lead must require one, never launch it.
        try:
            prompt = output.decode("utf-8")
        except UnicodeError:
            raise VerificationError("candidate_setup_output_invalid", STAGE) from None
        if ("choose a provider and workspace" not in prompt or "dispatch/status/collect" not in prompt or
                "First-task example" in prompt):
            raise VerificationError("candidate_setup_first_task_missing", STAGE)
        raw = self.ports.run(
            self.receipt, f"candidate-{self.channel}-doctor", [str(delegate), "doctor", "--json"],
            self.environment(), capture=True, timeout=60, cwd=self.layout.cwd)
        try:
            report = json.loads(raw)
        except (UnicodeError, ValueError, RecursionError):
            raise VerificationError("candidate_setup_doctor_invalid", STAGE) from None
        if not isinstance(report, dict) or report.get("schema_version") != 1 or report.get("command") != "doctor":
            raise VerificationError("candidate_setup_doctor_invalid", STAGE)
        root, installation, supervisor = (report.get(key) for key in ("root", "installation", "supervisor"))
        if (not isinstance(root, dict) or root.get("status") != "not_initialized" or
                not isinstance(root.get("path"), str) or not Path(root["path"]).is_absolute() or
                Path(root["path"]).exists() or not isinstance(installation, dict) or
                installation.get("status") != "observed" or installation.get("scope") != "installation_bundle" or
                installation.get("verification") != "filesystem_metadata" or
                installation.get("compatibility") != "not_checked" or not isinstance(supervisor, dict) or
                any(supervisor.get(key) != value for key, value in
                    (("binding", "not_initialized"), ("liveness", "not_checked"), ("active_work", "unknown")))):
            raise VerificationError("candidate_setup_doctor_mismatch", STAGE)
        roles = installation.get("roles")
        if not isinstance(roles, dict) or any(
                not isinstance(roles.get(name), dict) or roles[name].get("status") != "available"
                for name in ("delegate", "delegate_run", "pueue", "pueued")):
            raise VerificationError("candidate_setup_doctor_mismatch", STAGE)
        observed_delegate = roles["delegate"].get("path")
        if not isinstance(observed_delegate, str) or Path(observed_delegate).resolve() != delegate.resolve():
            raise VerificationError("candidate_setup_doctor_mismatch", STAGE)
        recipe = self.verify_recipe(delegate)
        return {"recipe": recipe, "package_version": self.request.package_version(), "fresh": "completed", "repeat": "completed",
                "skill_files": len(before.files), "first_task": "provider-choice-required", "doctor": report}

    def _require_output(self, output: bytes) -> None:
        expected = (f"Package: delegation-layer {self.request.package_version()}", "Setup completed.",
                    "Skill activation remains unknown")
        if not all(value.encode() in output for value in expected):
            raise VerificationError("candidate_setup_incomplete", STAGE)

    def verify_recipe(self, delegate: Path) -> str:
        """Check the printed opt-in recipe and prove passive setup never runs the provider."""
        providers = self.base / "setup-providers"
        providers.mkdir(mode=0o700)
        launched = providers / "launched"
        fixture = providers / "pi"
        fixture.write_text("#!/bin/sh\nprintf launched > " + shlex.quote(str(launched)) + "\nexit 97\n", encoding="utf-8")
        fixture.chmod(0o700)
        output = self.setup("recipe", ("--provider", "pi:json", "--cwd", str(self.layout.cwd)))
        self._require_output(output)
        if launched.exists():
            raise VerificationError("candidate_setup_launched_provider", STAGE)
        try:
            recipe = output.decode("utf-8")
        except UnicodeError:
            raise VerificationError("candidate_setup_output_invalid", STAGE) from None
        try:
            commands = [shlex.split(line) for line in recipe.splitlines()
                        if " dispatch " in line or " status " in line or " collect " in line]
        except ValueError:
            raise VerificationError("candidate_setup_recipe_invalid", STAGE) from None
        dispatch = next((line for line in commands if line[:1] == ["if"] and line[2:3] == ["dispatch"]), [])
        expected = [str(delegate), "dispatch", "--provider", "pi:json", "--cwd", str(self.layout.cwd)]
        # The recipe may refer to the selected symlink rather than its resolved target.
        if (len(dispatch) < 2 or dispatch[0] != "if" or
                Path(dispatch[1]).resolve() != delegate.resolve() or dispatch[2:7] != expected[1:] or
                not all(token in dispatch for token in ("--brief", "--permission", "read-only", "--budget", "--json")) or
                not all(any(line[:2] == [dispatch[1], operation] for line in commands)
                        for operation in ("status", "collect")) or
                "DELEGATE_BRIEF" not in recipe or "task_record is created" not in recipe or
                " cancel " not in recipe or "evidence" not in recipe):
            raise VerificationError("candidate_setup_recipe_invalid", STAGE)
        return "validated-absolute-command-provider-workspace-read-only"
