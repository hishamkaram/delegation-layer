package task

import "time"

// QuotaAdvice is bounded provider reset advice, not an admission policy.
// Only validated immutable rejection evidence may supply it.
type QuotaAdvice struct {
	NotBefore string `json:"not_before"`
}

func ValidateQuotaAdvice(advice *QuotaAdvice) error {
	if advice == nil {
		return nil
	}
	instant, err := time.Parse(time.RFC3339Nano, advice.NotBefore)
	if err != nil || instant.Year() < 1970 || instant.Year() > 9999 || instant.UTC().Format(time.RFC3339Nano) != advice.NotBefore {
		return ErrIdentityMismatch
	}
	return nil
}

func CloneQuotaAdvice(advice *QuotaAdvice) *QuotaAdvice {
	if advice == nil {
		return nil
	}
	copy := *advice
	return &copy
}

func validateQuotaEvidence(advice *QuotaAdvice, verdict string, diagnostics []OutputDiagnostic) error {
	if advice == nil {
		return nil
	}
	if err := ValidateQuotaAdvice(advice); err != nil {
		return err
	}
	if verdict != VerdictRejected {
		return ErrIdentityMismatch
	}
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == "provider_quota_exhausted" {
			return nil
		}
	}
	return ErrIdentityMismatch
}
