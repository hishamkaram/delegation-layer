//go:build linux

package lifetime

import "testing"

func exerciseSelectedNativeBackend(t *testing.T, plan Plan, receipt *nativeBackendProofReceipt) nativeBackendObservation {
	t.Helper()
	if !nativeProofExpected(plan) {
		t.Fatalf("Linux Prepare selected unsupported backend %s/%s", plan.Kind(), plan.Assurance())
	}
	switch plan.Kind() {
	case KindCgroupV2:
		return exerciseLinuxNativeCgroupContainment(t, plan, receipt)
	case KindProcessGroup:
		return exerciseNativeProcessGroup(t, plan, "Linux private session with owner-bound process termination", receipt)
	default:
		t.Fatalf("Linux Prepare selected unknown backend %q", plan.Kind())
		return nativeBackendObservation{}
	}
}
