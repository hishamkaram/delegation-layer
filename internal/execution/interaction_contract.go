package execution

import (
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

var (
	errMachineProtocolEvents    = errors.New("machine protocol update carries user interaction events")
	errProtocolIdentityObserver = errors.New("a native protocol is the sole stdout observer; identity observer must be nil")
)

// Protocol only translates a native machine protocol. The runner owns all I/O,
// persistence, deadlines and process lifetime on one transport. Calls are
// serialized. Complete runs after the stream ends, possibly more than once,
// and must return the same result each time.
type Protocol interface {
	Start(brief []byte) ([][]byte, error)
	Receive(line []byte) (InteractionUpdate, error)
	Complete() error
}

// InteractionProtocol adds the optional user channel to the same transport.
// Encode translates one durable user message into a native frame.
type InteractionProtocol interface {
	Protocol
	Encode(task.InteractionMessage) ([]byte, error)
}

// InteractionUpdate is a bounded protocol transition. Outbound frames omit the
// newline, which is supplied by the execution core. Done closes native stdin.
// Events belong to the user interaction journal; a machine Protocol that
// returns them is rejected.
type InteractionUpdate struct {
	Outbound [][]byte
	Events   []task.InteractionEvent
	Done     bool
}

// ValidateProtocolSelection is the canonical transport selection rule. A
// non-interactive task uses finite stdin or a machine Protocol. An interactive
// task uses only the user InteractionProtocol. Every other combination is
// refused before provider Start.
func ValidateProtocolSelection(interactive, hasProtocol, hasInteraction bool) error {
	if (interactive && hasInteraction && !hasProtocol) || (!interactive && !hasInteraction) {
		return nil
	}
	return task.ErrInteractionUnavailable
}

// selectedProtocol normalizes validated options to the engine's protocol and
// its optional user channel. Both names hold the same value for interaction,
// so optional record methods remain visible to the engine.
func (o Options) selectedProtocol() (Protocol, InteractionProtocol) {
	if o.Interaction != nil {
		return o.Interaction, o.Interaction
	}
	return o.Protocol, nil
}

func validateProtocolOptions(interactive bool, opts Options) error {
	if err := ValidateProtocolSelection(interactive, opts.Protocol != nil, opts.Interaction != nil); err != nil {
		return err
	}
	// Historical Interaction supersedes a supplied Identity observer, which is
	// never invoked. Only a machine Protocol refuses the unused observer.
	if opts.Protocol != nil && opts.Identity != nil {
		return errProtocolIdentityObserver
	}
	return nil
}
