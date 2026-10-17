"""One owner for fixture paths, process registry, and finalization."""

from pathlib import Path
from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from scripts.delegate_acceptance.processes.operations import Processes
from scripts.delegate_acceptance.evidence.files import digest
from scripts.delegate_acceptance.processes.observation import inherited_environment
from scripts.delegate_acceptance.evidence.files import read_json
from scripts.delegate_acceptance.directories.operations import rollback_owned_directories
from scripts.delegate_acceptance.processes.observation import wait_until
from scripts.delegate_acceptance.evidence.files import write_json
from scripts.delegate_acceptance.hermetic.contract import CASE_TIMEOUT
from scripts.delegate_acceptance.hermetic.contract import HarnessFailure
from scripts.delegate_acceptance.hermetic.contract import MAX_CONTROL_BYTES
from scripts.delegate_acceptance.hermetic.contract import MODE
from scripts.delegate_acceptance.hermetic.contract import PREDICATE
from scripts.delegate_acceptance.hermetic.contract import SCHEMA_VERSION
from scripts.delegate_acceptance.hermetic.contract import SUPERVISOR_VERSION
from scripts.delegate_acceptance.hermetic.contract import add_entry_label
from scripts.delegate_acceptance.hermetic.contract import label_parts
from scripts.delegate_acceptance.hermetic.contract import private_pueue_yaml
from scripts.delegate_acceptance.hermetic.contract import require_true
from scripts.delegate_acceptance.hermetic.contract import status_document
from scripts.delegate_acceptance.hermetic.contract import status_job
from scripts.delegate_acceptance.hermetic.evidence import require_bound_records
from scripts.delegate_acceptance.hermetic.files import canonical_executable
from scripts.delegate_acceptance.hermetic.files import copy_executable
from scripts.delegate_acceptance.hermetic.files import owned_directory_path
from scripts.delegate_acceptance.hermetic.files import task_id
from scripts.delegate_acceptance.hermetic.files import write_bytes
from scripts.delegate_acceptance.hermetic.observations import invocation_counts
from scripts.delegate_acceptance.hermetic.observations import observed_supervisor_completions
from scripts.delegate_acceptance.hermetic.observations import wait_entry
import shutil
import tempfile
import traceback


class Case:
    """One fresh task/root/supervisor fixture and its owned processes."""

    def __init__(self, suite, case_id, scenario="success", hook="normal",
                 budget="1m0s", add_release=True, answer=None,
                 provider_kwargs=None):
        self.suite = suite
        self.case_id = case_id
        ownership = DirectoryOwner()
        try:
            temporary_parent = Path("/tmp").resolve(strict=True)
            allocated = Path(tempfile.mkdtemp(
                prefix="dl-hermetic-" + case_id + "-", dir=temporary_parent))
            allocated_parent = allocated.parent.resolve(strict=True)
            allocated = allocated_parent / allocated.name
            self.base_token = ownership.adopt_created(
                "hermetic case", allocated, allocated_parent)
            self.base = owned_directory_path(self.base_token).resolve(strict=True)
            self.base.chmod(0o700)
        except BaseException:
            ownership.rollback()
            raise
        self.outcome = None
        self.root_id = None
        self.helper_completion_observed = None
        # A supervisor completion receipt is authored by the finite helper;
        # only its matching observer event proves that this process also
        # completed its natural Wait.  Keep that fact separate from the
        # helper's own receipt because the dispatcher may exit first.
        self.external_wait_observed = None
        self.external_wait_required = False
        self.external_wait_unknown = {}
        self.task_id = task_id()
        # The ordinary cases own one task.  The focused continuation control
        # deliberately reuses one root and records every chained task ID so
        # its immutable provider receipts can be counted together.
        self.chain_task_ids = [self.task_id]
        self.chain_records = []
        self.numeric_id = 7
        self.budget = budget
        self.answer = answer if answer is not None else (
            "Exact fixture answer: 'quoted' \"double\"\n"
            "literal slash-n token: \\n\n"
            "$(touch DO_NOT_CREATE) ; <&> | literal\n"
        )
        self.provider_kwargs = dict(provider_kwargs or {})
        self.add_release_enabled = add_release
        try:
            self._create_layout()
            self._copy_tools()
            self._write_configs(scenario, hook)
            self.processes = Processes(self.base / "processes", owner=ownership)
        except BaseException:
            ownership.rollback()
            raise
        self.owned_directories = ownership.transfer()

    def _create_layout(self):
        self.bin_dir = self.base / "cli space ' ; literal"
        self.supervisor_dir = self.base / "supervisor space ' ; literal"
        self.work = self.base / "work space ' ; literal"
        self.root = self.base / "task state ' ; literal"
        self.events_dir = self.base / "events"
        self.provider_records = self.base / "provider records"
        self.supervisor_records = self.base / "supervisor records"
        for path in (
            self.bin_dir,
            self.supervisor_dir,
            self.work,
            self.events_dir,
            self.provider_records,
            self.supervisor_records,
        ):
            path.mkdir(mode=0o700, parents=True)
        self.aliases = self.base / "aliases.yml"
        write_bytes(self.aliases, b"{}\n")
        self.production_config = self.base / "pueue config.yml"
        write_bytes(self.production_config, private_pueue_yaml(self.base))
        self.status_path = self.base / "status.json"
        write_json(self.status_path, status_document([]))
        self.fake_config = self.supervisor_dir / "fake-supervisor.json"
        self.add_release = self.base / "add.release"
        self.kill_release = self.base / "kill.release"
        self.remove_release = self.base / "remove.release"
        self.provider_release = self.base / "provider.release"
        self.brief = self.base / "brief ' ; literal.md"
        write_bytes(
            self.brief,
            (
                "Exact brief: 'quoted' \"double\"\n"
                "literal slash-n token: \\n\n"
                "$(touch DO_NOT_CREATE) ; <&> | literal\n"
            ).encode(),
        )

    def _copy_tools(self):
        tools = self.suite.tools
        self.delegate = copy_executable(tools / "delegate", self.bin_dir / "delegate")
        self.runner = copy_executable(tools / "delegate-run", self.bin_dir / "delegate-run")
        self.provider = copy_executable(tools / "provider", self.bin_dir / "provider")
        fake_supervisor = tools / "pueue-fake"
        self.pueue = copy_executable(fake_supervisor, self.supervisor_dir / "pueue")
        copy_executable(fake_supervisor, self.supervisor_dir / "pueued")
        copy_executable(fake_supervisor, self.supervisor_dir / "pueue-fake")
        self.probe = canonical_executable(tools / "harnessprobe")

    def _write_configs(self, scenario, hook):
        provider = {
            "scenario": scenario,
            "artifact_dir": str(self.provider_records),
            "lifetime_ms": int(self.provider_kwargs.pop("lifetime_ms", 250)),
            "task_id": self.task_id,
            "session_id": "supervisor-" + self.task_id,
            "argv": self.provider_kwargs.pop(
                "argv",
                ["literal argument", "$(touch DO_NOT_CREATE); 'quoted' \"double\""],
            ),
            "answer": self.provider_kwargs.pop("answer", self.answer),
            "stdout_bytes": int(self.provider_kwargs.pop("stdout_bytes", 0)),
            "stderr_bytes": int(self.provider_kwargs.pop("stderr_bytes", 0)),
            "rendezvous_path": str(self.provider_release)
            if self.provider_kwargs.pop("provider_hold", False)
            else "",
            "child_lifetime_ms": int(self.provider_kwargs.pop("child_lifetime_ms", 0)),
        }
        provider.update(self.provider_kwargs)
        self.provider_config = self.base / "provider config.json"
        write_json(self.provider_config, provider)
        self.provider_settings = provider
        fake = {
            "schema_version": 1,
            "config_path": str(self.fake_config),
            "expected_config_path": str(self.production_config),
            "artifact_dir": str(self.supervisor_records),
            "status_path": str(self.status_path),
            "version": SUPERVISOR_VERSION,
            "add_id": self.numeric_id,
            "status": {"delay": 0, "exit_code": 0},
            "parallel": {"delay": 0, "exit_code": 0},
            "add": {
                "delay": 0,
                "release_path": str(self.add_release) if self.add_release_enabled else "",
                "exit_code": 0,
            },
            "enqueue": {"delay": 0, "exit_code": 0},
            "kill": {"delay": 0, "exit_code": 0},
            "remove": {"delay": 0, "exit_code": 0},
        }
        write_json(self.fake_config, fake)
        hook_config = {
            "mode": hook,
            "delay_ms": 0,
            "release_path": "",
        }
        if hook == "start-delayed":
            hook_config["delay_ms"] = 10000
            hook_config["release_path"] = str(self.base / "start.release")
            self.start_release = Path(hook_config["release_path"])
        else:
            self.start_release = None
        if hook == "publication-delayed":
            hook_config["release_path"] = str(self.base / "publication.release")
            self.publication_release = Path(hook_config["release_path"])
        else:
            self.publication_release = None
        profile = {
            "schema_version": SCHEMA_VERSION,
            "provider_executable": str(self.provider),
            "provider_sha256": digest(self.provider),
            "provider_config": str(self.provider_config),
            "provider_config_sha256": digest(self.provider_config),
            "supervisor_executable": str(self.pueue),
            "events_directory": str(self.events_dir),
            "environment": [
                key + "=" + value for key, value in sorted(inherited_environment().items())
            ],
            "hooks": hook_config,
        }
        self.profile_config = self.bin_dir / "provider-fixture.json"
        write_json(self.profile_config, profile)
        self.profile_settings = profile

    def fake_config_data(self):
        return read_json(self.fake_config)

    def update_fake(self, verb, **changes):
        data = self.fake_config_data()
        data[verb] = dict(data[verb])
        data[verb].update(changes)
        write_json(self.fake_config, data, replace=True)

    def set_status(self, state, label=None, numeric_id=None, duplicate=False):
        if label is None:
            require_true(self.root_id is not None, "status label requested before add identity")
            label = "delegate:" + self.root_id + ":" + self.task_id
        if numeric_id is None:
            numeric_id = self.numeric_id
        rows = [status_job(numeric_id, label, state)]
        if duplicate:
            rows.append(status_job(numeric_id + 1, label, state))
        write_json(self.status_path, status_document(rows), replace=True)

    def set_empty_status(self):
        write_json(self.status_path, status_document([]), replace=True)

    def set_malformed_status(self, value=None):
        if value is None:
            value = b'{"tasks":{"7":{"label":"incomplete"}},"groups":{}}\n'
        write_bytes(self.status_path, value, replace=True)

    def release(self, path):
        if not Path(path).exists():
            write_bytes(path, b"release\n")

    def release_add(self):
        self.release(self.add_release)

    def release_provider(self):
        # The finite provider treats rendezvous_path as a prefix and waits for
        # the corresponding ".release" marker. Keep this distinct from the
        # direct release files used by the supervisor and Start hooks. Parent
        # exit and its child intentionally share this configured rendezvous.
        self.release(str(self.provider_release) + ".release")

    def update_provider(self, **changes):
        self.provider_settings.update(changes)
        write_json(self.provider_config, self.provider_settings, replace=True)
        profile = read_json(self.profile_config)
        profile["provider_config_sha256"] = digest(self.provider_config)
        write_json(self.profile_config, profile, replace=True)

    def dispatch_argv(self, brief=None, cwd=None, budget=None, task=None,
                      pueue_config=None, provider=PREDICATE, extra=(),
                      omit_budget=False):
        argv = [
            str(self.delegate),
            "--root",
            str(self.root),
            "--pueue-config",
            str(self.production_config if pueue_config is None else pueue_config),
            "--runner",
            str(self.runner),
            "dispatch",
            "--provider",
            provider,
            "--brief",
            str(self.brief if brief is None else brief),
            "--cwd",
            str(self.work if cwd is None else cwd),
            "--id",
            self.task_id if task is None else task,
            "--permission",
            MODE,
        ]
        if not omit_budget:
            argv.extend(["--budget", self.budget if budget is None else budget])
        argv.extend(["--json", *extra])
        return argv

    def start_dispatch(self, **kwargs):
        return self.processes.start(
            "dispatch",
            self.dispatch_argv(**kwargs),
            self.base,
        )

    def dispatch_dynamic(self, expected=(0,), status="queued"):
        process = self.start_dispatch()
        entry = wait_entry(self, "add")
        self.root_id, observed_task = label_parts(add_entry_label(entry))
        require_true(observed_task == self.task_id, "add receipt changed task identity")
        self.set_status(status)
        self.release_add()
        process.wait(CASE_TIMEOUT, expected=expected)
        response = process.json()
        require_true(response.get("task_id") == self.task_id, "dispatch response task mismatch")
        self.numeric_id = int(read_json(self.fake_config)["add_id"])
        require_bound_records(self)
        return process, response













    def dispatch_without_admission(self, expected=(2,), **kwargs):
        process = self.start_dispatch(**kwargs)
        process.wait(CASE_TIMEOUT, expected=expected)
        if (process.directory / "stdout").stat().st_size == 0:
            return process, {"error": (process.directory / "stderr").read_text()}
        return process, process.json()

    def retry_dispatch(self, expected=(0,), **kwargs):
        process = self.start_dispatch(**kwargs)
        process.wait(CASE_TIMEOUT, expected=expected)
        return process, process.json()

    def command_argv(self, command, extra=()):
        return [
            str(self.delegate),
            "--root",
            str(self.root),
            command,
            self.task_id,
            "--json",
            *extra,
        ]

    def run_command(self, name, command, expected=(0,), extra=()):
        process = self.processes.run(
            name,
            self.command_argv(command, extra),
            self.base,
            expected=expected,
            timeout=CASE_TIMEOUT,
        )
        return process, process.json()

    def run_command_with_environment(self, name, command, environment,
                                     expected=(0,), extra=()):
        """Run one fresh CLI with an explicit ambient environment overlay."""
        previous = self.processes.environment
        self.processes.environment = dict(environment)
        try:
            return self.run_command(name, command, expected, extra)
        finally:
            self.processes.environment = previous

    def start_runner(self, expected=(0, 1), name="delegate-run"):
        process = self.processes.start(
            name,
            [str(self.runner), "--root", str(self.root), self.task_id, "--json"],
            self.base,
        )
        return process

    def finish_runner(self, process, expected=(0, 1)):
        process.wait(CASE_TIMEOUT, expected=expected)
        return process.json()



























    def task_dir(self):
        return self.root / "tasks" / self.task_id

    def wait_path(self, path, timeout=CASE_TIMEOUT):
        return wait_until(
            lambda: Path(path).exists(),
            timeout=timeout,
            description="path " + str(path),
        )

    def wait_provider_hold(self, timeout=CASE_TIMEOUT):
        """Observe the finite provider's actual held-pipe rendezvous."""
        waiting = Path(str(self.provider_release) + ".waiting")
        self.wait_path(waiting, timeout=timeout)
        content = waiting.read_text()
        fields = content.rstrip("\n").split(":")
        require_true(len(fields) == 2 and fields[1] == "waiting",
                     "provider rendezvous receipt is malformed")
        try:
            pid = int(fields[0])
        except ValueError as error:
            raise HarnessFailure("provider rendezvous PID is malformed") from error
        known = []
        for path in (
            self.provider_records / "provider.entry.json",
            self.provider_records / "child.provider.entry.json",
        ):
            if path.exists():
                known.append(read_json(path, MAX_CONTROL_BYTES).get("pid"))
        require_true(pid > 0 and pid in known,
                     "provider rendezvous PID is not a recorded fixture process")
        return pid

    def stop_record(self, request_id):
        path = self.task_dir() / "stop" / (request_id + ".request.json")
        return read_json(path, MAX_CONTROL_BYTES)

    def receipt(self):
        return {
            "case": self.case_id,
            "task_id": self.task_id,
            "chain_task_ids": list(self.chain_task_ids),
            "chain": [
                {
                    key: value
                    for key, value in record.items()
                    if key in ("label", "task_id", "answer", "session_id", "scenario")
                }
                for record in self.chain_records
            ],
            "chain_summary": getattr(self, "chain_summary", None),
            "root_id": self.root_id,
            "base": str(self.base),
            "helper_completion_observed": self.helper_completion_observed,
            "external_wait_observed": self.external_wait_observed,
            "external_wait_required": self.external_wait_required,
            "external_wait_unknown": [
                {"invocation_id": invocation_id, "pid": pid}
                for invocation_id, pid in sorted(self.external_wait_unknown.items())
            ],
            "supervisor_observer_completions": observed_supervisor_completions(self),
            "counts": invocation_counts(self),
            "supervisor_binding": {
                "config_path": str(self.production_config),
                "config_sha256": digest(self.production_config),
                "fake_executable": str(self.pueue),
            },
        }

    def copy_evidence(self):
        destination = self.suite.output / "cases" / self.case_id
        destination.mkdir(mode=0o700, parents=True, exist_ok=True)
        for source_name, target_name in (
            ("task state ' ; literal", "task-state"),
            ("events", "events"),
            ("provider records", "provider-records"),
            ("supervisor records", "supervisor-records"),
            ("processes", "processes"),
        ):
            source = self.base / source_name
            if source.exists():
                shutil.copytree(source, destination / target_name)
        for source in (
            self.production_config,
            self.fake_config,
            self.profile_config,
            self.provider_config,
            self.status_path,
            self.brief,
        ):
            if source.exists():
                shutil.copyfile(source, destination / source.name)
        return destination

    def finish(self, status="pass", error=None):
        active = self.processes.drain(timeout=0, exclude=())
        require_true(not active, "owned process remained active at case completion")
        require_true(self.processes.finished_naturally(exclude=()),
                     "owned process did not complete with a natural wait")
        self.outcome = {"status": status}
        if error is not None:
            self.outcome["error"] = str(error)
        destination = self.copy_evidence()
        result = self.receipt()
        result["status"] = status
        if error is not None:
            result["error"] = str(error)
        if status == "pass":
            require_true(
                not self.external_wait_required or
                (self.external_wait_observed is True and not self.external_wait_unknown),
                "external supervisor Wait was not proven",
            )
            rollback_succeeded = rollback_owned_directories(self.owned_directories)
            require_true(rollback_succeeded is True,
                         "owned-directory rollback was not proven")
        write_json(destination / "receipt.json", result)
        return result

    def retain_failure(self, error):
        # Cooperatively release any finite provider rendezvous before waiting
        # for owned processes. This is safe even when no provider reached the
        # hold and prevents a failed assertion from outliving the drain bound.
        if getattr(self, "provider_release", None) is not None:
            self.release_provider()
        active = self.processes.drain(timeout=10, exclude=())
        destination = self.suite.output / "cases" / self.case_id
        destination.mkdir(mode=0o700, parents=True, exist_ok=True)
        write_json(
            destination / "failure.json",
            {
                "case": self.case_id,
                "error": str(error),
                "traceback": traceback.format_exc(),
                "private_base": str(self.base),
                "unresolved_owned_processes": active,
                "signals_sent": 0,
                "termination_unknown_preserved": True,
            },
        )
        # This is a diagnostic snapshot, not sealed execution evidence. Keep
        # the original base and preserve links without reading their targets.
        shutil.copytree(self.base, destination / "private-base", symlinks=True)
