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
		r.entries = append(r.entries, entry{ref: ref, interpreter: v})
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
