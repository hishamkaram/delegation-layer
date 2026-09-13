package task

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestTaskIDValidation(t *testing.T) {
	validID, err := NewTaskID()
	if err != nil {
		t.Fatalf("unexpected error generating task ID: %v", err)
	}
	if err := ValidateTaskID(validID); err != nil {
		t.Errorf("expected valid ID %s, got error: %v", validID, err)
	}

	invalidCases := []string{
		"",
		"abc",
		"1234567890abcdef1234567890abcde",   // 31 chars
		"1234567890abcdef1234567890abcdef1", // 33 chars
		"1234567890ABCDEF1234567890ABCDEF",  // uppercase
		"../1234567890abcdef1234567890ab",   // path traversal
		"1234567890abcdef1234567890abcd-g",  // non-hex
	}
	for _, tc := range invalidCases {
		if err := ValidateTaskID(tc); !errors.Is(err, ErrInvalidTaskID) {
			t.Errorf("expected ErrInvalidTaskID for %q, got: %v", tc, err)
		}
	}
}

func TestRootIDValidation(t *testing.T) {
	validID, err := NewRootID()
	if err != nil {
		t.Fatalf("unexpected error generating root ID: %v", err)
	}
	if err := ValidateRootID(validID); err != nil {
		t.Errorf("expected valid root ID %s, got error: %v", validID, err)
	}
	if err := ValidateRootID("invalid"); !errors.Is(err, ErrInvalidRootID) {
		t.Errorf("expected ErrInvalidRootID, got: %v", err)
	}
}

func TestStrictJSONDecoding(t *testing.T) {
	validJSON := []byte(`{"schema_version":1,"root_id":"0123456789abcdef0123456789abcdef","created_at":"2026-09-13T00:00:00Z"}`)
	var rec RootRecord
	if err := DecodeStrict(validJSON, &rec); err != nil {
		t.Fatalf("unexpected error decoding valid JSON: %v", err)
	}
	if rec.SchemaVersion != 1 {
		t.Errorf("expected SchemaVersion 1, got %d", rec.SchemaVersion)
	}

	// 1. Duplicate keys
	dupJSON := []byte(`{"schema_version":1,"root_id":"a","root_id":"b"}`)
	if err := DecodeStrict(dupJSON, &rec); !errors.Is(err, ErrDuplicateKey) {
		t.Errorf("expected ErrDuplicateKey, got: %v", err)
	}

	// 2. Trailing JSON
	trailingJSON := []byte(`{"schema_version":1,"root_id":"a","created_at":"now"} trailing`)
	if err := DecodeStrict(trailingJSON, &rec); !errors.Is(err, ErrTrailingJSON) {
		t.Errorf("expected ErrTrailingJSON, got: %v", err)
	}

	// 3. Unknown field
	unknownFieldJSON := []byte(`{"schema_version":1,"root_id":"a","created_at":"now","unknown_field":true}`)
	if err := DecodeStrict(unknownFieldJSON, &rec); err == nil {
		t.Errorf("expected error for unknown field, got nil")
	}

	// 4. Oversized control record (> 1 MiB)
	hugeData := bytes.Repeat([]byte("a"), MaxControlRecordSize+1)
	hugeJSON := append([]byte(`{"data":"`), append(hugeData, []byte(`"}`)...)...)
	if err := DecodeStrict(hugeJSON, &rec); !errors.Is(err, ErrControlRecordTooBig) {
		t.Errorf("expected ErrControlRecordTooBig, got: %v", err)
	}
}

func TestCanonicalJSONMarshal(t *testing.T) {
	rec := RootRecord{
		SchemaVersion: 1,
		RootID:        "0123456789abcdef0123456789abcdef",
		CreatedAt:     "2026-09-13T00:00:00Z",
	}
	data, err := MarshalCanonical(rec)
	if err != nil {
		t.Fatalf("unexpected error marshaling canonical JSON: %v", err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Errorf("expected newline suffix in canonical JSON")
	}
	digest := ComputeSHA256(data)
	if len(digest) != 64 {
		t.Errorf("expected 64-char sha256 digest, got %d", len(digest))
	}
}

func TestCompareRequests(t *testing.T) {
	reqA := &TaskRecord{
		SchemaVersion: 1,
		RootID:        "root1",
		TaskID:        "task1",
		Provider:      "antigravity:print",
		Mode:          "workspace-write",
		CanonicalCwd:  "/workspace",
		BudgetNanos:   60000000000,
		BriefSHA256:   "hash1",
		BriefLength:   100,
		RequestedConfig: TaskConfig{
			Model: "model1",
		},
	}
	reqB := &TaskRecord{
		SchemaVersion: 1,
		RootID:        "root1",
		TaskID:        "task1",
		Provider:      "antigravity:print",
		Mode:          "workspace-write",
		CanonicalCwd:  "/workspace",
		BudgetNanos:   60000000000,
		BriefSHA256:   "hash1",
		BriefLength:   100,
		RequestedConfig: TaskConfig{
			Model: "model1",
		},
	}
	if !CompareRequests(reqA, reqB) {
		t.Errorf("expected identical requests to compare equal")
	}

	reqDifferentBudget := *reqB
	reqDifferentBudget.BudgetNanos = 120000000000
	if CompareRequests(reqA, &reqDifferentBudget) {
		t.Errorf("expected different budget to compare unequal")
	}

	reqDifferentCwd := *reqB
	reqDifferentCwd.CanonicalCwd = "/other"
	if CompareRequests(reqA, &reqDifferentCwd) {
		t.Errorf("expected different cwd to compare unequal")
	}
}

func TestCompareOutcomes(t *testing.T) {
	outA := &OutcomeRecord{
		SchemaVersion:  1,
		RootID:         "root1",
		TaskID:         "task1",
		SpecSHA256:     "spec1",
		MetaSHA256:     "meta1",
		Verdict:        VerdictCommitted,
		EvidenceSHA256: "ev1",
		Predicate: PredicateRef{
			Adapter: "antigravity:print",
			Mode:    "workspace-write",
			Version: "1.0",
			SHA256:  "pred1",
		},
		Payload: PayloadDescriptor{
			Basename: "result.txt",
			Length:   42,
			SHA256:   "payloadhash",
		},
	}
	outB := *outA
	if !CompareOutcomes(outA, &outB) {
		t.Errorf("expected identical outcomes to compare equal")
	}

	outB.Verdict = VerdictRejected
	if CompareOutcomes(outA, &outB) {
		t.Errorf("expected different verdicts to compare unequal")
	}
}

func TestReducePublication(t *testing.T) {
	validHex32 := "0123456789abcdef0123456789abcdef"
	validHex64 := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	outcome := &OutcomeRecord{
		SchemaVersion:  1,
		RootID:         validHex32,
		TaskID:         validHex32,
		SpecSHA256:     validHex64,
		MetaSHA256:     validHex64,
		Verdict:        VerdictCommitted,
		EvidenceSHA256: validHex64,
		Predicate:      FixturePredicateRef(),
		Payload: PayloadDescriptor{
			Basename: "result.txt",
			Length:   10,
			SHA256:   validHex64,
		},
	}

	// 1. Valid outcome + matching payload
	pub, err := ReducePublication(outcome, true, 10, validHex64, nil)
	if err != nil || pub != PublicationCommitted {
		t.Errorf("expected PublicationCommitted, got %v, err=%v", pub, err)
	}

	// 2. Valid outcome + mismatched payload sha
	pub, err = ReducePublication(outcome, true, 10, "wronghash", nil)
	if !errors.Is(err, ErrInvariantFault) || pub != PublicationUnknown {
		t.Errorf("expected ErrInvariantFault for mismatched payload, got %v, err=%v", pub, err)
	}

	// 3. Valid outcome + missing payload
	pub, err = ReducePublication(outcome, false, 0, "", nil)
	if !errors.Is(err, ErrInvariantFault) || pub != PublicationUnknown {
		t.Errorf("expected ErrInvariantFault for missing payload, got %v, err=%v", pub, err)
	}

	// 4. No outcome + valid seal
	seal := fixtureSeal(t, []byte("ok"))
	pub, err = ReducePublication(nil, false, 0, "", seal)
	if err != nil || pub != PublicationPending {
		t.Errorf("expected PublicationPending for seal, got %v, err=%v", pub, err)
	}

	// 5. No outcome + payload alone
	pub, err = ReducePublication(nil, true, 10, validHex64, nil)
	if err != nil || pub != PublicationPending {
		t.Errorf("expected PublicationPending for payload alone, got %v, err=%v", pub, err)
	}

	// 6. No outcome, no seal, no payload
	pub, err = ReducePublication(nil, false, 0, "", nil)
	if err != nil || pub != PublicationUnknown {
		t.Errorf("expected PublicationUnknown, got %v, err=%v", pub, err)
	}
}

func TestEnumsExhaustive(t *testing.T) {
	// Liveness
	if LivenessUndetermined.String() != "undetermined" {
		t.Errorf("unexpected string: %s", LivenessUndetermined)
	}
	if LivenessRunning.String() != "running" {
		t.Errorf("unexpected string: %s", LivenessRunning)
	}
	if LivenessEnded.String() != "ended" {
		t.Errorf("unexpected string: %s", LivenessEnded)
	}
	if Liveness(99).String() != "unknown" {
		t.Errorf("unexpected string: %s", Liveness(99))
	}

	// Admission
	if AdmissionUnknown.String() != "unknown" {
		t.Errorf("unexpected string: %s", AdmissionUnknown)
	}
	if AdmissionNotAdmitted.String() != "not-admitted" {
		t.Errorf("unexpected string: %s", AdmissionNotAdmitted)
	}
	if AdmissionAdmitted.String() != "admitted" {
		t.Errorf("unexpected string: %s", AdmissionAdmitted)
	}
	if Admission(99).String() != "unknown" {
		t.Errorf("unexpected string: %s", Admission(99))
	}

	// Publication
	if PublicationUnknown.String() != "unknown" {
		t.Errorf("unexpected string: %s", PublicationUnknown)
	}
	if PublicationPending.String() != "pending" {
		t.Errorf("unexpected string: %s", PublicationPending)
	}
	if PublicationCommitted.String() != "committed" {
		t.Errorf("unexpected string: %s", PublicationCommitted)
	}
	if PublicationRejected.String() != "rejected" {
		t.Errorf("unexpected string: %s", PublicationRejected)
	}
	if Publication(99).String() != "unknown" {
		t.Errorf("unexpected string: %s", Publication(99))
	}
}

func TestValidateJSONStructure_Edges(t *testing.T) {
	// 1. null
	if err := ValidateJSONStructure([]byte("null")); err == nil {
		t.Errorf("expected error for 'null', got nil")
	}

	// 2. empty
	if err := ValidateJSONStructure([]byte("   ")); err == nil {
		t.Errorf("expected error for empty data, got nil")
	}

	// 3. case-folded duplicate keys
	aliased := []byte(`{"schema_version":1,"Schema_Version":2}`)
	if err := ValidateJSONStructure(aliased); !errors.Is(err, ErrDuplicateKey) {
		t.Errorf("expected ErrDuplicateKey for case-folded alias, got: %v", err)
	}

	// 4. primitive root
	if err := ValidateJSONStructure([]byte("12345")); err == nil {
		t.Errorf("expected error for primitive number, got nil")
	}
	if err := ValidateJSONStructure([]byte(`"string"`)); err == nil {
		t.Errorf("expected error for primitive string, got nil")
	}
}

func TestTypedValidators(t *testing.T) {
	validHex32 := "0123456789abcdef0123456789abcdef"
	validHex64 := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	// 1. RootRecord
	rootRec := &RootRecord{
		SchemaVersion: 1,
		RootID:        validHex32,
		CreatedAt:     "2026-09-13T00:00:00Z",
	}
	if err := ValidateRootRecord(rootRec); err != nil {
		t.Errorf("valid root record rejected: %v", err)
	}
	badRoot := *rootRec
	badRoot.SchemaVersion = 999
	if err := ValidateRootRecord(&badRoot); !errors.Is(err, ErrUnsupportedSchema) {
		t.Errorf("expected ErrUnsupportedSchema, got: %v", err)
	}

	// 2. TaskRecord
	taskRec := &TaskRecord{
		SchemaVersion:   1,
		RootID:          validHex32,
		TaskID:          validHex32,
		Provider:        "antigravity:print",
		Mode:            "workspace-write",
		CanonicalCwd:    "/fixture/workspace",
		RequestedConfig: TaskConfig{Permission: "workspace-write", Budget: "1µs"},
		BudgetNanos:     1000,
		BriefSHA256:     validHex64,
		BriefLength:     42,
	}
	if err := ValidateTaskRecord(taskRec); err != nil {
		t.Errorf("valid task record rejected: %v", err)
	}
	badTask := *taskRec
	badTask.BudgetNanos = 0
	if err := ValidateTaskRecord(&badTask); err == nil {
		t.Errorf("expected error for zero budget, got nil")
	}

	// 3. ProviderExitRecord & RawManifest
	manifest := []RawManifestEntry{
		{Path: "raw/stderr", Size: 10, SHA256: validHex64},
		{Path: "raw/stdout", Size: 20, SHA256: validHex64},
	}
	mBytes, mErr := MarshalCanonical(manifest)
	if mErr != nil {
		t.Fatal(mErr)
	}
	exitRec := &ProviderExitRecord{
		SchemaVersion:   1,
		RootID:          validHex32,
		TaskID:          validHex32,
		SpecSHA256:      validHex64,
		MetaSHA256:      validHex64,
		InvocationState: InvocationStarted,
		ExitCode:        0,
		Predicate:       FixturePredicateRef(),
		ClosedAt:        "2026-09-13T00:00:00Z",
		RawManifest:     manifest,
		ManifestSHA256:  ComputeSHA256(mBytes),
	}
	if err := ValidateProviderExitRecord(exitRec); err != nil {
		t.Errorf("valid exit record rejected: %v", err)
	}

	// Unsorted manifest
	unsortedManifest := []RawManifestEntry{
		{Path: "raw/stdout", Size: 20, SHA256: validHex64},
		{Path: "raw/stderr", Size: 10, SHA256: validHex64},
	}
	umBytes, umErr := MarshalCanonical(unsortedManifest)
	if umErr != nil {
		t.Fatal(umErr)
	}
	badExit := *exitRec
	badExit.RawManifest = unsortedManifest
	badExit.ManifestSHA256 = ComputeSHA256(umBytes)
	if err := ValidateProviderExitRecord(&badExit); !errors.Is(err, ErrEvidenceFault) {
		t.Errorf("expected ErrEvidenceFault for unsorted manifest, got: %v", err)
	}

	// 4. OutcomeRecord
	outcomeRec := &OutcomeRecord{
		SchemaVersion:  1,
		RootID:         validHex32,
		TaskID:         validHex32,
		SpecSHA256:     validHex64,
		MetaSHA256:     validHex64,
		Verdict:        VerdictCommitted,
		EvidenceSHA256: validHex64,
		Predicate:      FixturePredicateRef(),
		Payload: PayloadDescriptor{
			Basename: "result.txt",
			Length:   10,
			SHA256:   validHex64,
		},
	}
	if err := ValidateOutcomeRecord(outcomeRec); err != nil {
		t.Errorf("valid outcome rejected: %v", err)
	}
	badOutcome := *outcomeRec
	badOutcome.Payload.Basename = "wrong.txt"
	if err := ValidateOutcomeRecord(&badOutcome); !errors.Is(err, ErrInvariantFault) {
		t.Errorf("expected ErrInvariantFault for mismatched basename, got: %v", err)
	}
}
