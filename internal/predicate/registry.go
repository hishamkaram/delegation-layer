package predicate

import (
	"fmt"
	"reflect"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type (
	entry struct {
		ref         task.PredicateRef
		interpreter Interpreter
		recovery    RecoveryInterpreter
	}
	// Registry is immutable; constructor snapshots keys and no mutation API is exposed.
	Registry struct{ entries []entry }
)

func New(interpreters ...Interpreter) (Registry, error) {
	r := Registry{entries: make([]entry, 0, len(interpreters))}
	for _, v := range interpreters {
		if v == nil || isNilInterpreter(v) {
			return Registry{}, fmt.Errorf("nil interpreter")
		}
		ref := v.Reference()
		if err := task.ValidatePredicateRef(ref); err != nil {
			return Registry{}, err
		}
		for _, old := range r.entries {
			if ref.Equal(old.ref) {
				return Registry{}, fmt.Errorf("duplicate predicate reference")
			}
		}
		var recovery RecoveryInterpreter
		if candidate, ok := v.(RecoveryInterpreter); ok {
			recovery = candidate
			if !candidate.Target().Equal(ref) {
				return Registry{}, fmt.Errorf("recovery target must match registered interpreter")
			}
			if err := task.ValidatePredicateRef(candidate.Target()); err != nil {
				return Registry{}, fmt.Errorf("invalid recovery target: %w", err)
			}
		}
		r.entries = append(r.entries, entry{ref: ref, interpreter: v, recovery: recovery})
	}
	return r, nil
}

func (r Registry) Resolve(ref task.PredicateRef) (Interpreter, error) {
	for _, v := range r.entries {
		if ref.Equal(v.ref) {
			return v.interpreter, nil
		}
	}
	return nil, task.ErrIncompatiblePredicate
}

// ResolveRecovery returns the one registered interpreter that explicitly
// accepts source for offline recovery. Selection is independent of catalog
// registration order so equal catalogs produce equal answers. A target equal
// to source is ignored to prevent an endless recheck loop.
func (r Registry) ResolveRecovery(source task.PredicateRef) (RecoveryInterpreter, error) {
	if err := task.ValidatePredicateRef(source); err != nil {
		return nil, err
	}
	candidates := make([]RecoveryInterpreter, 0)
	for _, candidate := range r.entries {
		if candidate.recovery == nil || candidate.recovery.Target().Equal(source) || !candidate.recovery.CanRecover(source) {
			continue
		}
		candidates = append(candidates, candidate.recovery)
	}
	if len(candidates) == 0 {
		return nil, ErrRecoveryUnsupported
	}
	if len(candidates) != 1 {
		return nil, ErrRecoveryAmbiguous
	}
	return candidates[0], nil
}

// Default retains exactly the built-in fixture contract.
func Default() Registry {
	return Registry{entries: []entry{{ref: task.FixturePredicateRef(), interpreter: fixtureV1{}}}}
}

func isNilInterpreter(v Interpreter) bool {
	value := reflect.ValueOf(v)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	case reflect.Invalid, reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128, reflect.Array, reflect.String,
		reflect.Struct, reflect.UnsafePointer:
		return false
	}
	panic("unreachable reflect.Kind")
}
