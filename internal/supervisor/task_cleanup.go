package supervisor

import (
	"context"
	"errors"
	"log"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func CleanupCompletedTask(ctx context.Context, root string, td *taskdir.TaskDir, client *pueue.Client) {
	if client == nil || !pueue.IsPrivateConfig(root, client.Binding().ConfigPath) {
		return
	}
	receipt, err := td.ReadSupervisorReceipt()
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		log.Printf("delegation-layer: cannot read completed supervisor receipt for cleanup: %v", err)
		return
	}
	if !task.SameSupervisorIdentity(receipt.SupervisorRef(), client.Binding()) {
		migrated, migrationErr := pueue.PrivateSupervisorMigrationComplete(root, client.Binding())
		if migrationErr != nil {
			log.Printf("delegation-layer: cannot verify private supervisor migration for completed task cleanup: %v", migrationErr)
			return
		}
		if !migrated {
			log.Printf("delegation-layer: completed supervisor receipt no longer matches the current private supervisor")
			return
		}
	}
	numericTaskID := receipt.NumericTaskID
	identity := pueue.Identity{
		RootID:        receipt.RootID,
		TaskID:        receipt.TaskID,
		SpecSHA256:    receipt.SpecSHA256,
		MetaSHA256:    receipt.MetaSHA256,
		NumericTaskID: &numericTaskID,
	}
	if err = client.RemoveCompletedTask(ctx, identity); err != nil && !errors.Is(err, pueue.ErrCleanupDeferred) {
		log.Printf("delegation-layer: completed supervisor task %d retained for retry: %v", receipt.NumericTaskID, err)
	}
}

func CleanupCompletedTaskFromSaved(root string, td *taskdir.TaskDir, meta *task.MetaRecord, options pueue.Options, initialExecutable string) {
	if td == nil || meta == nil {
		return
	}
	submit, err := td.ReadSubmission()
	if err != nil || !pueue.IsPrivateConfig(root, submit.Supervisor.ConfigPath) || submit.Supervisor.ConfigPath != pueue.PrivateConfigPath(root) {
		// A retained recovery alias belongs to an explicit migration and is
		// cleaned by that transition, never by ordinary task collection.
		return
	}
	if _, err = os.Stat(submit.Supervisor.ConfigPath); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), pueue.PrivateControlTimeout(options))
	defer cancel()
	clientExecutable, daemonExecutable, resolveErr := ResolvePrivateSupervisorExecutables(root, initialExecutable)
	if resolveErr != nil {
		log.Printf("delegation-layer: cannot resolve private supervisor for completed task cleanup: %v", resolveErr)
		return
	}
	client, ready, err := pueue.ProbePrivate(ctx, clientExecutable, daemonExecutable, root, OptionsWithEnvironment(options, meta.Environment))
	if err != nil {
		log.Printf("delegation-layer: cannot bind private supervisor for completed task cleanup: %v", err)
		return
	}
	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			log.Printf("delegation-layer: private supervisor cleanup close failed: %v", closeErr)
		}
	}()
	if !ready {
		return
	}
	CleanupCompletedTask(ctx, root, td, client)
}
