package task

// SelectEnvironment applies a saved scope without changing the caller's input.
// Unrecorded empty scopes retain the base; recorded empty scopes select an
// explicit empty environment. Provider record eligibility is a separate policy.
// Every successful selection is nonnil, including an empty base scope.
func SelectEnvironment(base, values []string, recorded bool) ([]string, error) {
	if len(values) == 0 && !recorded {
		values = base
	} else if err := ValidateEnvironment(values); err != nil {
		return nil, err
	}
	selected := make([]string, len(values))
	copy(selected, values)
	return selected, nil
}
