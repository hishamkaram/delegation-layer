package app

import (
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// writePrivateSchedulingFixture seeds the persisted v4 input for app consumers.
func writePrivateSchedulingFixture(root string, binding task.SupervisorRef, capability pueue.StagedLaunchCapability) error {
	record := struct {
		SchemaVersion int                          `json:"schema_version"`
		Revision      string                       `json:"revision"`
		Supervisor    task.SupervisorRef           `json:"supervisor"`
		StagedLaunch  pueue.StagedLaunchCapability `json:"staged_launch"`
	}{task.SchemaVersion, "unlimited-v4", binding, capability}
	data, err := task.MarshalCanonical(record)
	if err != nil {
		return err
	}
	return pueue.ReplacePrivateSchedulingEvidence(root, data)
}
