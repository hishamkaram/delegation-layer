"""Private raw-evidence inventory budgets stay separate from public staging caps."""

import os
from pathlib import Path
import tempfile
import unittest

from scripts import native_acceptance_evidence as evidence
from scripts.delegate_acceptance.directories.journal import directory_identity_record
from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from scripts.delegate_acceptance.native_evidence.contract import (
    PRIVATE_INVENTORY_MAX_BYTES,
    PRIVATE_INVENTORY_MAX_ENTRIES,
    PUBLIC_STAGING_MAX_ENTRIES,
)
from scripts.delegate_acceptance.native_evidence.filesystem import _tree_summary
from scripts.delegate_acceptance.native_evidence.stage_publication import (
    StagePlan,
    _inventory,
    publish_stage,
)
# Import the module, not the TestCase, so its tests are discovered only once.
from scripts import test_native_acceptance_evidence as evidence_tests


# The retained Mac timeout case: one capture root holding 1,153 runner-status
# captures of five files each, 6,919 entries and 3,183,628 bytes in total.
OBSERVED_CAPTURES, OBSERVED_ENTRIES, OBSERVED_BYTES = 1153, 6919, 3_183_628
CAPTURE_FILES = ("invocation.json", "started.json", "completed.json", "stderr", "stdout")
CAPTURE_FILE_BYTES = 552


def sized_file(path: Path, size: int) -> None:
    """Create a sparse regular file whose reported size is exact."""
    with path.open("xb") as stream:
        stream.truncate(size)


def write_observed_tree(directory: Path) -> Path:
    """Reproduce the observed capture shape, entry count, and byte count."""
    captures = directory / "provider-captures"
    captures.mkdir()
    remaining = OBSERVED_BYTES
    for index in range(1, OBSERVED_CAPTURES + 1):
        capture = captures / f"{index:04d}-runner-status"
        capture.mkdir()
        for name in CAPTURE_FILES:
            last = index == OBSERVED_CAPTURES and name == CAPTURE_FILES[-1]
            size = remaining if last else CAPTURE_FILE_BYTES
            sized_file(capture / name, size)
            remaining -= size
    return captures


def write_flat_tree(directory: Path, *, entries: int, total_bytes: int) -> Path:
    """Reproduce an entry and byte count in one directory.

    One directory keeps owner cleanup, which syncs every removed directory, fast.
    """
    captures = directory / "provider-captures"
    captures.mkdir()
    sized_file(captures / "00000", total_bytes)
    for index in range(1, entries - 1):
        os.close(os.open(captures / f"{index:05d}", os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600))
    return captures


class PrivateInventoryTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="native-private-inventory-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve(strict=True)

    def test_observed_case_size_is_a_complete_inventory(self):
        write_observed_tree(self.root)
        summary = _tree_summary(self.root)
        self.assertTrue(summary["complete"])
        self.assertEqual(summary["file_count"], OBSERVED_CAPTURES * len(CAPTURE_FILES))
        self.assertEqual(summary["file_count"] + OBSERVED_CAPTURES + 1, OBSERVED_ENTRIES)
        self.assertEqual(summary["byte_count"], OBSERVED_BYTES)

    def test_entry_budget_is_exact(self):
        self.assertEqual(PRIVATE_INVENTORY_MAX_ENTRIES, 65536)
        write_flat_tree(self.root, entries=PRIVATE_INVENTORY_MAX_ENTRIES, total_bytes=0)
        summary = _tree_summary(self.root)
        self.assertEqual((summary["complete"], summary["file_count"]),
                         (True, PRIVATE_INVENTORY_MAX_ENTRIES - 1))
        sized_file(self.root / "one-more", 0)
        self.assertFalse(_tree_summary(self.root)["complete"])

    def test_byte_budget_is_exact(self):
        self.assertEqual(PRIVATE_INVENTORY_MAX_BYTES, 128 * 1024 * 1024)
        sized_file(self.root / "budget", PRIVATE_INVENTORY_MAX_BYTES)
        summary = _tree_summary(self.root)
        self.assertEqual((summary["complete"], summary["byte_count"]),
                         (True, PRIVATE_INVENTORY_MAX_BYTES))
        sized_file(self.root / "one-more", 1)
        self.assertFalse(_tree_summary(self.root)["complete"])


class InventoryAggregateTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="native-inventory-aggregate-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve(strict=True)
        self.fixture = object.__new__(evidence_tests.NativeAcceptanceEvidenceTests)
        self.fixture.root = self.root

    def test_observed_size_resolves_a_pass_and_retains_a_genuine_failure(self):
        root = self.root / "native-aggregate-observed-inventory"
        matrix = self.fixture.make_matrix(root, status="PASS", failed_index=0)
        failed, passed = (root / case.case_output for case in evidence.EXPECTED_CASES[:2])
        for directory in (failed, passed):
            write_flat_tree(directory, entries=OBSERVED_ENTRIES, total_bytes=OBSERVED_BYTES)
        value, code = evidence.build_aggregate(
            root, matrix, root / evidence.AGGREGATE_RECEIPT_NAME,
            tag="v1.2.3", source_revision="a" * 40,
        )
        self.assertEqual((code, value["status"], value["unresolved_case_count"]), (1, "FAIL", 1))
        self.assertEqual([case["status"] for case in value["cases"][:2]], ["FAIL", "PASS"])
        self.assertEqual([case["evidence_status"] for case in value["cases"][:2]],
                         ["UNRESOLVED", "RESOLVED"])
        retained = _tree_summary(failed / "provider-captures")
        self.assertEqual((retained["complete"], retained["file_count"], retained["byte_count"]),
                         (True, OBSERVED_ENTRIES - 1, OBSERVED_BYTES))
        self.assertTrue((failed / "raw-provider-output").is_file())
        self.assertFalse(passed.exists())


class PublicStagingCapTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="native-public-staging-cap-")
        self.addCleanup(temporary.cleanup)
        parent = Path(temporary.name).resolve(strict=True) / "owned-parent"
        parent.mkdir()
        self.output = parent / "stage"
        owner = DirectoryOwner()
        try:
            self.identity = directory_identity_record(
                owner.create("test stage", self.output, parent))
        finally:
            owner.release()

    def plan(self, count: int) -> StagePlan:
        plan = StagePlan(self.output)
        for index in range(count - 1):
            plan.put(self.output / "cases" / f"{index:04d}.json", b"{}")
        plan.put(self.output / "manifest.json", b"{}")
        return plan

    def test_plan_cap_is_unchanged(self):
        self.assertEqual(PUBLIC_STAGING_MAX_ENTRIES, 4096)
        plan = self.plan(PUBLIC_STAGING_MAX_ENTRIES)
        with self.assertRaisesRegex(evidence.EvidenceError, "stage_plan_invalid"):
            plan.put(self.output / "extra.json", b"{}")

    def test_staged_tree_scan_cap_is_unchanged(self):
        # The planned cases directory is an entry too: 4,095 files fill the cap.
        at_cap = self.plan(PUBLIC_STAGING_MAX_ENTRIES - 1)
        (self.output / "cases").mkdir(mode=0o700)
        for item in at_cap.files():
            (self.output / item.name).write_bytes(item.data)
        owner = DirectoryOwner()
        try:
            root = owner.adopt_created("test stage", self.output, self.output.parent)
            observed, temporary = _inventory(root, at_cap.files())
        finally:
            owner.release()
        self.assertEqual((len(observed), temporary), (PUBLIC_STAGING_MAX_ENTRIES - 1, []))
        over_cap = self.plan(PUBLIC_STAGING_MAX_ENTRIES)
        extra = over_cap.files()[-2]
        (self.output / extra.name).write_bytes(extra.data)
        with self.assertRaisesRegex(evidence.EvidenceError, "stage_inventory_too_large"):
            publish_stage(over_cap, self.identity)


if __name__ == "__main__":
    unittest.main()
