package task

import (
	"fmt"
)

// CompareRequests compares two TaskRecord requests for semantic equality,
// including canonical cwd, provider, mode, requested config, budget, brief digest/length,
// and prior session, while ignoring incidental fields.
func CompareRequests(a, b *TaskRecord) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Provider != b.Provider ||
		a.Mode != b.Mode ||
		a.CanonicalCwd != b.CanonicalCwd ||
		a.BudgetNanos != b.BudgetNanos ||
		a.BriefSHA256 != b.BriefSHA256 ||
		a.BriefLength != b.BriefLength {
		return false
	}
	if a.RequestedConfig != b.RequestedConfig {
		return false
	}
	if (a.PriorSession == nil) != (b.PriorSession == nil) {
		return false
	}
	if a.PriorSession != nil {
		if *a.PriorSession != *b.PriorSession {
			return false
		}
	}
	return true
}

// CompareOutcomes compares two OutcomeRecord values for semantic equality,
// covering task/spec/meta identity, verdict, sealed evidence digest, predicate reference,
// and payload descriptor.
func CompareOutcomes(a, b *OutcomeRecord) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.SchemaVersion != b.SchemaVersion ||
		a.RootID != b.RootID ||
		a.TaskID != b.TaskID ||
		a.SpecSHA256 != b.SpecSHA256 ||
		a.MetaSHA256 != b.MetaSHA256 ||
		a.Verdict != b.Verdict ||
		a.EvidenceSHA256 != b.EvidenceSHA256 {
		return false
	}
	if !a.Predicate.Equal(b.Predicate) {
		return false
	}
	if a.Payload != b.Payload {
		return false
	}
	return true
}

// ReducePublication determines publication state according to the delegation invariants.
func ReducePublication(outcome *OutcomeRecord, payloadExists bool, payloadLength int64, payloadSHA256 string, seal *ProviderExitRecord) (Publication, error) {
	return ReducePublicationWithIdentity(outcome, payloadExists, payloadLength, payloadSHA256, seal, "", "", "", "")
}

func validateOutcomeIdentity(outcome *OutcomeRecord, expectedRootID, expectedTaskID, expectedSpecSHA256, expectedMetaSHA256 string) error {
	if expectedRootID != "" && outcome.RootID != expectedRootID {
		return fmt.Errorf("%w: root ID mismatch in outcome: got %s, expected %s", ErrIdentityMismatch, outcome.RootID, expectedRootID)
	}
	if expectedTaskID != "" && outcome.TaskID != expectedTaskID {
		return fmt.Errorf("%w: task ID mismatch in outcome: got %s, expected %s", ErrIdentityMismatch, outcome.TaskID, expectedTaskID)
	}
	if expectedSpecSHA256 != "" && outcome.SpecSHA256 != expectedSpecSHA256 {
		return fmt.Errorf("%w: spec digest mismatch in outcome", ErrIdentityMismatch)
	}
	if expectedMetaSHA256 != "" && outcome.MetaSHA256 != expectedMetaSHA256 {
		return fmt.Errorf("%w: meta digest mismatch in outcome", ErrIdentityMismatch)
	}
	return nil
}

// ReducePublicationWithIdentity determines publication state and validates expected terminal identity.
func ReducePublicationWithIdentity(outcome *OutcomeRecord, payloadExists bool, payloadLength int64, payloadSHA256 string, seal *ProviderExitRecord, expectedRootID, expectedTaskID, expectedSpecSHA256, expectedMetaSHA256 string) (Publication, error) {
	if outcome != nil {
		if err := ValidateOutcomeRecord(outcome); err != nil {
			return PublicationUnknown, err
		}
		if idErr := validateOutcomeIdentity(outcome, expectedRootID, expectedTaskID, expectedSpecSHA256, expectedMetaSHA256); idErr != nil {
			return PublicationUnknown, idErr
		}
		if !payloadExists || outcome.Payload.Length != payloadLength || outcome.Payload.SHA256 != payloadSHA256 {
			return PublicationUnknown, ErrInvariantFault
		}
		switch outcome.Verdict {
		case VerdictCommitted:
			return PublicationCommitted, nil
		case VerdictRejected:
			return PublicationRejected, nil
		default:
			return PublicationUnknown, ErrInvalidEnum
		}
	}

	if seal != nil {
		if err := ValidateProviderExitRecord(seal); err != nil {
			return PublicationUnknown, err
		}
		return PublicationPending, nil
	}
	if payloadExists {
		return PublicationPending, nil
	}
	return PublicationUnknown, nil
}

// EvaluateFixturePredicate is a byte-slice convenience for bounded unit fixtures.
// Production collectors use EvaluateRegisteredPredicate and stream raw files.
func EvaluateFixturePredicate(seal *ProviderExitRecord, rawStdoutContent []byte) (verdict string, payloadBasename string, payloadContent []byte, err error) {
	decision, err := EvaluateRegisteredPredicate(FixturePredicateRef(), seal)
	if err != nil {
		return "", "", nil, err
	}
	entry := seal.RawManifest[1]
	if entry.Size != int64(len(rawStdoutContent)) || entry.SHA256 != ComputeSHA256(rawStdoutContent) {
		return "", "", nil, ErrEvidenceFault
	}
	if decision.RawPath != "" {
		return decision.Verdict, decision.PayloadBasename, rawStdoutContent, nil
	}
	return decision.Verdict, decision.PayloadBasename, decision.PayloadContent, nil
}
