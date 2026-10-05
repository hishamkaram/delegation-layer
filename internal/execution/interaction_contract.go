package execution

import "github.com/hishamkaram/delegation-layer/internal/task"

// InteractionProtocol only translates a native protocol. The runner owns all
// I/O, persistence, deadlines and process lifetime. Calls are serialized.
type InteractionProtocol interface {
	Start(brief []byte) ([][]byte, error)
	Receive(line []byte) (InteractionUpdate, error)
	Encode(task.InteractionMessage) ([]byte, error)
	Complete() error
}

// InteractionUpdate is a bounded protocol transition. Outbound frames omit the
// newline, which is supplied by the execution core. Done closes native stdin.
type InteractionUpdate struct {
	Outbound [][]byte
	Events   []task.InteractionEvent
	Done     bool
}
