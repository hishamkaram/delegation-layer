package resourceadmission

// outcomeState is the verdict of one metric for one sample or interval.
type outcomeState int

// The zero value is unknown so an unset outcome is never healthy.
const (
	outcomeUnknown outcomeState = iota
	outcomeClear
	outcomeHot
	outcomeCritical
	outcomeSkipped
)

// outcome is one verdict. A clear outcome with a reason means "no pressure
// seen, but part of the evidence was unavailable".
type outcome struct {
	state  outcomeState
	reason Reason
}

var (
	clearOutcome    = outcome{state: outcomeClear}
	hotOutcome      = outcome{state: outcomeHot}
	criticalOutcome = outcome{state: outcomeCritical}
	skippedOutcome  = outcome{state: outcomeSkipped}
)

func unknownOutcome(reason Reason) outcome {
	return outcome{state: outcomeUnknown, reason: reason}
}

func boolOutcome(hot bool) outcome {
	if hot {
		return hotOutcome
	}
	return clearOutcome
}

// anyOf is hot when any outcome is hot, else unknown when any is unknown (the
// first reason wins), else clear.
func anyOf(outcomes []outcome) outcome {
	result := clearOutcome
	for _, o := range outcomes {
		switch o.state {
		case outcomeHot:
			return hotOutcome
		case outcomeUnknown:
			if result.state == outcomeClear {
				result = o
			}
		case outcomeClear, outcomeCritical, outcomeSkipped:
		}
	}
	return result
}

// allOf is clear when either side is known clear, hot when both are hot, else
// unknown. A known-false side decides that there is no pressure, but a missing
// other side stays visible as the clear outcome's reason so the coverage gap
// is not erased.
func allOf(a, b outcome) outcome {
	switch {
	case a.state == outcomeClear && b.state == outcomeUnknown:
		return outcome{state: outcomeClear, reason: b.reason}
	case b.state == outcomeClear && a.state == outcomeUnknown:
		return outcome{state: outcomeClear, reason: a.reason}
	case a.state == outcomeClear || b.state == outcomeClear:
		return clearOutcome
	case a.state == outcomeUnknown:
		return a
	case b.state == outcomeUnknown:
		return b
	default:
		return hotOutcome
	}
}

// track accumulates one metric's sequence of outcomes.
type track struct {
	seen        bool
	streak      int
	critical    bool
	sustained   bool
	baselineHot bool
	unknown     Reason
}

// baseline folds the first sample of an instantaneous metric. Only a critical
// sample counts at once; a hot baseline is not part of the required streak
// but still needs a following interval to be confirmed or cleared.
func (t *track) baseline(o outcome) {
	if o.state == outcomeHot {
		t.seen = true
		t.baselineHot = true
		return
	}
	t.record(o)
}

// pending reports that the metric is hot but not yet confirmed, so sampling
// must continue.
func (t *track) pending() bool {
	return !t.confirmed() && (t.streak > 0 || t.baselineHot)
}

// record folds one outcome into the sequence. A clear outcome ends the streak.
// An unknown outcome ends the streak and marks only this metric unavailable.
func (t *track) record(o outcome) {
	if o.state == outcomeSkipped {
		return
	}
	t.seen = true
	t.baselineHot = false
	switch o.state {
	case outcomeCritical:
		t.critical = true
	case outcomeHot:
		t.streak++
		if t.streak >= requiredStreak {
			t.sustained = true
		}
	case outcomeUnknown:
		t.streak = 0
		t.unknown = o.reason
	case outcomeClear, outcomeSkipped:
		t.streak = 0
		if o.reason != "" {
			t.unknown = o.reason
		}
	}
}

func (t *track) confirmed() bool { return t.critical || t.sustained }

// invalidate drops any verdict, including a confirmed pressure, and marks the
// metric unavailable.
func (t *track) invalidate(reason Reason) {
	if !t.seen {
		return
	}
	*t = track{seen: true, unknown: reason}
}

// expire marks an unfinished metric unavailable when the monitoring deadline
// stops sampling. A confirmed pressure stands.
func (t *track) expire(reason Reason) {
	if t.seen && !t.confirmed() {
		t.unknown = reason
	}
}
