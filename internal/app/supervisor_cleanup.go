package app

import (
	"github.com/hishamkaram/delegation-layer/internal/admission"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
)

func privateCleanupOwner() *supervisorcore.Cleanup {
	return supervisorcore.NewCleanup(admission.TerminalEvidence{}, workspaces.InspectPrivateOperation)
}
