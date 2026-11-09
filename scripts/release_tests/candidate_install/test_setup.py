"""Exact packed setup exercises installation, reuse and repeat through real npm children."""

from __future__ import annotations

import json
import os
from pathlib import Path
import shutil
import unittest

from scripts.release_tests.candidate_install import fixtures as support
from scripts.release_verification.candidate_install_contract import CHANNEL_NPM
from scripts.release_verification.candidate_install_contract import CHANNEL_SHELL
from scripts.release_verification.candidate_install_contract import RequestRecord
from scripts.release_verification.candidate_install_contract import release_urls
from scripts.release_verification.assets import platform_identity
from scripts.release_verification import candidate_install as install
from scripts.release_tests.release_fixture import FixtureTree
from scripts.release_verification.candidate_install_setup import CandidateSetup
from scripts.release_verification.published_ports import prepare_layout
from scripts.acceptance.evidence.source import VerificationError
from scripts.acceptance.evidence.source import child_environment


def doctor_binary(version: str) -> bytes:
    """A finite doctor wire fixture; any attempted provider launch is detected by setup proof."""
    report = {
        "schema_version": 1, "command": "doctor",
        "root": {"path": "ROOT_PATH", "status": "not_initialized"},
        "installation": {"scope": "installation_bundle", "verification": "filesystem_metadata",
                         "compatibility": "not_checked", "status": "observed",
                         "roles": {name: {"path": "ROLE_DIRECTORY/" + executable, "status": "available"}
                                   for name, executable in (("delegate", "delegate"), ("delegate_run", "delegate-run"),
                                                            ("pueue", "pueue"), ("pueued", "pueued"))}},
        "supervisor": {"binding": "not_initialized", "liveness": "not_checked", "active_work": "unknown"},
        "issues": [],
    }
    base = support.fake_binary("delegate", version).decode()
    wire = json.dumps(report).replace("ROOT_PATH", "$HOME/doctor-state").replace("ROLE_DIRECTORY", "${0%/*}")
    provider = ',"provider":{"id":"pi:json","status":"static_ready","executable":"fixture-pi","authentication":"unknown","verification":"static_preflight"}'
    branch = '''  doctor)
    provider=""
    for argument in "$@"; do
      if [ "$argument" = pi:json ]; then provider=''' + "'" + provider + "'" + '''; fi
    done
    cat <<EOF
''' + wire[:-1] + '''$provider}
EOF
    ;;
'''
    return base.replace('  version)', branch + '  version)').encode()


@unittest.skipUnless(shutil.which("npm") and shutil.which("node"), "npm and node required")
class PackedSetupTests(support.PackedCandidateCase):
    def verify_setup(self, channel: str):
        self.binaries["delegate"] = doctor_binary(self.tag[1:])
        request = self.request()
        self.layout = install.open_workspace(owner=self.tree.owner, parent=self.parent, receipt=self.receipt)
        with support.observed_children(self.tree, dependencies=self.dependencies), \
                support.deny_installed_script_reads(self.layout.root / channel / "setup-home" / ".claude" / "skills" / "agent-integration"):
            facts = install.verify_candidate_install(
                request, ports=self.recording.ports(), probe=self.probe, receipt=self.receipt, layout=self.layout,
                info=platform_identity(), channels=(channel,), onboarding=True)
        return request, facts

    def test_absent_bundle_setup_pins_package_over_environment_and_repeats_without_download(self):
        request, facts = self.verify_setup(CHANNEL_NPM)
        (observed,) = facts.installs
        self.assertEqual(observed.requests, tuple(RequestRecord("served", url)
                                               for url in release_urls(request, platform_identity()).expected()))
        self.assertEqual(observed.onboarding["recipe"], "validated-absolute-command-provider-workspace-read-only")
        calls = [call for call in self.recording.calls if call.name.startswith("candidate-npm-setup")]
        self.assertEqual([call.name for call in calls], ["candidate-npm-setup-fresh", "candidate-npm-setup-repeat",
                                                       "candidate-npm-setup-recipe"])
        self.assertTrue(all(call.env["DELEGATION_LAYER_VERSION"] == "v0.0.0" for call in calls))
        self.assertTrue(all(f"--package={self.package}" in call.argv for call in calls))
        self.assertEqual(len(self.probe.targets), 1)
        self.finish_workspace()

    def test_shell_bundle_is_reused_by_exact_packed_setup_and_fixture_follows_setup(self):
        _, facts = self.verify_setup(CHANNEL_SHELL)
        (observed,) = facts.installs
        self.assertEqual(len(observed.requests), 2)
        self.assertEqual(observed.onboarding["repeat"], "completed")
        self.assertEqual(len(self.probe.targets), 1)
        self.assertFalse((self.layout.root / "shell" / "setup-providers" / "launched").exists())
        self.assertFalse((self.layout.root / "shell" / "setup-home" / "doctor-state").exists())
        self.finish_workspace()

    def test_recipe_passes_real_provider_doctor_without_launching_provider(self):
        # Pi treats system temp trees as writable. Keep the passive state outside
        # them, as distribution CI does, and build this owned candidate in place.
        tree = FixtureTree.open(Path.home(), prefix="candidate-setup-doctor", label="real setup doctor")
        self.addCleanup(tree.dispose)
        recording = support.RecordingPorts(tree, dependencies=self.dependencies)
        layout = prepare_layout(tree.root)
        base = tree.root / "npm"
        base.mkdir()
        binaries = base / "bin"
        binaries.mkdir()
        recording.run(self.receipt, "build-setup-doctor",
                      ["go", "build", "-buildvcs=false", "-o", str(binaries / "delegate"), "./cmd/delegate"],
                      child_environment(os.environ), timeout=180, cwd=support.REPO_ROOT)
        for name in ("delegate-run", "pueue", "pueued"):
            role = binaries / name
            role.write_text("#!/bin/sh\nexit 98\n", encoding="utf-8")
            role.chmod(0o700)
        setup = CandidateSetup(self.request(), recording.ports(), self.receipt, layout,
                               CHANNEL_NPM, base, binaries, {})
        setup.fresh()
        self.assertEqual(setup.verify_recipe(binaries / "delegate"),
                         "validated-absolute-command-provider-workspace-read-only")
        self.assertFalse((base / "setup-providers" / "launched").exists())
        doctor = json.loads(recording.run(
            self.receipt, "passive-doctor-after-recipe", [str(binaries / "delegate"), "doctor", "--json"],
            setup.environment(), capture=True, cwd=layout.cwd))
        self.assertEqual(doctor["root"]["status"], "not_initialized")
        self.assertFalse(Path(doctor["root"]["path"]).exists())

    def test_unsupported_doctor_blocks_packed_setup_before_skill_publication_and_bootstrap(self):
        self.layout = install.open_workspace(owner=self.tree.owner, parent=self.parent, receipt=self.receipt)
        with support.observed_children(self.tree, dependencies=self.dependencies), self.assertRaises(VerificationError) as caught:
            install.verify_candidate_install(
                self.request(), ports=self.recording.ports(), probe=self.probe, receipt=self.receipt,
                layout=self.layout, info=platform_identity(), channels=(CHANNEL_SHELL,), onboarding=True)
        self.assertEqual(caught.exception.code, "command_failed")
        self.assertFalse((self.layout.root / "shell" / "setup-home" / ".claude").exists())
        self.assertEqual(self.probe.targets, [])


if __name__ == "__main__":
    unittest.main()
