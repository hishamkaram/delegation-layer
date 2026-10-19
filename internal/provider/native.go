package provider

import (
	"slices"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// NativeProfileRevision identifies profiles that leave configuration to the provider.
const NativeProfileRevision = "native-permissions-v1"

// HasNativeProfile reports whether meta records a provider-owned native
// profile. Records without a policy, and records of any other profile
// revision, keep their historical preparation path.
func HasNativeProfile(meta task.MetaRecord) bool {
	return meta.EffectiveConfig.Policy != nil && meta.EffectiveConfig.Policy.ProfileRevision == NativeProfileRevision
}

// NativeEffectiveConfig records the requested native permission mode and runtime
// identity. It makes no claim to inventory or independently enforce user policy.
func NativeEffectiveConfig(request task.TaskRecord, roots []string, runtimeSHA256, approval string) (task.EffectiveConfig, error) {
	writable := slices.Clone(roots)
	slices.Sort(writable)
	effective := task.EffectiveConfig{
		Containment: request.Mode,
		Approval:    approval,
		Policy: &task.PolicyDetails{
			ProfileRevision: NativeProfileRevision,
			RuntimeSHA256:   runtimeSHA256,
			Workspace:       request.CanonicalCwd,
			WritableRoots:   slices.Compact(writable),
		},
	}
	encoded, err := task.MarshalCanonical(effective)
	if err != nil {
		return task.EffectiveConfig{}, err
	}
	effective.Digest = task.ComputeSHA256(encoded)
	if err = task.ValidateEffectiveConfig(effective); err != nil {
		return task.EffectiveConfig{}, err
	}
	return effective, nil
}
