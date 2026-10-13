//go:build darwin

package lifetime

import "testing"

func exerciseSelectedNativeBackend(t *testing.T, plan Plan, receipt *nativeBackendProofReceipt) nativeBackendObservation {
	t.Helper()
	if plan.Kind() != KindProcessGroup || plan.Assurance() != AssuranceCooperative {
		t.Fatalf("Darwin Prepare selected unsupported backend %s/%s", plan.Kind(), plan.Assurance())
	}
	return exerciseNativeProcessGroup(t, plan, "Darwin private session with process identity-checked termination", receipt)
}
