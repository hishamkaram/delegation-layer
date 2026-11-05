package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/teams"
	"github.com/hishamkaram/delegation-layer/internal/teams/contract"
)

const teamsOperationTimeout = 10 * time.Second

func runTeams(a Arguments, deps Dependencies, stdout, stderr io.Writer) int {
	deps = deps.withCapturedEnvironment()
	ctx, cancel := context.WithTimeout(context.Background(), teamsOperationTimeout)
	defer cancel()
	response, resultErr := teamsCommand(ctx, a, deps)
	exit := TeamsErrorExitCode(resultErr)
	if resultErr != nil {
		response.Status = "failed"
		response.Error, response.ErrorTruncated = truncateDiagnostic(resultErr.Error(), 4096)
		response.Failure = teamsFailure(resultErr)
	}
	output := stdout
	if resultErr != nil && !a.JSON {
		output = stderr
	}
	if err := writeTeamsResponse(output, response, a.JSON); err != nil {
		if response.Mutation != nil && resultErr == nil {
			if reportErr := reportTeamsMutationOutputFailure(stderr, response, err); reportErr != nil {
				return 1
			}
		} else if _, reportErr := fmt.Fprintf(stderr, "writing Team response: %v\n", err); reportErr != nil {
			return 1
		}
		return 1
	}
	return exit
}

func reportTeamsMutationOutputFailure(stderr io.Writer, response TeamsResponse, outputErr error) error {
	mutation := response.Mutation
	scope, id := "", ""
	if mutation.Reference != nil {
		scope, id = mutation.Reference.Scope, mutation.Reference.TeamID
	}
	_, err := fmt.Fprintf(stderr, "Team mutation succeeded: operation=%s scope=%s team=%s revision=%d catalog_id=%s. Output failed: %v. Inspect with teams show or teams default show using the same scope, Team ID and project context; do not repeat the mutation.\n", mutation.Operation, scope, id, mutation.Revision, response.CatalogID, outputErr)
	return err
}

func teamsCommand(ctx context.Context, a Arguments, deps Dependencies) (TeamsResponse, error) {
	response := TeamsResponse{SchemaVersion: OutputSchemaVersion, Command: "teams", Action: a.TeamAction, DefaultAction: a.TeamDefaultAction, Status: "ok"}
	if err := validateTeamsArguments(a); err != nil {
		return response, &teamsInputError{cause: err}
	}
	if a.TeamAction == "register" || a.TeamAction == "update" {
		return saveTeamsCommand(ctx, a, deps, response)
	}
	catalog, err := catalogForTeams(ctx, deps)
	if err != nil {
		return response, err
	}
	if a.TeamAction == "list" {
		return listTeamsCommand(ctx, a, deps, catalog, response)
	}
	if a.TeamAction == "default" {
		return defaultTeamsCommand(ctx, a, deps, catalog, response)
	}
	ref, err := teamsCommandReference(ctx, a.TeamScope, a.TeamID, a, deps)
	if err != nil {
		return response, err
	}
	switch a.TeamAction {
	case "show":
		record, id, lookupErr := catalog.Lookup(ctx, ref)
		if lookupErr != nil {
			return response, lookupErr
		}
		response.CatalogID, response.Team = id, &record
	case "delete":
		revision, deleteErr := catalog.Delete(ctx, ref, a.TeamExpectedRevision)
		if deleteErr != nil {
			return response, deleteErr
		}
		response.Mutation = &TeamsMutation{Operation: "delete", Reference: &ref, Revision: revision}
	default:
		return response, fmt.Errorf("%w: unknown teams action", task.ErrInvalidTeamSelection)
	}
	return response, nil
}

func saveTeamsCommand(ctx context.Context, a Arguments, deps Dependencies, response TeamsResponse) (TeamsResponse, error) {
	definition, err := readTeamsDefinition(a.TeamDefinition, deps.Input)
	if err != nil {
		return response, err
	}
	id := a.TeamID
	if a.TeamAction == "register" {
		id = definition.ID
	}
	if definition.ID != id {
		return response, fmt.Errorf("%w: definition ID must match --team", contract.ErrInvalidDefinition)
	}
	ref, err := teamsCommandReference(ctx, a.TeamScope, id, a, deps)
	if err != nil {
		return response, err
	}
	catalog, err := catalogForTeams(ctx, deps)
	if err != nil {
		return response, err
	}
	var record contract.Record
	var catalogID string
	if a.TeamAction == "register" {
		record, catalogID, err = catalog.Register(ctx, ref, definition)
	} else {
		record, catalogID, err = catalog.Update(ctx, ref, a.TeamExpectedRevision, definition)
	}
	if err != nil {
		return response, err
	}
	response.CatalogID = catalogID
	response.Mutation = &TeamsMutation{
		Operation: a.TeamAction, Reference: &ref, Revision: record.Revision,
		Validation: &TeamsValidation{Definition: "validated", ProviderCapabilities: "validated", Readiness: "not_checked"},
	}
	return response, nil
}

func teamsCommandReference(ctx context.Context, scope, id string, a Arguments, deps Dependencies) (task.TeamReference, error) {
	ref := task.TeamReference{Scope: scope, TeamID: id}
	if scope == "project" {
		project, err := resolveTeamsProject(ctx, a.Cwd, a.ProjectRoot, deps)
		if err != nil {
			return ref, err
		}
		ref.Project = &project
	}
	return ref, task.ValidateTeamReference(ref)
}

func listTeamsCommand(ctx context.Context, a Arguments, deps Dependencies, catalog *teams.Catalog, response TeamsResponse) (TeamsResponse, error) {
	var project *task.TeamProject
	if a.Cwd != "" {
		resolved, err := resolveTeamsProject(ctx, a.Cwd, a.ProjectRoot, deps)
		if err != nil {
			message, _ := truncateDiagnostic(err.Error(), 768)
			response.Diagnostics = append(response.Diagnostics, TeamsDiagnostic{Code: "project_context", Message: "Only user Teams are listed; project context failed: " + message})
		} else {
			project = &resolved
		}
	}
	listing, err := catalog.List(ctx, project)
	if err != nil {
		return response, err
	}
	response.CatalogID = listing.CatalogID
	response.Diagnostics = append(response.Diagnostics, teamsDiagnostics(listing.Diagnostics)...)
	slices.SortFunc(listing.Records, func(left, right contract.Record) int {
		return strings.Compare(teamsListCursor(left.Reference), teamsListCursor(right.Reference))
	})
	rows := make([]TeamsListRow, 0, a.TeamLimit)
	for _, record := range listing.Records {
		if teamsListCursor(record.Reference) <= a.TeamAfter {
			continue
		}
		if len(rows) == a.TeamLimit {
			response.NextCursor = teamsListCursor(rows[len(rows)-1].Reference)
			break
		}
		rows = append(rows, teamsListRow(record))
	}
	response.Teams = &rows
	if len(rows) == 0 {
		response.Status = "empty"
	}
	if len(response.Diagnostics) > 0 {
		response.Status = "partial"
	}
	return response, nil
}

func defaultTeamsCommand(ctx context.Context, a Arguments, deps Dependencies, catalog *teams.Catalog, response TeamsResponse) (TeamsResponse, error) {
	project, err := resolveTeamsProject(ctx, a.Cwd, a.ProjectRoot, deps)
	if err != nil {
		return response, err
	}
	switch a.TeamDefaultAction {
	case "show":
		result, readErr := catalog.Default(ctx, project)
		if readErr != nil {
			return response, readErr
		}
		response.Default = &TeamsDefaultResponse{State: result.Status, Reference: result.Reference, Team: result.Record}
		if result.Status == "stale" {
			response.Status = "partial"
			response.Diagnostics = []TeamsDiagnostic{{Code: "stale_default", Message: "The project default references a deleted Team; explicitly set or clear it.", Reference: result.Reference}}
		}
	case "set":
		ref := task.TeamReference{Scope: a.TeamScope, TeamID: a.TeamID}
		if ref.Scope == "project" {
			ref.Project = &project
		}
		if err = catalog.SetDefault(ctx, project, ref); err != nil {
			return response, err
		}
		response.Default = &TeamsDefaultResponse{State: "set", Reference: &ref}
		response.Mutation = &TeamsMutation{Operation: "default_set", Reference: &ref}
	case "clear":
		if err = catalog.ClearDefault(ctx, project); err != nil {
			return response, err
		}
		response.Default = &TeamsDefaultResponse{State: "unset"}
		response.Mutation = &TeamsMutation{Operation: "default_clear"}
	default:
		return response, fmt.Errorf("%w: unknown teams default action", task.ErrInvalidTeamSelection)
	}
	return response, nil
}

// teamsInputError marks failures from an already completed pure validation.
// Its cause cannot contain I/O; transport and catalog failures stay outside it.
type teamsInputError struct{ cause error }

func (err *teamsInputError) Error() string   { return err.cause.Error() }
func (err *teamsInputError) Unwrap() error   { return err.cause }
func (err *teamsInputError) teamsPureInput() {}

// TeamsErrorExitCode accepts only errors whose entire tree is known input
// failure. Any joined I/O or unrecognized fault retains operational exit 1.
func TeamsErrorExitCode(err error) int {
	if err == nil {
		return 0
	}
	// Match this node's private marker only; searching descendants with As
	// would let a pure child hide an unrelated joined I/O sibling.
	if _, pure := err.(interface{ teamsPureInput() }); pure {
		return 2
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return 1
		}
		for _, child := range children {
			if TeamsErrorExitCode(child) != 2 {
				return 1
			}
		}
		return 2
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		cause := wrapped.Unwrap()
		if cause == nil {
			return 1
		}
		return TeamsErrorExitCode(cause)
	}
	for _, inputError := range []error{contract.ErrInvalidDefinition, task.ErrInvalidTeamSelection, task.ErrControlRecordTooBig, teams.ErrNotFound, teams.ErrConflict, teams.ErrCatalogIdentity, teams.ErrProjectContext, teams.ErrProjectMismatch} {
		if errors.Is(err, inputError) {
			return 2
		}
	}
	return 1
}

func teamsFailure(err error) *TeamsFailure {
	failure := &TeamsFailure{Code: "catalog_io", NextAction: "inspect_before_retry"}
	if TeamsErrorExitCode(err) == 1 {
		if errors.Is(err, teams.ErrSchema) {
			failure.Code = "catalog_schema"
		}
		return failure
	}
	failure.Code, failure.NextAction = "invalid_input", "correct_request"
	for _, item := range []struct {
		err  error
		code string
	}{{teams.ErrNotFound, "not_found"}, {teams.ErrConflict, "revision_conflict"}, {teams.ErrCatalogIdentity, "catalog_identity"}, {teams.ErrProjectMismatch, "project_mismatch"}, {teams.ErrProjectContext, "project_context"}, {contract.ErrInvalidDefinition, "invalid_definition"}} {
		if errors.Is(err, item.err) {
			failure.Code = item.code
			break
		}
	}
	if failure.Code == "revision_conflict" {
		failure.NextAction = "inspect_current_revision"
	}
	return failure
}
