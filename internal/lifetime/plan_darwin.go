//go:build darwin

package lifetime

import "context"

func preparePlatformPlan(rootID, ownerKind, taskID string) (Plan, error) {
	return processGroupPlan(rootID, ownerKind, taskID), nil
}

func createPreparedPlatformScope(plan Plan) (*Scope, error) {
	ref, err := NewRef(plan.kind, plan.assurance, plan.identifier)
	if err != nil {
		return nil, err
	}
	return openProcessGroupScope(context.Background(), ref)
}

func openPreparedPlatformScope(Plan) (*Scope, error) {
	return nil, ErrScopeAbsent
}

func openRegisteredPlatformScope(ctx context.Context, plan Plan, ref Ref) (*Scope, error) {
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
	return openProcessGroupScope(ctx, ref)
}

func validatePlatformPlan(Plan) error {
	return ErrUnsupported
}
