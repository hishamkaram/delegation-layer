package workspaces

import (
	"time"

	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// Authorization is derived from its durable owner before reading the view.
// This acknowledgement is not a second journal, and a read error is not a
// negative acknowledgement. OperationID names only a real, linked helper.
type ActionAcknowledgement struct {
	authorized  bool
	operationID string
}

type OperationSpec struct {
	Purpose   string
	Kind      string
	Subject   workspace.Subject
	Budget    time.Duration
	Deadline  string
	Arguments []string
	Discard   bool
}

type OperationError struct{ reason string }

type workspaceActionOperationState uint8

const (
	workspaceActionClosedFailure workspaceActionOperationState = iota + 1
	workspaceActionClosedSuccess
	workspaceActionUnfinished
)
