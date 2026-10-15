//go:build !linux && !darwin

package lifetime

import "context"

func preparePlatformPlan(string, string, string) (Plan, error) {
	return Plan{}, ErrUnsupported
}

func createPreparedPlatformScope(Plan) (*Scope, error) {
	return nil, ErrUnsupported
}

func openPreparedPlatformScope(Plan) (*Scope, error) {
	return nil, ErrUnsupported
}

func openRegisteredPlatformScope(context.Context, Plan, Ref) (*Scope, error) {
	return nil, ErrUnsupported
}

func validatePlatformPlan(Plan) error {
	return ErrUnsupported
}
