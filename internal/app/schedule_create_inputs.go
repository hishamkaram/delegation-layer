package app

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// scheduleCreateInput owns one captured creation request. Recurrence resolution
// later supplies the stored timezone; requested options are not normalized.
type scheduleCreateInput struct {
	Root        string
	ID          string
	CreatedAt   time.Time
	Cron        string
	Timezone    string
	TimezoneSet bool
	Request     contract.Request
	Brief       []byte
}

var errScheduleInput = errors.New("invalid schedule request")

func invalidScheduleInput(err error) error {
	return fmt.Errorf("%w: %w", errScheduleInput, err)
}

// prepareScheduleCreate validates only input and filesystem boundaries. The
// caller supplies clock and ID sources; neither providers nor storage are used.
func prepareScheduleCreate(a Arguments, now func() time.Time, newID func() (string, error)) (scheduleCreateInput, error) {
	if err := validateScheduleCreateShape(a); err != nil {
		return scheduleCreateInput{}, err
	}
	if err := a.applyReviewTarget(); err != nil {
		return scheduleCreateInput{}, invalidScheduleInput(err)
	}
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return scheduleCreateInput{}, err
	}
	root, cwd, err := config.ValidateDirectories(root, a.Cwd)
	if err != nil {
		return scheduleCreateInput{}, classifyScheduleDirectoryError(err)
	}
	request := contract.Request{Provider: a.Provider, Auto: a.Auto, CanonicalCwd: cwd, Config: a.Config}
	if request.Config.ReviewTarget != nil {
		target := *request.Config.ReviewTarget
		request.Config.ReviewTarget = &target
	}
	if _, err = contract.MarshalRequest(request); err != nil {
		return scheduleCreateInput{}, invalidScheduleInput(err)
	}
	brief, err := readScheduleBrief(a.Brief, tasklifecycle.ReadBrief)
	if err != nil {
		return scheduleCreateInput{}, err
	}
	id, createdAt, err := captureScheduleIdentity(a.ScheduleID, now, newID)
	if err != nil {
		return scheduleCreateInput{}, err
	}
	return scheduleCreateInput{
		Root: root, ID: id, CreatedAt: createdAt, Cron: a.ScheduleCron,
		Timezone: a.ScheduleTimezone, TimezoneSet: a.ScheduleTimezoneSet, Request: request, Brief: brief,
	}, nil
}

func validateScheduleCreateShape(a Arguments) error {
	if a.Brief == "" || !filepath.IsAbs(a.Cwd) {
		return invalidScheduleInput(errors.New("schedule create requires --brief FILE and --cwd ABS"))
	}
	if a.ScheduleCron == "" {
		return invalidScheduleInput(errors.New("schedule create requires --cron RULE"))
	}
	if a.ScheduleTimezoneSet && a.ScheduleTimezone == "" {
		return invalidScheduleInput(errors.New("schedule --timezone must not be empty"))
	}
	return nil
}

func classifyScheduleDirectoryError(err error) error {
	if !errors.Is(err, fs.ErrPermission) && (errors.Is(err, config.ErrInvalidCwd) || errors.Is(err, config.ErrRootWorkspaceOverlap)) {
		return invalidScheduleInput(err)
	}
	return err
}

func readScheduleBrief(path string, readBrief func(string) ([]byte, error)) ([]byte, error) {
	briefPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	length, err := config.ValidateBriefFile(briefPath)
	if err != nil {
		return nil, classifyScheduleBriefError(err)
	}
	if length == 0 {
		return nil, invalidScheduleInput(config.ErrBriefEmpty)
	}
	brief, err := readBrief(briefPath)
	return brief, classifyScheduleBriefError(err)
}

func classifyScheduleBriefError(err error) error {
	if scheduleBriefInputError(err) {
		return invalidScheduleInput(err)
	}
	return err
}

// A joined storage or cleanup fault remains operational even beside a known
// invalid-brief marker. Wrappers and every cause are kept in the returned error.
func scheduleBriefInputError(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !scheduleBriefInputError(cause) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return scheduleBriefInputError(wrapped.Unwrap())
	}
	return errors.Is(err, config.ErrBriefNotFound) || errors.Is(err, config.ErrBriefNotRegular) || errors.Is(err, config.ErrBriefTooLarge) || errors.Is(err, config.ErrBriefEmpty) || errors.Is(err, task.ErrControlRecordTooBig)
}

func captureScheduleIdentity(requested string, now func() time.Time, newID func() (string, error)) (string, time.Time, error) {
	id := requested
	if id == "" {
		var err error
		id, err = newID()
		if err != nil {
			return "", time.Time{}, fmt.Errorf("allocate schedule ID: %w", err)
		}
	}
	if err := task.ValidateTaskID(id); err != nil {
		return "", time.Time{}, invalidScheduleInput(err)
	}
	createdAt := now().UTC()
	if err := contract.ValidateInstant("schedule created_at", createdAt); err != nil {
		return "", time.Time{}, err
	}
	return id, createdAt, nil
}
