package task

import "fmt"

// ControlFactError names a failed boundary observation without publishing its
// unrestricted native cause. The original cause remains available to owners.
type ControlFactError struct {
	ResourceKind string
	ResourceID   string
	FactKind     string
	Name         string
	ReasonCode   string
	NextAction   string
	Cause        error
}

func (e *ControlFactError) Error() string {
	return fmt.Sprintf("%s %s: %s %q unavailable (%s); next action: %s", e.ResourceKind, e.ResourceID, e.FactKind, e.Name, e.ReasonCode, e.NextAction)
}

func (e *ControlFactError) Unwrap() error { return e.Cause }
