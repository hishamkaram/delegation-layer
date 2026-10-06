package task

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// OutputDiagnostic values are control metadata, not a second provider
	// output channel. Keep every component bounded so a malformed provider
	// record cannot turn status or collect into an unbounded response.
	MaxOutputDiagnostics      = 32
	MaxDiagnosticCodeBytes    = 64
	MaxDiagnosticMessageBytes = 512
	MaxDiagnosticFieldBytes   = 128
	RecheckSchemaVersion      = 1
	RecheckStatusRecovered    = "recovered"
	RecheckStatusRejected     = "rejected"
)

var (
	ErrInvalidDiagnostic  = errors.New("invalid output diagnostic")
	ErrInvalidRecheck     = errors.New("invalid recheck report")
	ErrRecheckNotRejected = errors.New("recheck requires a rejected terminal task")
	ErrRecheckConflict    = errors.New("recheck assessment conflict")
)

// OutputDiagnostic is a bounded, adapter-sanitized explanation of a rejected
// interpretation. It deliberately contains no provider-owned value or raw
// output fragment. Event and Offset identify a location in sealed evidence,
// when the adapter can provide one, without copying that evidence.
type OutputDiagnostic struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
	Event   uint64 `json:"event,omitempty"`
	Offset  int64  `json:"offset,omitempty"`
}

func ValidateOutputDiagnostic(diagnostic OutputDiagnostic) error {
	if !validDiagnosticText(diagnostic.Code, MaxDiagnosticCodeBytes, true) {
		return fmt.Errorf("%w: code", ErrInvalidDiagnostic)
	}
	if !validDiagnosticText(diagnostic.Message, MaxDiagnosticMessageBytes, true) {
		return fmt.Errorf("%w: message", ErrInvalidDiagnostic)
	}
	if diagnostic.Field != "" && !validDiagnosticText(diagnostic.Field, MaxDiagnosticFieldBytes, true) {
		return fmt.Errorf("%w: field", ErrInvalidDiagnostic)
	}
	if diagnostic.Offset < 0 {
		return fmt.Errorf("%w: negative offset", ErrInvalidDiagnostic)
	}
	return nil
}

func ValidateOutputDiagnostics(diagnostics []OutputDiagnostic) error {
	if len(diagnostics) > MaxOutputDiagnostics {
		return fmt.Errorf("%w: too many diagnostics", ErrInvalidDiagnostic)
	}
	for index, diagnostic := range diagnostics {
		if err := ValidateOutputDiagnostic(diagnostic); err != nil {
			return fmt.Errorf("%w at index %d: %w", ErrInvalidDiagnostic, index, err)
		}
	}
	return nil
}

func CloneOutputDiagnostics(diagnostics []OutputDiagnostic) []OutputDiagnostic {
	if diagnostics == nil {
		return nil
	}
	return append([]OutputDiagnostic(nil), diagnostics...)
}

func validDiagnosticText(value string, limit int, requireNonblank bool) bool {
	if len(value) == 0 || len(value) > limit || !utf8.ValidString(value) {
		return !requireNonblank && value == ""
	}
	if requireNonblank && strings.TrimSpace(value) == "" {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// RecheckReport is the durable derived assessment. It never replaces or
// mutates the source task's outcome, seal, raw manifest, payload, identity, or
// session records. Result is present only for a recovered answer.
type RecheckReport struct {
	SchemaVersion        int                `json:"schema_version"`
	AssessmentID         string             `json:"assessment_id"`
	SourceOutcomeSHA256  string             `json:"source_outcome_sha256"`
	SourceSealSHA256     string             `json:"source_seal_sha256"`
	SourcePredicate      PredicateRef       `json:"source_predicate"`
	SourceEvidenceSHA256 string             `json:"source_evidence_sha256"`
	TargetPredicate      PredicateRef       `json:"target_predicate"`
	Status               string             `json:"status"`
	Diagnostics          []OutputDiagnostic `json:"diagnostics,omitempty"`
	Result               *PayloadDescriptor `json:"result,omitempty"`
}

// RecheckIdentity is the exact immutable input to assessment ID derivation.
type RecheckIdentity struct {
	SourceOutcomeSHA256 string       `json:"source_outcome_sha256"`
	SourceSealSHA256    string       `json:"source_seal_sha256"`
	TargetPredicate     PredicateRef `json:"target_predicate"`
}

func RecheckAssessmentID(sourceOutcomeSHA256, sourceSealSHA256 string, target PredicateRef) (string, error) {
	identity := RecheckIdentity{SourceOutcomeSHA256: sourceOutcomeSHA256, SourceSealSHA256: sourceSealSHA256, TargetPredicate: target}
	if err := validateRecheckIdentity(identity); err != nil {
		return "", err
	}
	data, err := MarshalCanonical(identity)
	if err != nil {
		return "", err
	}
	return ComputeSHA256(data), nil
}

func ValidatePayloadDescriptor(descriptor PayloadDescriptor) error {
	if descriptor.Basename != "result.txt" && descriptor.Basename != "publish.reject" {
		return fmt.Errorf("%w: invalid payload basename %q", ErrInvariantFault, descriptor.Basename)
	}
	if descriptor.Length <= 0 {
		return fmt.Errorf("%w: payload must be nonempty", ErrInvariantFault)
	}
	if err := ValidateSHA256(descriptor.SHA256); err != nil {
		return fmt.Errorf("payload sha256: %w", err)
	}
	return nil
}

func ValidateRecheckReport(report *RecheckReport) error {
	if report == nil {
		return ErrInvalidRecheck
	}
	if report.SchemaVersion != RecheckSchemaVersion {
		return fmt.Errorf("%w: schema version %d", ErrUnsupportedSchema, report.SchemaVersion)
	}
	if err := ValidateSHA256(report.SourceOutcomeSHA256); err != nil {
		return fmt.Errorf("%w: source outcome digest: %w", ErrInvalidRecheck, err)
	}
	if err := ValidateSHA256(report.SourceSealSHA256); err != nil {
		return fmt.Errorf("%w: source seal digest: %w", ErrInvalidRecheck, err)
	}
	if err := ValidatePredicateRef(report.SourcePredicate); err != nil {
		return fmt.Errorf("%w: source predicate: %w", ErrInvalidRecheck, err)
	}
	if err := ValidateSHA256(report.SourceEvidenceSHA256); err != nil {
		return fmt.Errorf("%w: source evidence digest: %w", ErrInvalidRecheck, err)
	}
	if err := ValidatePredicateRef(report.TargetPredicate); err != nil {
		return fmt.Errorf("%w: target predicate: %w", ErrInvalidRecheck, err)
	}
	if report.SourcePredicate.Equal(report.TargetPredicate) {
		return fmt.Errorf("%w: target equals source predicate", ErrInvalidRecheck)
	}
	if report.Status != RecheckStatusRecovered && report.Status != RecheckStatusRejected {
		return fmt.Errorf("%w: invalid status %q", ErrInvalidRecheck, report.Status)
	}
	if err := ValidateOutputDiagnostics(report.Diagnostics); err != nil {
		return fmt.Errorf("%w: diagnostics: %w", ErrInvalidRecheck, err)
	}
	identityID, err := RecheckAssessmentID(report.SourceOutcomeSHA256, report.SourceSealSHA256, report.TargetPredicate)
	if err != nil {
		return err
	}
	if report.AssessmentID != identityID {
		return fmt.Errorf("%w: assessment ID mismatch", ErrInvalidRecheck)
	}
	return validateRecheckResult(report)
}

func validateRecheckResult(report *RecheckReport) error {
	if report.Status == RecheckStatusRecovered {
		if report.Result == nil || report.Result.Basename != "result.txt" {
			return fmt.Errorf("%w: recovered report requires result", ErrInvalidRecheck)
		}
		if err := ValidatePayloadDescriptor(*report.Result); err != nil {
			return fmt.Errorf("%w: result: %w", ErrInvalidRecheck, err)
		}
	} else if report.Result != nil {
		return fmt.Errorf("%w: rejected report cannot contain result", ErrInvalidRecheck)
	}
	return nil
}

func validateRecheckIdentity(identity RecheckIdentity) error {
	if err := ValidateSHA256(identity.SourceOutcomeSHA256); err != nil {
		return fmt.Errorf("invalid recheck identity source outcome: %w", err)
	}
	if err := ValidateSHA256(identity.SourceSealSHA256); err != nil {
		return fmt.Errorf("invalid recheck identity source seal: %w", err)
	}
	if err := ValidatePredicateRef(identity.TargetPredicate); err != nil {
		return fmt.Errorf("invalid recheck identity target: %w", err)
	}
	return nil
}
