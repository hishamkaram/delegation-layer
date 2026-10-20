package opencode

import (
	"fmt"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	// acpProfileRevision persists the current ACP launch dialect in
	// Policy.ProfileRevision.
	acpProfileRevision = "opencode-acp-v1"
	// nativeV2ProfileRevision persists the native run dialect that passed the
	// workspace as the process directory. It is reconstructed, never admitted.
	nativeV2ProfileRevision = "opencode-native-v2"
)

// launchDialect identifies the OpenCode command shape a task was admitted
// with. The adapter owns it; an admitted record's profile revision selects it,
// so reconstruction never depends on the installed OpenCode release.
type launchDialect int

const (
	// dialectACP is the current profile: `opencode acp` driven by the
	// adapter's machine protocol on the core-owned stdio transport.
	dialectACP launchDialect = iota + 1
	// dialectNativeV2 is the historical native run profile. The workspace is
	// the process directory and a model variant is part of the --model value.
	dialectNativeV2
	// dialectNativeV1 is the historical native profile, which passed the
	// workspace with --dir and the effort with --variant.
	dialectNativeV1
	// dialectPure is the historical adapter-owned pure-mode profile.
	dialectPure
)

// dialectForRecord selects the launch dialect recorded by an admitted task.
// Every known profile revision maps explicitly; a record without a policy is
// the historical pure-mode profile. An unknown revision is refused rather
// than reinterpreted under another dialect.
func dialectForRecord(meta task.MetaRecord) (launchDialect, error) {
	policy := meta.EffectiveConfig.Policy
	if policy == nil {
		return dialectPure, nil
	}
	switch policy.ProfileRevision {
	case acpProfileRevision:
		return dialectACP, nil
	case nativeV2ProfileRevision:
		return dialectNativeV2, nil
	case commonprovider.NativeProfileRevision:
		return dialectNativeV1, nil
	case ProfileRevision:
		return dialectPure, nil
	default:
		return 0, fmt.Errorf("%w: unknown OpenCode profile revision", ErrUnsupportedProfile)
	}
}

// native reports whether OpenCode owns configuration and permissions.
func (dialect launchDialect) native() bool {
	return dialect == dialectACP || dialect == dialectNativeV2 || dialect == dialectNativeV1
}

// nativeRun reports whether the dialect is a historical native `run` launch,
// whose exact-session successors continue through ACP.
func (dialect launchDialect) nativeRun() bool {
	return dialect == dialectNativeV2 || dialect == dialectNativeV1
}

// runtimeRequirements returns the command and flags used by the dialect's
// launch argv.
func (dialect launchDialect) runtimeRequirements() (commonprovider.RuntimeCapability, error) {
	switch dialect {
	case dialectACP:
		return RuntimeRequirements(), nil
	case dialectNativeV2:
		return nativeV2RuntimeRequirements(), nil
	case dialectNativeV1:
		return nativeV1RuntimeRequirements(), nil
	case dialectPure:
		return legacyRuntimeRequirements(), nil
	default:
		return commonprovider.RuntimeCapability{}, fmt.Errorf("%w: unknown OpenCode launch dialect", ErrUnsupportedProfile)
	}
}

// approval names the native approval behavior recorded with the dialect.
// ACP answers native permission requests itself; the run dialects pass the
// native --auto flag for workspace-write.
func (dialect launchDialect) approval(mode string) (string, error) {
	agent, err := nativeAgent(mode)
	if err != nil {
		return "", err
	}
	switch {
	case dialect == dialectACP && mode == ModeReadOnly:
		return agent + ":" + permissionRejectOnce, nil
	case dialect == dialectACP:
		return agent + ":" + permissionAllowOnce, nil
	case dialect.nativeRun() && mode == ModeWorkspaceWrite:
		return agent + ":auto", nil
	case dialect.nativeRun():
		return agent, nil
	default:
		return "", fmt.Errorf("%w: pure-mode profile has no native approval", ErrUnsupportedProfile)
	}
}

// nativeEffectiveConfig records a native dialect under its profile revision.
// The pure-mode dialect records the adapter-owned policy instead.
func (dialect launchDialect) nativeEffectiveConfig(request task.TaskRecord, roots []string, runtimeSHA256, approval string) (task.EffectiveConfig, error) {
	switch dialect {
	case dialectACP:
		return commonprovider.NativeEffectiveConfigForRevision(request, roots, runtimeSHA256, approval, acpProfileRevision)
	case dialectNativeV2:
		return commonprovider.NativeEffectiveConfigForRevision(request, roots, runtimeSHA256, approval, nativeV2ProfileRevision)
	case dialectNativeV1:
		return commonprovider.NativeEffectiveConfig(request, roots, runtimeSHA256, approval)
	case dialectPure:
		return task.EffectiveConfig{}, fmt.Errorf("%w: pure-mode profile has no native effective configuration", ErrUnsupportedProfile)
	default:
		return task.EffectiveConfig{}, fmt.Errorf("%w: unknown OpenCode launch dialect", ErrUnsupportedProfile)
	}
}

// recordedPredicate returns the predicate a reconstructed record keeps. A
// native run record keeps any registered run revision; an ACP record keeps
// only the ACP revision of its mode. The pure-mode dialect keeps its recorded
// predicate, or the legacy one when none was recorded.
func (dialect launchDialect) recordedPredicate(mode string, recorded task.PredicateRef) (task.PredicateRef, error) {
	switch {
	case dialect == dialectACP:
		if !recorded.Equal(acpReferenceForMode(mode)) {
			return task.PredicateRef{}, fmt.Errorf("%w: OpenCode ACP record has an unknown predicate", ErrUnsupportedProfile)
		}
		return recorded, nil
	case dialect.nativeRun():
		if isUpgradableOutputReference(mode, recorded) {
			return recorded, nil
		}
		return runReferenceForMode(mode), nil
	case recorded != (task.PredicateRef{}):
		return recorded, nil
	default:
		return LegacyReferenceForMode(mode), nil
	}
}
