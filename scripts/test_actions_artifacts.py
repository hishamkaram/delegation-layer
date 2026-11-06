"""The shared Actions artifact listing and exact-inventory plain ZIP mechanisms, independent of their callers.

Replies come from an in-memory fake of the client's ``json`` port; archives are
built in memory. No network, file or process is used.
"""

from __future__ import annotations

from collections.abc import Sequence
import io
from pathlib import Path
import sys
import unittest
import zipfile

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.delegate_acceptance.release_verification import actions_artifacts as artifacts
from scripts.delegate_acceptance.release_verification.contract import VerificationError


REPOSITORY = "owner/repo"
RUN_ID = 42


class FakeClient:
    """Answers the listing pages in order and records each endpoint with its reply bound."""

    def __init__(self, pages: Sequence[object]):
        self.pages = pages
        self.requests: list[tuple[str, int]] = []

    def json(self, endpoint: str, *, limit: int) -> object:
        self.requests.append((endpoint, limit))
        page = int(endpoint.rsplit("&page=", 1)[1])
        reply = self.pages[page - 1]
        if isinstance(reply, Exception):
            raise reply
        return reply


def rows(first: int, count: int) -> list[dict[str, object]]:
    return [{"id": first + index, "name": f"artifact-{first + index}"} for index in range(count)]


def plain_zip(members: dict[str, bytes], *, mode: int = 0o100644,
              compression: int = zipfile.ZIP_DEFLATED) -> bytes:
    buffer = io.BytesIO()
    with zipfile.ZipFile(buffer, "w") as bundle:
        for name, data in members.items():
            info = zipfile.ZipInfo(name, date_time=(2026, 10, 23, 0, 0, 0))
            info.external_attr = mode << 16
            info.compress_type = compression
            bundle.writestr(info, data)
    return buffer.getvalue()


class ListingTests(unittest.TestCase):
    def listing(self, pages: Sequence[object]) -> tuple[list[dict[str, object]], FakeClient]:
        client = FakeClient(pages)
        return artifacts.list_run_artifacts(client, REPOSITORY, RUN_ID, reply_limit=1234), client

    def assertRefused(self, pages: Sequence[object], code: str) -> None:
        with self.assertRaises(VerificationError) as caught:
            self.listing(pages)
        self.assertEqual((caught.exception.code, caught.exception.stage), (code, artifacts.STAGE))

    def test_every_page_is_read_until_the_stated_total_with_the_callers_reply_bound(self):
        found, client = self.listing([{"total_count": 101, "artifacts": rows(1, 100)},
                                      {"total_count": 101, "artifacts": rows(101, 1)}])
        self.assertEqual([row["id"] for row in found], list(range(1, 102)))
        self.assertEqual(client.requests, [
            (f"repos/{REPOSITORY}/actions/runs/{RUN_ID}/artifacts?per_page=100&page={page}", 1234)
            for page in (1, 2)])
        self.assertEqual(self.listing([{"total_count": 0, "artifacts": []}])[0], [])

    def test_an_unfinished_listing_refuses_as_incomplete_and_the_page_bound_holds(self):
        self.assertRefused([{"total_count": 2, "artifacts": rows(1, 1)}], artifacts.LISTING_INCOMPLETE)
        self.assertRefused([{"total_count": 150, "artifacts": rows(1, 100)},
                            {"total_count": 150, "artifacts": rows(101, 20)}], artifacts.LISTING_INCOMPLETE)
        self.assertRefused([{"total_count": 1001, "artifacts": rows(1, 100)}], artifacts.LISTING_INVALID)
        many = [{"total_count": 1000, "artifacts": rows(page * 100, 100)} for page in range(1, 11)]
        self.assertEqual(len(self.listing(many)[0]), 1000)

    def test_inconsistent_duplicated_or_malformed_listings_refuse_as_invalid(self):
        cases = {
            "duplicate id": [{"total_count": 2, "artifacts": [{"id": 1}, {"id": 1}]}],
            "duplicate across pages": [{"total_count": 101, "artifacts": rows(1, 100)},
                                       {"total_count": 101, "artifacts": [{"id": 1}]}],
            "total changed": [{"total_count": 101, "artifacts": rows(1, 100)},
                              {"total_count": 102, "artifacts": rows(101, 1)}],
            "boolean total": [{"total_count": True, "artifacts": [{"id": 1}]}],
            "text id": [{"total_count": 1, "artifacts": [{"id": "1"}]}],
            "zero id": [{"total_count": 1, "artifacts": [{"id": 0}]}],
            "row not object": [{"total_count": 1, "artifacts": [[1]]}],
            "reply not object": [[{"id": 1}]],
            "artifacts missing": [{"total_count": 1}],
            "oversized page": [{"total_count": 101, "artifacts": rows(1, 101)}],
        }
        for label, pages in cases.items():
            with self.subTest(label):
                self.assertRefused(pages, artifacts.LISTING_INVALID)

    def test_a_transport_failure_propagates_unchanged(self):
        failure = VerificationError("github_request_failed", "github-api")
        with self.assertRaises(VerificationError) as caught:
            self.listing([failure])
        self.assertIs(caught.exception, failure)
        self.assertIsNone(artifacts.refusal_code(failure, {"github_request_failed": "translated"}))


class ZipTests(unittest.TestCase):
    LIMITS = {"receipt.json": 64, "other.json": 8}
    MEMBERS = {"receipt.json": b'{"ok": true}', "other.json": b"{}"}

    def assertRefused(self, data: bytes, code: str, limits: dict[str, int] | None = None) -> VerificationError:
        with self.assertRaises(VerificationError) as caught:
            artifacts.read_exact_zip(data, self.LIMITS if limits is None else limits)
        self.assertEqual((caught.exception.code, caught.exception.stage), (code, artifacts.STAGE))
        return caught.exception

    def test_exactly_the_named_plain_members_are_returned_for_both_compressions(self):
        for compression in (zipfile.ZIP_STORED, zipfile.ZIP_DEFLATED):
            with self.subTest(compression=compression):
                data = plain_zip(self.MEMBERS, compression=compression)
                self.assertEqual(artifacts.read_exact_zip(data, self.LIMITS), self.MEMBERS)
        single = plain_zip({"receipt.json": b"{}"}, mode=0)
        self.assertEqual(artifacts.read_exact_zip(single, {"receipt.json": 2}), {"receipt.json": b"{}"})

    def test_the_inventory_must_equal_the_callers_names(self):
        for members in ({"receipt.json": b"{}"}, {**self.MEMBERS, "extra.json": b"{}"},
                        {"out/receipt.json": b"{}", "other.json": b"{}"},
                        {"receipt.json/": b"", "other.json": b"{}"}):
            with self.subTest(members=sorted(members)):
                self.assertRefused(plain_zip(members), artifacts.ZIP_INVENTORY_MISMATCH)

    def test_links_directories_unsupported_methods_and_encryption_refuse(self):
        for options in ({"mode": 0o120777}, {"mode": 0o040755}, {"compression": zipfile.ZIP_BZIP2}):
            with self.subTest(options=options):
                self.assertRefused(plain_zip(self.MEMBERS, **options), artifacts.ZIP_MEMBER_INVALID)
        data = bytearray(plain_zip(self.MEMBERS, compression=zipfile.ZIP_STORED))
        data[data.index(b"PK\x01\x02") + 8] |= 0x01  # the central directory's general-purpose flags
        self.assertRefused(bytes(data), artifacts.ZIP_MEMBER_INVALID)

    def test_each_member_is_bounded_by_its_own_limit(self):
        self.assertRefused(plain_zip({**self.MEMBERS, "other.json": b"0" * 9}), artifacts.ZIP_MEMBER_TOO_LARGE)
        self.assertEqual(artifacts.read_exact_zip(plain_zip({**self.MEMBERS, "other.json": b"0" * 8}),
                                                  self.LIMITS)["other.json"], b"0" * 8)

    def test_corrupt_or_foreign_bytes_refuse_with_their_cause(self):
        stored = plain_zip(self.MEMBERS, compression=zipfile.ZIP_STORED)
        corrupt = stored.replace(self.MEMBERS["receipt.json"], b"X" * len(self.MEMBERS["receipt.json"]), 1)
        error = self.assertRefused(corrupt, artifacts.ZIP_INVALID)
        self.assertIsInstance(error.__cause__, zipfile.BadZipFile)
        self.assertIsInstance(self.assertRefused(b"PK\x03\x04 not a zip", artifacts.ZIP_INVALID).__cause__,
                              zipfile.BadZipFile)

    def test_only_this_modules_refusals_are_translated(self):
        codes = {artifacts.ZIP_INVALID: "caller_zip_invalid"}
        self.assertEqual(artifacts.refusal_code(VerificationError(artifacts.ZIP_INVALID, artifacts.STAGE), codes),
                         "caller_zip_invalid")
        self.assertIsNone(artifacts.refusal_code(VerificationError(artifacts.ZIP_INVALID, "other"), codes))
        self.assertIsNone(artifacts.refusal_code(VerificationError(artifacts.LISTING_INVALID, artifacts.STAGE),
                                                 codes))


if __name__ == "__main__":
    unittest.main()
