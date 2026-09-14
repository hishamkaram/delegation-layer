package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func dispatch(a Arguments, deps Dependencies) (result commandResult) {
	response := newResponse("dispatch")
	root, cwd, brief, err := dispatchInputs(a)
	if err != nil {
		return failed(response, err, classifyCode(err, 2))
	}
	store, err := openStore(root, deps.storeDependencies(), true)
	if err != nil {
		return failed(response, err, classifyCode(err, 1))
	}
	defer func() { mergeCommandClose(&result, store.Close) }()
	response.RootID = store.RootID
	id := a.TaskID
	if id == "" {
		id, err = task.NewTaskID()
		if err != nil {
			return failed(response, err, 1)
		}
	}
	prior := (*task.PriorSession)(nil)
	base, err := buildRequest(store.RootID, id, a.Provider, cwd, a.Config, brief, nil)
	if err != nil {
		return failed(response, err, classifyCode(err, 2))
	}
	if a.ResumeTask != "" {
		prior, err = resolvePredecessor(store, base, a.ResumeTask, deps.SupervisorOptions)
		if err != nil {
			return failed(response, err, classifyCode(err, 1))
		}
		base.PriorSession = prior
	}
	req := base
	response.TaskID = req.TaskID

	td, openErr := store.OpenTask(id)
	if openErr == nil {
		return dispatchExisting(a, deps, store, td, req, response)
	}
	if !errors.Is(openErr, os.ErrNotExist) {
		return failed(response, openErr, classifyCode(openErr, 1))
	}
	return dispatchNew(a, deps, store, req, brief, response)
}

func dispatchInputs(a Arguments) (root, cwd string, brief []byte, err error) {
	root, err = resolveRoot(a.Root)
	if err != nil {
		return "", "", nil, err
	}
	_, cwd, err = validateRootAndCwd(root, a.Cwd)
	if err != nil {
		return "", "", nil, err
	}
	briefPath, pathErr := filepath.Abs(a.Brief)
	if pathErr != nil {
		return "", "", nil, pathErr
	}
	brief, err = readBrief(briefPath)
	return root, cwd, brief, err
}

func dispatchExisting(a Arguments, deps Dependencies, store *taskdir.Store, td *taskdir.TaskDir, req task.TaskRecord, response Response) (result commandResult) {
	defer func() { mergeCommandClose(&result, td.Close) }()
	oldReq, oldMeta, err := td.PreparedRecords()
	if err == nil {
		if !sameRequest(&req, oldReq) {
			return failed(response, task.ErrRequestConflict, 2)
		}
		if a.PueueConfig != "" && a.PueueConfig != oldMeta.SupervisorConfig.ConfigPath {
			return failed(response, task.ErrRequestConflict, 2)
		}
		response.RootID, response.TaskID = oldReq.RootID, oldReq.TaskID
		inspection, inspectErr := td.Inspect()
		fillInspection(&response, inspection)
		if inspectErr != nil {
			return failed(response, inspectErr, classifyCode(inspectErr, 1))
		}
		if isTerminal(inspection) {
			if releaseErr := releaseContinuationSession(td, oldReq, inspection.Outcome); releaseErr != nil {
				return failed(response, releaseErr, 1)
			}
			return commandResult{response: response, code: 0}
		}
		submit, submitErr := td.ReadSubmission()
		if submitErr == nil {
			return reconcileExisting(td, oldReq, oldMeta, submit, deps.SupervisorOptions, response)
		}
		if !errors.Is(submitErr, os.ErrNotExist) {
			return failed(response, submitErr, classifyCode(submitErr, 1))
		}
		if inspection.StartExists {
			return failed(response, task.ErrAlreadyStarted, 1)
		}
		return submitPrepared(a, deps, td, oldReq, oldMeta, oldMeta.SupervisorConfig, response)
	}
	return dispatchNew(a, deps, store, req, nil, response)
}

func dispatchNew(a Arguments, deps Dependencies, store *taskdir.Store, req task.TaskRecord, brief []byte, response Response) commandResult {
	profile, err := prepareProfile(deps, req)
	if err != nil {
		return failed(response, err, classifyCode(err, 2))
	}
	supervisor, err := bindInitial(a, deps)
	if err != nil {
		return failed(response, err, classifyCode(err, 1))
	}
	if err = profile.Validate(req); err != nil {
		return failed(response, err, classifyCode(err, 2))
	}
	meta := newMeta(req, profile, supervisor.Binding(), deps.PublisherVersion)
	if brief == nil {
		brief, err = readBriefFileForRequest(a.Brief, req)
		if err != nil {
			return failed(response, err, classifyCode(err, 1))
		}
	}
	td, err := store.CreateTask(req.TaskID, &req, brief, &meta)
	if err != nil {
		return failed(response, err, classifyCode(err, 1))
	}
	return submitPreparedWithProfile(a, deps, td, &req, &meta, supervisor, profile, response)
}

func readBriefFileForRequest(path string, req task.TaskRecord) ([]byte, error) {
	briefPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	brief, err := readBrief(briefPath)
	if err != nil {
		return nil, err
	}
	if int64(len(brief)) != req.BriefLength || task.ComputeSHA256(brief) != req.BriefSHA256 {
		return nil, errors.New("brief changed after request preparation")
	}
	return brief, nil
}

func submitPrepared(a Arguments, deps Dependencies, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, supervisor task.SupervisorRef, response Response) (result commandResult) {
	client, err := pueue.NewClient(supervisor, deps.SupervisorOptions)
	if err != nil {
		return failed(response, err, classifyCode(err, 1))
	}
	runner, err := resolveRunner(a.Runner, deps)
	if err != nil {
		return failed(response, err, classifyCode(err, 2))
	}
	if req.PriorSession != nil {
		if err = td.ClaimSession(req.Provider, req.PriorSession.ConversationID); err != nil {
			return failed(response, err, classifyCode(err, 1))
		}
	}
	permit, err := td.PrepareSubmission(supervisor)
	if err != nil {
		return failed(response, err, classifyCode(err, 1))
	}
	defer func() { mergeCommandClose(&result, permit.Release) }()
	return submitWithClient(td, req, meta, client, runner, permit, response)
}

func submitPreparedWithProfile(a Arguments, deps Dependencies, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, client *pueue.Client, profile PreparedProfile, response Response) (result commandResult) {
	defer func() { mergeCommandClose(&result, td.Close) }()
	if err := profile.Validate(*req); err != nil {
		return failed(response, err, classifyCode(err, 2))
	}
	runner, err := resolveRunner(a.Runner, deps)
	if err != nil {
		return failed(response, err, classifyCode(err, 2))
	}
	if req.PriorSession != nil {
		if err = td.ClaimSession(req.Provider, req.PriorSession.ConversationID); err != nil {
			return failed(response, err, classifyCode(err, 1))
		}
	}
	permit, err := td.PrepareSubmission(client.Binding())
	if err != nil {
		return failed(response, err, classifyCode(err, 1))
	}
	defer func() { mergeCommandClose(&result, permit.Release) }()
	return submitWithClient(td, req, meta, client, runner, permit, response)
}

func submitWithClient(td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, client *pueue.Client, runner string, permit *taskdir.SubmissionPermit, response Response) commandResult {
	observation, err := client.Submit(context.Background(), permit, pueue.Launch{RunnerExecutable: runner, RootPath: filepath.Clean(filepath.Dir(filepath.Dir(td.Dir)))})
	response.Supervisor = supervisorResponse(observation)
	response.Liveness = mapPueueState(observation.State)
	response.Pending = pendingResponse(observation)
	if observation.Matched {
		response.Admission = task.AdmissionAdmitted.String()
		if receipt, receiptErr := observation.Receipt(); receiptErr != nil {
			err = errors.Join(err, receiptErr)
		} else {
			if recordErr := td.RecordSupervisorReceipt(*receipt); recordErr != nil {
				err = errors.Join(err, recordErr)
			}
		}
	} else {
		response.Admission = task.AdmissionUnknown.String()
	}
	if req != nil && meta != nil {
		inspection, inspectErr := td.Inspect()
		fillInspection(&response, inspection)
		err = errors.Join(err, inspectErr)
	}
	if err != nil {
		return failed(response, err, classifyCode(err, 1))
	}
	return commandResult{response: response, code: 0}
}

func reconcileExisting(td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, submit *task.SubmitRecord, supervisorOptions pueue.Options, response Response) commandResult {
	client, err := pueue.NewClient(submit.Supervisor, supervisorOptions)
	if err != nil {
		return failed(response, err, classifyCode(err, 1))
	}
	_, _, spec, metaHash, hashErr := requestHashes(td)
	if hashErr != nil {
		return failed(response, hashErr, 1)
	}
	receipt, receiptErr := td.ReadSupervisorReceipt()
	if receiptErr != nil && !errors.Is(receiptErr, os.ErrNotExist) {
		return failed(response, receiptErr, 1)
	}
	observation, reconcileErr := client.Reconcile(context.Background(), taskIdentity(req, meta, spec, metaHash, receipt))
	response.Supervisor = supervisorResponse(observation)
	response.Liveness = mapPueueState(observation.State)
	response.Pending = pendingResponse(observation)
	if observation.Matched {
		response.Admission = task.AdmissionAdmitted.String()
		if positive, receiptErr := observation.Receipt(); receiptErr != nil {
			reconcileErr = errors.Join(reconcileErr, receiptErr)
		} else {
			recordErr := td.RecordSupervisorReceipt(*positive)
			reconcileErr = errors.Join(reconcileErr, recordErr)
		}
	} else {
		response.Admission = task.AdmissionUnknown.String()
	}
	if reconcileErr != nil {
		return failed(response, reconcileErr, classifyCode(reconcileErr, 1))
	}
	return commandResult{response: response, code: 0}
}

func reconcileStatus(td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, spec, metaHash string, supervisorOptions pueue.Options, response *Response) (pueue.Observation, error) {
	submit, err := td.ReadSubmission()
	if err != nil {
		return pueue.Observation{}, err
	}
	client, err := pueue.NewClient(submit.Supervisor, supervisorOptions)
	if err != nil {
		return pueue.Observation{}, err
	}
	receipt, receiptErr := td.ReadSupervisorReceipt()
	if receiptErr != nil && !errors.Is(receiptErr, os.ErrNotExist) {
		return pueue.Observation{}, receiptErr
	}
	observation, reconcileErr := client.Reconcile(context.Background(), taskIdentity(req, meta, spec, metaHash, receipt))
	response.Supervisor = supervisorResponse(observation)
	response.Liveness = mapPueueState(observation.State)
	response.Pending = pendingResponse(observation)
	if observation.Matched {
		response.Admission = task.AdmissionAdmitted.String()
		if positive, positiveErr := observation.Receipt(); positiveErr != nil {
			reconcileErr = errors.Join(reconcileErr, positiveErr)
		} else {
			reconcileErr = errors.Join(reconcileErr, td.RecordSupervisorReceipt(*positive))
		}
	} else {
		response.Admission = task.AdmissionUnknown.String()
	}
	return observation, reconcileErr
}

func updateStatusStops(td *taskdir.TaskDir, response *Response, observation pueue.Observation) error {
	records, err := td.ReadStopRecords()
	if err != nil {
		return err
	}
	if observation.State != pueue.StateEnded || !observation.Matched {
		response.Stops = stopResponses(records)
		return nil
	}
	var recordErr error
	for _, record := range records {
		if record.Request == nil || record.Request.NumericTaskID == nil || *record.Request.NumericTaskID != observation.NumericTaskID {
			continue
		}
		recordErr = errors.Join(recordErr, td.RecordStopObservation(record.Request.RequestID, task.StopObservationFacts{NumericTaskID: observation.NumericTaskID, State: "ended", Terminated: true}))
	}
	refreshed, refreshErr := td.ReadStopRecords()
	if refreshErr == nil {
		response.Stops = stopResponses(refreshed)
	}
	return errors.Join(recordErr, refreshErr)
}

func status(a Arguments, deps Dependencies) (result commandResult) {
	response := newResponse("status")
	root, err := resolveRoot(a.Root)
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
	inspection, err := td.Inspect()
	fillInspection(&response, inspection)
	if err != nil {
		return failed(response, err, 1)
	}
	if isTerminal(inspection) {
		return commandResult{response: response, code: 0}
	}
	req, meta, spec, metaHash, err := requestHashes(td)
	if err != nil {
		return failed(response, err, 1)
	}
	_, err = td.ReadSubmission()
	if errors.Is(err, os.ErrNotExist) {
		response.Admission = task.AdmissionNotAdmitted.String()
		return commandResult{response: response, code: 0}
	}
	if err != nil {
		return failed(response, err, 1)
	}
	observation, reconcileErr := reconcileStatus(td, req, meta, spec, metaHash, deps.SupervisorOptions, &response)
	reconcileErr = errors.Join(reconcileErr, updateStatusStops(td, &response, observation))
	if reconcileErr != nil {
		if isPureSupervisorUncertaintyError(reconcileErr) {
			return commandResult{response: response, code: 0, err: reconcileErr}
		}
		return failed(response, reconcileErr, 1)
	}
	return commandResult{response: response, code: 0}
}

func collectionResult(response Response, outcome *task.OutcomeRecord, collectErr error) commandResult {
	response.Outcome = outcome
	response.Payload = &outcome.Payload
	response.EvidenceSHA256 = outcome.EvidenceSHA256
	if outcome.Verdict == task.VerdictCommitted {
		response.Publication = task.PublicationCommitted.String()
		if collectErr != nil {
			return failed(response, collectErr, 1)
		}
		return commandResult{response: response, code: 0}
	}
	response.Publication = task.PublicationRejected.String()
	if collectErr != nil {
		return failed(response, collectErr, 1)
	}
	return commandResult{response: response, code: 4}
}

func collectionInspectionResult(response Response, inspection *taskdir.TaskInspection, collectErr, inspectErr, descriptorErr error) (commandResult, bool) {
	if inspection == nil || !isTerminal(inspection) || inspection.Outcome == nil {
		return commandResult{}, false
	}
	if isPureCollectionPendingError(collectErr) {
		collectErr = nil
	}
	return collectionResult(response, inspection.Outcome, errors.Join(collectErr, inspectErr, descriptorErr)), true
}

func releaseCollectedContinuation(result commandResult, td *taskdir.TaskDir, req *task.TaskRecord, outcome *task.OutcomeRecord) commandResult {
	if releaseErr := releaseContinuationSession(td, req, outcome); releaseErr != nil {
		result.err = errors.Join(result.err, releaseErr)
		result.code = 1
		result.response.setError(result.err)
	}
	return result
}

func collectAttempt(td *taskdir.TaskDir, predicate task.PredicateRef) (*task.OutcomeRecord, error) {
	outcome, cleanupErr, collectErr := td.Collect(predicate)
	return outcome, errors.Join(cleanupErr, collectErr)
}

func waitForCollection(deadline time.Time) bool {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false
	}
	if remaining > 20*time.Millisecond {
		remaining = 20 * time.Millisecond
	}
	timer := time.NewTimer(remaining)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()
	<-timer.C
	return time.Now().Before(deadline)
}

func collect(a Arguments, deps storeDependencies) (result commandResult) {
	response := newResponse("collect")
	root, err := resolveRoot(a.Root)
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
	req, meta, err := td.PreparedRecords()
	if err != nil {
		return failed(response, err, 1)
	}
	deadline := time.Now().Add(a.Watch)
	for {
		outcome, collectErr := collectAttempt(td, meta.Predicate)
		if outcome != nil {
			result := collectionResult(response, outcome, collectErr)
			return releaseCollectedContinuation(result, td, req, outcome)
		}
		inspection, inspectErr := td.Inspect()
		fillInspection(&response, inspection)
		descriptors, descriptorErr := td.LogDescriptors()
		response.Raw = descriptors
		if terminal, handled := collectionInspectionResult(response, inspection, collectErr, inspectErr, descriptorErr); handled {
			return releaseCollectedContinuation(terminal, td, req, terminal.response.Outcome)
		}
		combinedErr := errors.Join(collectErr, inspectErr, descriptorErr)
		if combinedErr != nil && !isPureCollectionPendingError(combinedErr) {
			return failed(response, combinedErr, 1)
		}
		if inspectErr == nil && isTerminal(inspection) {
			return commandResult{response: response, code: 0}
		}
		if a.Watch == 0 || time.Now().After(deadline) {
			response.Pending = &PendingResponse{Kind: "publication"}
			return commandResult{response: response, code: 3}
		}
		if !waitForCollection(deadline) {
			response.Pending = &PendingResponse{Kind: "publication"}
			return commandResult{response: response, code: 3}
		}
	}
}

func stopTask(td *taskdir.TaskDir, requestID, cause string, supervisorOptions pueue.Options) (StopResponse, error) {
	permit, err := td.PrepareStop(requestID, cause, time.Time{})
	if err != nil {
		return StopResponse{}, err
	}
	request, err := permit.Request()
	if err != nil {
		return StopResponse{}, errors.Join(err, permit.Release())
	}
	client, err := pueue.NewClient(request.Supervisor, supervisorOptions)
	if err != nil {
		return StopResponse{}, errors.Join(err, permit.Release())
	}
	stopResult, stopErr := client.Stop(context.Background(), permit)
	stopErr = errors.Join(stopErr, permit.Release())
	if stopResult.Acknowledged != nil && stopResult.NumericTaskID != nil {
		stopErr = errors.Join(stopErr, td.RecordStopReply(requestID, task.StopReplyFacts{NumericTaskID: *stopResult.NumericTaskID, Action: stopResult.Action, Acknowledged: *stopResult.Acknowledged, Message: stopResult.Message}))
	}
	if stopResult.ObservedState == pueue.StateEnded && stopResult.NumericTaskID != nil {
		stopErr = errors.Join(stopErr, td.RecordStopObservation(requestID, task.StopObservationFacts{NumericTaskID: *stopResult.NumericTaskID, State: "ended", Terminated: true}))
	}
	return stopResponseFromResult(requestID, cause, stopResult), stopErr
}

func cancel(a Arguments, deps Dependencies) (result commandResult) {
	response := newResponse("cancel")
	root, err := resolveRoot(a.Root)
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
	inspection, err := td.Inspect()
	fillInspection(&response, inspection)
	if err != nil {
		return failed(response, err, 1)
	}
	if isTerminal(inspection) {
		return commandResult{response: response, code: 0}
	}
	requestID, err := task.NewRandomID()
	if err != nil {
		return failed(response, err, 1)
	}
	stopResponse, stopErr := stopTask(td, requestID, "user", deps.SupervisorOptions)
	if errors.Is(stopErr, task.ErrTerminalTask) {
		return terminalAfterCancelRace(response, td)
	}
	response.Stop = &stopResponse
	if records, recordsErr := td.ReadStopRecords(); recordsErr == nil {
		response.Stops = stopResponses(records)
	} else {
		stopErr = errors.Join(stopErr, recordsErr)
	}
	finalInspection, inspectErr := td.Inspect()
	fillInspection(&response, finalInspection)
	stopErr = errors.Join(stopErr, inspectErr)
	if stopErr != nil {
		return failed(response, stopErr, 1)
	}
	return commandResult{response: response, code: 0}
}

func terminalAfterCancelRace(response Response, td *taskdir.TaskDir) commandResult {
	inspection, err := td.Inspect()
	fillInspection(&response, inspection)
	if err != nil {
		return failed(response, err, 1)
	}
	if isTerminal(inspection) {
		return commandResult{response: response, code: 0}
	}
	return failed(response, task.ErrTerminalTask, 1)
}

func logs(a Arguments, deps storeDependencies) (result commandResult) {
	response := newResponse("logs")
	root, err := resolveRoot(a.Root)
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
	inspection, err := td.Inspect()
	fillInspection(&response, inspection)
	if err != nil {
		return failed(response, err, 1)
	}
	descriptors, descriptorErr := td.LogDescriptors()
	response.Raw = descriptors
	if descriptorErr != nil {
		return failed(response, descriptorErr, 1)
	}
	return commandResult{response: response, code: 0}
}

func stopResponseFromResult(id, cause string, result pueue.StopResult) StopResponse {
	terminated := result.ObservedState == pueue.StateEnded && result.NumericTaskID != nil
	return StopResponse{RequestID: id, Cause: cause, Requested: result.Requested, Matched: result.Matched, Attempted: result.Attempted, Action: result.Action, NumericTaskID: copyTaskID(result.NumericTaskID), Acknowledged: result.Acknowledged, InFlight: result.InFlight, ObservedState: string(result.ObservedState), Terminated: terminated, Message: result.Message}
}

func stopResponses(records []taskdir.StopRecord) []StopResponse {
	responses := make([]StopResponse, 0, len(records))
	for _, record := range records {
		responses = append(responses, describeStopRecord(record))
	}
	return responses
}

func failed(response Response, err error, code int) commandResult {
	response.setError(err)
	return commandResult{response: response, code: code, err: err}
}

func isPureCollectionPendingError(err error) bool {
	return isPureErrorSet(err, func(candidate error) bool {
		return errors.Is(candidate, task.ErrNoSeal) || errors.Is(candidate, task.ErrLockBusy)
	})
}

func isPureSupervisorUncertaintyError(err error) bool {
	return isPureErrorSet(err, func(candidate error) bool {
		return errors.Is(candidate, pueue.ErrUnknown) || errors.Is(candidate, pueue.ErrInFlight)
	})
}

func isPureErrorSet(err error, leaf func(error) bool) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !isPureErrorSet(cause, leaf) {
				return false
			}
		}
		return true
	}
	if cause := errors.Unwrap(err); cause != nil {
		return isPureErrorSet(cause, leaf)
	}
	return leaf(err)
}

func resolvePredecessor(store *taskdir.Store, request task.TaskRecord, id string, supervisorOptions pueue.Options) (prior *task.PriorSession, resultErr error) {
	if err := task.ValidateTaskID(id); err != nil {
		return nil, err
	}
	td, err := store.OpenTask(id)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	predecessorReq, _, err := td.PreparedRecords()
	if err != nil {
		return nil, err
	}
	if predecessorReq.Provider != request.Provider || predecessorReq.Mode != request.Mode {
		return nil, task.ErrIdentityMismatch
	}
	identity, err := td.ReadProviderIdentity()
	if err != nil {
		return nil, fmt.Errorf("predecessor session is unavailable: %w", err)
	}
	if err = predecessorTerminated(td, predecessorReq, supervisorOptions); err != nil {
		return nil, err
	}
	return &task.PriorSession{Provider: identity.Provider, ConversationID: identity.ConversationID, PredecessorTaskID: id}, nil
}

func predecessorTerminated(td *taskdir.TaskDir, req *task.TaskRecord, supervisorOptions pueue.Options) error {
	inspection, err := td.Inspect()
	if err != nil {
		return err
	}
	if isTerminal(inspection) {
		return nil
	}
	submit, err := td.ReadSubmission()
	if err != nil {
		return err
	}
	client, err := pueue.NewClient(submit.Supervisor, supervisorOptions)
	if err != nil {
		return err
	}
	receipt, receiptErr := td.ReadSupervisorReceipt()
	if receiptErr != nil && !errors.Is(receiptErr, os.ErrNotExist) {
		return receiptErr
	}
	_, _, spec, metaHash, hashErr := requestHashes(td)
	if hashErr != nil {
		return hashErr
	}
	identity := pueue.Identity{RootID: req.RootID, TaskID: req.TaskID, SpecSHA256: spec, MetaSHA256: metaHash, NumericTaskID: nil}
	if receipt != nil {
		id := receipt.NumericTaskID
		identity.NumericTaskID = &id
	}
	observation, err := client.Reconcile(context.Background(), identity)
	if err != nil {
		return err
	}
	if observation.State != pueue.StateEnded {
		return pueue.ErrUnknown
	}
	return nil
}
