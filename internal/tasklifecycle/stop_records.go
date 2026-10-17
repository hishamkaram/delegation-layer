package tasklifecycle

// StopOutcome is an immutable observation of one stop attempt. Durable stop
// records and lifetime evidence remain owned by taskdir and Guardian.
type StopOutcome struct {
	requestID     string
	cause         string
	requested     bool
	matched       bool
	attempted     bool
	action        string
	numericTaskID *int64
	acknowledged  *bool
	inFlight      bool
	observedState string
	terminated    bool
	message       string
}

func (s StopOutcome) RequestID() string { return s.requestID }

func (s StopOutcome) Cause() string { return s.cause }

func (s StopOutcome) Requested() bool { return s.requested }

func (s StopOutcome) Matched() bool { return s.matched }

func (s StopOutcome) Attempted() bool { return s.attempted }

func (s StopOutcome) Action() string { return s.action }

func (s StopOutcome) NumericTaskID() *int64 { return copyTaskID(s.numericTaskID) }

func (s StopOutcome) Acknowledged() *bool {
	return copyAcknowledgement(s.acknowledged)
}

func copyAcknowledgement(acknowledged *bool) *bool {
	if acknowledged == nil {
		return nil
	}
	value := *acknowledged
	return &value
}

func (s StopOutcome) InFlight() bool { return s.inFlight }

func (s StopOutcome) ObservedState() string { return s.observedState }

func (s StopOutcome) Terminated() bool { return s.terminated }

func (s StopOutcome) Message() string { return s.message }
