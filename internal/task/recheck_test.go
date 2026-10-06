package task

import (
	"errors"
	"strings"
	"testing"
)

func TestOutputDiagnosticValidationIsBoundedAndControlFree(t *testing.T) {
	valid := OutputDiagnostic{Code: "missing_field", Message: "required field was not observed", Field: "result.text", Event: 3, Offset: 12}
	if err := ValidateOutputDiagnostic(valid); err != nil {
		t.Fatal(err)
	}
	for _, diagnostic := range []OutputDiagnostic{
		{Code: "", Message: "message"},
		{Code: "code", Message: "\nsecret"},
		{Code: "code", Message: strings.Repeat("x", MaxDiagnosticMessageBytes+1)},
		{Code: "code", Message: "message", Field: " "},
		{Code: "code", Message: "message", Offset: -1},
	} {
		if err := ValidateOutputDiagnostic(diagnostic); !errors.Is(err, ErrInvalidDiagnostic) {
			t.Fatalf("diagnostic %+v accepted with %v", diagnostic, err)
		}
	}
	tooMany := make([]OutputDiagnostic, MaxOutputDiagnostics+1)
	for i := range tooMany {
		tooMany[i] = valid
	}
	if err := ValidateOutputDiagnostics(tooMany); !errors.Is(err, ErrInvalidDiagnostic) {
		t.Fatalf("diagnostic list bound was not enforced: %v", err)
	}
}

func TestRecheckAssessmentIDAndReportValidation(t *testing.T) {
	source := FixturePredicateRef()
	target := PredicateRef{Adapter: "codex:exec", Mode: "read-only", Version: "current", SHA256: ComputeSHA256([]byte("target"))}
	outcomeHash := ComputeSHA256([]byte("outcome"))
	sealHash := ComputeSHA256([]byte("seal"))
	id, err := RecheckAssessmentID(outcomeHash, sealHash, target)
	if err != nil {
		t.Fatal(err)
	}
	report := RecheckReport{
		SchemaVersion:        RecheckSchemaVersion,
		AssessmentID:         id,
		SourceOutcomeSHA256:  outcomeHash,
		SourceSealSHA256:     sealHash,
		SourcePredicate:      source,
		SourceEvidenceSHA256: ComputeSHA256([]byte("evidence")),
		TargetPredicate:      target,
		Status:               RecheckStatusRecovered,
		Result:               &PayloadDescriptor{Basename: "result.txt", Length: 4, SHA256: ComputeSHA256([]byte("done"))},
	}
	if err = ValidateRecheckReport(&report); err != nil {
		t.Fatal(err)
	}
	report.TargetPredicate = source
	if err = ValidateRecheckReport(&report); !errors.Is(err, ErrInvalidRecheck) {
		t.Fatalf("source target loop accepted: %v", err)
	}
}

func TestHistoricalOutcomeWithoutDiagnosticsRemainsValid(t *testing.T) {
	record := OutcomeRecord{
		SchemaVersion:  SchemaVersion,
		RootID:         strings.Repeat("a", 32),
		TaskID:         strings.Repeat("b", 32),
		SpecSHA256:     ComputeSHA256([]byte("spec")),
		MetaSHA256:     ComputeSHA256([]byte("meta")),
		Verdict:        VerdictRejected,
		EvidenceSHA256: ComputeSHA256([]byte("evidence")),
		Predicate:      FixturePredicateRef(),
		Payload:        PayloadDescriptor{Basename: "publish.reject", Length: 1, SHA256: ComputeSHA256([]byte("x"))},
	}
	if err := ValidateOutcomeRecord(&record); err != nil {
		t.Fatal(err)
	}
}
