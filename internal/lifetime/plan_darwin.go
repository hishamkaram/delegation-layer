//go:build darwin

package lifetime

func preparePlatformPlan(rootID, ownerKind, taskID string) (Plan, error) {
	return processGroupPlan(rootID, ownerKind, taskID), nil
}

func createPreparedPlatformScope(plan Plan) (*Scope, error) {
	ref, err := NewRef(plan.kind, plan.assurance, plan.identifier)
	if err != nil {
		return nil, err
	}
	return openProcessGroupScope(ref)
}

func openPreparedPlatformScope(Plan) (*Scope, error) {
	return nil, ErrScopeAbsent
}

func openRegisteredPlatformScope(plan Plan, ref Ref) (*Scope, error) {
	if ref.kind != plan.kind || ref.assurance != plan.assurance {
		return nil, ErrInvalidRef
	}
	identifier := ref.identifier
	if validateProcessGroupPlanIdentifier(identifier) != nil {
		var err error
		identifier, _, err = decodeDarwinProcessGroupPinReference(identifier)
		if err != nil {
			return nil, err
		}
	}
	if identifier != plan.identifier {
		return nil, ErrInvalidRef
	}
	return openProcessGroupScope(ref)
}

func validatePlatformPlan(Plan) error {
	return ErrUnsupported
}
