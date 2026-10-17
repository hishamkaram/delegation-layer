package contract

import (
	"encoding/json"
	"errors"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	MaxProjectedFacts = 4 << 10
	// Runtime facts include the observed provider version and capability
	// digest. Their version is bounded by the durable runtime-evidence limit,
	// so the complete envelope can use the normal control-record budget.
	MaxRuntimeFacts = task.MaxControlRecordSize
)

var (
	// ErrProjectionDefinition is returned when the immutable inspection
	// description cannot be validated before invoking a callback.
	ErrProjectionDefinition = errors.New("invalid inspection projection definition")
	// ErrProjectionDeadline rejects an unbounded caller context. The caller
	// owns the deadline; this package never installs a second timer.
	ErrProjectionDeadline = errors.New("inspection projection requires a finite deadline")
	// ErrProjectionNativeTooLarge reports native bytes outside the compiled
	// inspection output bound.
	ErrProjectionNativeTooLarge = errors.New("inspection native output exceeds its bound")
	// ErrProjectionCallback is the fixed result for callback errors and panics.
	ErrProjectionCallback = errors.New("inspection projection callback failed")
	// ErrProjectionFacts is the fixed result for malformed or oversized facts.
	ErrProjectionFacts = errors.New("inspection projection facts are invalid")
	// ErrProjectionHTTP is the fixed result for remote request, response, and
	// response-body failures. No underlying error text crosses this boundary.
	ErrProjectionHTTP = errors.New("inspection projection remote request failed")
	// ErrProjectionResponseTooLarge distinguishes the bounded-body failure
	// without exposing response bytes or transport diagnostics.
	ErrProjectionResponseTooLarge = errors.New("inspection projection response exceeds its bound")
	// ErrProjectionCanceled preserves only the standard context outcome when a
	// caller deadline or cancellation ends a remote operation.
	ErrProjectionCanceled = errors.New("inspection projection canceled")
)

func CanonicalProjectionFacts(raw json.RawMessage) (json.RawMessage, error) {
	return CanonicalFactsWithin(raw, MaxProjectedFacts)
}

func CanonicalFactsWithin(raw json.RawMessage, limit int) (json.RawMessage, error) {
	if len(raw) == 0 || len(raw) > limit || !utf8.Valid(raw) {
		return nil, ErrProjectionFacts
	}
	if err := task.ValidateJSONStructure(raw); err != nil {
		return nil, ErrProjectionFacts
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, ErrProjectionFacts
	}
	canonical, err := task.MarshalCanonical(object)
	if err != nil || len(canonical) > limit {
		return nil, ErrProjectionFacts
	}
	return json.RawMessage(canonical), nil
}
