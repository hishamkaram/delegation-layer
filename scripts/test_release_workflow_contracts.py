"""Cross-tag publication and proof-retention constraints for release workflows."""

from pathlib import Path
import os
import re
import sys
import textwrap
import unittest

from scripts.delegate_acceptance.processes.operations import run_bounded_command
from scripts.test_published_install import PublishedBase, REVISION, TAG


WORKFLOWS = Path(__file__).resolve().parent.parent / ".github/workflows"
NATIVE_GATE = "Validate exact proof and public installation prerequisites"
ACCEPTED_FAILURE = "v0.1.32-macos-opencode-timeout-evidence"
EXCEPTION_REPO, EXCEPTION_TAG = "hishamkaram/delegation-layer", "v0.1.32"
RUNTIME_REVISION, PROMOTION_REVISION = "a6e8e103c688412e1ef8cb3db59624bd54fe7603", "b" * 40
MACOS_RUN, LINUX_RUN = 39402274329, 39403240260
# Pinned production digests are replaced by digests of these fixed fake bytes.
PROOFS = {
    MACOS_RUN: {"platform": "Darwin", "asset": 658890558, "bytes": "darwin-proof",
                "invocation": "1ea656aaecb78a5355ff3d8222c31e26",
                "pinned": "0d0ccbaed8076949616fb4b6e2116d1e6e777abb94d49ea669b6ad9596e5931a",
                "sha": "9d2c957e3e32eba3f749abc3980e74e22750e255b559bf29c50d3857c0bcb761"},
    LINUX_RUN: {"platform": "Linux", "asset": 658909723, "bytes": "linux-proof",
                "invocation": "3638cd62f371dc38eae345dee9bb7247",
                "pinned": "efa6375dd6f89a9313e481a2246a19325ddd1e00251ed617fd54dcf04b711950",
                "sha": "af299bd71edd1c2a0c16a5ced481b3eccd5b13a20fa7a653ab1c3ab2335e4de8"},
}
PROOF_SOURCE_HARNESS = "94b6d3b0139367adcb53a53a8b3207af7eb27cffbc1ed3c0a15835d169b4c228"
# Deterministic fake release archives shared by the fake gh and the fake proof parser.
ARCHIVE_HELPER = """import gzip, hashlib, io, tarfile


def build_archive(members):
    buffer = io.BytesIO()
    with gzip.GzipFile(fileobj=buffer, mode='wb', mtime=0) as compressed:
        with tarfile.open(fileobj=compressed, mode='w') as bundle:
            for name, content in sorted(members.items()):
                info = tarfile.TarInfo(name)
                info.size, info.mode = len(content.encode()), 0o755
                bundle.addfile(info, io.BytesIO(content.encode()))
    return buffer.getvalue()
"""
FAKE_GH = ARCHIVE_HELPER + """import ast, json, os, sys, zipfile
with open(os.environ['FAKE_GH_WORLD'], encoding='utf-8') as source:
    world = ast.literal_eval(source.read())
command = sys.argv[1:3]
if command == ['release', 'download']:
    options = list(zip(sys.argv[4::2], sys.argv[5::2]))
    directory = dict(options)['--dir']
    checksums = []
    for name in [value for option, value in options if option == '--pattern' and value != 'checksums.txt']:
        data = build_archive(world['archives'][name])
        with open(os.path.join(directory, name), 'wb') as output:
            output.write(data)
        checksums.append((world['checksum_override'] or hashlib.sha256(data).hexdigest()) + '  ' + name)
    with open(os.path.join(directory, 'checksums.txt'), 'w', encoding='ascii') as output:
        output.write('\\n'.join(checksums) + '\\n')
    sys.exit(0)
if command == ['attestation', 'verify']:
    options = dict(zip(sys.argv[4::2], sys.argv[5::2]))
    sys.exit(0 if os.path.basename(sys.argv[3]) in world['attested']
             and options == world['attestation'] else 1)
endpoint = next(argument for argument in sys.argv[1:] if argument.startswith('repos/'))
if endpoint in world['reports']:
    buffer = io.BytesIO()
    with zipfile.ZipFile(buffer, 'w') as bundle:
        bundle.writestr('report.json', json.dumps(world['reports'][endpoint]))
    sys.stdout.buffer.write(buffer.getvalue())
elif endpoint in world['text']:
    sys.stdout.write(world['text'][endpoint])
elif endpoint in world['json']:
    sys.stdout.write(json.dumps(world['json'][endpoint]))
else:
    sys.exit('unexpected endpoint ' + endpoint)
"""
# Real ZIP parsing is covered by public-proof tests; this double checks only the trusted
# expectation the exception passes, so archive download/checksum/attestation stay real.
FAKE_PROOF_PARSER = ARCHIVE_HELPER + """import ast, os, sys
sys.path.insert(0, os.environ['HARNESS_ROOT'])
from scripts.delegate_acceptance.native_evidence import public_validation
from scripts.delegate_acceptance.native_evidence.public_contract import PublicProofError
with open(os.environ['FAKE_GH_WORLD'], encoding='utf-8') as source:
    claims = ast.literal_eval(source.read())['claims']
BINARY_FIELDS = {'delegate': 'delegate', 'runner': 'delegate-run', 'pueue': 'pueue', 'pueued': 'pueued'}


def digest(data):
    return hashlib.sha256(data).hexdigest()


def inspect(data):
    return {'platform': claims[data.decode()]['identity']['platform']}


def validate(data, expectation):
    claim = claims[data.decode()]
    observed = {key: getattr(expectation, key) for key in claim['identity'] if key != 'platform'}
    observed['platform'] = {'os': expectation.platform_os, 'arch': expectation.platform_arch}
    if (observed != claim['identity']
            or expectation.archive_sha256 != digest(build_archive(claim['archive']))):
        raise PublicProofError('public_identity_binding_mismatch')
    if any(getattr(expectation, field + '_sha256') != digest(claim['binaries'][name].encode())
           for field, name in BINARY_FIELDS.items()):
        raise PublicProofError('public_binary_binding_mismatch')
    return {'status': 'IDENTITY_VALID'}


public_validation.inspect_public_proof = inspect
public_validation.validate_public_identity = validate
exec(compile(sys.stdin.read(), 'accepted-native-failure', 'exec'), {'__name__': '__main__'})
"""


def workflow_step(source: str, name: str) -> str:
    return source.split(f"      - name: {name}\n", 1)[1].split("\n      - name: ", 1)[0]


def step_script(name: str) -> str:
    source = (WORKFLOWS / "release.yml").read_text(encoding="utf-8")
    return textwrap.dedent(workflow_step(source, name).split("run: |\n", 1)[1])


class ReleaseWorkflowContractTests(unittest.TestCase):
    def test_published_recovery_skips_publication_and_keeps_consumer_dependencies(self):
        source = (WORKFLOWS / "release.yml").read_text(encoding="utf-8")
        self.assertIn("      resume_published:\n", source)
        for name in ("Validate exact proof and public installation prerequisites",
                     "Configure npm credentials", "Publish or verify npm package",
                     "Publish GitHub Release"):
            with self.subTest(name=name):
                self.assertIn("if: ${{ !inputs.resume_published }}", workflow_step(source, name))
        self.assertIn("if: ${{ inputs.resume_published }}",
                      workflow_step(source, "Validate already published release"))
        prerequisites = workflow_step(source, "Validate release prerequisites")
        self.assertNotIn("inputs.resume_published", prerequisites)
        self.assertNotIn("accepted_native_failure", prerequisites)
        self.assertNotIn("NPM_TOKEN", prerequisites)
        for check in ("package.json", "git merge-base --is-ancestor", "push_conclusion", "checksums.txt"):
            self.assertIn(check, prerequisites)
        self.assertIn("    needs: promote\n", source)
        self.assertIn("    needs: published-npm\n", source)
        self.assertIn("    needs: publish-homebrew\n", source)

    def test_accepted_native_failure_input_defaults_to_strict_promotion(self):
        source = (WORKFLOWS / "release.yml").read_text(encoding="utf-8")
        declaration = source.split("      accepted_native_failure:\n", 1)[1].split("\n      resume_published:", 1)[0]
        self.assertIn("        required: false\n        default: ''\n        type: string", declaration)
        gate = workflow_step(source, NATIVE_GATE)
        self.assertIn("ACCEPTED_NATIVE_FAILURE: ${{ inputs.accepted_native_failure }}", gate)
        self.assertEqual(source.count(ACCEPTED_FAILURE), 1)
        for proof in PROOFS.values():
            self.assertIn(proof["pinned"], gate)
        self.assertIn(f"PROOF_SOURCE_HARNESS_SHA256 = '{PROOF_SOURCE_HARNESS}'", gate)

    def test_every_tag_uses_one_workflow_wide_publication_lock(self):
        source = (WORKFLOWS / "release.yml").read_text(encoding="utf-8")
        concurrency = source.split("\nconcurrency:\n", 1)[1].split("\njobs:\n", 1)[0]
        group = re.search(r"^  group: ([^\n]+)$", concurrency, re.MULTILINE)
        self.assertIsNotNone(group)
        if group is None:
            raise AssertionError("workflow-wide publication lock missing")
        self.assertEqual(group.group(1), "release-promotion")
        self.assertNotIn("${{", group.group(1))
        self.assertIn("  queue: max", concurrency)
        self.assertIn("  cancel-in-progress: false", concurrency)
        self.assertNotRegex(source.split("\njobs:\n", 1)[1], r"(?m)^ +concurrency:")

    def test_native_reports_have_a_documented_public_promotion_window(self):
        source = (WORKFLOWS / "release-native-acceptance.yml").read_text(encoding="utf-8")
        self.assertIn("retention-days: 90", source)
        documentation = (WORKFLOWS.parent.parent / "docs/contributing.md").read_text(encoding="utf-8")
        self.assertIn("Attempt reports request 90-day retention", documentation)
        self.assertIn("all tags", documentation)


class ConsumerReceiptIntegrationTests(PublishedBase):
    def test_recovery_guard_requires_latest_public_stable_release(self):
        source = (WORKFLOWS / "release.yml").read_text(encoding="utf-8")
        code = textwrap.dedent(workflow_step(
            source, "Validate already published release").split("run: |\n", 1)[1])
        (self.fake_bin / "gh").write_text(
            '#!/bin/sh\ncase "$*" in\n'
            '  *"/releases/latest"*) printf "%s\\n" "$LATEST_RELEASE" ;;\n'
            '  *"/releases/tags/"*) printf "%s\\n" "$PUBLISHED_READY" ;;\n'
            '  *) printf "%s\\n" "$REPOSITORY_PRIVATE" ;;\nesac\n')
        for changes, expected in (({}, 0), ({"REPOSITORY_PRIVATE": "true"}, 1),
                                  ({"PUBLISHED_READY": "false"}, 1),
                                  ({"LATEST_RELEASE": "v9.9.9"}, 1),
                                  ({"ACCEPTED_NATIVE_FAILURE": ACCEPTED_FAILURE}, 1)):
            with self.subTest(changes=changes):
                env = {"PATH": str(self.fake_bin) + os.pathsep + os.defpath,
                       "GITHUB_REPOSITORY": "owner/repository", "TAG": TAG,
                       "ACCEPTED_NATIVE_FAILURE": "",
                       "LATEST_RELEASE": TAG, "PUBLISHED_READY": "true",
                       "REPOSITORY_PRIVATE": "false", **changes}
                result = run_bounded_command(["/bin/bash", "-euo", "pipefail", "-c", code], env,
                                             max_output=4096, capture=True, timeout=10)
                self.assertEqual(result.exit_code, expected, result.output.decode())

    def workflow_python(self, name: str) -> None:
        source = (WORKFLOWS / "release-consumer.yml").read_text(encoding="utf-8")
        step = workflow_step(source, name)
        code = step.split("python3 - <<'PY'\n", 1)[1].split("\n          PY", 1)[0]
        env = {"TAG": TAG, "CHANNEL": self.channel, "GITHUB_SHA": REVISION,
               "RUNNER_TEMP": str(self.runner_temp), "GITHUB_OUTPUT": str(self.root / "outputs")}
        result = run_bounded_command([sys.executable, "-c", textwrap.dedent(code)], env,
                                     max_output=4096, capture=True, timeout=10)
        self.assertEqual(result.exit_code, 0, result.output.decode())

    def initialize_workflow(self) -> None:
        self.receipt_path = self.runner_temp / "published-install-receipt.json"
        self.workflow_python("Initialize partial-publication failure receipt before setup")
        self.assertFalse(self.receipt_path.exists())

    def selected_report(self) -> Path:
        self.workflow_python("Select consumer report without replacing verifier state")
        value = (self.root / "outputs").read_text().strip()
        self.assertTrue(value.startswith("path="))
        return Path(value.removeprefix("path="))

    def test_workflow_bootstrap_allows_real_consumer_verification(self):
        self.initialize_workflow()
        self.assertEqual(self.publish(), 0)
        before = self.receipt_path.read_bytes()
        self.assertEqual(self.selected_report(), self.receipt_path)
        self.assertEqual(self.receipt_path.read_bytes(), before)
        self.assertEqual(self.receipt()["verdict"], "pass")

    def test_setup_failure_retains_fallback_without_creating_verifier_state(self):
        self.initialize_workflow()
        report = self.selected_report()
        self.assertEqual(report.name, "published-install-setup-failure.json")
        self.assertIn("published_verification_incomplete", report.read_text())
        self.assertFalse(self.receipt_path.exists())

    def test_verifier_failure_is_preserved_instead_of_setup_fallback(self):
        self.initialize_workflow()
        self.world.view_overrides = {"dist.integrity": "invalid"}
        self.assertEqual(self.publish(), 1)
        self.assertEqual(self.selected_report(), self.receipt_path)
        self.assertEqual(self.receipt()["verdict"], "fail")


class AcceptedNativeFailureGateTests(PublishedBase):
    def setUp(self):
        super().setUp()
        gh = self.fake_bin / "gh"
        gh.write_text(f"#!{sys.executable}\n{FAKE_GH}", encoding="utf-8")
        gh.chmod(0o755)
        parser = self.fake_bin / "python3"
        parser.write_text(f"#!{sys.executable} -I\n{FAKE_PROOF_PARSER}", encoding="utf-8")
        parser.chmod(0o755)
        self.runs = 0
        self.workspace = self.root / "workspace"
        (self.workspace / ".local").mkdir(parents=True)
        (self.workspace / ".local/release-harness").symlink_to(WORKFLOWS.parent.parent)
        self.summary = self.root / "step-summary.md"
        self.github_world = self.exception_world()

    def exception_world(self) -> dict:
        repo, world = f"repos/{EXCEPTION_REPO}", {"json": {}, "reports": {}, "text": {}}
        world["json"][repo] = {"id": 1368219645, "full_name": EXCEPTION_REPO,
                               "private": False, "visibility": "public"}
        world["json"][f"{repo}/branches/main"] = {"protected": True,
                                                  "commit": {"sha": PROMOTION_REVISION}}
        world["text"][f"{repo}/git/ref/tags/{EXCEPTION_TAG}"] = (
            f"refs/tags/{EXCEPTION_TAG}\tcommit\t{RUNTIME_REVISION}\n")
        runs, assets = [], []
        for index, (run_id, proof) in enumerate(PROOFS.items()):
            runs.append({"id": run_id, "run_attempt": 1, "display_title":
                         f"Native proof {EXCEPTION_TAG} {proof['invocation']} {proof['sha']}"})
            self.add_attempt(world, run_id, 1, f"2026-10-19T1{index}:00:00Z", self.report(run_id))
            assets.append({"id": proof["asset"], "uploader": {"id": 24391550},
                           "name": f"native-proof-{proof['platform']}-arm64-{proof['invocation']}.zip"})
            world["text"][f"{repo}/releases/assets/{proof['asset']}"] = proof["bytes"]
        world["json"][self.runs_endpoint()] = {"total_count": len(runs), "workflow_runs": runs}
        world["json"][f"{repo}/releases?per_page=30&page=1"] = [
            {"id": 7, "tag_name": EXCEPTION_TAG, "prerelease": False, "assets": assets}]
        world.update(archives={}, claims={}, attested=[], checksum_override=None, attestation={
            "--repo": EXCEPTION_REPO, "--source-ref": f"refs/tags/{EXCEPTION_TAG}",
            "--signer-workflow": f"{EXCEPTION_REPO}/.github/workflows/release.yml",
            "--source-digest": RUNTIME_REVISION})
        for proof in PROOFS.values():
            members = {name: f"{proof['platform']}-{name}"
                       for name in ("delegate", "delegate-run", "pueue", "pueued")}
            world["archives"][self.archive_name(proof)] = dict(members)
            world["attested"].append(self.archive_name(proof))
            world["claims"][proof["bytes"]] = {"archive": dict(members), "binaries": dict(members),
                                               "identity": {
                "repo": EXCEPTION_REPO, "tag": EXCEPTION_TAG, "release_revision": RUNTIME_REVISION,
                "harness_revision": RUNTIME_REVISION, "opencode_model": "opencode/big-pickle",
                "ccr_model": "litellm-glm-5-2", "source_harness_sha256": PROOF_SOURCE_HARNESS,
                "platform": {"os": proof["platform"], "arch": "arm64"}}}
        return world

    @staticmethod
    def archive_name(proof: dict) -> str:
        return f"delegation-layer_{EXCEPTION_TAG[1:]}_{proof['platform']}_arm64.tar.gz"

    @staticmethod
    def runs_endpoint() -> str:
        return (f"repos/{EXCEPTION_REPO}/actions/workflows/release-native-acceptance.yml/runs"
                f"?event=workflow_dispatch&head_sha={RUNTIME_REVISION}&branch=main&per_page=100&page=1")

    @staticmethod
    def report(run_id: int) -> dict:
        proof = PROOFS[run_id]
        report = {"schema_version": 1, "kind": "native-proof-validation",
                  "repository_id": 1368219645, "tag": EXCEPTION_TAG,
                  "harness_revision": RUNTIME_REVISION, "release_revision": RUNTIME_REVISION,
                  "run_id": run_id, "run_attempt": 1, "sender_id": 24391550,
                  "proof_sha256": proof["sha"], "proof_asset_id": proof["asset"],
                  "invocation_id": proof["invocation"], "opencode_model": "opencode/big-pickle",
                  "ccr_model": "litellm-glm-5-2", "admission": "admitted"}
        if run_id == MACOS_RUN:
            return {**report, "verdict": "fail", "native_status": "BLOCKED",
                    "stage": "validate-native-facts",
                    "failure": {"code": "native_acceptance_not_passing",
                                "stage": "validate-native-facts"}}
        return {**report, "verdict": "pass", "native_verdict": "pass", "native_status": "PASS",
                "stage": "validate-history",
                "history": {"allowed": False, "reason": "native_history_blocked",
                            "observed_attempts": 2},
                "failure": {"code": "native_history_blocked", "stage": "validate-history"}}

    @staticmethod
    def add_attempt(world: dict, run_id: int, attempt: int, started: str, report: dict) -> None:
        repo, artifact = f"repos/{EXCEPTION_REPO}", run_id * 10 + attempt
        world["json"][f"{repo}/actions/runs/{run_id}/attempts/{attempt}"] = {
            "id": run_id, "run_attempt": attempt, "head_sha": RUNTIME_REVISION,
            "head_branch": "main", "event": "workflow_dispatch", "run_started_at": started,
            "status": "completed", "conclusion": "failure"}
        world["json"].setdefault(f"{repo}/actions/runs/{run_id}/artifacts?per_page=100&page=1",
                                 {"artifacts": []})["artifacts"].append(
            {"name": f"native-proof-validation-{run_id}-{attempt}", "id": artifact, "expired": False})
        world["reports"][f"{repo}/actions/artifacts/{artifact}/zip"] = report

    def run_gate(self, **changes: str):
        code = step_script(NATIVE_GATE)
        for proof in PROOFS.values():
            code = code.replace(proof["pinned"], proof["sha"])
        world_path = self.root / "github-world.txt"
        world_path.write_text(repr(self.github_world), encoding="utf-8")
        self.runs += 1
        self.runner = self.root / f"runner-{self.runs}"
        self.runner.mkdir()
        env = {"PATH": str(self.fake_bin) + os.pathsep + os.defpath, "GH_TOKEN": "token",
               "GITHUB_REPOSITORY": EXCEPTION_REPO, "GITHUB_SHA": PROMOTION_REVISION,
               "TAG": EXCEPTION_TAG, "NATIVE_VALIDATION_RUN_ID": str(LINUX_RUN),
               "ACCEPTED_NATIVE_FAILURE": ACCEPTED_FAILURE, "RUNNER_TEMP": str(self.runner),
               "GITHUB_STEP_SUMMARY": str(self.summary), "FAKE_GH_WORLD": str(world_path),
               **changes}
        # The bounded runner discards stderr; route it to stdout to observe failure codes.
        return run_bounded_command(["/bin/bash", "-euo", "pipefail", "-c", "exec 2>&1\n" + code], env,
                                   max_output=65536, capture=True, timeout=60, cwd=self.workspace)

    def test_known_exception_accepts_exact_history_and_proof_bytes(self):
        result = self.run_gate()
        self.assertEqual(result.exit_code, 0, result.output.decode())
        summary = self.summary.read_text(encoding="utf-8")
        self.assertIn("Strict native acceptance did not pass", summary)
        self.assertIn("this promotion is not a native acceptance pass", summary)
        self.assertIn("case-17-opencode-run-workspace-write-timeout-resume", summary)
        for run_id, proof in PROOFS.items():
            self.assertIn(f"https://github.com/{EXCEPTION_REPO}/actions/runs/{run_id}", summary)
            self.assertIn(f"asset `{proof['asset']}`", summary)
        self.assertIn(f"`{PROMOTION_REVISION}` (current protected main)", summary)
        self.assertIn(f"source fingerprint `{PROOF_SOURCE_HARNESS}`", summary)
        self.assertIn("revalidated against the current release archive, checksums.txt, "
                      "release attestation and binary hashes", summary)
        self.assertIn("expected timeout occurred, then the parent FIFO had extra or malformed "
                      "evidence before continuation was invoked", summary)
        self.assertIn("Two Pi cases on macOS were BLOCKED", summary)
        for proof in PROOFS.values():
            downloaded = (self.runner / "accepted-native-failure" / proof["platform"]
                          / "assets" / self.archive_name(proof))
            self.assertTrue(downloaded.is_file(), downloaded)

    def test_known_exception_rejects_any_changed_fact(self):
        repo = f"repos/{EXCEPTION_REPO}"
        linux_report = f"{repo}/actions/artifacts/{LINUX_RUN * 10 + 1}/zip"
        linux_archive = self.archive_name(PROOFS[LINUX_RUN])
        linux_claim = PROOFS[LINUX_RUN]["bytes"]

        def extra_attempt(world):
            world["json"][self.runs_endpoint()]["workflow_runs"][1]["run_attempt"] = 2
            self.add_attempt(world, LINUX_RUN, 2, "2026-10-19T12:00:00Z",
                             {**self.report(LINUX_RUN), "run_attempt": 2})

        def missing_attempt(world):
            world["json"][self.runs_endpoint()] = {
                "total_count": 1, "workflow_runs": world["json"][self.runs_endpoint()]["workflow_runs"][1:]}

        cases = {
            "accepted_native_failure_scope_mismatch": [
                ({"TAG": "v0.1.31"}, None), ({"GITHUB_REPOSITORY": "other/repository"}, None)],
            "accepted_native_failure_selected_run_mismatch": [
                ({"NATIVE_VALIDATION_RUN_ID": str(MACOS_RUN)}, None)],
            "native_harness_not_current_protected_main": [
                ({"GITHUB_SHA": RUNTIME_REVISION}, None)],
            "accepted_native_failure_revision_mismatch": [({}, lambda world: world["text"].update(
                {f"{repo}/git/ref/tags/{EXCEPTION_TAG}": f"refs/tags/{EXCEPTION_TAG}\tcommit\t{'c' * 40}\n"}))],
            "accepted_native_failure_history_mismatch": [({}, extra_attempt), ({}, missing_attempt)],
            "accepted_native_failure_report_mismatch": [({}, lambda world: world["reports"][linux_report].update(
                native_status="BLOCKED"))],
            "accepted_native_failure_asset_mismatch": [({}, lambda world: world["json"][
                f"{repo}/releases?per_page=30&page=1"][0]["assets"][0]["uploader"].update(id=1))],
            "accepted_native_failure_proof_digest_mismatch": [({}, lambda world: world["text"].update(
                {f"{repo}/releases/assets/{PROOFS[LINUX_RUN]['asset']}": "tampered"}))],
            "accepted_native_failure_platform_mismatch": [({}, lambda world: world["claims"][
                linux_claim]["identity"]["platform"].update(arch="amd64"))],
            "archive_checksum_mismatch": [({}, lambda world: world.update(checksum_override="0" * 64))],
            "command_failed (native-attestation)": [
                ({}, lambda world: world["attested"].remove(linux_archive)),
                ({}, lambda world: world["attestation"].update({"--source-digest": "c" * 40}))],
            "public_identity_binding_mismatch": [
                ({}, lambda world: world["archives"][linux_archive].update(delegate="replaced")),
                ({}, lambda world: world["claims"][linux_claim]["identity"].update(
                    source_harness_sha256="f" * 64))],
            "public_binary_binding_mismatch": [({}, lambda world: world["claims"][
                linux_claim]["binaries"].update(pueue="replaced"))],
        }
        for code, variants in cases.items():
            for changes, mutate in variants:
                with self.subTest(code=code, changes=changes):
                    self.github_world = self.exception_world()
                    if mutate is not None:
                        mutate(self.github_world)
                    result = self.run_gate(**changes)
                    self.assertEqual(result.exit_code, 1, result.output.decode())
                    self.assertIn(code, result.output.decode())
                    self.assertFalse(self.summary.exists())

    def test_gate_keeps_strict_default_and_rejects_unknown_exception(self):
        record = self.root / "python-argv"
        (self.fake_bin / "python3").unlink()
        (self.fake_bin / "python3").write_text(
            f'#!/bin/sh\nprintf "%s\\n" "$@" > "{record}"\n', encoding="utf-8")
        (self.fake_bin / "python3").chmod(0o755)
        strict = [".local/release-harness/scripts/validate_native_release.py", "promotion",
                  "--tag", EXCEPTION_TAG, "--repo", EXCEPTION_REPO,
                  "--source-revision", PROMOTION_REVISION,
                  "--native-validation-run-id", str(LINUX_RUN), "--work-dir"]
        for value, expected, argv in (("", 0, strict), (ACCEPTED_FAILURE, 0, ["-I", "-"]),
                                      ("v0.1.32-unknown", 1, None),
                                      (ACCEPTED_FAILURE + " ", 1, None)):
            with self.subTest(value=value):
                record.unlink(missing_ok=True)
                result = self.run_gate(ACCEPTED_NATIVE_FAILURE=value)
                self.assertEqual(result.exit_code, expected, result.output.decode())
                if argv is None:
                    self.assertFalse(record.exists())
                else:
                    if argv is strict:
                        argv = [*strict, f"{self.runner}/native-promotion"]
                    self.assertEqual(record.read_text(encoding="utf-8").splitlines(), argv)


if __name__ == "__main__":
    unittest.main()
