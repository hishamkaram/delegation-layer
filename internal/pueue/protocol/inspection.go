// Package protocol contains pure Pueue protocol values and parsers.
package protocol

const InspectionGroupPrefix = "delegation-inspection-"

// InspectionIdentity binds one inspection worker to one root-scoped group.
// NumericTaskID is optional while preparing a submission and required to stop
// an already admitted worker.
type InspectionIdentity struct {
	RootID        string
	TaskID        string
	NumericTaskID *int64
	// QueueGroup is copied from the immutable inspection request. Empty keeps
	// the historical root-scoped group; new private runtime probes use default.
	QueueGroup string
}

// Group returns the saved queue, defaulting to the historical root group.
func (i InspectionIdentity) Group() string {
	if i.QueueGroup != "" {
		return i.QueueGroup
	}
	return InspectionGroup(i.RootID)
}

// Label returns the exact supervisor label derived from the task ID.
func (i InspectionIdentity) Label() string { return InspectionGroup(i.RootID) + "-" + i.TaskID }

func InspectionGroup(rootID string) string { return InspectionGroupPrefix + rootID }
