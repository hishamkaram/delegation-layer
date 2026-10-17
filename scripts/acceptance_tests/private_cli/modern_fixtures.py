"""Plain composition fixtures for modern cross-binary tests; no TestCase exports."""

import json
from pathlib import Path
import shlex
import sys
import tempfile

from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from scripts.delegate_acceptance.processes.operations import Processes
from scripts.delegate_acceptance.private_cli.contention_controls import ModernFixture, install_contention_client
from scripts.delegate_acceptance.private_cli.contention_policy import ModernInstallation
from scripts.delegate_acceptance.tasks.operations import NativeTaskOps
from scripts.acceptance_tests.private_cli.modern_application_fixture import migration_response, publish_pair


class ModernControlFixture:
    def __init__(self, test):
        self.owner = DirectoryOwner()
        parent = Path(tempfile.gettempdir()).resolve()
        root = Path(tempfile.mkdtemp(prefix="modern-contention-", dir=parent)).resolve()
        self.root = self.owner.adopt_created("modern test root", root, parent)
        test.addCleanup(self.cleanup)
        self.processes = None
        self.state = self.owner.create("modern state", root / "state", root).directory
        self.supervisor = self.owner.create("modern supervisor", self.state / ".supervisor", self.state).directory
        self.owner.create("modern tasks", self.state / "tasks", self.state)
        self.controls = self.owner.create("modern control ports", root / "controls", root)
        captures = self.owner.create("modern process captures", root / "captures", root)
        self.processes = Processes(
            captures.directory,
            capture_root=captures,
            environment={"HOME": str(root), "PATH": "/usr/bin:/bin", "LANG": "C.UTF-8"},
        )
        self.task_id = "b" * 32
        self.root_id = "a" * 32
        (self.state / "root.json").write_text(json.dumps({"schema_version": 1, "root_id": self.root_id}))
        (self.supervisor / "pueue.yml").write_text("finite test config\n")
        (self.supervisor / "bootstrap.lock").touch(mode=0o600)
        self.baseline = self.install("baseline")
        self.candidate = self.install("candidate")
        self.options = {
            "baseline_libexec": str(self.baseline.libexec),
            "candidate_libexec": str(self.candidate.libexec),
            "active": True,
            "baseline_prepared_observation_error": True,
        }
        self.write_options()
        publish_pair(self.state, self.baseline.libexec)

    def cleanup(self):
        if self.processes is not None:
            pending = self.processes.drain(40)
            if pending or not self.processes.finished_naturally():
                self.owner.release()
                raise RuntimeError("finite test processes retained: " + str(self.root.directory))
        self.owner.rollback()

    def install(self, role):
        directory = self.owner.create(role + " bundle", self.root.directory / role, self.root.directory).directory
        libexec = self.owner.create(role + " libexec", directory / "libexec", directory).directory
        delegate = directory / "delegate"
        application = Path(__file__).with_name("modern_application_fixture.py")
        delegate.write_text(
            "#!/bin/sh\nexec " + shlex.join([sys.executable, str(application), "--role", role]) + ' "$@"\n'
        )
        delegate.chmod(0o700)
        runner = directory / "delegate-run"
        runner.write_text(role + " finite runner fingerprint; never executed\n")
        runner.chmod(0o700)
        (libexec / "pueued").write_text(role + " finite daemon fingerprint; never executed\n")
        (libexec / "pueued").chmod(0o700)
        install_contention_client(libexec / "pueue", Path("/usr/bin/true"), self.controls)
        return ModernInstallation(delegate, runner, libexec)

    def write_options(self, **changes):
        self.options.update(changes)
        (self.state / "fixture-options.json").write_text(json.dumps(self.options))

    def minimal_task(self):
        directory = self.owner.create(
            "modern supplied task", self.state / "tasks" / self.task_id, self.state / "tasks"
        ).directory
        (directory / "meta.json").write_text("unchanged\n")
        return directory

    def prepare_migration(self):
        """Seed the precise pre-shutdown journal/alias fixture, never native proof."""
        pair = publish_pair(self.state, self.candidate.libexec, activate=False)
        response, code = migration_response(
            self.state, {**self.options, "prepare_intent": True}, "candidate", self.root_id, pair
        )
        if code != 1 or response["supervisor_control"]["state"] != "blocked":
            raise RuntimeError("finite migration fixture did not preserve active work")

    def task_operations(self):
        return NativeTaskOps(
            self.processes, self.baseline.delegate, self.baseline.runner, self.baseline.libexec / "pueue",
            self.root.directory, self.state, self.controls.directory,
            expected_inspection_binding_required=False, managed_runner=True,
        )

    def modern_input(self):
        brief = self.root.directory / "brief.md"
        brief.write_text("finite provider fixture\n")
        return ModernFixture(self.controls, brief, self.root.directory, self.root.directory / "provider-gate",
                             self.root.directory, "test-nonce")
