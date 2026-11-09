package contract

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// AttemptFailure is immutable, public-safe admission evidence. It never proves
// absence, closure, or permission to retry the claimed occurrence.
type AttemptFailure struct {
	Code       string `json:"code"`
	Stage      string `json:"stage"`
	NextAction string `json:"next_action"`
	Message    string `json:"message"`
}

func ValidateAttempt(f AttemptFailure) error {
	for _, value := range []string{f.Code, f.NextAction} {
		if err := validateCode("attempt code", value); err != nil {
			return err
		}
	}
	if f.Stage == "" || len(f.Stage) > 128 || !utf8.ValidString(f.Stage) {
		return errors.New("invalid attempt stage")
	}
	for _, r := range f.Stage {
		if !unicode.IsLower(r) && r != '-' && r != '_' {
			return errors.New("invalid attempt stage")
		}
	}
	if f.Message == "" || len(f.Message) > 512 || !utf8.ValidString(f.Message) {
		return errors.New("invalid attempt message")
	}
	for _, r := range f.Message {
		if unicode.IsControl(r) {
			return errors.New("attempt message contains control character")
		}
	}
	return nil
}

// FailureHook belongs to the immutable schedule header, never the task request.
type FailureHook struct {
	Executable string        `json:"executable"`
	Threshold  int64         `json:"threshold"`
	Timeout    time.Duration `json:"timeout_nanos"`
}

func ValidateFailureHook(h *FailureHook) error {
	if h == nil {
		return nil
	}
	if !filepath.IsAbs(h.Executable) || filepath.Clean(h.Executable) != h.Executable || len(h.Executable) > 4096 {
		return errors.New("failure hook requires a clean absolute executable path")
	}
	for _, r := range h.Executable {
		if unicode.IsControl(r) {
			return errors.New("invalid failure hook path")
		}
	}
	if h.Threshold < 1 {
		return errors.New("failure threshold must be positive")
	}
	if h.Timeout <= 0 || h.Timeout > time.Minute {
		return errors.New("failure hook timeout must be positive and at most 1m")
	}
	return nil
}

// ResultFact is a derived projection of validated local evidence. Unknown facts
// may later resolve; the task outcome remains authoritative.
type ResultFact struct {
	TaskID         string
	ObservedAt     time.Time
	Verdict        string
	Code           string
	OutcomeSHA256  string
	QuotaNotBefore time.Time
}

func ValidateResultFact(f ResultFact) error {
	if err := validateScheduleID(f.TaskID); err != nil {
		return err
	}
	if err := ValidateInstant("result observation", f.ObservedAt); err != nil {
		return err
	}
	switch f.Verdict {
	case "unknown", "committed", "rejected", "not_created", "closed_without_outcome":
	default:
		return errors.New("invalid schedule result verdict")
	}
	if f.Code != "" {
		if err := task.ValidateOutputDiagnostic(task.OutputDiagnostic{Code: f.Code, Message: "Schedule occurrence did not commit."}); err != nil {
			return err
		}
	}
	if f.Verdict == "committed" || f.Verdict == "rejected" {
		if err := task.ValidateSHA256(f.OutcomeSHA256); err != nil {
			return fmt.Errorf("terminal projection outcome: %w", err)
		}
	}
	if !f.QuotaNotBefore.IsZero() {
		if f.Verdict != "rejected" || f.Code != "provider_quota_exhausted" {
			return errors.New("quota advice requires quota rejection")
		}
		return ValidateInstant("quota reset", f.QuotaNotBefore)
	}
	return nil
}

type QuotaPolicy struct {
	Enabled  bool          `json:"enabled"`
	MaxHold  time.Duration `json:"max_hold_nanos"`
	Revision int64         `json:"revision"`
}

func ValidateQuotaPolicy(p QuotaPolicy) error {
	if p.MaxHold <= 0 || p.MaxHold > 7*24*time.Hour {
		return errors.New("quota maximum hold must be positive and at most 168h")
	}
	if p.Revision < 0 {
		return errors.New("invalid quota policy revision")
	}
	return nil
}
func DefaultQuotaPolicy() QuotaPolicy { return QuotaPolicy{MaxHold: 6 * time.Hour} }

type QuotaHold struct {
	SourceTaskID string    `json:"source_task_id"`
	Reason       string    `json:"reason"`
	CreatedAt    time.Time `json:"created_at"`
	Until        time.Time `json:"until"`
	Capped       bool      `json:"capped"`
}

type HealthOccurrence struct {
	TaskID     string     `json:"task_id"`
	Due        time.Time  `json:"due"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
}

// Health is a passive view of derived evidence, including its coverage gaps.
type Health struct {
	ScheduleID              string            `json:"schedule_id"`
	LastOccurrence          *HealthOccurrence `json:"last_occurrence,omitempty"`
	LastCommitted           *HealthOccurrence `json:"last_committed,omitempty"`
	ConsecutiveNonCommitted int64             `json:"consecutive_non_committed"`
	DominantFailureCode     string            `json:"dominant_failure_code,omitempty"`
	Complete                bool              `json:"complete"`
	Unresolved              int64             `json:"unresolved"`
	Hold                    *QuotaHold        `json:"hold,omitempty"`
	HoldDeferred            bool              `json:"hold_deferred"`
	LastNotification        *Notification     `json:"last_notification,omitempty"`
}

type Notification struct {
	EventID      string    `json:"event_id"`
	ScheduleID   string    `json:"schedule_id"`
	Kind         string    `json:"kind"`
	AnchorTaskID string    `json:"anchor_task_id"`
	LastTaskID   string    `json:"last_task_id"`
	StreakCount  int64     `json:"streak_count"`
	FailureCode  string    `json:"failure_code,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	Result       string    `json:"result"`
}

// ReliabilityStore owns durable operational projections. It is separate from
// Store so existing minimal scheduler consumers need not implement new policy.
type ReliabilityStore interface {
	RecordAttempt(context.Context, string, AttemptFailure) error
	RecordResult(context.Context, ResultFact) error
	ReconciliationPage(context.Context, string, string, int) ([]Occurrence, bool, error)
	Health(context.Context, string, time.Time) (Health, error)
	QuotaPolicy(context.Context, string) (QuotaPolicy, error)
	SetQuotaPolicy(context.Context, string, QuotaPolicy) error
	QuotaHold(context.Context, string) (*QuotaHold, error)
	ConsiderQuotaHold(context.Context, string, time.Time) error
	ReconsiderQuotaHolds(context.Context, string, time.Time) error
	ResumeWithHold(context.Context, string, time.Time) error
	ClaimNotification(context.Context, string, string, time.Time) (*Notification, error)
	FinishNotification(context.Context, string, string) error
}

func ValidateNotificationResult(result string) error {
	switch result {
	case "succeeded", "failed", "timed_out", "unavailable", "uncertain":
		return nil
	default:
		return fmt.Errorf("invalid notification result %q", result)
	}
}

// PlanOperationalResume preserves a forward-only cursor while accounting once
// for the paused or quota-held interval. A clock rollback never invents a span.
func PlanOperationalResume(state State, cursor, now time.Time, hold *QuotaHold) (*SkipSpan, error) {
	if err := ValidateState(state); err != nil {
		return nil, err
	}
	if err := ValidateInstant("resume cursor", cursor); err != nil {
		return nil, err
	}
	if err := ValidateInstant("resume now", now); err != nil {
		return nil, err
	}
	if state == StateDeleted {
		return nil, ErrConflict
	}
	if state == StateEnabled && hold == nil {
		return nil, ErrConflict
	}
	end := now
	reason := SkipPaused
	if state == StateEnabled {
		reason = SkipQuotaExhausted
		if hold.Until.Before(end) {
			end = hold.Until
		}
	}
	if !end.After(cursor) {
		return nil, nil
	}
	return &SkipSpan{From: cursor, To: end, Reason: reason}, nil
}
