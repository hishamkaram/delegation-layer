package supervisor

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ValidateInfrastructureArtifacts checks staged custody data in the target root
// without selecting a binding or creating artifact-source authority. The caller
// retains and revalidates the target's actual root pin through its transaction.
// This check establishes neither the executing image nor native start authority.
func ValidateInfrastructureArtifacts(ctx context.Context, root string, artifacts infrastructure.Artifacts) error {
	runtime := infrastructureRuntimeSource{root: root, executable: artifacts.Runner, sha256: artifacts.RunnerSHA256}
	if err := runtime.validate(ctx); err != nil {
		return err
	}
	saved := task.SupervisorRef{
		ClientExecutable: artifacts.Client, ClientSHA256: artifacts.ClientSHA256,
		DaemonExecutable: artifacts.Daemon, DaemonSHA256: artifacts.DaemonSHA256,
	}
	if err := requireInfrastructureStatePair(root, saved); err != nil {
		return err
	}
	if err := ValidateSavedStateSupervisorPair(root, saved); err != nil {
		return err
	}
	return ctx.Err()
}
