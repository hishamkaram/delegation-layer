package app

import (
	"slices"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// projectConnectorWorkspace changes only output copies after control validation.
// Legacy consumers retain their strict JSON shape even with mixed task history.
func projectConnectorWorkspace(response Response, include bool) Response {
	if include || response.Connector == nil {
		return response
	}
	connector := *response.Connector
	connector.WorkspaceConstraint = nil
	connector.Receipt = projectConnectorReceipt(connector.Receipt)
	if connector.Inventory != nil {
		inventory := *connector.Inventory
		inventory.Entries = slices.Clone(inventory.Entries)
		for i := range inventory.Entries {
			inventory.Entries[i].Receipt = projectConnectorReceipt(inventory.Entries[i].Receipt)
		}
		connector.Inventory = &inventory
	}
	response.Connector = &connector
	return response
}

func projectConnectorReceipt(receipt *taskdir.ConnectorReceipt) *taskdir.ConnectorReceipt {
	if receipt == nil {
		return nil
	}
	projection := *receipt
	projection.WorkspaceConstraint = nil
	return &projection
}
