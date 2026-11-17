"""Prepared-context rejection fixtures, not native model or hook-firing proof."""

import hashlib
import json
import shlex
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from scripts.acceptance.skill_routing import candidate_context as candidate
from scripts.acceptance.skill_routing import cli


class PreparedContextTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-candidate-context-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.source = self.root / "source"
        self.source_skill = self.source / "skills/agent-integration/SKILL.md"
        self.source_skill.parent.mkdir(parents=True)
        self.text = "# Candidate skill\nRead the source.\nAllocate authorized work only.\n"
        self.source_skill.write_text(self.text)
        self.workspace = self.root / "workspace"
        self.skill = self.workspace / ".agents/skills/agent-integration/SKILL.md"
        self.skill.parent.mkdir(parents=True)
        self.skill.write_text(self.text)
        self.controls = {"candidate_skill": str(self.skill), "selection": "invocation-skill-overrides",
                         "skill_sha256": hashlib.sha256(self.text.encode()).hexdigest()}
        self.transcript = self.root / "stdout.txt"

    def context(self, paths):
        lines = ["### Available skills", "- `r0` = `" + str(self.workspace / ".agents/skills") + "`"]
        lines.extend("- agent-integration: candidate (file: " + path + ")" for path in paths)
        return [{"role": "developer", "content": [{"type": "input_text", "text": "\n".join(lines)}]}]

    def test_metadata_aliases_resolve_and_invalid_or_ambiguous_paths_fail(self):
        self.assertEqual(candidate.skill_paths(self.context(["r0/agent-integration/SKILL.md"])), [self.skill])
        for document in (self.context(["unknown/agent-integration/SKILL.md"]), {}, [1],
                         [{"content": "not a block list"}]):
            with self.subTest(document=document), self.assertRaises(ValueError):
                candidate.skill_paths(document)

    def test_codex_discovers_plugin_competitor_then_validates_final_same_workspace_context(self):
        competitor = self.root / "installed-plugin/agent-integration/SKILL.md"
        captures = []
        def capture(name, argv):
            captures.append((name, argv))
            path = self.root / (name + ".json")
            paths = [str(self.skill), str(competitor)] if name == "prepared-discovery-context" else [str(self.skill)]
            path.write_text(json.dumps(self.context(paths)))
            return path
        hooks = ("-c", 'hooks.SessionStart=[{hooks=[{type="command",command="/candidate/hook"}]}]')
        with patch.object(candidate.Path, "home", return_value=self.root):
            prepared = candidate.prepare(SimpleNamespace(capture=capture), "/codex", "codex", self.workspace,
                                         self.source, self.root / "delegate", model="exact-model", routing_arguments=hooks)
        self.assertIn(str(competitor), next(argument for argument in prepared.arguments if argument.startswith("skills.config=")))
        self.assertEqual(prepared.skill, self.skill)
        for _name, argv in captures:
            self.assertEqual(argv[1:3], list(hooks))
            self.assertEqual(argv[-2:], ["debug", "prompt-input"])
            self.assertIn('model="exact-model"', argv)
            for control in ("allow_login_shell=false", "features.shell_snapshot=false", "features.shell_snapshot_v2=false"):
                self.assertIn(control, argv)
                self.assertIn(control, prepared.arguments)
            for forbidden in ("--ignore-user-config", "--ignore-rules", "--disable", "mcp_servers={}"):
                self.assertNotIn(forbidden, argv)
        self.assertFalse(any(argument.startswith("skills.config=") for argument in captures[0][1]))
        self.assertEqual(prepared.evidence["shell_controls"], [
            "allow_login_shell=false", "features.shell_snapshot=false", "features.shell_snapshot_v2=false",
        ])

    def test_prepared_visible_skills_preserve_unrelated_baseline_entries(self):
        unrelated = self.root / "unrelated/SKILL.md"
        def context(include_unrelated):
            document = self.context([str(self.skill)])
            if include_unrelated:
                document[0]["content"][0]["text"] += (
                    "\n- other-skill: Keep normal skills; may mention agent-integration (file: " + str(unrelated) + ")")
            return document
        for before, after in ((True, False), (False, True), (True, True)):
            calls = []
            def capture(name, argv):
                calls.append(argv)
                path = self.root / (name + ".json")
                path.write_text(json.dumps(context(before if name == "prepared-discovery-context" else after)))
                return path
            with self.subTest(before=before, after=after), patch.object(candidate.Path, "home", return_value=self.root):
                if before != after:
                    with self.assertRaisesRegex(ValueError, "unrelated visible skills.*dedicated native context"):
                        candidate.prepare(SimpleNamespace(capture=capture), "/codex", "codex", self.workspace,
                                          self.source, self.root / "delegate")
                else:
                    prepared = candidate.prepare(SimpleNamespace(capture=capture), "/codex", "codex", self.workspace,
                                                 self.source, self.root / "delegate")
                    self.assertEqual(prepared.evidence["visible_skill_comparison"], "baseline-with-target-replaced")
                self.assertEqual(len(calls), 2)
                self.assertFalse(any(argument.startswith("skills.config=") for argument in calls[0]))

    def test_conflicting_plugin_with_other_skills_requires_a_dedicated_context(self):
        profile = self.root / "codex"
        plugin = profile / "plugins/cache/delegation-market/delegation-layer/0.7.0"
        def capture(name, _argv):
            document = self.context([str(self.skill)])
            if name == "prepared-discovery-context":
                document[0]["content"][0]["text"] += (
                    "\n- delegation-layer:agent-integration: Old skill (file: "
                    + str(plugin / "skills/agent-integration/SKILL.md") + ")"
                    + "\n- delegation-layer:delegate-upgrade: Preserve upgrade guidance (file: "
                    + str(plugin / "skills/delegate-upgrade/SKILL.md") + ")")
            path = self.root / (name + ".json")
            path.write_text(json.dumps(document))
            return path
        with patch.dict(candidate.os.environ, {"CODEX_HOME": str(profile)}), \
                self.assertRaisesRegex(candidate.PreparedSkillConfigurationChanged, "selected native plugin cache"):
            candidate.prepare(SimpleNamespace(capture=capture), "/codex", "codex", self.workspace,
                              self.source, self.root / "delegate")

    def test_discovered_selected_cache_plugin_blocks_without_plugin_overrides(self):
        profile = self.root / "codex-profile with space"
        competitor = profile / "plugins/cache/delegation-market/delegation-layer/0.7.0/skills/agent-integration/SKILL.md"
        calls = []
        def capture(name, argv):
            calls.append(argv)
            paths = [str(self.skill), str(competitor)] if name == "prepared-discovery-context" else [str(self.skill)]
            path = self.root / (name + ".json")
            path.write_text(json.dumps(self.context(paths)))
            return path
        with patch.dict(candidate.os.environ, {"CODEX_HOME": str(profile)}):
            with self.assertRaisesRegex(candidate.PreparedSkillConfigurationChanged, "dedicated native context"):
                candidate.prepare(SimpleNamespace(capture=capture), "/codex", "codex", self.workspace,
                                  self.source, self.root / "delegate")
        self.assertEqual(len(calls), 1)
        self.assertFalse(any(argument.startswith(("plugins.", "skills.config=")) for argument in calls[0]))
        self.assertEqual([argument for argument in calls[0] if argument.startswith("features.")], [
            "features.shell_snapshot=false", "features.shell_snapshot_v2=false",
        ])

    def test_selected_cache_and_malformed_native_metadata_boundaries_remain_explicit(self):
        profile = self.root / "codex"
        cache = profile / "plugins/cache"
        with patch.dict(candidate.os.environ, {"CODEX_HOME": str(profile)}):
            self.assertTrue(candidate._selected_plugin_skill(
                cache / "marketplace/delegation-layer/0.7.0/skills/agent-integration/SKILL.md"))
            self.assertFalse(candidate._selected_plugin_skill(
                self.root / "unrelated/marketplace/delegation-layer/0.7.0/skills/agent-integration/SKILL.md"))
            for path in ("relative/agent-integration/SKILL.md", str(cache / "../other/agent-integration/SKILL.md"),
                         str(cache / "agent-integration/not-a-skill.txt")):
                with self.subTest(path=path), self.assertRaises(ValueError):
                    candidate.skill_paths(self.context([path]))
        ambiguous = self.context([str(self.skill)])
        ambiguous[0]["content"][0]["text"] += '\n- `r0` = `/different/root`'
        with self.assertRaisesRegex(ValueError, "ambiguous native skill root alias"):
            candidate.skill_paths(ambiguous)

    def test_clean_profile_personal_symlink_to_other_profile_cache_is_not_an_active_plugin(self):
        physical = self.root / "physical-parent"
        physical.mkdir()
        logical = self.root / "logical-parent"
        logical.symlink_to(physical, target_is_directory=True)
        profile = logical / "clean-profile"
        external = logical / "other-profile/plugins/cache/market/plugin/0.7.0/skills/agent-integration/SKILL.md"
        external.parent.mkdir(parents=True)
        external.write_text("old personal skill target")
        personal = profile / "skills/agent-integration/SKILL.md"
        personal.parent.mkdir(parents=True)
        personal.symlink_to(external)
        def capture(name, _argv):
            paths = [str(self.skill), str(personal)] if name == "prepared-discovery-context" else [str(self.skill)]
            path = self.root / (name + ".json")
            path.write_text(json.dumps(self.context(paths)))
            return path
        with patch.dict(candidate.os.environ, {"CODEX_HOME": str(profile)}):
            self.assertFalse(candidate._selected_plugin_skill(personal))
            self.assertFalse(candidate._selected_plugin_skill(personal.resolve()))
            prepared = candidate.prepare(SimpleNamespace(capture=capture), "/codex", "codex", self.workspace,
                                         self.source, self.root / "delegate")
        disabled = prepared.evidence["disabled_skills"]
        if not isinstance(disabled, list):
            raise AssertionError("prepared context did not record its disabled skill paths")
        self.assertIn(str(personal), disabled)
        self.assertNotEqual(str(external), str(external.resolve()))
        self.assertIn(str(external.resolve(strict=True)), disabled)
        self.assertFalse(any(argument.startswith("plugins.") for argument in prepared.arguments))
        self.assertEqual([argument for argument in prepared.arguments if argument.startswith("features.")], [
            "features.shell_snapshot=false", "features.shell_snapshot_v2=false",
        ])

    def test_duplicate_or_wrong_final_candidate_blocks_before_execution(self):
        for paths in ([str(self.skill), str(self.skill)], [str(self.root / "other/agent-integration/SKILL.md")]):
            def capture(name, _argv):
                path = self.root / (name + ".json")
                path.write_text(json.dumps(self.context(paths)))
                return path
            with self.subTest(paths=paths), patch.object(candidate.Path, "home", return_value=self.root), \
                    self.assertRaisesRegex(ValueError, "uniquely"):
                candidate.prepare(SimpleNamespace(capture=capture), "/codex", "codex", self.workspace,
                                  self.source, self.root / "delegate")

    def write_events(self, events):
        self.transcript.write_text("".join(json.dumps(event) + "\n" for event in events))

    def codex_event(self, **changes):
        return {"type": "item.completed", "item": {"type": "command_execution", "status": "completed", "exit_code": 0,
                                                   "command": "cat " + str(self.skill), "aggregated_output": self.text,
                                                   **changes}}

    def verify(self, *, require_read=True):
        return candidate.verify_run("codex", self.controls, self.transcript, source_skill=self.source_skill,
                                    require_read=require_read)

    def test_codex_requires_full_successful_structured_read_not_path_echo_or_model_prose(self):
        self.write_events([self.codex_event()])
        evidence = self.verify()
        self.assertTrue(evidence["candidate_read"])
        self.assertFalse(evidence["native_hook_observed"])
        cases = [self.codex_event(exit_code=1), self.codex_event(aggregated_output=str(self.skill)),
                 self.codex_event(command="printf " + str(self.skill)),
                 {"type": "item.completed", "item": {"type": "agent_message", "text": self.text + str(self.skill)}}]
        for event in cases:
            with self.subTest(event=event):
                self.write_events([event])
                with self.assertRaisesRegex(ValueError, "successful candidate skill read"):
                    self.verify()

    def test_codex_accepts_completed_literal_reads_inside_observed_shell_compounds(self):
        path = shlex.quote(str(self.skill))
        for command in (
            "find .claude -name SKILL.md; for f in .claude/*; do cat \"$f\"; done; echo; cat " + path,
            shlex.join(["/bin/zsh", "-lc", "pwd; rg routing .; cat /engineering/SKILL.md /tests/SKILL.md "
                        + path + " /old/SKILL.md"]),
            shlex.join(["/bin/bash", "-lc", "pwd && sed -n '1,240p' " + path]),
            "pwd\ncat -- " + path,
        ):
            with self.subTest(command=command):
                self.write_events([self.codex_event(command=command)])
                self.assertTrue(self.verify()["candidate_read"])
        for command in ("echo " + path, "printf '%s' " + path, "cat /wrong/SKILL.md", "cat SKILL.md"):
            with self.subTest(command=command):
                self.write_events([self.codex_event(command=command)])
                with self.assertRaisesRegex(ValueError, "successful candidate skill read"):
                    self.verify()

    def test_claude_bash_read_requires_correlated_successful_complete_literal_output(self):
        command = "find .claude -name SKILL.md; echo; cat " + shlex.quote(str(self.skill))
        def events(*, output=self.text, error=False, result_id="bash-1", read_command=command):
            return self.claude_events()[:2] + [
                {"type": "assistant", "message": {"content": [{"type": "tool_use", "name": "Bash", "id": "bash-1",
                                                                 "input": {"command": read_command}}]}},
                {"type": "user", "message": {"content": [{"type": "tool_result", "tool_use_id": result_id,
                                                            "content": output, "is_error": error}]}},
            ]
        def verify(value):
            self.write_events(value)
            return candidate.verify_run("claude", self.controls, self.transcript,
                                        source_skill=self.source_skill, require_read=True)
        self.assertTrue(verify(events())["candidate_read"])
        self.assertTrue(verify(events(output=[{"type": "text", "text": self.text}]))["candidate_read"])
        for value in (events(output=str(self.skill)), events(output=self.text[:10]), events(error=True),
                      events(result_id="unmatched"), events(read_command="echo " + str(self.skill)),
                      events(read_command="printf '%s' " + str(self.skill)), events(read_command="cat /wrong/SKILL.md"),
                      events(read_command="cat .agents/skills/agent-integration/SKILL.md")):
            with self.subTest(value=value), self.assertRaisesRegex(ValueError, "successful candidate skill read"):
                verify(value)

    def test_codex_exact_workspace_relative_spelling_keeps_full_source_and_native_success_requirements(self):
        relative = ".agents/skills/agent-integration/SKILL.md"
        command = shlex.join(["/bin/zsh", "-lc", "cat " + relative])
        self.write_events([self.codex_event(command=command)])
        self.assertTrue(self.verify()["candidate_read"])
        for changed in (
            {"command": "cat ./" + relative}, {"command": "cat ../" + relative},
            {"command": 'cat "$candidate"'}, {"command": "cat " + relative + "/../SKILL.md"},
            {"command": "cd /other; cat " + relative}, {"command": "pushd /other; cat " + relative},
            {"command": "popd; cat " + relative}, {"aggregated_output": relative},
            {"aggregated_output": self.text[:10]}, {"exit_code": 1},
        ):
            with self.subTest(changed=changed):
                self.write_events([self.codex_event(command=command, **changed)] if "command" not in changed
                                  else [self.codex_event(**changed)])
                with self.assertRaisesRegex(ValueError, "successful candidate skill read"):
                    self.verify()
        self.write_events([self.codex_event(command=command)])
        elsewhere = {**self.controls, "candidate_skill": str(self.root / "elsewhere" / relative)}
        with self.assertRaisesRegex(ValueError, "successful candidate skill read"):
            candidate.verify_run("codex", elsewhere, self.transcript, source_skill=self.source_skill, require_read=True)

    def test_codex_relative_read_anchors_parent_aliases_without_accepting_other_workspaces(self):
        physical = self.root / "physical-parent"
        physical.mkdir()
        logical = self.root / "logical-parent"
        logical.symlink_to(physical, target_is_directory=True)
        relative = ".agents/skills/agent-integration/SKILL.md"
        skill = logical / "workspace" / relative
        skill.parent.mkdir(parents=True)
        skill.write_text(self.text)
        controls = {**self.controls, "candidate_skill": str(skill)}
        transcript = logical / "stdout.txt"
        events = [self.codex_event(command="cat " + relative)]
        transcript.write_text(json.dumps(events[0]) + "\n")
        self.assertNotEqual(skill, skill.resolve())
        self.assertTrue(candidate.verify_run("codex", controls, transcript, source_skill=self.source_skill,
                                             require_read=True)["candidate_read"])
        for candidate_skill, workspace in ((skill, physical / "workspace"), (skill.resolve(), logical / "workspace")):
            with self.subTest(candidate_skill=candidate_skill, workspace=workspace):
                self.assertTrue(candidate._codex_read(events, candidate_skill, self.text, workspace))
        for changed in ({"command": "cat ./" + relative}, {"command": "cat ../" + relative},
                        {"command": 'cat "$candidate"'}, {"command": "cd /other; cat " + relative},
                        {"aggregated_output": self.text[:10]}, {"exit_code": 1}):
            with self.subTest(changed=changed):
                event = self.codex_event(**{"command": "cat " + relative, **changed})
                transcript.write_text(json.dumps(event) + "\n")
                with self.assertRaisesRegex(ValueError, "successful candidate skill read"):
                    candidate.verify_run("codex", controls, transcript, source_skill=self.source_skill, require_read=True)
        elsewhere = self.root / "distinct-workspace" / relative
        elsewhere.parent.mkdir(parents=True)
        elsewhere.write_text(self.text)
        transcript.write_text(json.dumps(events[0]) + "\n")
        with self.assertRaisesRegex(ValueError, "successful candidate skill read"):
            candidate.verify_run("codex", {**controls, "candidate_skill": str(elsewhere)}, transcript,
                                 source_skill=self.source_skill, require_read=True)

    def test_negative_case_may_avoid_read_but_still_requires_current_candidate_identity(self):
        self.write_events([])
        self.assertFalse(self.verify(require_read=False)["candidate_read"])
        self.source_skill.write_text("changed current source")
        with self.assertRaisesRegex(ValueError, "current source"):
            self.verify(require_read=False)

    def claude_events(self, *, read_input=None, content="candidate source contents", error=False):
        plugin = self.skill.parents[2]
        return [
            {"type": "system", "subtype": "init", "plugins": [{"name": "delegation-layer", "path": str(plugin)}]},
            {"type": "system", "subtype": "hook_response", "hook_event": "SessionStart", "exit_code": 0,
             "stdout": "Read the complete agent-integration skill at " + str(self.skill)},
            {"type": "assistant", "message": {"content": [{"type": "tool_use", "name": "Read", "id": "read-1",
                                                             "input": {"file_path": str(self.skill), **(read_input or {})}}]}},
            {"type": "user", "message": {"content": [{"type": "tool_result", "tool_use_id": "read-1",
                                                        "content": content, "is_error": error}]}},
        ]

    def test_claude_requires_hook_plugin_identity_and_successful_whole_file_read(self):
        self.write_events(self.claude_events())
        verify = lambda: candidate.verify_run("claude", self.controls, self.transcript,
                                              source_skill=self.source_skill, require_read=True)
        self.assertTrue(verify()["native_hook_observed"])
        for events in (self.claude_events(read_input={"limit": 1}), self.claude_events(read_input={"offset": 2}),
                       self.claude_events(content=""), self.claude_events(error=True), self.claude_events()[2:]):
            with self.subTest(events=events):
                self.write_events(events)
                with self.assertRaises(ValueError):
                    verify()

    def test_claude_preparation_uses_plugin_settings_without_duplicate_hook_injection(self):
        skill = self.source / "plugins/delegation-layer/skills/agent-integration/SKILL.md"
        skill.parent.mkdir(parents=True)
        skill.write_text(self.text)
        run = self.root / "run"
        (run / "candidate-bin").mkdir(parents=True)
        binaries = self.root / "bin"
        binaries.mkdir()
        for name in ("delegate", "delegate-run", "pueue", "pueued"):
            path = binaries / name
            path.write_text("#!/bin/sh\nexit 0\n")
            path.chmod(0o700)
        calls = []
        def capture(name, argv):
            calls.append(argv)
            path = self.root / (name + ".txt")
            path.write_text("--plugin-dir --settings --include-hook-events" if name == "prepared-native-help" else "")
            return path
        owner = SimpleNamespace(capture=capture, run=run, client=binaries / "pueue", daemon_path=binaries / "pueued",
                                processes=SimpleNamespace(environment={}))
        prepared = candidate.prepare(owner, "/claude", "claude", self.workspace, self.source, binaries / "delegate")
        self.assertEqual(json.loads(prepared.arguments[3]), {"skillOverrides": {"agent-integration": "off"}})
        self.assertIn("--include-hook-events", prepared.arguments)
        self.assertNotIn("--safe-mode", prepared.arguments)
        self.assertNotIn("--bare", prepared.arguments)
        self.assertNotIn("hooks", prepared.arguments[3])
        self.assertEqual(owner.processes.environment, {"DELEGATION_LAYER_INSTALL_DIR": str(run / "candidate-bin")})
        self.assertTrue((run / "candidate-bin/delegate-run").samefile(binaries / "delegate-run"))
        self.assertIn("CLAUDE_PLUGIN_ROOT=" + str(self.source / "plugins/delegation-layer"), calls[-1])

    def test_claude_native_skill_load_requires_correlated_success_exact_path_and_complete_body(self):
        source = "---\nname: agent-integration\n---\n\n" + self.text
        self.source_skill.write_text(source)
        self.controls["skill_sha256"] = hashlib.sha256(source.encode()).hexdigest()
        events = self.claude_events()[:2] + [
            {"type": "assistant", "message": {"content": [{"type": "tool_use", "name": "Skill", "id": "skill-1",
                                                             "input": {"skill": "delegation-layer:agent-integration"}}]}},
            {"type": "user", "message": {"content": [{"type": "tool_result", "tool_use_id": "skill-1",
                                                        "content": "Launching skill: delegation-layer:agent-integration"}]}},
            {"type": "user", "message": {"content": [{"type": "text", "text":
                "Base directory for this skill: " + str(self.skill.parent) + "\n\n" + self.text}]}},
        ]
        def verify(value):
            self.write_events(value)
            return candidate.verify_run("claude", self.controls, self.transcript,
                                        source_skill=self.source_skill, require_read=True)
        self.assertTrue(verify(events)["candidate_read"])
        for index, field, changed in (
            (2, "input", {"skill": "other-plugin:agent-integration"}),
            (3, "is_error", True), (3, "tool_use_id", "unmatched"),
            (3, "content", "Launching skill: agent-integration"),
            (4, "text", "Base directory for this skill: /old/agent-integration\n\n" + self.text),
            (4, "text", "Base directory for this skill: " + str(self.skill.parent) + "\n\n# Candidate skill\n"),
        ):
            with self.subTest(field=field, changed=changed):
                value = json.loads(json.dumps(events))
                value[index]["message"]["content"][0][field] = changed
                with self.assertRaisesRegex(ValueError, "successful candidate skill read"):
                    verify(value)
        with self.assertRaisesRegex(ValueError, "successful candidate skill read"):
            verify([*events[:4], {"type": "assistant", "message": events[4]["message"]}])

    def test_native_scratch_is_owned_outside_workspace_and_state_and_cleaned_on_setup_failure(self):
        executable = self.root / "delegate"
        executable.write_text("#!/bin/sh\nexit 0\n")
        executable.chmod(0o700)
        observed = []
        def refuse_setup(run, environment, _ownership, *, state_root):
            scratch = Path(environment["TMPDIR"])
            observed.append(scratch)
            self.assertTrue(scratch.is_dir())
            self.assertFalse(scratch.is_relative_to(run / "workspace"))
            self.assertFalse(scratch.is_relative_to(state_root))
            raise FileNotFoundError("fixture stops before any native process")
        with patch.object(cli, "_candidate_shadow_reason", return_value=None), \
                patch.object(cli.shutil, "which", return_value="/mock/codex"), \
                patch.object(cli, "AttemptSupervisor", side_effect=refuse_setup):
            receipt = cli.run_case(output=self.root, harness="codex", model=None,
                                   case={"name": "scratch", "prompt": "Local work", "external": False},
                                   repetition=1, mode="routing", delegate_bin=executable)
        self.assertEqual(receipt["status"], "blocked")
        self.assertEqual(len(observed), 1)
        self.assertFalse(observed[0].exists())

    def test_only_prepared_codex_owns_empty_shell_startup_and_cleanup_preserves_native_profile(self):
        executable = self.root / "delegate"
        executable.write_text("#!/bin/sh\nexit 0\n")
        executable.chmod(0o700)
        observed = []
        for harness, prepared in (("codex", True), ("codex", False), ("claude", True), ("claude", False)):
            def refuse_setup(run, environment, _ownership, *, state_root):
                self.assertEqual(environment["HOME"], "/native/home")
                self.assertEqual(environment["CODEX_HOME"], "/native/codex-profile")
                if prepared and harness == "codex":
                    directory = Path(environment["ZDOTDIR"])
                    observed.append(directory)
                    self.assertEqual(directory, run / "native-shell")
                    self.assertEqual(list(directory.iterdir()), [])
                    self.assertFalse(directory.is_relative_to(run / "workspace"))
                    self.assertFalse(directory.is_relative_to(state_root))
                else:
                    self.assertEqual(environment["ZDOTDIR"], "/native/zsh-startup")
                    self.assertFalse((run / "native-shell").exists())
                raise FileNotFoundError("fixture stops before native launch")
            with self.subTest(harness=harness, prepared=prepared), \
                    patch.dict(cli.os.environ, {"HOME": "/native/home", "CODEX_HOME": "/native/codex-profile",
                                                "ZDOTDIR": "/native/zsh-startup"}), \
                    patch.object(cli, "_candidate_shadow_reason", return_value=None), \
                    patch.object(cli.shutil, "which", return_value="/mock/native"), \
                    patch.object(cli, "AttemptSupervisor", side_effect=refuse_setup):
                receipt = cli.run_case(output=self.root, harness=harness, model=None,
                                       case={"name": harness + "-" + str(prepared), "prompt": "Local work", "external": False},
                                       repetition=1, routing_context=True, prepared_context=prepared, delegate_bin=executable)
                self.assertEqual(receipt["status"], "blocked")
                self.assertEqual(cli.os.environ["ZDOTDIR"], "/native/zsh-startup")
        self.assertEqual(len(observed), 1)
        self.assertFalse(observed[0].exists())

    def test_local_composite_still_requires_full_candidate_load_during_capture_and_summary(self):
        executable = self.root / "delegate"
        executable.write_text("#!/bin/sh\nexit 0\n")
        executable.chmod(0o700)
        capture = self.root / "capture.txt"
        capture.write_text("controlled metadata fixture")
        case = {"name": "composite", "prompt": "Plan and research the correction.",
                "external": False, "require_candidate_read": True}
        observation = {"candidate_read": True, "read_required": True}
        prepared = candidate.PreparedContext((), cli.SKILL / "SKILL.md", {
            "candidate_skill": str(cli.SKILL / "SKILL.md"),
            "visible_skill_comparison": "baseline-with-target-replaced",
        })
        with patch.object(cli, "_candidate_shadow_reason", return_value=None), \
                patch.object(cli.shutil, "which", return_value="/mock/codex"), \
                patch.object(cli, "AttemptSupervisor") as supervisor, \
                patch.object(candidate, "prepare", return_value=prepared), \
                patch.object(candidate, "verify_run", return_value=observation) as verify:
            owner = supervisor.return_value
            owner.processes.environment = {}
            owner.capture.return_value = capture
            owner.execute.return_value = {"result": "Success"}
            captured = cli.run_case(output=self.root, harness="codex", model=None, case=case, repetition=1,
                                    routing_context=True, prepared_context=True, delegate_bin=executable)
            self.assertTrue(verify.call_args.kwargs["require_read"])
            run = self.root / "composite-1"
            self.assertEqual(captured["prepared_controls"]["shell_startup_directory"], str(run / "native-shell"))
            self.assertFalse((run / "native-shell").exists())
            self.assertNotIn("shell_startup_directory", prepared.evidence)
            for name in ("prepared-discovery-context", "prepared-native-context"):
                directory = run / name
                directory.mkdir()
                (directory / "stdout.txt").write_text(json.dumps(self.context([str(cli.SKILL / "SKILL.md")])))
            cli.write_json(run / "judgment.json", {"status": "passed", "receipt_sha256": cli.digest(run / "receipt.json"),
                                                    "evidence": "Controlled capture/summary policy test fixture."})
            cli.summarize(output=self.root, harness="codex", cases=[case], repetitions=1,
                          routing_context=True, prepared_context=True)
            self.assertEqual(verify.call_count, 2)
            self.assertTrue(verify.call_args.kwargs["require_read"])

    def test_unrelated_skill_change_has_actionable_static_receipt_before_paid_execution(self):
        executable = self.root / "delegate"
        executable.write_text("#!/bin/sh\nexit 0\n")
        executable.chmod(0o700)
        capture = self.root / "capture.txt"
        capture.write_text("controlled metadata fixture")
        with patch.object(cli, "_candidate_shadow_reason", return_value=None), \
                patch.object(cli.shutil, "which", return_value="/mock/codex"), \
                patch.object(cli, "AttemptSupervisor") as supervisor, \
                patch.object(candidate, "prepare", side_effect=candidate.PreparedSkillConfigurationChanged("PRIVATE_CANARY")):
            owner = supervisor.return_value
            owner.processes.environment = {}
            owner.capture.return_value = capture
            receipt = cli.run_case(output=self.root, harness="codex", model=None,
                                   case={"name": "strict-context", "prompt": "Plan the correction.", "external": False},
                                   repetition=1, routing_context=True, prepared_context=True, delegate_bin=executable)
            owner.execute.assert_not_called()
            self.assertEqual((receipt["status"], receipt["reason"]), ("blocked", "prepared_skill_config_replaced"))
            self.assertEqual(receipt["guidance"], "Prepare a dedicated native context that preserves unrelated visible skills.")
            self.assertNotIn("PRIVATE_CANARY", json.dumps(receipt))
