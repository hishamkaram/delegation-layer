package contract

import (
	"errors"
	"fmt"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// MaxPageLimit bounds every list, active, and history page.
const MaxPageLimit = 100

var errEmptyHistoryEntry = errors.New("empty schedule history entry")

type historyKind uint8

const (
	historyInvalid historyKind = iota
	historyOccurrence
	historySkip
)

// HistoryEntry is exactly one occurrence or one skip span of a schedule. Its
// endpoint is the cursor value that recording it set: the occurrence's due time
// or the span's end. The cursor strictly advances, so endpoints are unique
// within a schedule and order its history. The zero value is invalid.
type HistoryEntry struct {
	kind       historyKind
	occurrence Occurrence
	skip       SkipSpan
}

// OccurrenceEntry wraps a copy of an occurrence as a history entry.
func OccurrenceEntry(o Occurrence) HistoryEntry {
	return HistoryEntry{kind: historyOccurrence, occurrence: o.clone()}
}

// SkipEntry wraps a skip span as a history entry.
func SkipEntry(span SkipSpan) HistoryEntry {
	return HistoryEntry{kind: historySkip, skip: span}
}

// Occurrence returns a copy of the entry's occurrence when it is one.
func (e HistoryEntry) Occurrence() (Occurrence, bool) {
	return e.occurrence.clone(), e.kind == historyOccurrence
}

// Skip returns the entry's span when it is one.
func (e HistoryEntry) Skip() (SkipSpan, bool) {
	return e.skip, e.kind == historySkip
}

// Endpoint returns the entry's history key.
func (e HistoryEntry) Endpoint() (time.Time, error) {
	switch e.kind {
	case historyOccurrence:
		return e.occurrence.Due, nil
	case historySkip:
		return e.skip.To, nil
	case historyInvalid:
		return time.Time{}, errEmptyHistoryEntry
	default:
		return time.Time{}, errEmptyHistoryEntry
	}
}

// ValidateHistoryEntry checks a stored entry of scheduleID's history.
func ValidateHistoryEntry(scheduleID string, entry HistoryEntry) error {
	switch entry.kind {
	case historyOccurrence:
		if err := ValidateOccurrence(entry.occurrence); err != nil {
			return err
		}
		if entry.occurrence.ScheduleID != scheduleID {
			return fmt.Errorf("%w: occurrence %s belongs to another schedule", task.ErrIdentityMismatch, entry.occurrence.TaskID)
		}
		return nil
	case historySkip:
		return ValidateSkipSpan(entry.skip)
	case historyInvalid:
		return errEmptyHistoryEntry
	default:
		return errEmptyHistoryEntry
	}
}

// SchedulePage lists schedule summaries in ascending schedule ID order after
// the exclusive AfterScheduleID; empty starts at the first schedule.
type SchedulePage struct {
	AfterScheduleID string
	Limit           int
}

// SchedulePageResult holds at most Limit summaries; More reports a later page.
type SchedulePageResult struct {
	Schedules []ScheduleSummary
	More      bool
}

// ActivePage lists active occurrences in ascending schedule ID order after the
// exclusive AfterScheduleID. A schedule has at most one active occurrence.
type ActivePage struct {
	AfterScheduleID string
	Limit           int
}

// ActivePageResult holds at most Limit occurrences; More reports a later page.
type ActivePageResult struct {
	Occurrences []Occurrence
	More        bool
}

// HistoryPage lists one schedule's history in descending endpoint order, below
// the exclusive Before endpoint; nil starts at the newest entry.
type HistoryPage struct {
	ScheduleID string
	Before     *time.Time
	Limit      int
}

// HistoryPageResult holds at most Limit entries; More reports an older page.
type HistoryPageResult struct {
	Entries []HistoryEntry
	More    bool
}

// ValidateSchedulePage checks a schedule list request.
func ValidateSchedulePage(page SchedulePage) error {
	if page.AfterScheduleID != "" {
		if err := validateScheduleID(page.AfterScheduleID); err != nil {
			return err
		}
	}
	return validateLimit(page.Limit)
}

// ValidateActivePage checks an active-occurrence list request.
func ValidateActivePage(page ActivePage) error {
	if page.AfterScheduleID != "" {
		if err := validateScheduleID(page.AfterScheduleID); err != nil {
			return err
		}
	}
	return validateLimit(page.Limit)
}

// ValidateHistoryPage checks a history request.
func ValidateHistoryPage(page HistoryPage) error {
	if err := validateScheduleID(page.ScheduleID); err != nil {
		return err
	}
	if page.Before != nil {
		if err := ValidateInstant("history before", *page.Before); err != nil {
			return err
		}
	}
	return validateLimit(page.Limit)
}

// Next returns the request for the following older page, keyed by the last
// returned endpoint, or false when there is none.
func (r HistoryPageResult) Next(page HistoryPage) (HistoryPage, bool, error) {
	if !r.More || len(r.Entries) == 0 {
		return HistoryPage{}, false, nil
	}
	endpoint, err := r.Entries[len(r.Entries)-1].Endpoint()
	if err != nil {
		return HistoryPage{}, false, err
	}
	return HistoryPage{ScheduleID: page.ScheduleID, Before: &endpoint, Limit: page.Limit}, true, nil
}

func validateLimit(limit int) error {
	if limit < 1 || limit > MaxPageLimit {
		return fmt.Errorf("page limit must be between 1 and %d", MaxPageLimit)
	}
	return nil
}
