package workspaces

// RequestedAction carries the caller's requested operation into its view. It
// grants no authorization; only a successful canonical journal read can do so.
func RequestedAction(operationID string) ActionAcknowledgement {
	return ActionAcknowledgement{operationID: operationID}
}

func (ack ActionAcknowledgement) Authorized() bool { return ack.authorized }

func (ack ActionAcknowledgement) OperationID() string { return ack.operationID }
