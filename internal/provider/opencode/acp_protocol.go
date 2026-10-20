package opencode

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/providerjson"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// acpProtocol is the live machine protocol for one finite ACP task. The
// execution core owns stdin, stdout capture, persistence, deadlines and the
// process, and serializes every call. The protocol only translates frames
// through the shared reducer; it has no user channel and emits no user
// interaction events.
type acpProtocol struct {
	session *acpSession
	started bool
}

var _ execution.CurrentRecordInteractionProtocol = (*acpProtocol)(nil)

// newACPProtocol derives the protocol from the immutable request and the
// core-owned session expectation and identity callback. A continuation must
// expect exactly the request's recorded prior session.
func newACPProtocol(request task.TaskRecord, expected task.SessionExpectation, record func(task.SessionIdentity) error) (*acpProtocol, error) {
	if record == nil {
		return nil, errors.New("nil OpenCode identity recorder")
	}
	if !filepath.IsAbs(request.CanonicalCwd) {
		return nil, fmt.Errorf("%w: workspace must be absolute", ErrUnsupportedProfile)
	}
	prior := request.PriorSession
	if expected.Required != (prior != nil) || (prior != nil && (prior.Provider != Provider || prior.ConversationID != expected.ID)) {
		return nil, task.ErrIdentityMismatch
	}
	session, err := newACPSession(request.Mode, request.RequestedConfig, expected, &acpOutbound{cwd: request.CanonicalCwd, record: record})
	if err != nil {
		return nil, err
	}
	return &acpProtocol{session: session}, nil
}

// UseRecordCapture opts into callback-scoped record capture, so large native
// frames are parsed in place without a line-length limit.
func (p *acpProtocol) UseRecordCapture() bool { return true }

// Start validates and encodes the whole prompt before any native request, so
// an unencodable brief fails before a session exists. It returns initialize.
func (p *acpProtocol) Start(brief []byte) ([][]byte, error) {
	if p.started {
		return nil, fmt.Errorf("%w: protocol already started", errACPProtocol)
	}
	p.started = true
	prompt, err := encodeACPPrompt(brief)
	if err != nil {
		return nil, err
	}
	p.session.outbound.prompt = prompt
	return p.session.begin()
}

// Receive is the line-framed path; live capture uses ReceiveRecord.
func (p *acpProtocol) Receive(line []byte) (execution.InteractionUpdate, error) {
	return p.ReceiveRecord(providerjson.Value{Reader: bytes.NewReader(line), End: int64(len(line))})
}

// ReceiveRecord consumes one callback-scoped stdout record. Records after the
// terminal response are still validated, and none of them may send a frame.
func (p *acpProtocol) ReceiveRecord(value providerjson.Value) (execution.InteractionUpdate, error) {
	if !p.started {
		return execution.InteractionUpdate{}, fmt.Errorf("%w: native output before the protocol started", errACPProtocol)
	}
	return p.session.receive(value)
}

// Complete reports the protocol verdict: only an end_turn response to the
// task's own prompt completes. EOF, text or process exit alone never do.
func (p *acpProtocol) Complete() error { return p.session.completion() }
