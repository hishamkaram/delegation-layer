"""GitHub transport uses the canonical bounded owner without logging secrets."""

import base64
import contextlib
import errno
from functools import partial
import hashlib
import io
import json
import os
import stat
import sys
import tracemalloc
import unittest
from unittest.mock import Mock, patch

from scripts.acceptance_tests.release_fixture import FixtureTree, open_descriptors
from scripts.delegate_acceptance.release_verification.candidate_manifest import MAX_CONTAINER_BYTES
from scripts.delegate_acceptance.processes import operations
from scripts.delegate_acceptance.processes.contracts import (
    BoundedCommandError, BoundedCommandResult, CommandFailure,
)
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.delegate_acceptance.release_verification import filesystem, github
from scripts.delegate_acceptance.release_verification.github import GitHubClient, positive_id

_MIB = 1 << 20

# A finite stand-in for ``gh``: it logs its own invocation, then behaves per FIXTURE_MODE.
_FAKE_GH = """#!{python}
import json, os, signal, sys
argv = sys.argv[1:]
log = {{"argv": argv, "cwd": os.getcwd(), "pid": os.getpid(), "body": None}}
if "--input" in argv:
    with open(argv[argv.index("--input") + 1], "rb") as source:
        log["body"] = source.read().decode("ascii")
with open(os.environ["FIXTURE_LOG"], "w") as sink:
    json.dump(log, sink)
mode, total = os.environ["FIXTURE_MODE"], int(os.environ.get("FIXTURE_BYTES", "0"))
out = sys.stdout.buffer
if mode == "json":
    out.write(b'{{"ok":true}}')
elif mode == "bad-header":
    out.write(b"<html>not an archive</html>")
elif mode == "empty":
    pass
else:
    out.write(b"PK\\x03\\x04")
    block = bytes(range(256)) * 4096
    left = total - 4
    while left > 0:
        out.write(block[:left])
        left -= len(block)
    out.flush()
    if mode == "hang":
        signal.pause()
    if mode == "fail":
        sys.exit(3)
"""


def archive_digest(total: int) -> str:
    """Digest of the bytes the fake ``gh`` streams in ``stream`` mode."""
    digest = hashlib.sha256(b"PK\x03\x04")
    block, left = bytes(range(256)) * 4096, total - 4
    while left > 0:
        digest.update(block[:left])
        left -= len(block)
    return digest.hexdigest()


def stalled_write(descriptor: int, data: bytes) -> None:
    """The shared writer over a descriptor that accepts no bytes."""
    with patch.object(os, "write", return_value=0):
        filesystem.write_all(descriptor, data)


class ObservedGitHubClient(GitHubClient):
    """Every transport operation reports its failure to the one containing tree.

    The tree alone classifies closure (canonical unresolved ownership codes) and alone
    decides disposal, so no test decides for itself whether a failed download may be removed.
    """

    def __init__(self, receipt: dict, environment: dict[str, str], tree: FixtureTree):
        super().__init__(receipt, environment)
        self.tree = tree

    def json(self, *args, **kwargs):
        return self.tree.observe_operation(partial(super().json, *args, **kwargs))

    def json_body(self, *args, **kwargs):
        return self.tree.observe_operation(partial(super().json_body, *args, **kwargs))

    def bytes(self, *args, **kwargs):
        return self.tree.observe_operation(partial(super().bytes, *args, **kwargs))

    def artifact_archive(self, *args, **kwargs):
        return self.tree.observe_operation(partial(super().artifact_archive, *args, **kwargs))

    def raw_blob(self, *args, **kwargs):
        return self.tree.observe_operation(partial(super().raw_blob, *args, **kwargs))

    def download_artifact(self, *args, **kwargs):
        return self.tree.observe_operation(partial(super().download_artifact, *args, **kwargs))

    def release(self, *args, **kwargs):
        return self.tree.observe_operation(partial(super().release, *args, **kwargs))


class FakeGhCase(unittest.TestCase):
    """Real finite ``gh`` children found through PATH; no network, no shell."""

    def setUp(self):
        self.descriptors = open_descriptors()
        # The tree resolves its root: the transport rejects symlinked path components.
        self.tree = FixtureTree.open(prefix="github-transport-test",
                                     label="github transport test fixture")
        self.addCleanup(self.tree.dispose)
        self.root = self.tree.root
        bin_dir = self.root / "bin"
        bin_dir.mkdir()
        executable = bin_dir / "gh"
        executable.write_text(_FAKE_GH.format(python=sys.executable))
        executable.chmod(0o755)
        self.work = self.root / "work"
        self.work.mkdir()
        self.log_path = self.root / "invocation.json"
        self.receipt: dict = {"commands": []}

    def client(self, mode: str, **extra: str) -> ObservedGitHubClient:
        """A client observed by this case's tree, logging its invocation inside that tree."""
        environment = {"PATH": str(self.root / "bin"), "FIXTURE_LOG": str(self.log_path),
                       "FIXTURE_MODE": mode, "GH_TOKEN": "private-canary", **extra}
        return ObservedGitHubClient(self.receipt, environment, self.tree)

    def invocation(self) -> dict:
        return json.loads(self.log_path.read_text())

    def assert_child_closed(self):
        with self.assertRaises(ProcessLookupError):
            os.kill(self.invocation()["pid"], 0)

    def settle(self) -> tuple[bool, str]:
        """Dispose of the containing tree as the registered cleanup would.

        Returns (removed, stderr); every pin is consumed either way. Retained evidence is
        deleted only by an explicit ``remove_retained`` after the test's own oracle passes.
        """
        with contextlib.redirect_stderr(io.StringIO()) as stderr:
            removed = self.tree.dispose()
        self.assertEqual(open_descriptors(), self.descriptors)
        return removed, stderr.getvalue()

    @contextlib.contextmanager
    def no_launches(self):
        """Refuse every child launch at the canonical owner's boundary and record attempts."""
        refusal = AssertionError("no child may be launched by a synthetic fault")
        with patch.object(operations.subprocess, "Popen", Mock(side_effect=refusal)) as launches:
            yield launches

    def assert_no_secret(self, *values: object):
        for value in values:
            self.assertNotIn("private-canary", repr(value))

    @contextlib.contextmanager
    def closing_fault(self, owner=github):
        """Fail only the close of a descriptor ``owner`` created, after really closing it.

        ``owner`` is the module whose ``create_new_file`` opens the descriptor: the client
        for downloads, the filesystem owner for request bodies. Yields each such close
        attempt, counted before the real close; other descriptors are untouched.
        """
        real_create, real_close, created, attempts = owner.create_new_file, os.close, [], []

        def create(path, code, *, stage):
            created.append(real_create(path, code, stage=stage))
            return created[-1]

        def close(descriptor):
            if descriptor in created:
                attempts.append(descriptor)
            real_close(descriptor)
            if descriptor in created:
                raise OSError(errno.EIO, "private-close-detail")

        with patch.object(owner, "create_new_file", side_effect=create), \
                patch.object(owner.os, "close", side_effect=close):
            yield attempts


class GitHubTransportTests(unittest.TestCase):
    def test_binary_downloads_use_the_endpoint_media_type_and_preserve_bytes(self):
        client = GitHubClient({"commands": []}, {})
        payload = b"PK\x03\x04\xff\x00\n"
        for artifact in (False, True):
            with self.subTest(artifact=artifact):
                with patch("scripts.delegate_acceptance.release_verification.github.run_bounded_command",
                           return_value=BoundedCommandResult(0, payload)) as run:
                    if artifact:
                        actual = client.artifact_archive("owner/repo", 17, limit=1024)
                    else:
                        actual = client.bytes("repos/owner/repo/releases/assets/17", limit=1024)
                self.assertEqual(actual, payload)
                argv = run.call_args.args[0]
                accept = ("application/vnd.github+json" if artifact else "application/octet-stream")
                self.assertIn(f"Accept: {accept}", argv)
                endpoint = ("repos/owner/repo/actions/artifacts/17/zip" if artifact
                            else "repos/owner/repo/releases/assets/17")
                self.assertEqual(argv[-1], endpoint)
                self.assertEqual(run.call_args.kwargs["max_output"], 1024)

    def test_json_request_is_bounded_and_records_only_safe_transport_facts(self):
        receipt = {"commands": []}
        env = {"GH_TOKEN": "private-canary"}
        client = GitHubClient(receipt, env)
        with patch("scripts.delegate_acceptance.release_verification.github.run_bounded_command",
                   return_value=BoundedCommandResult(0, b'{"workflow_run_id":17}')) as run:
            value = client.json("repos/owner/repo/actions/workflows/proof.yml/dispatches",
                                method="POST", fields={"ref": "main", "inputs[tag]": "v1.2.3"})
        self.assertEqual(value, {"workflow_run_id": 17})
        self.assertEqual(run.call_args.args[1], env)
        self.assertEqual(run.call_args.kwargs["timeout"], 120)
        self.assertNotIn("private-canary", repr(run.call_args.args[0]))
        self.assertNotIn("private-canary", repr(receipt))
        self.assertEqual(receipt["commands"][0]["exit_code"], 0)

    def test_scalar_or_malformed_response_is_rejected(self):
        for data in (b'null', b'42', b'{bad'):
            with self.subTest(data=data):
                client = GitHubClient({"commands": []}, {})
                with patch("scripts.delegate_acceptance.release_verification.github.run_bounded_command",
                           return_value=BoundedCommandResult(0, data)):
                    with self.assertRaisesRegex(VerificationError, "github_response_invalid"):
                        client.json("repos/owner/repo")

    def test_id_does_not_accept_boolean_or_string(self):
        for value in (True, "17", 0, -1):
            with self.subTest(value=value):
                with self.assertRaisesRegex(VerificationError, "github_id_invalid"):
                    positive_id(value, "test")


class GitHubReleaseLookupTests(unittest.TestCase):
    def setUp(self):
        self.client = GitHubClient({"commands": []}, {})
        self.release = {"id": 17, "tag_name": "v1.2.3", "draft": True,
                        "prerelease": False}
        self.endpoint = "repos/owner/repo/releases?per_page=30&page=1"

    def test_draft_and_published_releases_use_listing_not_published_only_lookup(self):
        for draft in (True, False):
            with self.subTest(draft=draft):
                self.release["draft"] = draft
                with patch.object(self.client, "json", return_value=[self.release]) as request:
                    self.assertEqual(self.client.release("owner/repo", "v1.2.3"), self.release)
                request.assert_called_once_with(self.endpoint, limit=1 << 20)

    def test_release_on_later_page_is_found(self):
        unrelated = {"id": 18, "tag_name": "v9.9.9"}
        with patch.object(self.client, "json",
                          side_effect=[[unrelated] * 30, [self.release]]) as request:
            self.assertEqual(self.client.release("owner/repo", "v1.2.3"), self.release)
        self.assertEqual(request.call_count, 2)
        self.assertEqual(request.call_args.args[0],
                         "repos/owner/repo/releases?per_page=30&page=2")

    def test_exact_tag_is_required(self):
        rows = [{"id": 18, "tag_name": "v1.2.30"}]
        with patch.object(self.client, "json", return_value=rows):
            with self.assertRaisesRegex(VerificationError, "github_release_not_found"):
                self.client.release("owner/repo", "v1.2.3")

    def test_duplicate_match_across_pages_is_ambiguous(self):
        unrelated = {"id": 18, "tag_name": "v9.9.9"}
        first = [self.release] + [unrelated] * 29
        with patch.object(self.client, "json", side_effect=[first, [self.release]]):
            with self.assertRaisesRegex(VerificationError, "github_release_ambiguous"):
                self.client.release("owner/repo", "v1.2.3")

    def test_malformed_page_is_rejected(self):
        for rows in ({"releases": []}, [self.release] * 31, [None]):
            with self.subTest(rows=rows):
                with patch.object(self.client, "json", return_value=rows):
                    with self.assertRaises(VerificationError):
                        self.client.release("owner/repo", "v1.2.3")

    def test_match_requires_numeric_release_identity(self):
        for release_id in (True, "17", 0, -1, None):
            with self.subTest(release_id=release_id):
                row = {**self.release, "id": release_id}
                with patch.object(self.client, "json", return_value=[row]):
                    with self.assertRaisesRegex(VerificationError, "github_id_invalid"):
                        self.client.release("owner/repo", "v1.2.3")

    def test_pagination_exhaustion_cannot_accept_an_early_match(self):
        unrelated = {"id": 18, "tag_name": "v9.9.9"}
        first = [self.release] + [unrelated] * 29
        pages = [first] + [[unrelated] * 30] * 99
        with patch.object(self.client, "json", side_effect=pages) as request:
            with self.assertRaisesRegex(VerificationError, "github_releases_pagination_incomplete"):
                self.client.release("owner/repo", "v1.2.3")
        self.assertEqual(request.call_count, 100)

    def test_transport_error_is_reported_without_fallback(self):
        error = VerificationError("github_request_failed", "github-api")
        with patch.object(self.client, "json", side_effect=error) as request:
            with self.assertRaisesRegex(VerificationError, "github_request_failed"):
                self.client.release("owner/repo", "v1.2.3")
        request.assert_called_once_with(self.endpoint, limit=1 << 20)

    def test_realistic_metadata_history_fits_the_transport_page_budget(self):
        releases = [{"id": index + 1, "tag_name": f"v0.0.{index}",
                     "body": "x" * 11292} for index in range(95)]
        releases[0].update(self.release)
        observed = []

        def bounded(argv, _env, *, max_output, **_options):
            endpoint = next(value for value in argv if value.startswith("repos/"))
            fields = dict(value.split("=") for value in endpoint.split("?", 1)[1].split("&"))
            count, page = int(fields["per_page"]), int(fields["page"])
            rows = releases[(page - 1) * count:page * count]
            data = json.dumps(rows).encode()
            if len(data) > max_output:
                raise BoundedCommandError(CommandFailure.OUTPUT_EXCEEDED)
            observed.append((len(rows), len(data)))
            return BoundedCommandResult(0, data)

        with patch("scripts.delegate_acceptance.release_verification.github.run_bounded_command",
                   side_effect=bounded):
            self.assertEqual(self.client.release("owner/repo", "v1.2.3"), releases[0])
        self.assertEqual([count for count, _ in observed], [30, 30, 30, 5])
        self.assertTrue(all(size <= 1 << 20 for _, size in observed))

    def test_oversized_page_remains_an_explicit_transport_failure(self):
        error = BoundedCommandError(CommandFailure.OUTPUT_EXCEEDED)
        with patch("scripts.delegate_acceptance.release_verification.github.run_bounded_command",
                   side_effect=error) as request:
            with self.assertRaisesRegex(VerificationError, "command_output_exceeded"):
                self.client.release("owner/repo", "v1.2.3")
        request.assert_called_once()
        self.assertEqual(request.call_args.kwargs["max_output"], 1 << 20)


class GitHubReleaseListingTests(unittest.TestCase):
    """The shared release listing owns page shape and exhaustion; a consumer sees each row in order."""

    def setUp(self):
        self.client = GitHubClient({"commands": []}, {})

    def rows(self, page: int, count: int = 30) -> list[dict]:
        return [{"id": 1000 * page + n, "tag_name": f"v0.{page}.{n}"} for n in range(count)]

    def consume(self, pages: list[object]) -> tuple[list[object], VerificationError | None, int]:
        """Every row the iterator yielded before it ended or refused, the refusal and the request count."""
        seen: list[object] = []
        with patch.object(self.client, "json", side_effect=pages) as request:
            try:
                for row in github.iter_releases(self.client, "owner/repo"):
                    seen.append(row)
            except VerificationError as error:
                return seen, error, request.call_count
        return seen, None, request.call_count

    def test_every_row_is_yielded_in_listing_order_until_a_short_page(self):
        pages = [self.rows(1), self.rows(2, 3)]
        seen, error, requests = self.consume(pages)
        self.assertEqual((seen, error, requests), (pages[0] + pages[1], None, 2))
        seen, error, requests = self.consume([[]])
        self.assertEqual((seen, error, requests), ([], None, 1))

    def test_a_late_malformed_page_refuses_after_the_earlier_rows(self):
        for late in ({"releases": []}, self.rows(2, 31), "rows"):
            with self.subTest(late=type(late).__name__):
                seen, error, requests = self.consume([self.rows(1), late])
                self.assertEqual(seen, self.rows(1))
                self.assertEqual((error.code, error.stage, requests),
                                 ("github_releases_invalid", "github-release", 2))

    def test_a_full_final_page_leaves_the_listing_incomplete(self):
        seen, error, requests = self.consume([self.rows(page) for page in range(1, 101)])
        self.assertEqual(len(seen), 3000)
        self.assertEqual((error.code, error.stage, requests),
                         ("github_releases_pagination_incomplete", "github-release", 100))

    def test_a_failed_page_read_is_never_the_end_of_the_listing(self):
        failure = VerificationError("github_request_failed", "github-api")
        seen, error, requests = self.consume([self.rows(1), failure])
        self.assertEqual((seen, error, requests), (self.rows(1), failure, 2))


class GitHubJsonBodyTests(FakeGhCase):
    ENDPOINT = "repos/owner/repo/git/trees"

    def test_an_original_unconverted_transport_failure_reaches_the_tree(self):
        injected = BoundedCommandError(CommandFailure.TERMINATION_FAILED)
        with self.no_launches() as launches, patch.object(GitHubClient, "json_body", side_effect=injected) as sent:
            with self.assertRaises(BoundedCommandError) as caught:
                self.send({"tree": []})
        self.assertIs(caught.exception, injected)
        self.assertEqual(sent.call_count, 1)
        self.assertIs(self.tree.unresolved, injected)
        removed, message = self.settle()
        self.assertFalse(removed)
        self.assertIn(f"(command_termination_failed): {self.root}", message)
        self.assertEqual(launches.call_count, 0)
        self.tree.remove_retained()
        self.assertFalse(self.root.exists())

    def send(self, body, *, endpoint=ENDPOINT, **options):
        options.setdefault("method", "POST")
        options.setdefault("body_path", self.work / "request.json")
        return self.client("json").json_body(endpoint, body, **options)

    def test_nested_body_reaches_gh_through_an_owned_input_file(self):
        body = {"tree": [{"path": "a/b.txt", "type": "blob", "size": 2.5, "x": None}],
                "note": "caf\u00e9", "token": "private-canary"}
        self.assertEqual(self.send(body), {"ok": True})
        encoded = json.dumps(body, allow_nan=False, sort_keys=True, separators=(",", ":"))
        invocation, path = self.invocation(), self.work / "request.json"
        self.assertEqual(invocation["body"], encoded)
        self.assertEqual(path.read_text(), encoded)
        self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
        argv = invocation["argv"]
        self.assertEqual(argv[argv.index("--input") + 1], str(path))
        self.assertEqual(argv[argv.index("--method") + 1], "POST")
        self.assertIn("Content-Type: application/json", argv)
        self.assertNotIn("-f", argv)
        self.assertEqual(invocation["cwd"], os.getcwd())
        record = self.receipt["commands"][0]
        self.assertEqual(record["exit_code"], 0)
        self.assertEqual(record["body_bytes"], len(encoded))
        self.assertEqual(record["body_sha256"], hashlib.sha256(encoded.encode()).hexdigest())
        self.assert_no_secret(argv, self.receipt)

    def test_largest_supported_proof_fits_a_base64_git_blob_request(self):
        # Capacity only: the bytes are a pattern, not a semantic proof. If the public
        # proof bound grows past this transport's cap, this test fails before a PR does.
        proof = (bytes(range(256)) * (MAX_CONTAINER_BYTES // 256 + 1))[:MAX_CONTAINER_BYTES]
        body = {"content": base64.b64encode(proof).decode("ascii"), "encoding": "base64"}
        encoded = json.dumps(body, allow_nan=False, sort_keys=True, separators=(",", ":"))
        self.assertLess(len(encoded), github.MAX_REQUEST_BODY)
        self.assertEqual(self.send(body, endpoint="repos/owner/repo/git/blobs"), {"ok": True})
        invocation = self.invocation()
        self.assertEqual(invocation["body"], encoded)
        argv = invocation["argv"]
        self.assertEqual(argv[argv.index("--input") + 1], str(self.work / "request.json"))
        self.assertEqual((self.work / "request.json").stat().st_size, len(encoded))
        record = self.receipt["commands"][0]
        self.assertEqual(record["body_bytes"], len(encoded))
        self.assertEqual(record["body_sha256"], hashlib.sha256(encoded.encode()).hexdigest())
        self.assertEqual(record["exit_code"], 0)

    def test_ambiguous_or_non_finite_bodies_are_rejected_before_any_effect(self):
        deep: dict = {}
        for _ in range(40):
            deep = {"k": deep}
        cases = [{"n": float("nan")}, {"n": float("inf")}, {1: "int key"}, {"t": (1, 2)},
                 {"o": object()}, {"s": {1, 2}}, deep, {"big": "x" * (github.MAX_REQUEST_BODY + 1)},
                 ["not", "an", "object"], None]
        for body in cases:
            with self.subTest(body=type(body).__name__):
                with self.assertRaisesRegex(VerificationError, "github_request_body_invalid"):
                    self.send(body)
        self.assertEqual(self.receipt["commands"], [])
        self.assertFalse(self.log_path.exists())
        self.assertFalse((self.work / "request.json").exists())

    def test_a_stalled_body_write_is_refused_before_gh_starts(self):
        descriptors = open_descriptors()
        # The descriptor accepts no bytes, so the real shared writer refuses the stall.
        with patch.object(filesystem.os, "write", return_value=0):
            with self.assertRaises(VerificationError) as caught:
                self.send({"a": 1})
        error = caught.exception
        self.assertEqual((error.code, error.stage), ("github_request_body_write_failed", "github-api"))
        self.assertIsNone(error.__cause__)
        self.assertTrue(error.__suppress_context__)
        self.assertEqual(open_descriptors(), descriptors)
        self.assertTrue((self.work / "request.json").is_file())
        self.assertFalse(self.log_path.exists())

    def failed_preparation(self, fault: contextlib.AbstractContextManager[object]) -> tuple[BaseException, list[VerificationError]]:
        """Prepare one body under ``fault`` and a body close fault; no request may follow.

        Returns the escaping exception and every typed error the client constructed.
        """
        constructed: list[VerificationError] = []

        def typed(code: str, stage: str) -> VerificationError:
            constructed.append(VerificationError(code, stage))
            return constructed[-1]

        encoded = json.dumps({"a": 1}, allow_nan=False, sort_keys=True, separators=(",", ":")).encode()
        with self.no_launches() as launches, self.closing_fault(filesystem) as closes, fault, \
                patch.object(github, "VerificationError", side_effect=typed), \
                patch.object(filesystem, "VerificationError", side_effect=typed), \
                patch.object(github, "run_bounded_command") as requests:
            with self.assertRaises(BaseException) as caught:
                self.send({"a": 1})
        self.assertEqual(len(closes), 1)
        with self.assertRaises(OSError) as closed:
            os.fstat(closes[0])
        self.assertEqual(closed.exception.errno, errno.EBADF)
        self.assertEqual((requests.call_count, launches.call_count), (0, 0))
        record = self.receipt["commands"][0]
        self.assertEqual((record["body_bytes"], record["body_sha256"]),
                         (len(encoded), hashlib.sha256(encoded).hexdigest()))
        self.assertTrue((self.work / "request.json").is_file())
        self.assertFalse(self.log_path.exists())
        return caught.exception, constructed

    def test_a_body_close_failure_alone_is_the_typed_write_failure(self):
        error, constructed = self.failed_preparation(contextlib.nullcontext())
        self.assertEqual(constructed, [error])
        self.assertEqual((error.code, error.stage), ("github_request_body_write_failed", "github-api"))
        self.assertIsNone(error.__cause__)
        self.assertTrue(error.__suppress_context__)
        self.assertFalse(hasattr(error, "__notes__"))
        self.assertEqual((self.work / "request.json").read_text(), '{"a":1}')

    def test_a_body_close_failure_keeps_the_mapped_write_or_sync_failure(self):
        faults = {
            "write": patch.object(filesystem, "write_all", side_effect=OSError(errno.ENOSPC, "private-disk-detail")),
            "sync": patch.object(filesystem.os, "fsync", side_effect=OSError(errno.EINVAL, "private-sync-detail")),
        }
        for name, fault in faults.items():
            with self.subTest(fault=name):
                self.receipt["commands"].clear()
                (self.work / "request.json").unlink(missing_ok=True)
                error, constructed = self.failed_preparation(fault)
                self.assertEqual(len(constructed), 1)
                self.assertIs(error, constructed[0])
                self.assertEqual((error.code, error.stage), ("github_request_body_write_failed", "github-api"))
                self.assertIsNone(error.__cause__)
                self.assertTrue(error.__suppress_context__)
                self.assertEqual(error.__notes__, ["file close failed: EIO"])

    def test_a_body_close_failure_never_replaces_an_interrupt(self):
        interrupt = KeyboardInterrupt()
        error, constructed = self.failed_preparation(patch.object(filesystem, "write_all", side_effect=interrupt))
        self.assertIs(error, interrupt)
        self.assertEqual(constructed, [])
        self.assertEqual(error.__notes__, ["file close failed: EIO"])

    def test_invalid_method_or_endpoint_is_rejected_before_the_file_is_created(self):
        with self.assertRaisesRegex(VerificationError, "github_request_method_invalid"):
            self.send({"a": 1}, method="GET")
        with self.assertRaisesRegex(VerificationError, "github_endpoint_invalid"):
            self.client("json").json_body("graphql", {"a": 1}, method="POST",
                                          body_path=self.work / "request.json")
        self.assertEqual(list(self.work.iterdir()), [])
        self.assertFalse(self.log_path.exists())

    def test_existing_or_linked_body_path_is_never_reused(self):
        existing = self.work / "existing.json"
        existing.write_text("keep")
        target = self.work / "target.json"
        target.write_text("keep")
        (self.work / "link.json").symlink_to(target)
        (self.work / "dangling.json").symlink_to(self.work / "missing.json")
        linked_parent = self.root / "linked"
        linked_parent.symlink_to(self.work)
        # A refused path component stops at the guard; an existing leaf fails to be created.
        for path, stage in ((existing, "github-api"), (self.work / "link.json", "validate-paths"),
                            (self.work / "dangling.json", "validate-paths"),
                            (linked_parent / "request.json", "validate-paths")):
            with self.subTest(path=path.name):
                with self.assertRaises(VerificationError) as caught:
                    self.send({"a": 1}, body_path=path)
                self.assertEqual((caught.exception.code, caught.exception.stage),
                                 ("github_request_body_path_invalid", stage))
        self.assertEqual((existing.read_text(), target.read_text()), ("keep", "keep"))
        self.assertFalse((self.work / "missing.json").exists())
        self.assertFalse(self.log_path.exists())


BLOB_SHA = "f4b2d150488c7591719aeb17c9912136bdc78cea"


class GitHubRawBlobTests(FakeGhCase):
    """One GET of one Git blob under its fixed raw media type; the bytes are returned unparsed."""

    def blob(self, mode: str, *, total: int = 0, limit: int = 1 << 20, sha: str = BLOB_SHA) -> bytes:
        return self.client(mode, FIXTURE_BYTES=str(total)).raw_blob("owner/repo", sha, limit=limit)

    def test_one_get_with_the_raw_media_type_returns_the_exact_bytes(self):
        total = 1564
        value = self.blob("stream", total=total, limit=total)
        expected = b"PK\x03\x04" + (bytes(range(256)) * 4096)[:total - 4]
        self.assertEqual(value, expected)
        self.assertEqual(self.invocation()["argv"], [
            "api", "--method", "GET", "-H", "Accept: application/vnd.github.raw+json",
            "-H", f"X-GitHub-Api-Version: {github.API_VERSION}", f"repos/owner/repo/git/blobs/{BLOB_SHA}"])
        self.assertEqual([(record["name"], record["method"], record["exit_code"]) for record in self.receipt["commands"]],
                         [("github-api", "GET", 0)])
        self.assertEqual(self.receipt["commands"][0]["endpoint"], f"repos/owner/repo/git/blobs/{BLOB_SHA}")

    def test_malformed_repository_sha_or_limit_is_refused_before_any_request(self):
        client = self.client("json")
        cases = {
            "repo": [("owner/..", BLOB_SHA, 10), ("../repo", BLOB_SHA, 10), ("repo", BLOB_SHA, 10)],
            "sha": [("owner/repo", sha, 10) for sha in (BLOB_SHA[:39], BLOB_SHA.upper(), "g" * 40,
                                                      BLOB_SHA + "0", "a" * 64, BLOB_SHA + "\n", 17)],
            "limit": [("owner/repo", BLOB_SHA, limit) for limit in (0, -1, True, 1.0, "10")],
        }
        with self.no_launches() as launches:
            for kind, rows in cases.items():
                for repo, sha, limit in rows:
                    with self.subTest(kind=kind, value=repr((repo, sha, limit))), \
                            self.assertRaises(VerificationError) as caught:
                        client.raw_blob(repo, sha, limit=limit)
                    self.assertEqual((caught.exception.code, caught.exception.stage),
                                     ("github_raw_blob_invalid", "github-api"))
        self.assertEqual((launches.call_count, self.receipt["commands"]), (0, []))
        self.assertFalse(self.log_path.exists())

    def test_overflow_and_nonzero_exit_keep_their_codes_after_one_attempt(self):
        for mode, total, code in (("stream", 2048, "command_output_exceeded"), ("fail", 16, "github_request_failed")):
            with self.subTest(mode=mode):
                self.receipt["commands"].clear()
                with self.assertRaises(VerificationError) as caught:
                    self.blob(mode, total=total, limit=1024)
                self.assertEqual((caught.exception.code, caught.exception.stage), (code, "github-api"))
                self.assertEqual(len(self.receipt["commands"]), 1)
                self.assert_child_closed()
        self.assertIsNone(self.tree.unresolved)

    def test_launch_and_unproven_wait_failures_propagate_canonically_without_retry(self):
        for failure in (CommandFailure.START_FAILED, CommandFailure.TIMEOUT):
            with self.subTest(failure=failure):
                self.receipt["commands"].clear()
                with patch.object(github, "run_bounded_command",
                                  side_effect=BoundedCommandError(failure)) as run, \
                        self.assertRaises(VerificationError) as caught:
                    self.blob("json")
                self.assertEqual((caught.exception.code, caught.exception.stage), (failure.value, "github-api"))
                self.assertIsInstance(caught.exception.__cause__, BoundedCommandError)
                self.assertEqual(run.call_count, 1)
                self.assertEqual(self.receipt["commands"][0]["exit_code"], None)
        # The unproven Wait is the tree's to retain; its fixture never launched a child.
        self.assertEqual(self.tree.unresolved.code, "command_timeout")
        removed, message = self.settle()
        self.assertFalse(removed)
        self.assertIn("command_timeout", message)
        self.tree.remove_retained()


class GitHubArtifactDownloadTests(FakeGhCase):
    @contextlib.contextmanager
    def refused_by_writer(self, error: BoundedCommandError):
        """The owner delivers a bad header, then fails with ``error`` (no real child)."""
        def run(argv, environment, *, sink, **options):
            with self.assertRaises(OSError):
                sink(b"<html>")
            raise error

        with patch.object(github, "run_bounded_command", side_effect=run):
            yield

    def download(self, mode: str, *, total: int = 0, limit: int = 64 * _MIB,
                 name: str = "artifact.zip", **options):
        client = self.client(mode, FIXTURE_BYTES=str(total))
        return client.download_artifact("owner/repo", 17, self.work / name, limit=limit, **options)

    def test_multi_megabyte_archive_streams_to_a_new_file_with_an_exact_digest(self):
        total = 16 * _MIB
        tracemalloc.start()
        try:
            result = self.download("stream", total=total, limit=total)
            peak = tracemalloc.get_traced_memory()[1]
        finally:
            tracemalloc.stop()
        self.assertLess(peak, 4 * _MIB)
        expected = archive_digest(total)
        self.assertEqual((result.path, result.size, result.sha256),
                         (self.work / "artifact.zip", total, expected))
        stored = hashlib.sha256()
        with result.path.open("rb") as source:
            for block in iter(lambda: source.read(_MIB), b""):
                stored.update(block)
        self.assertEqual(stored.hexdigest(), expected)
        self.assertEqual(stat.S_IMODE(result.path.stat().st_mode), 0o600)
        argv = self.invocation()["argv"]
        self.assertIn("Accept: application/vnd.github+json", argv)
        self.assertEqual(argv[-1], "repos/owner/repo/actions/artifacts/17/zip")
        record = self.receipt["commands"][0]
        self.assertEqual((record["exit_code"], record["bytes"], record["sha256"], record["limit"]),
                         (0, total, expected, total))
        self.assert_no_secret(argv, self.receipt)

    def test_output_beyond_the_limit_fails_and_closes_the_child(self):
        with self.assertRaisesRegex(VerificationError, "command_output_exceeded"):
            self.download("stream", total=3 * _MIB, limit=_MIB)
        partial = self.work / "artifact.zip"
        self.assertEqual(partial.stat().st_size, _MIB)
        self.assert_child_closed()
        record = self.receipt["commands"][0]
        self.assertEqual((record["bytes"], record["sha256"]), (_MIB, None))
        self.assertIsInstance(record["exit_code"], int)

    def test_timeout_retains_the_partial_and_request_evidence_until_closure_is_proven(self):
        with self.assertRaisesRegex(VerificationError, "command_timeout"):
            self.download("hang", total=4, timeout=1)
        self.assertEqual(self.tree.unresolved.code, "command_timeout")
        self.assertLessEqual((self.work / "artifact.zip").stat().st_size, 4)
        self.assertIsNone(self.receipt["commands"][0]["sha256"])
        removed, message = self.settle()
        self.assertFalse(removed)
        self.assertIn(f"command_timeout): {self.root}", message)
        self.assertTrue((self.work / "artifact.zip").is_file())
        self.assertTrue(self.log_path.is_file())
        self.assert_child_closed()
        self.tree.remove_retained()
        self.assertFalse(self.root.exists())
        self.assertEqual(open_descriptors(), self.descriptors)

    def test_unrecognized_or_missing_header_fails_without_a_usable_file(self):
        for mode in ("bad-header", "empty"):
            with self.subTest(mode=mode):
                with self.assertRaisesRegex(VerificationError, "github_artifact_header_invalid"):
                    self.download(mode, name=mode + ".zip")
                self.assertEqual((self.work / (mode + ".zip")).stat().st_size, 0)
                self.assertIsNone(self.receipt["commands"][-1]["sha256"])

    def test_gh_failure_after_output_is_not_a_complete_download(self):
        with self.assertRaisesRegex(VerificationError, "github_request_failed"):
            self.download("fail", total=1024)
        record = self.receipt["commands"][0]
        self.assertEqual((record["exit_code"], record["bytes"], record["sha256"]), (3, 1024, None))
        self.assertEqual((self.work / "artifact.zip").stat().st_size, 1024)

    def test_short_write_then_failure_accounts_for_the_bytes_actually_stored(self):
        real_write, calls = os.write, []

        def short_then_full_disk(descriptor, data):
            calls.append(descriptor)
            if len(calls) == 1:
                return real_write(descriptor, bytes(data[:100]))
            raise OSError(errno.ENOSPC, "private-disk-detail")

        with patch.object(github.os, "write", side_effect=short_then_full_disk):
            with self.assertRaisesRegex(VerificationError, "github_download_write_failed"):
                self.download("stream", total=_MIB)
        record = self.receipt["commands"][0]
        stored = (self.work / "artifact.zip").stat().st_size
        self.assertEqual((stored, record["bytes"]), (100, 100))
        self.assertEqual((record["sha256"], record["writer_failure"], record["close_failed"]),
                         (None, "github_download_write_failed", False))
        self.assert_child_closed()
        self.assert_no_secret(self.receipt)

    def test_close_failure_after_a_complete_write_publishes_no_success(self):
        with self.closing_fault():
            with self.assertRaisesRegex(VerificationError, "github_download_close_failed") as caught:
                self.download("stream", total=_MIB)
        self.assertEqual(caught.exception.__cause__.errno, errno.EIO)
        record = self.receipt["commands"][0]
        self.assertEqual((record["bytes"], record["sha256"], record["close_failed"],
                          record["exit_code"]), (_MIB, None, True, 0))
        self.assertEqual((self.work / "artifact.zip").stat().st_size, _MIB)

    def test_close_failure_keeps_a_closed_primary_failure(self):
        with self.closing_fault():
            with self.assertRaisesRegex(VerificationError, "github_artifact_header_invalid") as caught:
                self.download("bad-header")
        self.assertEqual(caught.exception.__notes__, ["download file close failed: EIO"])
        record = self.receipt["commands"][0]
        self.assertEqual((record["writer_failure"], record["close_failed"], record["sha256"]),
                         ("github_artifact_header_invalid", True, None))

    def test_unknown_wait_keeps_the_canonical_closure_failure_with_close_and_writer_faults(self):
        closure = BoundedCommandError(CommandFailure.TERMINATION_TIMEOUT)
        capture = BoundedCommandError(CommandFailure.CAPTURE_UNAVAILABLE)
        capture.__cause__ = closure
        with self.no_launches() as launches, self.closing_fault(), self.refused_by_writer(capture):
            with self.assertRaisesRegex(VerificationError, "command_capture_unavailable") as caught:
                self.download("stream", total=_MIB)
        self.assertIs(self.tree.unresolved, caught.exception)
        self.assertIs(caught.exception.__cause__, capture)
        self.assertIs(capture.__cause__, closure)
        self.assertEqual(caught.exception.__notes__, ["download file close failed: EIO"])
        record = self.receipt["commands"][0]
        self.assertEqual((record["exit_code"], record["writer_failure"], record["close_failed"],
                          record["sha256"]), (None, "github_artifact_header_invalid", True, None))
        self.assert_retained_then_removed(launches)

    def assert_retained_then_removed(self, launches: Mock) -> None:
        """Disposal retains the evidence; only the zero-launch oracle permits its removal."""
        removed, message = self.settle()
        self.assertFalse(removed)
        self.assertIn(f"{self.tree.unresolved.code}): {self.root}", message)
        self.assertTrue((self.work / "artifact.zip").is_file())
        self.assertEqual(launches.call_count, 0)
        self.tree.remove_retained()
        self.assertFalse(self.root.exists())
        self.assertEqual(open_descriptors(), self.descriptors)

    def assert_refusal(self, reason: CommandFailure, exit_code: int | None, expected: str,
                       retained: bool) -> None:
        error = BoundedCommandError(reason, exit_code)
        with self.no_launches() as launches, self.refused_by_writer(error):
            with self.assertRaisesRegex(VerificationError, expected):
                self.download("stream", total=_MIB)
        self.assertEqual(self.receipt["commands"][-1]["exit_code"], exit_code)
        self.assertEqual(self.tree.unresolved is not None, retained)
        if retained:
            self.assert_retained_then_removed(launches)
            return
        removed, message = self.settle()
        self.assertEqual((removed, self.root.exists(), message), (True, False, ""))
        self.assertEqual(launches.call_count, 0)

    def test_writer_refusal_is_named_when_the_wait_is_proven_and_the_tree_is_removed(self):
        self.assert_refusal(CommandFailure.CAPTURE_UNAVAILABLE, -9,
                            "github_artifact_header_invalid", retained=False)

    def test_output_exceeded_with_a_proven_wait_removes_the_tree(self):
        self.assert_refusal(CommandFailure.OUTPUT_EXCEEDED, -9, "command_output_exceeded",
                            retained=False)

    def test_capture_failure_with_an_unknown_wait_keeps_its_code_and_retains_the_tree(self):
        self.assert_refusal(CommandFailure.CAPTURE_UNAVAILABLE, None,
                            "command_capture_unavailable", retained=True)

    def test_termination_timeout_keeps_its_code_and_retains_the_tree(self):
        self.assert_refusal(CommandFailure.TERMINATION_TIMEOUT, None,
                            "command_termination_timeout", retained=True)

    def test_termination_failure_keeps_its_code_and_retains_the_tree(self):
        self.assert_refusal(CommandFailure.TERMINATION_FAILED, None,
                            "command_termination_failed", retained=True)

    def test_closed_failure_removes_the_tree_and_consumes_the_pins(self):
        with self.assertRaisesRegex(VerificationError, "github_artifact_header_invalid"):
            self.download("bad-header")
        self.assertTrue((self.work / "artifact.zip").is_file())
        self.assertTrue(self.log_path.is_file())
        self.assertIsNone(self.tree.unresolved)
        removed, message = self.settle()
        self.assertEqual((removed, self.root.exists(), message), (True, False, ""))

    def test_filesystem_write_failure_is_explicit_and_closes_the_child(self):
        fault = OSError(errno.ENOSPC, "private-disk-detail")
        with patch.object(github, "write_all", side_effect=fault):
            with self.assertRaisesRegex(VerificationError, "github_download_write_failed"):
                self.download("stream", total=_MIB)
        self.assert_child_closed()
        self.assert_no_secret(self.receipt)

    def test_a_stalled_filesystem_write_is_the_same_explicit_failure(self):
        with patch.object(github, "write_all", side_effect=stalled_write):
            with self.assertRaisesRegex(VerificationError, "github_download_write_failed"):
                self.download("stream", total=_MIB)
        self.assert_child_closed()
        self.assertEqual(self.receipt["commands"][0]["writer_failure"], "github_download_write_failed")

    def test_destination_is_created_once_and_never_follows_a_link(self):
        existing = self.work / "existing.zip"
        existing.write_text("keep")
        target = self.work / "target.zip"
        target.write_text("keep")
        (self.work / "link.zip").symlink_to(target)
        (self.work / "dangling.zip").symlink_to(self.work / "missing.zip")
        linked_parent = self.root / "linked"
        linked_parent.symlink_to(self.work)
        for name, stage in (("existing.zip", "github-api"), ("link.zip", "validate-paths"),
                            ("dangling.zip", "validate-paths")):
            with self.subTest(name=name):
                with self.assertRaises(VerificationError) as caught:
                    self.download("stream", total=1024, name=name)
                self.assertEqual((caught.exception.code, caught.exception.stage),
                                 ("github_download_destination_invalid", stage))
        with self.assertRaises(VerificationError) as caught:
            self.client("stream").download_artifact(
                "owner/repo", 17, linked_parent / "artifact.zip", limit=_MIB)
        self.assertEqual((caught.exception.code, caught.exception.stage),
                         ("github_download_destination_invalid", "validate-paths"))
        self.assertEqual((existing.read_text(), target.read_text()), ("keep", "keep"))
        self.assertFalse((self.work / "missing.zip").exists())
        self.assertFalse(self.log_path.exists())

    def test_invalid_request_is_rejected_before_any_effect(self):
        client = self.client("stream")
        destination = self.work / "artifact.zip"
        for repo, artifact, limit, code in (
                ("owner/repo?x=1", 17, _MIB, "github_download_invalid"),
                ("owner/{repo}", 17, _MIB, "github_download_invalid"),
                ("../repo", 17, _MIB, "github_download_invalid"),
                ("owner/..", 17, _MIB, "github_download_invalid"),
                ("./repo", 17, _MIB, "github_download_invalid"),
                ("owner/repo", 17, 0, "github_download_invalid"),
                ("owner/repo", 17, True, "github_download_invalid"),
                ("owner/repo", 0, _MIB, "github_id_invalid"),
                ("owner/repo", True, _MIB, "github_id_invalid")):
            with self.subTest(repo=repo, artifact=artifact, limit=limit):
                with self.assertRaisesRegex(VerificationError, code):
                    client.download_artifact(repo, artifact, destination, limit=limit)
        self.assertFalse(destination.exists())
        self.assertFalse(self.log_path.exists())
        self.assertEqual(self.receipt["commands"], [])


if __name__ == "__main__":
    unittest.main()
