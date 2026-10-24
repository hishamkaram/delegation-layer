"""Closure evidence bytes round-trip exactly, and the reader and encoder share one bound."""

from __future__ import annotations

import json
import unittest

from scripts.acceptance_tests import candidate_fixtures as fx
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.release_verification.candidate_attempts import (
    MAX_LIST_ITEMS, MAX_RECORD_BYTES,
)
from scripts.delegate_acceptance.release_verification.candidate_closure import (
    NATIVE_CHILD_RESOURCE, ClaimSubject, ClosureResolution, ProofSubject, ResourceDisposition,
    ResourceResolution, encode_closure_evidence, parse_closure_evidence,
)
from scripts.delegate_acceptance.release_verification.candidate_codec import encode_record
from scripts.delegate_acceptance.release_verification.contract import VerificationError


WAITED = ResourceDisposition.WAITED
NOT_STARTED = ResourceDisposition.NOT_STARTED
CASE = fx.case_id("codex:exec", "write")
CLAIM = ClaimSubject(claim_sha256="1" * 64)
PROOF = ProofSubject(proof_sha256="2" * 64)
INVOCATION = "invocation-1-Darwin-arm64"


def mutated(data: bytes, change) -> bytes:
    record = json.loads(data)
    change(record)
    return encode_record(record, "test")


def resource_change(change):
    return lambda record: change(record["resources"][0])


def inventory(count: int, *, width: int) -> tuple[ResourceResolution, ...]:
    """Return ``count`` sorted unique resources whose identities are ``width`` characters long.

    The longer disposition makes the widest record exceed the byte bound.
    """
    return tuple(ResourceResolution(resource_id=f"r{index:05d}".ljust(width, "x"), disposition=NOT_STARTED,
                                    fact_sha256=fx.FACT)
                 for index in range(count))


class ClosureCase(unittest.TestCase):
    def assertCode(self, code, function, *args, **kwargs):
        with self.assertRaises(VerificationError) as caught:
            function(*args, **kwargs)
        self.assertEqual(caught.exception.code, code)

    def never_started(self) -> ClosureResolution:
        return fx.resolution(CLAIM, invocation=INVOCATION, resources=((NATIVE_CHILD_RESOURCE, NOT_STARTED),))

    def waited_claim(self) -> ClosureResolution:
        return fx.resolution(CLAIM, invocation=INVOCATION, resources=(
            (NATIVE_CHILD_RESOURCE, WAITED), (f"{CASE}:state", WAITED), (f"{CASE}:supervisor", NOT_STARTED)))

    def proof_closure(self) -> ClosureResolution:
        return fx.resolution(PROOF, invocation=INVOCATION, resources=(
            ("pueue-task-7", WAITED), ("workspace-lease-7", NOT_STARTED)))


class RoundTripTests(ClosureCase):
    def test_both_claim_shapes_and_a_proof_subject_round_trip_exactly(self):
        for value in (self.never_started(), self.waited_claim(), self.proof_closure()):
            with self.subTest(subject=value.subject, resources=value.resource_ids):
                data = encode_closure_evidence(value)
                parsed = parse_closure_evidence(data)
                self.assertEqual(parsed, value)
                self.assertEqual(encode_closure_evidence(parsed), data)
                self.assertEqual(parsed.evidence_sha256, sha(data))

    def test_record_carries_exactly_the_versioned_header_and_fields(self):
        record = json.loads(encode_closure_evidence(self.waited_claim()))
        self.assertEqual(set(record), {"schema_version", "kind", "subject", "invocation_id", "resources"})
        self.assertEqual((record["schema_version"], record["kind"]),
                         (1, "delegation-layer-candidate-closure-evidence"))
        self.assertEqual(record["subject"], {"claim_sha256": "1" * 64})
        self.assertIn({"disposition": "waited", "fact_sha256": fx.FACT, "resource_id": NATIVE_CHILD_RESOURCE},
                      record["resources"])


class StrictReaderTests(ClosureCase):
    def setUp(self):
        self.data = encode_closure_evidence(self.waited_claim())

    def test_header_and_field_sets_are_exact(self):
        cases = (
            ("candidate_record_kind_invalid", lambda r: r.update(schema_version=True)),
            ("candidate_record_kind_invalid", lambda r: r.update(schema_version=2)),
            ("candidate_record_kind_invalid", lambda r: r.update(schema_version=1.5)),
            ("candidate_record_kind_invalid", lambda r: r.update(kind="delegation-layer-candidate-platform-claim")),
            ("candidate_record_kind_invalid", lambda r: r.pop("kind")),
            ("candidate_record_fields_invalid", lambda r: r.update(evidence_sha256="3" * 64)),
            ("candidate_record_fields_invalid", lambda r: r.pop("invocation_id")),
            ("candidate_record_fields_invalid", lambda r: r.pop("resources")),
            ("candidate_record_fields_invalid", resource_change(lambda item: item.update(complete=True))),
            ("candidate_record_fields_invalid", resource_change(lambda item: item.pop("fact_sha256"))),
            ("candidate_record_fields_invalid", lambda r: r["resources"].__setitem__(0, NATIVE_CHILD_RESOURCE)),
            ("candidate_closure_subject_invalid", lambda r: r.update(subject={"claim_sha256": "1" * 64,
                                                                              "proof_sha256": "2" * 64})),
            ("candidate_closure_subject_invalid", lambda r: r.update(subject={"invocation_sha256": "1" * 64})),
            ("candidate_closure_subject_invalid", lambda r: r.update(subject="1" * 64)),
        )
        for code, change in cases:
            with self.subTest(code=code, change=change):
                self.assertCode(code, parse_closure_evidence, mutated(self.data, change))

    def test_digests_identities_and_dispositions_are_validated(self):
        cases = (
            ("candidate_field_invalid", lambda r: r.update(subject={"claim_sha256": "A" * 64})),
            ("candidate_field_invalid", lambda r: r.update(subject={"proof_sha256": "1" * 63})),
            ("candidate_field_invalid", lambda r: r.update(invocation_id="/proc/1")),
            ("candidate_field_invalid", lambda r: r.update(invocation_id="")),
            ("candidate_field_invalid", lambda r: r.update(invocation_id="i" * 129)),
            ("candidate_field_invalid", lambda r: r.update(invocation_id=7)),
            ("candidate_field_invalid", resource_change(lambda item: item.update(fact_sha256="F" * 64))),
            ("candidate_field_invalid", resource_change(lambda item: item.update(fact_sha256=None))),
            ("candidate_field_invalid", resource_change(lambda item: item.update(resource_id="../native-child"))),
            ("candidate_field_invalid", resource_change(lambda item: item.update(resource_id=""))),
            ("candidate_resolution_invalid", resource_change(lambda item: item.update(disposition="WAITED"))),
            ("candidate_resolution_invalid", resource_change(lambda item: item.update(disposition="closed"))),
            ("candidate_resolution_invalid", resource_change(lambda item: item.update(disposition=None))),
            ("candidate_resolution_invalid", resource_change(lambda item: item.update(disposition=["waited"]))),
        )
        for code, change in cases:
            with self.subTest(code=code, change=change):
                self.assertCode(code, parse_closure_evidence, mutated(self.data, change))

    def test_inventory_must_be_a_sorted_unique_nonempty_list(self):
        cases = (
            ("candidate_field_invalid", lambda r: r.update(resources=None)),
            ("candidate_field_invalid", lambda r: r.update(resources={})),
            ("candidate_resolution_invalid", lambda r: r.update(resources=[])),
            ("candidate_resolution_invalid", lambda r: r["resources"].reverse()),
            ("candidate_resolution_invalid", lambda r: r["resources"].append(r["resources"][0])),
        )
        for code, change in cases:
            with self.subTest(code=code, change=change):
                self.assertCode(code, parse_closure_evidence, mutated(self.data, change))

    def test_noncanonical_or_duplicate_json_is_refused(self):
        record = json.loads(self.data)
        self.assertCode("candidate_record_not_canonical", parse_closure_evidence, self.data.rstrip(b"\n"))
        self.assertCode("candidate_record_not_canonical", parse_closure_evidence,
                        json.dumps(record, sort_keys=True, indent=1).encode() + b"\n")
        self.assertCode("candidate_record_not_canonical", parse_closure_evidence,
                        json.dumps(dict(reversed(record.items())), separators=(",", ":")).encode() + b"\n")
        self.assertCode("candidate_record_not_canonical", parse_closure_evidence, b"[]\n")
        self.assertCode("candidate_record_invalid", parse_closure_evidence,
                        self.data.replace(b'{"invocation_id"', b'{"invocation_id":"other","invocation_id"', 1))
        self.assertCode("candidate_record_invalid", parse_closure_evidence, b"\xff")
        self.assertCode("candidate_record_invalid", parse_closure_evidence, b"")


class NoImpliedClosureTests(ClosureCase):
    """An empty or not-started assumption never stands in for the producer's inventory."""

    def setUp(self):
        self.data = encode_closure_evidence(self.never_started())

    def test_claim_closure_needs_the_native_child_and_nothing_beside_a_not_started_child(self):
        child = {"resource_id": NATIVE_CHILD_RESOURCE, "disposition": "not-started", "fact_sha256": fx.FACT}
        other = {"resource_id": f"{CASE}:state", "disposition": "waited", "fact_sha256": fx.FACT}
        for resources in ([other], [child, other], []):
            with self.subTest(resources=resources):
                self.assertCode("candidate_resolution_invalid", parse_closure_evidence,
                                mutated(self.data, lambda r: r.update(resources=resources)))

    def test_proof_closure_never_accepts_an_empty_inventory(self):
        data = encode_closure_evidence(self.proof_closure())
        self.assertCode("candidate_resolution_invalid", parse_closure_evidence,
                        mutated(data, lambda r: r.update(resources=[])))
        self.assertCode("candidate_resolution_invalid", ClosureResolution, subject=PROOF,
                        invocation_id=INVOCATION, resources=())


class ImmutableInventoryTests(ClosureCase):
    """A constructed closure owns its inventory; a caller's list can never change its digest."""

    def test_caller_owned_list_is_refused_and_its_tuple_copy_stays_fixed(self):
        original = self.proof_closure()
        caller_list = list(original.resources)
        self.assertCode("candidate_resolution_invalid", ClosureResolution, subject=PROOF,
                        invocation_id=INVOCATION, resources=caller_list)
        value = ClosureResolution(subject=PROOF, invocation_id=INVOCATION, resources=tuple(caller_list))
        data = encode_closure_evidence(value)
        self.assertEqual(value, original)
        caller_list.append(ResourceResolution(resource_id="zz-later", disposition=WAITED, fact_sha256=fx.FACT))
        self.assertEqual(value.resources, original.resources)
        self.assertEqual(encode_closure_evidence(value), data)
        self.assertEqual(value.evidence_sha256, sha(data))
        self.assertEqual(value.evidence_sha256, original.evidence_sha256)


class BoundTests(ClosureCase):
    """The reader and the encoder refuse exactly the same oversized records."""

    def at_byte_bound(self) -> ClosureResolution:
        """Return a maximal-item closure whose canonical bytes are exactly the record bound.

        Each identity character adds one canonical byte, so widening ``missing``
        identities of a narrower record by one character lands on the bound.
        """
        narrow = ClosureResolution(subject=PROOF, invocation_id="i" * 128,
                                   resources=inventory(MAX_LIST_ITEMS, width=127))
        missing = MAX_RECORD_BYTES - len(encode_closure_evidence(narrow))
        self.assertTrue(0 < missing <= MAX_LIST_ITEMS, missing)
        resources = tuple(ResourceResolution(resource_id=item.resource_id + "x", disposition=item.disposition,
                                             fact_sha256=fx.FACT) if index < missing else item
                          for index, item in enumerate(narrow.resources))
        return ClosureResolution(subject=PROOF, invocation_id=narrow.invocation_id, resources=resources)

    def test_record_at_the_byte_bound_round_trips_and_one_more_byte_is_refused_by_both(self):
        value = self.at_byte_bound()
        data = encode_closure_evidence(value)
        self.assertEqual(len(data), MAX_RECORD_BYTES)
        self.assertEqual(parse_closure_evidence(data), value)
        last = value.resources[-1]
        widened = ResourceResolution(resource_id=last.resource_id + "x", disposition=last.disposition,
                                     fact_sha256=fx.FACT)
        oversized = ClosureResolution(subject=PROOF, invocation_id=value.invocation_id,
                                      resources=value.resources[:-1] + (widened,))
        self.assertCode("candidate_record_too_large", encode_closure_evidence, oversized)
        self.assertCode("candidate_record_too_large", lambda: oversized.evidence_sha256)
        raw = mutated(data, lambda r: r["resources"][-1].update(resource_id=widened.resource_id))
        self.assertEqual(len(raw), MAX_RECORD_BYTES + 1)
        self.assertCode("candidate_record_too_large", parse_closure_evidence, raw)

    def test_item_bound_is_shared_by_the_value_and_the_reader(self):
        bounded = ClosureResolution(subject=PROOF, invocation_id=INVOCATION,
                                    resources=inventory(MAX_LIST_ITEMS, width=6))
        data = encode_closure_evidence(bounded)
        self.assertEqual(parse_closure_evidence(data), bounded)
        self.assertCode("candidate_resolution_invalid", ClosureResolution, subject=PROOF,
                        invocation_id=INVOCATION, resources=inventory(MAX_LIST_ITEMS + 1, width=6))
        extra = {"resource_id": "r99999", "disposition": "waited", "fact_sha256": fx.FACT}
        raw = mutated(data, lambda r: r["resources"].append(extra))
        self.assertLess(len(raw), MAX_RECORD_BYTES)
        self.assertCode("candidate_field_invalid", parse_closure_evidence, raw)


if __name__ == "__main__":
    unittest.main()
