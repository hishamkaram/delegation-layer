package stop

// Outcome is an immutable observation of one stop attempt. Durable stop
// records and lifetime evidence remain owned by taskdir and Guardian.
type Outcome struct {
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

func (s Outcome) RequestID() string { return s.requestID }

func (s Outcome) Cause() string { return s.cause }

func (s Outcome) Requested() bool { return s.requested }

func (s Outcome) Matched() bool { return s.matched }

func (s Outcome) Attempted() bool { return s.attempted }

func (s Outcome) Action() string { return s.action }

func (s Outcome) NumericTaskID() *int64 { return copyTaskID(s.numericTaskID) }

func (s Outcome) Acknowledged() *bool {
	return copyAcknowledgement(s.acknowledged)
}

func copyAcknowledgement(acknowledged *bool) *bool {
	if acknowledged == nil {
		return nil
	}
	value := *acknowledged
	return &value
}

func (s Outcome) InFlight() bool { return s.inFlight }

func (s Outcome) ObservedState() string { return s.observedState }

func (s Outcome) Terminated() bool { return s.terminated }

func (s Outcome) Message() string { return s.message }
