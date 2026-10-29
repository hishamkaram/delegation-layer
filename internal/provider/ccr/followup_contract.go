package ccr

import (
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
