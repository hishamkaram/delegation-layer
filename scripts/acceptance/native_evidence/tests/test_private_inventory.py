"""Private raw-evidence inventory budgets stay separate from public staging caps."""

import os
from pathlib import Path
import tempfile
import unittest

import scripts.acceptance.native_evidence.aggregate as native_evidence_aggregate
import scripts.acceptance.native_evidence.contract as native_evidence_contract
from scripts.acceptance.native_evidence.contract import PRIVATE_INVENTORY_MAX_BYTES
from scripts.acceptance.native_evidence.contract import PRIVATE_INVENTORY_MAX_ENTRIES
from scripts.acceptance.native_evidence.filesystem import _tree_summary
from scripts.acceptance.native_evidence.contract import _empty_summary
from scripts.acceptance.native_evidence.contract import _summary_with_removed
# Import the module, not the TestCase, so its tests are discovered only once.
from scripts.acceptance.native_evidence.tests import test_evidence as evidence_tests


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
    def test_invalid_retained_totals_cannot_be_reported_complete(self) -> None:
        for count in ([], {}, None, True, "1"):
            before: dict[str, object] = dict(_empty_summary())
            before["file_count"] = count
            with self.subTest(count=count):
                result = _summary_with_removed(_empty_summary(), [{"before": before}])
                self.assertIs(result["complete"], False)
                self.assertEqual((result["root_count"], result["file_count"], result["byte_count"]),
                                 (0, 0, 0))

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
        failed, passed = (root / case.case_output for case in native_evidence_contract.EXPECTED_CASES[:2])
        for directory in (failed, passed):
            write_flat_tree(directory, entries=OBSERVED_ENTRIES, total_bytes=OBSERVED_BYTES)
        value, code = native_evidence_aggregate.build_aggregate(
            root, matrix, root / native_evidence_contract.AGGREGATE_RECEIPT_NAME,
            tag="v1.2.3", source_revision="a" * 40,
        )
        self.assertEqual((code, value["status"], value["unresolved_case_count"]), (1, "FAIL", 1))
        cases = value["cases"]
        assert isinstance(cases, list)
        self.assertEqual([case["status"] for case in cases[:2]], ["FAIL", "PASS"])
        self.assertEqual([case["evidence_status"] for case in cases[:2]],
                         ["UNRESOLVED", "RESOLVED"])
        retained = _tree_summary(failed / "provider-captures")
        self.assertEqual((retained["complete"], retained["file_count"], retained["byte_count"]),
                         (True, OBSERVED_ENTRIES - 1, OBSERVED_BYTES))
        self.assertTrue((failed / "raw-provider-output").is_file())
        self.assertFalse(passed.exists())


if __name__ == "__main__":
    unittest.main()
