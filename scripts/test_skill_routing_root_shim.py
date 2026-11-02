"""Root isolation through real shim/fixture processes, without provider work."""

import contextlib
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch

import acceptance_skill_routing as gate


class RoutingFixture(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="routing shim ' Unicode-é-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.run = self.root / "attempt"
        self.run.mkdir()
        self.state = self.root / "state"
        self.state.mkdir()
        self.workspace = self.run / "workspace"
        self.workspace.mkdir()
        self.brief = self.workspace / "brief.md"
        self.brief.write_text("bounded fixture brief")
        self.tools = self.root / "tools"
        self.tools.mkdir()
        self.candidate = self.tools / "delegate"
        self.backend_calls = self.root / "backend-calls.jsonl"
        self.candidate.write_text(
            "#!" + sys.executable + "\n"
            "import json,os,sys\nfrom pathlib import Path\n"
            "argv=sys.argv[1:]\n"
            "record={'argv':argv,'environment':{key:os.environ.get(key) for key in "
            "('HOME','XDG_CONFIG_HOME','XDG_DATA_HOME','CLAUDE_CONFIG_DIR','CODEX_HOME',"
            "'DELEGATE_ROOT','PATH')}}\n"
            "with Path(" + repr(str(self.backend_calls)) + ").open('a') as stream:\n"
            " stream.write(json.dumps(record)+'\\n')\n"
            "command=next((value for value in argv if value in "
            "('dispatch','status','collect','recheck','providers')), 'version')\n"
            "if command=='dispatch':\n"
            " root=argv[argv.index('--root')+1]\n"
            " (Path(root)/'owned-mutation').write_text('owned')\n"
            "print(json.dumps({'command':command,'failure':{'code':'provider_preparation_failed',"
            "'next_action':'stop_and_report'}}))\n",
            encoding="utf-8")
        self.candidate.chmod(0o700)
        self.environment = dict(os.environ)
        self.environment.pop("DELEGATE_ROOT", None)
        self.environment["PATH"] = str(self.tools) + os.pathsep + os.environ.get("PATH", os.defpath)
        self.ownership = gate.DirectoryOwner()
        self.addCleanup(self.ownership.rollback)
        self.case = {"name": "review", "prompt": "Ask Codex for a review.", "external": True}

    def install(self, *, explicit=False, candidate=None):
        return gate._candidate_environment(self.run, self.ownership, self.environment, candidate,
                                          state_root=self.state, explicit_root=explicit)

    def invoke(self, argv, *, environment=None):
        return subprocess.run(["delegate", *argv], cwd=self.workspace,
                              env=self.environment if environment is None else environment,
                              capture_output=True, text=True, check=False, timeout=10)

    def records(self, path):
        return [json.loads(line) for line in path.read_text().splitlines()]

    def dispatch(self):
        return ["dispatch", "--provider", "codex:exec", "--brief", str(self.brief),
                "--cwd", str(self.workspace), "--permission", "read-only", "--budget", "30s", "--json"]

    def prepare_fixture(self, case=None):
        owner = Mock(client=sys.executable, daemon_path=sys.executable)
        owner.processes.environment = self.environment
        gate._prepare_routing_fixture(self.run, owner, self.case if case is None else case,
                                      self.candidate, state_root=self.state, ownership=self.ownership)
        return self.run / "fixture"


class RoutingRootShimTests(RoutingFixture):
    def test_default_omits_original_root_with_ambient_unset_and_set(self):
        original = dict(self.environment)
        self.assertEqual(self.install(), self.candidate)
        for ambient in (None, str(self.root / "unrelated ambient state")):
            with self.subTest(ambient=ambient):
                if ambient is not None:
                    self.environment["DELEGATE_ROOT"] = ambient
                result = self.invoke(self.dispatch())
                self.assertEqual(result.returncode, 0, result.stderr)
                recorded = self.records(self.backend_calls)[-1]
                self.assertEqual(recorded["argv"], ["--root", str(self.state), *self.dispatch()])
                self.assertEqual(recorded["environment"]["DELEGATE_ROOT"], ambient)
                for key in ("HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME"):
                    self.assertEqual(recorded["environment"][key], original.get(key))
                self.assertEqual(self.records(self.run / "fixture/agent-calls.jsonl")[-1],
                                 {"argv": self.dispatch(), "error": None})
        self.assertTrue((self.state / "owned-mutation").exists())
        self.assertFalse((self.root / "unrelated ambient state").exists())

    def test_default_rejects_all_original_roots_before_backend(self):
        self.install()
        roots = (["--root", str(self.state)], ["--root=" + str(self.state)],
                 ["-root", str(self.state)], ["-root=" + str(self.state)],
                 ["--root", str(self.root / "elsewhere")], ["--root"],
                 ["--root", str(self.state), "--root", str(self.state)])
        for root_flags in roots:
            with self.subTest(root_flags=root_flags):
                argv = [*root_flags, "providers", "--json"]
                result = self.invoke(argv)
                self.assertEqual(result.returncode, 2)
                self.assertIn("must omit", result.stderr)
                record = self.records(self.run / "fixture/agent-calls.jsonl")[-1]
                self.assertEqual(record["argv"], argv)
                self.assertIsNotNone(record["error"])
        self.assertFalse(self.backend_calls.exists())

    def test_explicit_root_passes_unchanged_and_rejects_missing_wrong_or_duplicate(self):
        self.install(explicit=True)
        for root_flags in (["--root", str(self.state)], ["--root=" + str(self.state)],
                           ["-root", str(self.state)], ["-root=" + str(self.state)]):
            argv = [*root_flags, "providers", "--json"]
            result = self.invoke(argv)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(self.records(self.backend_calls)[-1]["argv"], argv)
        before = self.backend_calls.read_bytes()
        for root_flags in ([], ["--root"], ["--root="], ["--root", "relative"],
                           ["--root", str(self.root / "other")],
                           ["--root", str(self.state), "--root=" + str(self.state)],
                           ["--root", str(self.state), "-root=" + str(self.state)]):
            with self.subTest(root_flags=root_flags):
                result = self.invoke([*root_flags, "providers", "--json"])
                self.assertEqual(result.returncode, 2)
                self.assertEqual(self.backend_calls.read_bytes(), before)

    def test_explicit_root_only_in_helper_tail_is_refused_before_backend(self):
        self.install(explicit=True)
        for flags in (["--root", str(self.state)], ["--root=" + str(self.state)],
                      ["-root", str(self.state)], ["-root=" + str(self.state)]):
            with self.subTest(flags=flags):
                argv = ["workspace", "exec", "a" * 32, "--budget", "1m", "--", "/bin/echo", *flags]
                result = self.invoke(argv)
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertIn("exactly one --root", result.stderr)
                self.assertFalse(self.backend_calls.exists())
                self.assertEqual(self.records(self.run / "fixture/agent-calls.jsonl")[-1]["argv"], argv)

    def test_explicit_root_looking_model_value_is_refused_without_leading_root(self):
        self.install(explicit=True)
        for value in ("--root=" + str(self.state), "-root=" + str(self.state)):
            with self.subTest(value=value):
                argv = ["schedule", "create", "--model", value, "--cwd", str(self.workspace),
                        "--brief", str(self.brief), "--auto", "--cron", "0 9 * * *", "--budget", "1m", "--json"]
                result = self.invoke(argv)
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertFalse(self.backend_calls.exists())
                record = self.records(self.run / "fixture/agent-calls.jsonl")[-1]
                self.assertEqual(record["argv"], argv)
                self.assertIsNotNone(record["error"])

    def test_explicit_cli_root_preserves_literal_helper_root(self):
        self.install(explicit=True)
        argv = ["--root", str(self.state), "workspace", "exec", "a" * 32, "--budget", "1m",
                "--", "/bin/echo", "--root", str(self.root / "helper-only"), "--", "-root=literal"]
        result = self.invoke(argv)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.records(self.backend_calls)[-1]["argv"], argv)
        self.assertEqual(self.records(self.run / "fixture/agent-calls.jsonl")[-1], {"argv": argv, "error": None})

    def test_default_cli_root_is_injected_without_interpreting_helper_root(self):
        self.install()
        argv = ["workspace", "exec", "a" * 32, "--budget", "1m", "--", "/bin/echo",
                "--root", str(self.root / "helper-only"), "-root=literal"]
        result = self.invoke(argv)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.records(self.backend_calls)[-1]["argv"], ["--root", str(self.state), *argv])
        self.assertEqual(self.records(self.run / "fixture/agent-calls.jsonl")[-1], {"argv": argv, "error": None})

    def test_default_rejects_root_after_separator_used_as_flag_value(self):
        self.install()
        argv = ["reply", "a" * 32, "--request", "--", "--root", str(self.root / "unowned")]
        result = self.invoke(argv)
        self.assertEqual(result.returncode, 2, result.stderr)
        self.assertFalse(self.backend_calls.exists())
        self.assertEqual(self.records(self.run / "fixture/agent-calls.jsonl")[-1]["argv"], argv)

    def test_explicit_checks_root_after_separator_used_as_flag_value(self):
        self.install(explicit=True)
        reply = ["reply", "a" * 32, "--request", "--"]
        argv = ["--root", str(self.state), *reply]
        result = self.invoke(argv)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.records(self.backend_calls)[-1]["argv"], argv)
        before = self.backend_calls.read_bytes()
        for argv in ([*reply, "--root", str(self.state)],
                     [*reply, "--root", str(self.root / "unowned")],
                     ["--root", str(self.state), *reply, "--root", str(self.root / "unowned")]):
            with self.subTest(argv=argv):
                result = self.invoke(argv)
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertEqual(self.backend_calls.read_bytes(), before)

    def test_default_cannot_forge_workspace_exec_from_flag_values_before_separator(self):
        self.install()
        cases = (["--runner", "workspace", "exec", "--request", "--"],
                 ["--runner", "workspace", "--pueue-config", "exec", "reply", "a" * 32, "--request", "--"],
                 ["workspace", "--runner", "exec", "status", "a" * 32, "--watch", "--"],
                 ["reply", "a" * 32, "--request", "workspace", "exec", "--id", "--"])
        for prefix in cases:
            with self.subTest(prefix=prefix):
                argv = [*prefix, "--root", str(self.root / "unowned")]
                result = self.invoke(argv)
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertFalse(self.backend_calls.exists())

    def test_explicit_cannot_forge_workspace_exec_from_flag_values_before_separator(self):
        self.install(explicit=True)
        cases = (["--runner", "workspace", "exec", "--request", "--"],
                 ["workspace", "--runner", "exec", "status", "a" * 32, "--watch", "--"],
                 ["reply", "a" * 32, "--request", "workspace", "exec", "--id", "--"])
        for prefix in cases:
            with self.subTest(prefix=prefix):
                argv = ["--root", str(self.state), *prefix, "--root", str(self.root / "unowned")]
                result = self.invoke(argv)
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertFalse(self.backend_calls.exists())

    def test_implicit_candidate_resolved_before_path_change_and_fixture_keeps_shim_first(self):
        original_path = self.environment["PATH"]
        with patch.object(gate.shutil, "which", wraps=shutil.which) as lookup:
            candidate = self.install()
        lookup.assert_called_once_with("delegate", path=original_path)
        self.assertEqual(candidate, self.candidate)
        path, bash_env = self.environment["PATH"], self.environment["BASH_ENV"]
        fixture = self.prepare_fixture()
        runtime = gate.read_json(fixture / "runtime.json")
        self.assertEqual(runtime["delegate"], str(self.candidate))
        self.assertEqual(runtime["delegate_sha256"], gate.digest(self.candidate))
        self.assertEqual(runtime["state_root"], str(self.state))
        self.assertEqual(self.environment["PATH"], path)
        self.assertEqual(self.environment["BASH_ENV"], bash_env)
        self.assertEqual(Path(shutil.which("delegate", path=path)), self.run / "candidate-bin/delegate")
        result = self.invoke(self.dispatch())
        self.assertEqual(result.returncode, 0, result.stderr)
        injected = ["--root", str(self.state), *self.dispatch()]
        self.assertEqual(self.records(fixture / "calls.jsonl"), [injected])
        backend = self.records(self.backend_calls)
        self.assertEqual(len(backend), 1)
        self.assertEqual(backend[0]["argv"], injected)
        self.assertEqual(backend[0]["environment"]["PATH"], runtime["path"])
        self.assertEqual(self.records(self.run / "fixture/agent-calls.jsonl"),
                         [{"argv": self.dispatch(), "error": None}])

    def test_synthetic_recovery_composes_behind_root_shim(self):
        self.install()
        fixture = self.prepare_fixture(dict(self.case, fixture_scenario="recoverable-rejection", provider="codex:exec"))
        result = self.invoke(self.dispatch())
        self.assertEqual(result.returncode, 0, result.stderr)
        task = json.loads(result.stdout)["task_id"]
        for command, code in (("status", 0), ("collect", 4), ("recheck", 0)):
            result = self.invoke([command, task, "--json"])
            self.assertEqual(result.returncode, code, result.stderr)
        saved = gate.read_json(fixture / "routing-state.json")
        self.assertEqual(saved["root"], str(self.state))
        self.assertEqual((saved["dispatch_count"], saved["recheck_count"]), (1, 1))
        original = self.records(self.run / "fixture/agent-calls.jsonl")
        self.assertEqual(len(original), 4)
        self.assertTrue(all("--root" not in entry["argv"] for entry in original))
        self.assertTrue(all(entry[:2] == ["--root", str(self.state)]
                            for entry in self.records(fixture / "calls.jsonl")))

    def test_diagnosis_uses_same_shim_without_forwarding_mutations(self):
        self.install()
        case = next(case for case in gate.read_json(gate.CASES)["cases"] if "paired_case_id" in case)
        fixture = self.prepare_fixture(case)
        arguments = gate.diagnosis_arguments(self.workspace, fixture)
        writable = [Path(arguments[index + 1]) for index, value in enumerate(arguments) if value == "--add-dir"]
        log = self.run / "fixture/agent-calls.jsonl"
        self.assertTrue(any(log.is_relative_to(path) for path in writable))
        response = gate.read_json(self.workspace / "handoff.json")
        for command, code in (("status", 0), ("collect", 4), ("logs", 0), ("cancel", 2)):
            result = self.invoke([command, response["task_id"], "--json"])
            self.assertEqual(result.returncode, code, result.stderr)
        self.assertFalse(self.backend_calls.exists())
        saved = gate.read_json(fixture / "routing-state.json")
        self.assertEqual(saved["refused_commands"], 1)
        self.assertEqual(saved["root"], str(self.state))
        self.assertEqual(len(self.records(self.run / "fixture/agent-calls.jsonl")), 4)
        self.assertTrue(all(entry[:2] == ["--root", str(self.state)]
                            for entry in self.records(fixture / "calls.jsonl")))

    def test_explicit_diagnosis_root_passes_through_native_root_spellings(self):
        self.install(explicit=True)
        case = next(case for case in gate.read_json(gate.CASES)["cases"] if "paired_case_id" in case)
        fixture = self.prepare_fixture(case)
        task = gate.read_json(self.workspace / "handoff.json")["task_id"]
        for flags in (["--root", str(self.state)], ["--root=" + str(self.state)],
                      ["-root", str(self.state)], ["-root=" + str(self.state)]):
            with self.subTest(flags=flags):
                argv = [*flags, "status", task, "--json"]
                result = self.invoke(argv)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(self.records(fixture / "calls.jsonl")[-1], argv)
                self.assertEqual(self.records(fixture / "agent-calls.jsonl")[-1], {"argv": argv, "error": None})
        self.assertFalse(self.backend_calls.exists())

    def test_login_shell_restores_shim_after_previous_bash_env(self):
        prior = self.root / "prior env.sh"
        prior.write_text("export ROUTING_PRIOR_ENV=preserved\nexport PATH=/usr/bin:/bin\n")
        self.environment["BASH_ENV"] = str(prior)
        self.install()
        result = subprocess.run(["/bin/bash", "-lc", 'test "$ROUTING_PRIOR_ENV" = preserved && delegate providers --json'],
                                cwd=self.workspace, env=self.environment, capture_output=True,
                                text=True, check=False, timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.records(self.backend_calls)[0]["argv"],
                         ["--root", str(self.state), "providers", "--json"])

    def test_runtime_identity_refuses_recursion_and_relative_candidates(self):
        self.install()
        path = self.run / "candidate-bin/runtime.json"
        original = gate.read_json(path)
        for key, value in (("delegate", "delegate"), ("backend", "delegate"),
                           ("delegate", str(self.run / "candidate-bin/delegate")),
                           ("backend", str(self.run / "candidate-bin/delegate"))):
            with self.subTest(key=key, value=value):
                path.write_text(json.dumps(dict(original, **{key: value})))
                result = self.invoke(["providers", "--json"])
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertIn("routing root isolation refused", result.stderr)
                self.assertFalse(self.backend_calls.exists())

    def test_fixture_refuses_shim_as_real_runtime_candidate(self):
        self.install()
        owner = Mock(client=sys.executable, daemon_path=sys.executable)
        owner.processes.environment = self.environment
        for candidate in (Path("delegate"), self.run / "candidate-bin/delegate"):
            with self.subTest(candidate=candidate), self.assertRaisesRegex(ValueError, "absolute real candidate"):
                gate._prepare_routing_fixture(self.run, owner, self.case, candidate,
                                              state_root=self.state, ownership=self.ownership)
        self.assertFalse((self.run / "fixture/bin").exists())

    def test_no_candidate_refuses_without_changing_path_or_installing_shim(self):
        self.environment["PATH"] = str(self.root / "empty")
        before = dict(self.environment)
        with self.assertRaisesRegex(FileNotFoundError, "candidate Delegate binary unavailable"):
            self.install()
        self.assertEqual(self.environment, before)
        self.assertFalse((self.run / "candidate-bin").exists())


class RoutingRunIsolationTests(RoutingFixture):
    # Only exercise the run owner here; the subprocess composition cases above
    # have their own directly owned fixture lifetime.
    def run_actor(self, case, actor, *, mode="live", candidate=None):
        metadata = self.root / "metadata.txt"
        metadata.write_text("candidate metadata")
        owner = Mock(client=sys.executable, daemon_path=sys.executable)
        def construct(run, environment, ownership, *, state_root):
            owner.processes.environment = dict(environment)
            owner.config = self.root / "private-queue.json"
            owner.capture.return_value = metadata
            owner.execute.side_effect = lambda argv, stdin, budget: actor(run, state_root, owner, stdin)
            return owner
        def which(name, *, path=None):
            return str(self.candidate) if name == "delegate" else sys.executable
        with patch.object(gate, "AttemptSupervisor", side_effect=construct), \
                patch.object(gate.shutil, "which", side_effect=which), \
                contextlib.redirect_stdout(io.StringIO()):
            receipt = gate.run_case(output=self.root, harness="codex", model="requested-model", case=case,
                                    repetition=1, mode=mode, delegate_bin=candidate)
        return receipt, owner

    def test_programmatic_none_installs_shim_in_live_and_routing_modes(self):
        original = dict(os.environ)
        for mode in ("live", "routing"):
            case = dict(self.case, name=mode)
            def actor(run, state, owner, stdin):
                env = owner.processes.environment
                self.assertEqual(env["PATH"].split(os.pathsep)[0], str(run / "candidate-bin"))
                runtime = gate.read_json(run / "candidate-bin/runtime.json")
                self.assertEqual(runtime["delegate"], str(self.candidate))
                self.assertEqual(runtime["state_root"], str(state))
                result = self.invoke(["providers", "--json"], environment=env)
                self.assertEqual(result.returncode, 0, result.stderr)
                for key in ("HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "DELEGATE_ROOT"):
                    self.assertEqual(env.get(key), original.get(key))
                return {"result": "Success"}
            receipt, owner = self.run_actor(case, actor, mode=mode)
            self.assertEqual(receipt["status"], "unreviewed")
            self.assertEqual(receipt["delegate_executable"], str(self.candidate))
            self.assertIn("fixture/agent-calls.jsonl_sha256", receipt)
            owner.close.assert_called_once()
            self.assertFalse(Path(receipt["state_root"]).exists())
            self.assertFalse((self.root / (mode + "-1/candidate-bin")).exists())
            self.assertFalse((self.root / (mode + "-1/fixture/bin")).exists())
        self.assertEqual(dict(os.environ), original)

    def test_explicit_root_prompt_supplies_owned_root_and_passes_it_unchanged(self):
        case = dict(self.case, root_mode="explicit", prompt="Use the exact custom root @ROOT@ to ask Codex.")
        def actor(run, state, owner, stdin):
            self.assertIn(str(state), stdin)
            self.assertIn("before the command", stdin)
            prefix = stdin.split("`", 2)[1]
            self.assertEqual(gate.shlex.split(prefix), ["delegate", "--root", str(state)])
            self.assertEqual((run / "prompt.txt").read_text(), stdin)
            argv = ["--root", str(state), "providers", "--json"]
            result = self.invoke(argv, environment=owner.processes.environment)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(self.records(self.backend_calls)[-1]["argv"], argv)
            return {"result": "Success"}
        receipt, _ = self.run_actor(case, actor)
        self.assertEqual(receipt["status"], "unreviewed")
        self.assertNotIn("@ROOT@", (self.root / "review-1/prompt.txt").read_text())

    def test_programmatic_no_candidate_blocks_before_provider_and_cleans_up(self):
        with patch.object(gate.shutil, "which", return_value=None), \
                patch.object(gate, "AttemptSupervisor") as owner, contextlib.redirect_stdout(io.StringIO()):
            receipt = gate.run_case(output=self.root, harness="codex", model=None, case=self.case, repetition=1)
        owner.assert_not_called()
        self.assertEqual((receipt["status"], receipt["reason"]), ("blocked", "required_executable_missing"))
        self.assertFalse(Path(receipt["state_root"]).exists())
        self.assertFalse((self.root / "review-1/workspace").exists())
        self.assertFalse((self.root / "review-1/candidate-bin").exists())
        self.assertEqual(gate.read_json(self.root / "review-1/cleanup.json")["status"], "removed")

    def test_rejected_root_cannot_be_hidden_by_successful_actor_and_cleanup_still_runs(self):
        def actor(run, state, owner, stdin):
            result = self.invoke(["--root", str(self.root / "unowned"), "dispatch"],
                                 environment=owner.processes.environment)
            self.assertEqual(result.returncode, 2)
            return {"result": "Success"}
        receipt, owner = self.run_actor(self.case, actor)
        self.assertEqual((receipt["status"], receipt["reason"]), ("failed", "root_isolation_violation"))
        self.assertFalse(self.backend_calls.exists())
        self.assertFalse((self.root / "unowned").exists())
        self.assertFalse(Path(receipt["state_root"]).exists())
        self.assertFalse((self.root / "review-1/candidate-bin").exists())
        owner.close.assert_called_once()

    def test_missing_original_argv_evidence_cannot_pass(self):
        def actor(run, state, owner, stdin):
            (run / "fixture/agent-calls.jsonl").unlink()
            return {"result": "Success"}
        receipt, owner = self.run_actor(self.case, actor)
        self.assertEqual((receipt["status"], receipt["reason"]), ("failed", "root_isolation_evidence_invalid"))
        self.assertFalse(Path(receipt["state_root"]).exists())
        owner.close.assert_called_once()

    def test_fixture_setup_error_removes_both_executables_and_state(self):
        copyfile = gate.shutil.copyfile
        def fail_fixture_copy(source, destination, *, follow_symlinks=True):
            if Path(source) == gate.REPO / "scripts/skill_routing_delegate_fixture.py":
                raise OSError("injected fixture setup failure")
            return copyfile(source, destination, follow_symlinks=follow_symlinks)
        with patch.object(gate.shutil, "copyfile", side_effect=fail_fixture_copy):
            receipt, owner = self.run_actor(self.case, Mock(), mode="routing")
        self.assertEqual(receipt["status"], "blocked")
        owner.execute.assert_not_called()
        owner.close.assert_called_once()
        self.assertFalse(Path(receipt["state_root"]).exists())
        for path in ("workspace", "candidate-bin", "fixture/bin"):
            self.assertFalse((self.root / "review-1" / path).exists())
        self.assertTrue(self.candidate.exists())


if __name__ == "__main__":
    unittest.main()
