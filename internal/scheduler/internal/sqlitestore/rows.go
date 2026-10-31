package sqlitestore

import (
	"database/sql"
	"errors"
	"fmt"
	"math"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Stored rows are decoded field by field and validated with the contract.
// Nullable columns form all-or-nothing tuples; a partial tuple, an empty
// optional text, or an out-of-range enum is an invariant fault, never a
// silently coerced zero value.

const scheduleColumns = `schedule_id, created_at_unix_nanos, cron, timezone, request_json,
	brief_sha256, definition_sha256, state, cursor_unix_nanos`

type scheduleRow struct {
	id, cron, timezone, briefSHA256, definitionSHA256, state string
	created, cursor                                          int64
	request                                                  []byte
}

// targets lists scan destinations in scheduleColumns order; database/sql's
// Scan API takes them as any.
func (r *scheduleRow) targets() []any {
	return []any{&r.id, &r.created, &r.cron, &r.timezone, &r.request, &r.briefSHA256, &r.definitionSHA256, &r.state, &r.cursor}
}

func (r *scheduleRow) header() (contract.DefinitionHeader, error) {
	request, err := contract.DecodeRequest(r.request)
	if err != nil {
		return contract.DefinitionHeader{}, storedFault("schedule "+r.id+" request", err)
	}
	return contract.DefinitionHeader{
		ScheduleID: r.id,
		CreatedAt:  contract.InstantFromUnixNanos(r.created),
		Cron:       r.cron,
		Timezone:   r.timezone,
		Request:    request,
	}, nil
}

func (r *scheduleRow) schedule(brief []byte) (contract.Schedule, error) {
	header, err := r.header()
	if err != nil {
		return contract.Schedule{}, err
	}
	definition := contract.Definition{DefinitionHeader: header, Brief: brief}
	stored := contract.Digests{BriefSHA256: r.briefSHA256, DefinitionSHA256: r.definitionSHA256}
	if err := contract.VerifyDefinitionDigests(definition, stored); err != nil {
		return contract.Schedule{}, storedFault("schedule "+r.id, err)
	}
	schedule := contract.Schedule{Definition: definition, State: contract.State(r.state), Cursor: contract.InstantFromUnixNanos(r.cursor)}
	if err := contract.ValidateSchedule(schedule); err != nil {
		return contract.Schedule{}, storedFault("schedule "+r.id, err)
	}
	return schedule, nil
}

func (r *scheduleRow) summary(briefLength int64) (contract.ScheduleSummary, error) {
	header, err := r.header()
	if err != nil {
		return contract.ScheduleSummary{}, err
	}
	summary := contract.ScheduleSummary{
		Header:      header,
		Digests:     contract.Digests{BriefSHA256: r.briefSHA256, DefinitionSHA256: r.definitionSHA256},
		BriefLength: briefLength,
		State:       contract.State(r.state),
		Cursor:      contract.InstantFromUnixNanos(r.cursor),
	}
	if err := contract.ValidateSummary(summary); err != nil {
		return contract.ScheduleSummary{}, storedFault("schedule "+r.id+" summary", err)
	}
	return summary, nil
}

const occurrenceColumns = `task_id, schedule_id, due_unix_nanos, armed_at_unix_nanos,
	observed_at_unix_nanos, admission, liveness, publication, outcome_sha256, fault_code,
	released_at_unix_nanos, release_kind, release_ref`

type occurrenceRow struct {
	taskID, scheduleID                             string
	due, armedAt                                   int64
	observedAt, admission, liveness, publication   sql.NullInt64
	outcomeSHA256, faultCode, releaseKind, release sql.NullString
	releasedAt                                     sql.NullInt64
}

// targets lists scan destinations in occurrenceColumns order.
func (r *occurrenceRow) targets() []any {
	return []any{
		&r.taskID, &r.scheduleID, &r.due, &r.armedAt,
		&r.observedAt, &r.admission, &r.liveness, &r.publication, &r.outcomeSHA256, &r.faultCode,
		&r.releasedAt, &r.releaseKind, &r.release,
	}
}

func (r *occurrenceRow) occurrence() (contract.Occurrence, error) {
	occurrence := contract.Occurrence{
		TaskID:     r.taskID,
		ScheduleID: r.scheduleID,
		Due:        contract.InstantFromUnixNanos(r.due),
		ArmedAt:    contract.InstantFromUnixNanos(r.armedAt),
	}
	observation, err := r.observation()
	if err == nil {
		occurrence.Observation = observation
		occurrence.Release, err = r.releaseFact()
	}
	if err == nil {
		err = contract.ValidateOccurrence(occurrence)
	}
	if err != nil {
		return contract.Occurrence{}, storedFault("occurrence "+r.taskID, err)
	}
	return occurrence, nil
}

func (r *occurrenceRow) observation() (*contract.Observation, error) {
	present := []bool{r.observedAt.Valid, r.admission.Valid, r.liveness.Valid, r.publication.Valid}
	if !allEqual(present) {
		return nil, errors.New("partial observation tuple")
	}
	if !r.observedAt.Valid {
		if r.outcomeSHA256.Valid || r.faultCode.Valid {
			return nil, errors.New("observation facts without an observation")
		}
		return nil, nil
	}
	admission, admissionErr := smallEnum("admission", r.admission.Int64)
	liveness, livenessErr := smallEnum("liveness", r.liveness.Int64)
	publication, publicationErr := smallEnum("publication", r.publication.Int64)
	outcome, outcomeErr := optionalText("outcome digest", r.outcomeSHA256)
	fault, faultErr := optionalText("fault code", r.faultCode)
	if err := errors.Join(admissionErr, livenessErr, publicationErr, outcomeErr, faultErr); err != nil {
		return nil, err
	}
	return &contract.Observation{
		ObservedAt:    contract.InstantFromUnixNanos(r.observedAt.Int64),
		Admission:     task.Admission(admission),
		Liveness:      task.Liveness(liveness),
		Publication:   task.Publication(publication),
		OutcomeSHA256: outcome,
		FaultCode:     fault,
	}, nil
}

func (r *occurrenceRow) releaseFact() (*contract.Release, error) {
	if !allEqual([]bool{r.releasedAt.Valid, r.releaseKind.Valid, r.release.Valid}) {
		return nil, errors.New("partial release tuple")
	}
	if !r.releasedAt.Valid {
		return nil, nil
	}
	return &contract.Release{
		ReleasedAt: contract.InstantFromUnixNanos(r.releasedAt.Int64),
		Kind:       contract.ReleaseKind(r.releaseKind.String),
		Ref:        r.release.String,
	}, nil
}

func allEqual(values []bool) bool {
	for _, value := range values {
		if value != values[0] {
			return false
		}
	}
	return true
}

// smallEnum range-checks a stored enum before narrowing it, so an out-of-range
// value is reported instead of wrapping into a valid one.
func smallEnum(name string, value int64) (uint8, error) {
	if value < 0 || value > math.MaxUint8 {
		return 0, fmt.Errorf("observation %s %d is out of range", name, value)
	}
	return uint8(value), nil
}

// optionalText maps NULL to the contract's empty absent value and rejects a
// stored empty string, which would otherwise be indistinguishable from NULL.
func optionalText(name string, value sql.NullString) (string, error) {
	if value.Valid && value.String == "" {
		return "", fmt.Errorf("stored empty %s", name)
	}
	return value.String, nil
}
