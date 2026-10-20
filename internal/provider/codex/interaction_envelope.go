package codex

import (
	"bytes"
	"encoding/json"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// requestUserInputMethod is the one server request Delegate answers; every
// other request is denied.
const requestUserInputMethod = "item/tool/requestUserInput"

// classifyEnvelope applies JSON-RPC classification to a method envelope before
// any typed handler sees it: with an id it is a request, without one it is a
// notification. It reports handled=true when the envelope is fully resolved
// here: a denial, or a fault for a request that is malformed. An unhandled
// envelope continues to the typed handler for its method. Only revisions that
// opt in classify, so earlier revisions keep their frozen dispatch.
func (p *appServerProtocol) classifyEnvelope(method string, nativeID []byte) (update execution.InteractionUpdate, handled bool, err error) {
	if !p.revision.classifiesEnvelopesByID() {
		return execution.InteractionUpdate{}, false, nil
	}
	isInput := method == requestUserInputMethod
	if len(nativeID) == 0 {
		if isInput {
			return execution.InteractionUpdate{}, true, task.ErrInteractionRequest
		}
		return execution.InteractionUpdate{}, false, nil
	}
	if !requestIDIsStringOrNumber(nativeID) {
		return execution.InteractionUpdate{}, true, task.ErrInteractionRequest
	}
	if isInput {
		return execution.InteractionUpdate{}, false, nil
	}
	update, err = denyAppServerRequest(json.RawMessage(nativeID))
	return update, true, err
}

// requestIDIsStringOrNumber accepts the two JSON-RPC id types. A null, boolean,
// object, or array id is malformed and is never echoed back in a reply.
func requestIDIsStringOrNumber(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return false
	}
	first := raw[0]
	return first == '"' || first == '-' || (first >= '0' && first <= '9')
}
