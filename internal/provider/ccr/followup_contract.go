package ccr

import (
	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/provider/claude"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// advisoryReference freezes both the CCR launch text and its embedded Claude
// advisory contract. These principal-only references remain replay-only.
func advisoryReference(mode string, preservesRoute bool) task.PredicateRef {
	version := advisoryModeledPredicateVersion
	if preservesRoute {
		version = advisoryPredicateVersion
	}
	contract := ccrContract(version, claude.AdvisoryNativeContractForMode(mode), preservesRoute)
	if contract == "" {
		return task.PredicateRef{}
	}
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: version, SHA256: task.ComputeSHA256([]byte(contract))}
}

// projectionReference freezes the route-preserving v7 launch text with
// Claude's projection contract, sealed before failed-request classification.
// Only read-only tasks were admitted under it (digest
// c70936cf9a3b1a247ffd12403675dbc714bec8bb0a4179e85b6f348d918cf0cf); no other
// mode or launch shape has a projection revision. It is replay-only.
func projectionReference(mode string) task.PredicateRef {
	if mode != config.ModeReadOnly {
		return task.PredicateRef{}
	}
	contract := ccrContract(currentPredicateVersion, claude.ProjectionNativeContractForMode(mode), true)
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: currentPredicateVersion, SHA256: task.ComputeSHA256([]byte(contract))}
}

// isProjectionReference reports whether reference is the projection revision
// of mode. The explicit mode guard keeps the zero reference of an unsupported
// mode from matching.
func isProjectionReference(mode string, reference task.PredicateRef) bool {
	return mode == config.ModeReadOnly && reference.Equal(projectionReference(mode))
}
