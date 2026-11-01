package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/teams"
	"github.com/hishamkaram/delegation-layer/internal/teams/contract"
)

// TeamsResponse describes configuration and mutation evidence, never readiness.
// It is separate from task responses because management has no task lifecycle.
type TeamsResponse struct {
	SchemaVersion  int                   `json:"schema_version"`
	Command        string                `json:"command"`
	Action         string                `json:"action"`
	DefaultAction  string                `json:"default_action,omitempty"`
	Status         string                `json:"status"`
	CatalogID      string                `json:"catalog_id,omitempty"`
	Team           *contract.Record      `json:"team,omitempty"`
	Teams          *[]TeamsListRow       `json:"teams,omitempty"`
	NextCursor     string                `json:"next_cursor,omitempty"`
	Mutation       *TeamsMutation        `json:"mutation,omitempty"`
	Default        *TeamsDefaultResponse `json:"default,omitempty"`
	Diagnostics    []TeamsDiagnostic     `json:"diagnostics,omitempty"`
	Failure        *TeamsFailure         `json:"failure,omitempty"`
	Error          string                `json:"error,omitempty"`
	ErrorTruncated bool                  `json:"error_truncated,omitempty"`
}

type TeamsListRow struct {
	Reference   task.TeamReference `json:"reference"`
	Revision    int64              `json:"revision"`
	Name        string             `json:"name"`
	Purpose     string             `json:"purpose"`
	RoleCount   int                `json:"role_count"`
	MemberCount int                `json:"member_count"`
	Truncated   bool               `json:"truncated,omitempty"`
}

type TeamsMutation struct {
	Operation  string              `json:"operation"`
	Reference  *task.TeamReference `json:"reference,omitempty"`
	Revision   int64               `json:"revision,omitempty"`
	Validation *TeamsValidation    `json:"validation,omitempty"`
}

type TeamsValidation struct {
	Definition           string `json:"definition"`
	ProviderCapabilities string `json:"provider_capabilities"`
	Readiness            string `json:"readiness"`
}

type TeamsDefaultResponse struct {
	State     string              `json:"state"`
	Reference *task.TeamReference `json:"reference,omitempty"`
	Team      *contract.Record    `json:"team,omitempty"`
}

type TeamsDiagnostic struct {
	Code      string              `json:"code"`
	Message   string              `json:"message"`
	Reference *task.TeamReference `json:"reference,omitempty"`
}

type TeamsFailure struct {
	Code       string `json:"code"`
	NextAction string `json:"next_action"`
}

func teamsListCursor(ref task.TeamReference) string { return ref.Scope + ":" + ref.TeamID }

func teamsListRow(record contract.Record) TeamsListRow {
	name, nameTruncated := truncateDiagnostic(record.Definition.Name, 256)
	purpose, purposeTruncated := truncateDiagnostic(record.Definition.Purpose, 1024)
	return TeamsListRow{
		Reference: record.Reference.Clone(), Revision: record.Revision,
		Name: name, Purpose: purpose, RoleCount: len(record.Definition.Roles), MemberCount: len(record.Definition.Members),
		Truncated: nameTruncated || purposeTruncated,
	}
}

func teamsDiagnostics(diagnostics []teams.Diagnostic) []TeamsDiagnostic {
	const maxDiagnostics = 100
	result := make([]TeamsDiagnostic, 0, min(len(diagnostics), maxDiagnostics)+1)
	for _, diagnostic := range diagnostics[:min(len(diagnostics), maxDiagnostics)] {
		message, _ := truncateDiagnostic(diagnostic.Message, 1024)
		result = append(result, TeamsDiagnostic{Code: diagnostic.Code, Message: message, Reference: diagnostic.Reference})
	}
	if len(diagnostics) > maxDiagnostics {
		result = append(result, TeamsDiagnostic{Code: "diagnostics_truncated", Message: fmt.Sprintf("%d additional invalid-record diagnostics omitted.", len(diagnostics)-maxDiagnostics)})
	}
	return result
}

func readTeamsDefinition(path string, input io.Reader) (definition contract.Team, err error) {
	if path != "-" {
		file, openErr := os.Open(path)
		if openErr != nil {
			return definition, fmt.Errorf("opening Team definition: %w", openErr)
		}
		defer func() { err = errors.Join(err, file.Close()) }()
		input = file
	}
	if input == nil {
		return definition, fmt.Errorf("%w: stdin is unavailable; supply Input or a definition file", contract.ErrInvalidDefinition)
	}
	data, err := io.ReadAll(io.LimitReader(input, task.MaxControlRecordSize+1))
	if err != nil {
		return definition, fmt.Errorf("reading Team definition: %w", err)
	}
	if len(data) > task.MaxControlRecordSize {
		return definition, fmt.Errorf("%w: %w", contract.ErrInvalidDefinition, task.ErrControlRecordTooBig)
	}
	definition, err = contract.Decode(data)
	if err != nil {
		return definition, &teamsInputError{cause: err}
	}
	return definition, nil
}

func writeTeamsResponse(output io.Writer, response TeamsResponse, asJSON bool) error {
	var buffer bytes.Buffer
	if asJSON {
		encoder := json.NewEncoder(&buffer)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(response); err != nil {
			return err
		}
	} else {
		writeHumanTeams(&buffer, response)
	}
	// A complete definition can occupy the whole control-record bound. Preserve
	// it and its typed envelope within the same extended bound as schedule show.
	if buffer.Len() > 2<<20 {
		return ErrJSONResponseTooLarge
	}
	return writeAll(output, buffer.Bytes())
}

func writeHumanTeams(output *bytes.Buffer, response TeamsResponse) {
	fmt.Fprintf(output, "teams action=%s status=%s", response.Action, response.Status)
	if response.DefaultAction != "" {
		fmt.Fprintf(output, " default_action=%s", response.DefaultAction)
	}
	output.WriteByte('\n')
	if response.Team != nil {
		writeHumanTeam(output, *response.Team)
	}
	if response.Teams != nil {
		for _, row := range *response.Teams {
			fmt.Fprintf(output, "team scope=%s id=%s revision=%d name=%q roles=%d members=%d\n", row.Reference.Scope, row.Reference.TeamID, row.Revision, row.Name, row.RoleCount, row.MemberCount)
		}
	}
	if response.Mutation != nil {
		fmt.Fprintf(output, "mutation=%s revision=%d\n", response.Mutation.Operation, response.Mutation.Revision)
		if response.Mutation.Validation != nil {
			output.WriteString("definition and static provider capabilities validated; provider readiness not checked\n")
		}
	}
	if response.Default != nil {
		fmt.Fprintf(output, "default=%s\n", response.Default.State)
		if response.Default.Team != nil {
			writeHumanTeam(output, *response.Default.Team)
		}
	}
	if response.NextCursor != "" {
		fmt.Fprintf(output, "next_cursor=%s\n", response.NextCursor)
	}
	for _, diagnostic := range response.Diagnostics {
		fmt.Fprintf(output, "diagnostic code=%s message=%q\n", diagnostic.Code, diagnostic.Message)
	}
	if response.Failure != nil {
		fmt.Fprintf(output, "error code=%s next_action=%s message=%q\n", response.Failure.Code, response.Failure.NextAction, response.Error)
	}
}

func writeHumanTeam(output *bytes.Buffer, record contract.Record) {
	fmt.Fprintf(output, "team scope=%s id=%s revision=%d name=%q purpose=%q\n", record.Reference.Scope, record.Reference.TeamID, record.Revision, record.Definition.Name, record.Definition.Purpose)
	for _, role := range record.Definition.Roles {
		fmt.Fprintf(output, "role id=%s name=%q responsibility=%q\n", role.ID, role.Name, role.Responsibility)
	}
	for _, member := range record.Definition.Members {
		fmt.Fprintf(output, "member id=%s name=%q roles=%s", member.ID, member.Name, strings.Join(member.RoleIDs, ","))
		if defaults := member.ExecutionDefaults; defaults != nil {
			fmt.Fprintf(output, " provider=%q model=%q effort=%q", defaults.Provider, defaults.Model, defaults.Effort)
		}
		output.WriteByte('\n')
	}
}
