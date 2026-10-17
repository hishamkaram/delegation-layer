package supervisor

import (
	"log"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

// CloseMaintenance releases a recovered supervisor client without
// changing the command's authority result. A temporary migration config may
// still be owned by an in-flight pueue command; CloseAfterPending records that
// cleanup boundary and the next lifecycle pass can finish it. Cleanup failure
// is maintenance telemetry, never evidence that changes a successful task.
func CloseMaintenance(client *pueue.Client, cause error) {
	if client == nil {
		return
	}
	if err := client.CloseAfterPending(cause); err != nil {
		log.Printf("delegate: supervisor cleanup deferred: %v", err)
	}
}
