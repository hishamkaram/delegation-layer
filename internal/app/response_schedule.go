package app

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
)

// ScheduleListRow deliberately excludes the unbounded requested options.
type ScheduleListRow struct {
	ID        string         `json:"id"`
	State     contract.State `json:"state"`
	CreatedAt time.Time      `json:"created_at"`
	Cursor    time.Time      `json:"cursor"`
	Cron      string         `json:"cron"`
	Timezone  string         `json:"timezone"`
}

// ScheduleResponse is an acknowledgement or a complete, brief-free show.
type ScheduleResponse struct {
	ID                string                `json:"id"`
	Saved             bool                  `json:"saved,omitempty"`
	State             contract.State        `json:"state,omitempty"`
	CreatedAt         *time.Time            `json:"created_at,omitempty"`
	Cursor            *time.Time            `json:"cursor,omitempty"`
	Cron              string                `json:"cron,omitempty"`
	Timezone          string                `json:"timezone,omitempty"`
	Request           *contract.Request     `json:"request,omitempty"`
	Hook              *contract.FailureHook `json:"hook,omitempty"`
	BriefLength       int                   `json:"brief_length,omitempty"`
	BriefSHA256       string                `json:"brief_sha256,omitempty"`
	DefinitionSHA256  string                `json:"definition_sha256,omitempty"`
	DefinitionMatched bool                  `json:"definition_matched,omitempty"`
	NextAction        string                `json:"next_action,omitempty"`
}

type ScheduleWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func scheduleListRow(item contract.ScheduleSummary) ScheduleListRow {
	return ScheduleListRow{ID: item.Header.ScheduleID, State: item.State, CreatedAt: item.Header.CreatedAt, Cursor: item.Cursor, Cron: item.Header.Cron, Timezone: item.Header.Timezone}
}

func scheduleShow(item contract.Schedule) (ScheduleResponse, error) {
	digests, err := contract.DefinitionDigests(item.Definition)
	if err != nil {
		return ScheduleResponse{}, err
	}
	definition := item.Definition
	return ScheduleResponse{
		ID: definition.ScheduleID, State: item.State, CreatedAt: &definition.CreatedAt,
		Cursor: &item.Cursor, Cron: definition.Cron, Timezone: definition.Timezone,
		Request: &definition.Request, Hook: definition.Hook, BriefLength: len(definition.Brief),
		BriefSHA256: digests.BriefSHA256, DefinitionSHA256: digests.DefinitionSHA256,
	}, nil
}

func writeHumanScheduleResponse(output *strings.Builder, response Response) error {
	writeHumanSchedulerResponse(output, response.Scheduler)
	writeHumanScheduleHistory(output, response.ScheduleHistory)
	writeHumanScheduleExecution(output, response.ScheduleExecution)
	if item := response.Schedule; item != nil {
		fmt.Fprintf(output, "schedule id=%s state=%s", item.ID, item.State)
		if item.Saved {
			output.WriteString(" saved=true")
		}
		if item.NextAction != "" {
			fmt.Fprintf(output, " next_action=%s", item.NextAction)
		}
		if item.DefinitionMatched {
			output.WriteString(" definition_matched=true")
		}
		output.WriteByte('\n')
		if item.Request != nil {
			encoded, err := json.Marshal(item.Request)
			if err != nil {
				return err
			}
			fmt.Fprintf(output, "created_at=%s cursor=%s cron=%q timezone=%q brief_length=%d brief_sha256=%s definition_sha256=%s\nrequest=%s\n", item.CreatedAt.Format(time.RFC3339Nano), item.Cursor.Format(time.RFC3339Nano), item.Cron, item.Timezone, item.BriefLength, item.BriefSHA256, item.DefinitionSHA256, encoded)
		}
	}
	if response.Schedules != nil {
		for _, item := range *response.Schedules {
			fmt.Fprintf(output, "schedule id=%s state=%s created_at=%s cursor=%s cron=%q timezone=%q\n", item.ID, item.State, item.CreatedAt.Format(time.RFC3339Nano), item.Cursor.Format(time.RFC3339Nano), item.Cron, item.Timezone)
		}
	}
	if response.ScheduleCursor != "" {
		fmt.Fprintf(output, "schedule_cursor=%s\n", response.ScheduleCursor)
	}
	for _, warning := range response.ScheduleWarnings {
		fmt.Fprintf(output, "warning code=%s message=%s\n", warning.Code, warning.Message)
	}
	return nil
}
