"""Deterministic tests for npm published-consumer verification over mock ports."""

import base64
import contextlib
import hashlib
import io
import json
import os
from argparse import Namespace
from pathlib import Path
import sys
import tarfile
import unittest
from unittest.mock import patch

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts import verify_release_archive as verifier
from scripts.release_tests.release_fixture import FixtureTree
from scripts.acceptance.evidence import source as commands
from scripts.release_verification import published as published
from scripts.release_verification import published_contract as rules
from scripts.acceptance.evidence.source import command
from scripts.acceptance.evidence.source import VerificationError
from scripts.release_verification.published_ports import ConsumerPorts
from scripts.test_candidate_source_support import ChildFault


TAG = "v1.2.3"
REVISION = "a" * 40
RELEASE_COMMIT = "c" * 40
SECRET = "published-test-secret-must-not-be-forwarded"
REPO = "owner/repository"
SKILL_SOURCE = {
    "SKILL.md": b"# skill\n",
    "references/a.md": b"reference a\n",
    "scripts/run.sh": b"#!/bin/sh\nexit 0\n",
}


def git_blob(data):
    return hashlib.sha1(b"blob %d\0" % len(data) + data).hexdigest()


def integrity(data):
    return "sha512-" + base64.b64encode(hashlib.sha512(data).digest()).decode()


def package_tarball(skill, *, version="1.2.3"):
    buffer = io.BytesIO()
    members = {"package/package.json": json.dumps({"name": "delegation-layer", "version": version}).encode()}
    members.update({f"package/skills/agent-integration/{path}": data for path, data in skill.items()})
    members["package/install.sh"] = b"#!/bin/sh\n"
    with tarfile.open(fileobj=buffer, mode="w:gz") as bundle:
        for name, data in sorted(members.items()):
            info = tarfile.TarInfo(name)
            info.size = len(data)
            bundle.addfile(info, io.BytesIO(data))
    return buffer.getvalue()


class FakeWorld:
    """Registry, GitHub and installer behind the injected run port."""

    def __init__(self, extract, skill=None):
        self.extract = extract
        self.calls = []
        self.skill = dict(SKILL_SOURCE if skill is None else skill)
        self.package_skill = dict(self.skill)
        self.tarball = package_tarball(self.package_skill)
        self.npm_latest = "1.2.3"
        self.github_latest = TAG
        self.binary_override = {}
        self.installed_skill = None
        self.stray = False
        self.raise_on = {}
        self.view_overrides = {}
        self.tree_lines = None
        self.blob_override = {}

    def which(self, name):
        return {"npm": "/fake/bin/npm", "node": "/fake/bin/node"}.get(name)

    def ports(self, tree, inherited=None):
        """The simulated run port; ``tree`` observes each original failure before the verifier converts it."""
        def run(*args, **options):
            return tree.observe_operation(lambda: self.run(*args, **options))

        return ConsumerPorts(run=run, which=self.which, inherited=inherited or {})

    def view_document(self):
        document = {
            "name": "delegation-layer", "version": "1.2.3",
            "dist.integrity": integrity(self.tarball),
            "dist.shasum": hashlib.sha1(self.tarball).hexdigest(),
            "dist.tarball": "https://registry.npmjs.org/delegation-layer/-/delegation-layer-1.2.3.tgz",
        }
        document.update(self.view_overrides)
        return document

    def tree_listing(self):
        if self.tree_lines is not None:
            return self.tree_lines
        rows = ["truncated\tfalse", "skills/agent-integration/references\ttree\t040000\t" + "d" * 40 + "\t0"]
        for path, data in sorted(self.skill.items()):
            rows.append(f"skills/agent-integration/{path}\tblob\t100644\t{git_blob(data)}\t{len(data)}")
        return ("\n".join(rows) + "\n").encode()

    def run(self, receipt, name, argv, env, *, capture=False, timeout=30, cwd=None):
        configs = [env.get("npm_config_userconfig"), env.get("npm_config_globalconfig")]
        self.calls.append({"name": name, "argv": list(argv), "env": dict(env), "cwd": cwd,
                           "npmrc_sizes": [os.stat(item).st_size for item in configs if item]})
        if not (argv[0] == "gh" or argv[0].endswith(("/npm", "/brew"))):
            return command(receipt, name, argv, env, capture=capture, timeout=timeout, cwd=cwd)
        receipt["commands"].append({"name": name, "argv": list(argv), "exit_code": 0})
        if name in self.raise_on:
            raise self.raise_on[name]
        if argv[0] == "gh":
            return self.github(argv)
        return self.npm(argv) if argv[0].endswith("/npm") else self.brew(argv)

    def brew(self, argv):
        raise AssertionError("unexpected brew call " + " ".join(argv))

    def github(self, argv):
        joined = " ".join(argv)
        if "git/trees/" in joined:
            return self.tree_listing()
        if "releases/latest" in joined:
            return self.github_latest.encode() + b"\n"
        sha = argv[-1].rsplit("/", 1)[-1]
        for path, data in self.skill.items():
            if git_blob(data) == sha:
                return self.blob_override.get(path, data)
        raise AssertionError("unexpected gh call " + joined)

    def npm(self, argv):
        verb = argv[1]
        if verb == "view" and "dist-tags.latest" in argv:
            return json.dumps([self.npm_latest]).encode()
        if verb == "view":
            return json.dumps([self.view_document()]).encode()
        if verb == "pack":
            destination = Path(argv[argv.index("--pack-destination") + 1])
            (destination / "delegation-layer-1.2.3.tgz").write_bytes(self.tarball)
            return json.dumps({"delegation-layer": {
                "name": "delegation-layer", "version": "1.2.3",
                "filename": "delegation-layer-1.2.3.tgz",
                "integrity": self.view_document()["dist.integrity"],
                "shasum": self.view_document()["dist.shasum"],
            }}).encode()
        assert verb == "exec", argv
        tail = argv[argv.index("--") + 2:]
        if tail[0] == "install-cli":
            self.install_binaries(Path(tail[tail.index("--install-dir") + 1]))
        else:
            self.install_skill(Path(tail[tail.index("--target") + 1]))
        return b""

    def binary_source(self, name):
        """Model channel installation from the distribution archive, never installed executable copies."""
        if name in self.binary_override:
            return self.binary_override[name]
        archive = self.extract.parent / verifier.expected_archive(verifier.platform_identity(), TAG)
        with tarfile.open(archive, "r:gz") as bundle:
            source = bundle.extractfile(name)
            if source is None:
                raise AssertionError("fixture archive is missing " + name)
            with source:
                return source.read()

    def install_binaries(self, directory):
        directory.mkdir(parents=True)
        for name in verifier.BINARIES:
            data = self.binary_source(name)
            (directory / name).write_bytes(data)
            (directory / name).chmod(0o755)
        if self.stray:
            (directory / "stray").write_bytes(b"x")

    def install_skill(self, directory):
        files = self.skill if self.installed_skill is None else self.installed_skill
        for path, data in files.items():
            (directory / path).parent.mkdir(parents=True, exist_ok=True)
            (directory / path).write_bytes(data)


def binary_script(name):
    if name == "delegate":
        return """#!/bin/sh
case "$1" in
  version) printf 'delegate 1.2.3\\n' ;;
  providers) printf '{"providers":[]}\\n' ;;
  capabilities) printf '{}\\n' ;;
  supervisor) printf '{"command":"supervisor","supervisor_control":{"state":"not_initialized"}}\\n' ;;
  *) exit 8 ;;
esac
"""
    if name in {"pueue", "pueued"}:
        return f"#!/bin/sh\nprintf '{name} 4.0.4\\n'\n"
    return "#!/bin/sh\nexit 0\n"


class PublishedBase(unittest.TestCase):
    channel = "npm"

    def setUp(self):
        self.open_fixture()
        self.prepare_archive()
        self.assertEqual(self.verify_archive(), 0)
        self.world = FakeWorld(self.extract)

    def open_fixture(self):
        """Open the one test-owned tree, then route every real fixture child through it."""
        self.tree = FixtureTree.open(prefix="published-test", label="published consumer test tree")
        self.addCleanup(self.tree.dispose)
        self.root = self.tree.root
        # Each real child is counted and reports its original failure to the tree before ``command``
        # converts it; a case replaces the counter to inject one fault that launches nothing.
        self.children = ChildFault(lambda argv: False)
        runner = patch.object(commands, "run_bounded_command", self.run_child)
        runner.start()
        self.addCleanup(runner.stop)
        self.runner_temp = self.root / "runner"
        self.assets = self.runner_temp / "delegation-layer-release"
        self.extract = self.assets / "extracted"
        self.assets.mkdir(parents=True)
        self.fake_bin = self.root / "bin"
        self.fake_bin.mkdir()
        self.install_fake_gh()
        self.env_patcher = patch.dict(os.environ, {
            "PATH": f"{self.fake_bin}{os.pathsep}{os.environ.get('PATH', os.defpath)}",
            "GH_TOKEN": SECRET, "HOME": str(self.root), "RUNNER_TEMP": str(self.runner_temp),
        })
        self.env_patcher.start()
        self.addCleanup(self.env_patcher.stop)
        self.archive_receipt = self.root / "archive-receipt.json"
        self.receipt_path = self.root / "published-receipt.json"

    def run_child(self, argv, environment, **options):
        return self.tree.observe_operation(lambda: self.children(argv, environment, **options))

    def reset(self):
        """Close this case's tree and restore every patch, then open a fresh fixture."""
        self.doCleanups()
        self.setUp()

    def settle_port_fault(self, injected):
        """A simulated port fault launched nothing; remove the retained tree once every real child closed."""
        self.assertIs(self.tree.unresolved, injected)
        with contextlib.redirect_stderr(io.StringIO()):
            self.doCleanups()
        self.assertTrue(self.tree.root.is_dir())
        self.assertEqual((self.children.faulted, self.children.unclosed), ([], 0))
        self.tree.remove_retained()
        self.assertFalse(self.tree.root.exists())

    def install_fake_gh(self):
        gh = self.fake_bin / "gh"
        gh.write_text(
            "#!/bin/sh\ncase \"$*\" in\n"
            f"  *'git/ref/tags/{TAG}'*) printf 'refs/tags/{TAG}\\tcommit\\t{RELEASE_COMMIT}\\n' ;;\n"
            f"  *'git/tags/'*) printf 'commit\\t{RELEASE_COMMIT}\\n' ;;\nesac\nexit 0\n",
            encoding="utf-8")
        gh.chmod(0o755)

    def prepare_archive(self):
        info = verifier.platform_identity()
        archive = self.assets / verifier.expected_archive(info, TAG)
        with tarfile.open(archive, "w:gz") as bundle:
            for name in verifier.BINARIES:
                data = binary_script(name).encode()
                member = tarfile.TarInfo(name)
                member.size, member.mode = len(data), 0o755
                bundle.addfile(member, io.BytesIO(data))
        digest = hashlib.sha256(archive.read_bytes()).hexdigest()
        (self.assets / "checksums.txt").write_text(f"{digest}  {archive.name}\n", encoding="ascii")

    def verify_archive(self):
        """Drive the real native-acceptance verifier; a passing run retains the archive receipt."""
        args = Namespace(
            receipt=str(self.archive_receipt), assets_dir=str(self.assets),
            extract_dir=str(self.extract), mode="native-acceptance", tag=TAG,
            repo=REPO, source_revision=REVISION)
        stub = {"sha256": "0" * 64, "files": []}
        with patch("scripts.release_verification.verification.source_fingerprint",
                   return_value=stub):
            return verifier.verify(args)

    def request(self, **changes):
        values = {
            "channel": self.channel, "tag": TAG, "repo": REPO, "source_revision": REVISION,
            "archive_receipt": self.archive_receipt, "extract_dir": self.extract,
            "receipt": self.receipt_path, "github_auth": "environment",
        }
        values.update(changes)
        return rules.PublishedRequest(**values)

    def publish(self, **changes):
        return published.verify_published(self.request(**changes), ports=self.world.ports(self.tree))

    def receipt(self):
        return json.loads(self.receipt_path.read_text(encoding="utf-8"))

    def workspaces(self):
        return sorted(self.runner_temp.glob("delegation-layer-published-*"))

    def assertFailed(self, code):
        self.assertEqual(self.publish(), 1)
        receipt = self.receipt()
        self.assertEqual(receipt["failure"]["code"], code)
        self.assertEqual(receipt["verdict"], "fail")
        self.assertEqual(receipt["published"]["status"], "partial")
        self.assertEqual(receipt["cleanup"]["status"], "retained-for-investigation")
        self.assertEqual(len(self.workspaces()), 1)
        return receipt


class PublishedNpmTests(PublishedBase):
    def test_explicit_and_default_install_record_selection_and_bind_tagged_skill(self):
        self.assertEqual(self.publish(), 0)
        receipt = self.receipt()
        self.assertEqual(receipt["verdict"], "pass")
        self.assertEqual(receipt["claim"], "published-consumer-install")
        self.assertEqual(receipt["published"]["status"], "verified")
        verified = receipt["published"]["verified"]
        self.assertEqual(verified["latest"]["default_pass"], "run")
        self.assertEqual({item["path"] for item in verified["skill_source"]["files"]}, set(SKILL_SOURCE))
        self.assertEqual(receipt["published"]["archive_binaries"], {
            "schema_version": 1, "representation": "installed-paths",
            "paths": {name: str((self.extract / name).resolve()) for name in verifier.BINARIES}})
        names = [call["name"] for call in self.world.calls]
        self.assertIn("npm-exec-explicit-install-cli", names)
        self.assertIn("npm-exec-default-skill", names)
        self.assertEqual(receipt["cleanup"]["status"], "complete")
        self.assertEqual(self.workspaces(), [])
        self.assertEqual(receipt["delegate_build_identity"]["version"], "1.2.3")
        self.assertEqual(self.publish(), 0)

    def test_registry_children_are_private_token_free_and_outside_checkout(self):
        inherited = {"GH_TOKEN": SECRET, "NPM_TOKEN": SECRET, "NODE_AUTH_TOKEN": SECRET,
                     "GITHUB_TOKEN": SECRET, "SSL_CERT_FILE": "/etc/ssl/cert.pem", "HOME": "/real/home"}
        ports = self.world.ports(self.tree, inherited)
        self.assertEqual(published.verify_published(self.request(), ports=ports), 0)
        checkout = Path(__file__).resolve().parent.parent
        npm_calls = [call for call in self.world.calls if call["argv"][0].endswith("/npm")]
        self.assertTrue(npm_calls)
        for call in npm_calls:
            self.assertNotIn(SECRET, json.dumps(call, default=str))
            self.assertEqual(set(call["env"]) - {"SSL_CERT_FILE"}, {
                "PATH", "HOME", "TMPDIR", "LANG", "LC_ALL", "npm_config_cache", "npm_config_userconfig",
                "npm_config_globalconfig", "npm_config_registry", "npm_config_update_notifier",
                "npm_config_fund", "npm_config_audit", "npm_config_loglevel"})
            self.assertEqual(call["env"]["npm_config_registry"], "https://registry.npmjs.org/")
            self.assertEqual(call["npmrc_sizes"], [0, 0])
            self.assertNotEqual(call["env"]["npm_config_userconfig"], call["env"]["npm_config_globalconfig"])
            self.assertEqual(call["cwd"].name, "cwd")
            self.assertNotIn(checkout, call["cwd"].parents)
            self.assertEqual(call["cwd"].parent.parent, self.runner_temp)
            self.assertEqual(call["env"]["HOME"], str(call["cwd"].parent / "home"))
        final_receipt = self.receipt()
        cli_calls = [call for call in final_receipt["commands"] if call["name"] in {
            "delegate-version", "pueue-version", "pueued-version", "delegate-providers",
            "delegate-capabilities", "delegate-default-supervisor-status",
        }]
        self.assertEqual(len(cli_calls), 6)
        npm_cwds = {str(call["cwd"]) for call in npm_calls}
        self.assertEqual(len(npm_cwds), 1)
        expected_cwd = next(iter(npm_cwds))
        for call in cli_calls:
            self.assertEqual(call["cwd"], expected_cwd)
            self.assertNotIn(checkout, Path(call["cwd"]).parents)
            self.assertNotIn(SECRET, json.dumps(call, default=str))
        self.assertTrue(any(call["env"].get("GH_TOKEN") == SECRET for call in self.world.calls
                            if call["argv"][0] == "gh"))

    def test_default_pass_skipped_unless_both_latest_equal_target(self):
        for attribute, value, reason in (
                ("npm_latest", "1.2.4", "npm_latest_differs_from_target"),
                ("github_latest", "v1.2.4", "github_latest_differs_from_target")):
            with self.subTest(attribute):
                self.reset()
                setattr(self.world, attribute, value)
                self.assertEqual(self.publish(), 0)
                latest = self.receipt()["published"]["verified"]["latest"]
                self.assertEqual((latest["default_pass"], latest["reason"]), ("skipped", reason))
                self.assertFalse([call for call in self.world.calls if "default" in call["name"]])

    def test_tampered_registry_tarball_fails_and_reports_partial_publication(self):
        self.world.view_overrides = {"dist.integrity": integrity(b"other"),
                                     "dist.shasum": hashlib.sha1(b"other").hexdigest()}
        receipt = self.assertFailed("npm_pack_integrity_mismatch")
        self.assertEqual(set(receipt["published"]["verified"]), {"npm_package"})
        self.assertFalse([call for call in self.world.calls if "exec" in call["name"]])

    def test_registry_identity_must_match_package_registry_and_release(self):
        bad = {
            "name": {"name": "other"}, "version": {"version": "1.2.4"},
            "tarball": {"dist.tarball": "file:///checkout/delegation-layer-1.2.3.tgz"},
            "mirror": {"dist.tarball": "https://mirror.example/delegation-layer-1.2.3.tgz"},
            "integrity": {"dist.integrity": "sha1-short"},
            "head": {"gitHead": "f" * 40},
        }
        for label, override in bad.items():
            with self.subTest(label):
                self.reset()
                self.world.view_overrides = override
                self.assertEqual(self.publish(), 1)
                self.assertIn(self.receipt()["failure"]["code"],
                              {"npm_package_identity_mismatch", "npm_package_revision_mismatch"})
                self.assertFalse([call for call in self.world.calls if call["name"] == "npm-pack"])

    def test_packed_skill_must_match_tagged_source(self):
        self.world.tarball = package_tarball({**SKILL_SOURCE, "SKILL.md": b"# drifted\n"})
        self.assertFailed("npm_pack_skill_mismatch")

    def test_compatible_installed_update_is_allowed(self):
        self.world.binary_override = {"pueued": b"#!/bin/sh\nprintf 'pueued 9\\n'\n"}
        self.assertEqual(self.publish(), 0)
        self.assertEqual(self.receipt()["verdict"], "pass")

    def test_installer_stray_entries_are_rejected(self):
        self.world.stray = True
        self.assertFailed("installed_binaries_unexpected_entries")

    def test_installed_skill_drift_missing_extra_and_changed_files_fail(self):
        drifts = {
            "missing": {k: v for k, v in SKILL_SOURCE.items() if k != "references/a.md"},
            "extra": {**SKILL_SOURCE, "extra.md": b"x"},
            "changed": {**SKILL_SOURCE, "SKILL.md": b"changed"},
        }
        for label, files in drifts.items():
            with self.subTest(label):
                self.reset()
                self.world.installed_skill = files
                self.assertFailed("installed_skill_mismatch")

    def test_skill_file_set_is_derived_not_counted(self):
        for skill in ({"SKILL.md": b"only\n"}, {**SKILL_SOURCE, "evals/x.json": b"{}", "examples/y.md": b"y"}):
            with self.subTest(len(skill)):
                self.reset()
                self.world = FakeWorld(self.extract, skill)
                self.assertEqual(self.publish(), 0)
                files = self.receipt()["published"]["verified"]["skill_source"]["files"]
                self.assertEqual(len(files), len(skill))

    def test_blob_content_must_match_git_hash(self):
        self.world.blob_override = {"SKILL.md": b"# forged\n"}
        self.assertFailed("skill_blob_mismatch")

    def test_moved_tag_is_rejected_before_any_registry_command(self):
        moved = {"repository": REPO, "tag": TAG, "ref": f"refs/tags/{TAG}", "commit_sha": "e" * 40}
        with patch.object(published, "resolve_release_identity", return_value=moved):
            self.assertFailed("release_identity_changed")
        self.assertFalse([call for call in self.world.calls if call["argv"][0].endswith("/npm")])

    def test_child_timeout_retains_workspace_with_unresolved_ownership_and_never_cleans(self):
        injected = VerificationError("command_timeout", "npm-exec-explicit-install-cli")
        self.world.raise_on = {"npm-exec-explicit-install-cli": injected}
        receipt = self.assertFailed("command_timeout")
        ownership = receipt["published"]["unresolved_ownership"]
        workspace = self.workspaces()[0]
        self.assertEqual(ownership["workspace"], str(workspace))
        self.assertEqual(receipt["cleanup"]["removed_paths"], [])
        self.assertIn(str(workspace), receipt["cleanup"]["retained_paths"])
        before = self.receipt_path.read_bytes()
        calls = len(self.world.calls)
        self.assertEqual(self.publish(), 1)
        self.assertEqual(self.receipt_path.read_bytes(), before)
        self.assertEqual(len(self.world.calls), calls)
        self.assertTrue(workspace.is_dir())
        self.settle_port_fault(injected)

    def test_other_failures_do_not_claim_unresolved_ownership(self):
        self.world.binary_override = {"delegate": b"#!/bin/sh\nexit 0\n"}
        receipt = self.assertFailed("version_output_invalid")
        self.assertNotIn("unresolved_ownership", receipt["published"])

    def test_archive_binding_rejects_wrong_tag_and_missing_executable(self):
        with self.subTest("tag"):
            self.assertEqual(self.publish(tag="v1.2.4"), 1)
            self.assertEqual(self.receipt()["failure"]["code"], "archive_receipt_not_passing")
        self.receipt_path.unlink()
        with self.subTest("binary"):
            (self.extract / "pueue").unlink()
            self.assertEqual(self.publish(), 1)
            self.assertEqual(self.receipt()["failure"]["code"], "release_binary_missing")
        self.assertEqual(self.workspaces(), [])


class PublishedAuthTests(PublishedBase):
    def test_hosted_default_still_requires_a_token(self):
        with patch.dict(os.environ):
            os.environ.pop("GH_TOKEN")
            self.assertEqual(self.publish(), 1)
        self.assertEqual(self.receipt()["failure"]["code"], "github_auth_unavailable")

    def test_local_config_mode_forwards_no_token_to_github_commands(self):
        config = self.root / "ghconfig"
        config.mkdir()
        (config / "hosts.yml").write_text("github.com:\n  user: someone\n", encoding="utf-8")
        with patch.dict(os.environ, {"GH_CONFIG_DIR": str(config)}):
            self.assertEqual(self.publish(github_auth="local-config"), 0)
        github = [call for call in self.world.calls if call["argv"][0] == "gh"]
        self.assertTrue(github)
        for call in github:
            self.assertNotIn("GH_TOKEN", call["env"])
            self.assertNotIn(SECRET, json.dumps(call["argv"]))
        self.assertNotIn(SECRET, self.receipt_path.read_text(encoding="utf-8"))


class PublishedRulesTests(unittest.TestCase):
    def test_decide_latest_requires_both_channels_to_equal_target(self):
        self.assertTrue(rules.decide_latest("1.2.3", "v1.2.3", TAG).run_default)
        self.assertFalse(rules.decide_latest("1.2.3", "v1.2.4", TAG).run_default)
        self.assertFalse(rules.decide_latest("1.2.4", "v1.2.3", TAG).run_default)

    def test_tree_listing_rejects_truncation_links_duplicates_and_missing_skill(self):
        sha = "1" * 40
        head = "truncated\tfalse\n"
        def row(path, kind="blob", mode="100644"):
            return f"skills/agent-integration/{path}\t{kind}\t{mode}\t{sha}\t1\n"
        cases = {
            "truncated": "truncated\ttrue\n" + row("SKILL.md"),
            "symlink": head + row("SKILL.md") + row("link", mode="120000"),
            "submodule": head + row("SKILL.md") + row("sub", kind="commit", mode="160000"),
            "duplicate": head + row("SKILL.md") + row("SKILL.md"),
            "traversal": head + row("SKILL.md") + row("../x"),
            "no-skill": head + row("other.md"),
            "empty": head,
        }
        for label, listing in cases.items():
            with self.subTest(label), self.assertRaises(VerificationError):
                rules.parse_tree_listing(listing.encode())
        self.assertEqual(len(rules.parse_tree_listing((head + row("SKILL.md") + row("a/b.md")).encode())), 2)

    def test_npm_view_accepts_nested_dist_form(self):
        data = b"package"
        document = {"name": "delegation-layer", "version": "1.2.3", "dist": {
            "integrity": integrity(data), "shasum": hashlib.sha1(data).hexdigest(),
            "tarball": rules.registry_tarball_url("1.2.3")}}
        identity = rules.parse_npm_view(json.dumps(document).encode(), "1.2.3", RELEASE_COMMIT)
        self.assertEqual(identity.integrity, integrity(data))

    def test_npm_output_shapes_from_every_supported_npm_are_accepted(self):
        data = b"package"
        flat = {"name": "delegation-layer", "version": "1.2.3", "dist.integrity": integrity(data),
                "dist.shasum": hashlib.sha1(data).hexdigest(),
                "dist.tarball": rules.registry_tarball_url("1.2.3")}
        for document in (flat, [flat]):
            identity = rules.parse_npm_view(json.dumps(document).encode(), "1.2.3", RELEASE_COMMIT)
            entry = {"name": "delegation-layer", "version": "1.2.3", "filename": "d-1.2.3.tgz",
                     "integrity": identity.integrity, "shasum": identity.shasum}
            for packed in ([entry], {"delegation-layer": entry}):
                self.assertEqual(rules.parse_pack_filename(json.dumps(packed).encode(), identity), "d-1.2.3.tgz")
        with self.assertRaises(VerificationError):
            rules.parse_npm_view(json.dumps([flat, flat]).encode(), "1.2.3", RELEASE_COMMIT)
        self.assertEqual(rules.parse_npm_latest(b'"1.2.3"'), "1.2.3")
        self.assertEqual(rules.parse_npm_latest(b'["1.2.3"]'), "1.2.3")
        with self.assertRaises(VerificationError):
            rules.parse_npm_latest(b'["1.2.3", "1.2.4"]')

    def test_environment_allowlist_drops_every_credential(self):
        inherited = {"NPM_TOKEN": "x", "NODE_AUTH_TOKEN": "x", "GH_TOKEN": "x", "SSL_CERT_DIR": "/certs",
                     "HOMEBREW_GITHUB_API_TOKEN": "x"}
        for env in (
                rules.npm_environment(
                    path="/p", home="/h", cache="/c", user_npmrc="/u", global_npmrc="/g", inherited=inherited),
                rules.homebrew_environment(path="/p", home="/h", inherited=inherited)):
            self.assertEqual(env["PATH"], "/p")
            self.assertEqual(env["SSL_CERT_DIR"], "/certs")
            self.assertFalse({"NPM_TOKEN", "NODE_AUTH_TOKEN", "GH_TOKEN", "HOMEBREW_GITHUB_API_TOKEN"} & set(env))


if __name__ == "__main__":
    unittest.main()
