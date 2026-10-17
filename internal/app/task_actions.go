package app

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func cancel(a Arguments, deps Dependencies) (result commandResult) {
	response := newResponse("cancel")
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return failed(response, err, 1)
	}
	store, err := openStore(root, deps.storeDependencies(), false)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, store.Close) }()
	td, err := store.OpenTask(a.TaskID)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, td.Close) }()
	response.RootID, response.TaskID = store.RootID, a.TaskID
	observed, err := tasklifecycle.Cancel(root, td, deps.SupervisorOptions, deps.InitialSupervisorExecutable, newSupervisorControlClient)
	fillInspection(&response, observed.Inspection)
	if observed.Stop != nil {
		stop := stopOutcomeResponse(*observed.Stop)
		response.Stop = &stop
	}
	if observed.StopsRead {
		response.Stops = stopResponses(observed.Stops)
	}
	if err != nil {
		code := 1
		if observed.Failure == tasklifecycle.CancellationFailureSubmissionRead {
			code = classifyCode(err, code)
		}
		return failed(response, err, code)
	}
	return commandResult{response: response, code: 0}
}

func logs(a Arguments, deps storeDependencies) (result commandResult) {
	response := newResponse("logs")
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return failed(response, err, 1)
	}
	store, err := openStore(root, deps, false)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, store.Close) }()
	td, err := store.OpenTask(a.TaskID)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, td.Close) }()
	response.RootID, response.TaskID = store.RootID, a.TaskID
	inspection, descriptors, err := tasklifecycle.ReadLogs(td)
	fillInspection(&response, inspection)
	response.Raw = descriptors
	if err != nil {
		return failed(response, err, 1)
	}
	return commandResult{response: response, code: 0}
}

func recheck(a Arguments, deps storeDependencies) (result commandResult) {
	response := newResponse("recheck")
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return failed(response, err, 2)
	}
	store, err := openStore(root, deps, false)
	if err != nil {
		code := 1
		if errors.Is(err, task.ErrUnsupportedSchema) {
			code = 2
		}
		return failed(response, err, code)
	}
	defer func() { mergeCommandClose(&result, store.Close) }()
	td, err := store.OpenTask(a.TaskID)
	if err != nil {
		code := 1
		if errors.Is(err, os.ErrNotExist) {
			code = 2
		}
		return failed(response, err, code)
	}
	defer func() { mergeCommandClose(&result, td.Close) }()
	response.RootID, response.TaskID = store.RootID, a.TaskID
	return recheckTask(response, td, deps.registry())
}

func recheckTask(response Response, td *taskdir.TaskDir, registry predicate.Registry) commandResult {
	result, err := tasklifecycle.Recheck(td, registry)
	fillInspection(&response, result.Inspection)
	if err != nil {
		return failed(response, err, recheckFailureCode(result.Stage, err))
	}
	assessment := result.Assessment
	response.Recheck = recheckResponse(assessment, td.Dir)
	response.Error = ""
	response.Failure = nil
	response.Diagnostics = nil
	applyOutputDiagnostics(&response, assessment.Report.Diagnostics)
	if assessment.Report.Status == task.RecheckStatusRecovered {
		return commandResult{response: response, code: 0}
	}
	return commandResult{response: response, code: 4}
}

func recheckFailureCode(stage tasklifecycle.RecheckStage, err error) int {
	switch stage {
	case tasklifecycle.RecheckEligibility, tasklifecycle.RecheckInterpreter:
		return 2
	case tasklifecycle.RecheckAssessment:
		if errors.Is(err, predicate.ErrRecoveryUnsupported) || errors.Is(err, predicate.ErrRecoveryAmbiguous) || errors.Is(err, task.ErrRecheckNotRejected) || errors.Is(err, task.ErrUnsupportedSchema) {
			return 2
		}
	case tasklifecycle.RecheckInspection:
		if errors.Is(err, task.ErrUnsupportedSchema) {
			return 2
		}
	}
	return 1
}

func recheckResponse(result *taskdir.RecheckResult, taskPath string) *RecheckResponse {
	if result == nil || result.Report == nil {
		return nil
	}
	report := result.Report
	response := &RecheckResponse{
		Status:               report.Status,
		AssessmentID:         report.AssessmentID,
		SourceOutcomeSHA256:  report.SourceOutcomeSHA256,
		SourceSealSHA256:     report.SourceSealSHA256,
		SourceEvidenceSHA256: report.SourceEvidenceSHA256,
		SourcePredicate:      report.SourcePredicate,
		Target:               report.TargetPredicate,
		ReportPath:           filepath.Join(taskPath, result.ReportPath),
		Diagnostics:          task.CloneOutputDiagnostics(report.Diagnostics),
	}
	if report.Result != nil {
		answer := *report.Result
		response.Answer = &answer
		response.AnswerPath = filepath.Join(taskPath, result.AnswerPath)
	}
	return response
}
