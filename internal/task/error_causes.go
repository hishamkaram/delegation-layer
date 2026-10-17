package task

import "errors"

// AllErrorCauses reports whether every leaf of a nonempty error tree matches
// the caller's predicate. Nil errors and joins without causes never match.
func AllErrorCauses(err error, leaf func(error) bool) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !AllErrorCauses(cause, leaf) {
				return false
			}
		}
		return true
	}
	if cause := errors.Unwrap(err); cause != nil {
		return AllErrorCauses(cause, leaf)
	}
	return leaf(err)
}
