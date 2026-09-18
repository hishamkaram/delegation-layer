package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

func TestRunNativeProfileRefusesBeforeBinding(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	briefPath := filepath.Join(t.TempDir(), "brief.md")
	writeAppTestFile(t, briefPath, []byte("dispatch brief"))
	missingSupervisor := filepath.Join(t.TempDir(), "pueue")
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"dispatch", "--json", "--root", root,
		"--pueue-config", filepath.Join(t.TempDir(), "missing.yml"),
		"--provider", "codex:exec", "--brief", briefPath, "--cwd", filepath.Dir(briefPath),
		"--id", strings.Repeat("a", 32), "--runner", filepath.Join(t.TempDir(), "runner"),
	}, &stdout, &stderr, Dependencies{InitialSupervisorExecutable: missingSupervisor})
	if code != 2 {
		t.Fatalf("native refusal exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.SchemaVersion != OutputSchemaVersion || response.Command != "dispatch" {
		t.Fatalf("unexpected response identity: %+v", response)
	}
	if !strings.Contains(response.Error, ErrProfileUnavailable.Error()) {
		t.Fatalf("profile refusal missing from response: %+v", response)
	}
	tasks, err := os.ReadDir(filepath.Join(root, "tasks"))
	if err == nil && len(tasks) != 0 {
		t.Fatalf("native refusal created task authority: %v", tasks)
	}
}

func TestCollectReturnsCommittedWinnerAndBoundedResponse(t *testing.T) {
	store, td, req := newAppTestTask(t, true)
	defer closeAppTestTask(t, store, td)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"collect", req.TaskID, "--root", store.Root, "--json"}, &stdout, &stderr, Dependencies{})
	if code != 0 {
		t.Fatalf("collect exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Publication != task.PublicationCommitted.String() || response.Outcome == nil || response.Payload == nil {
		t.Fatalf("committed publication missing: %+v", response)
	}
	if response.Payload.Basename != "result.txt" || response.Payload.Length != int64(len("answer\n")) {
		t.Fatalf("unexpected payload descriptor: %+v", response.Payload)
	}
	if response.Raw != nil {
		t.Fatalf("collect copied raw output into control response: %+v", response.Raw)
	}
	if stdout.Len() > 16*1024 {
		t.Fatalf("control response is unexpectedly large: %d", stdout.Len())
	}
}

func TestCollectPendingDoesNotAcquireSupervisorAuthority(t *testing.T) {
	store, td, req := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"collect", req.TaskID, "--root", store.Root, "--json"}, &stdout, &stderr, Dependencies{})
	if code != 3 {
		t.Fatalf("pending collect exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Pending == nil || response.Pending.Kind != "publication" || response.Admission != task.AdmissionUnknown.String() {
		t.Fatalf("pending publication was not explicit: %+v", response)
	}
	if len(response.Raw) != 2 {
		t.Fatalf("pending publication omitted raw descriptors: %+v", response.Raw)
	}
	for i, name := range []string{"stderr", "stdout"} {
		descriptor := response.Raw[i]
		if descriptor.Path != filepath.Join(td.Dir, "raw", name) || descriptor.Sealed || descriptor.Size != nil || descriptor.SHA256 != "" {
			t.Fatalf("pending publication returned an invalid live descriptor: %+v", descriptor)
		}
	}
	if _, err := td.ReadSubmission(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("collect acquired submission authority: %v", err)
	}
}

func TestCollectPredicateFailuresRemainOperationalErrors(t *testing.T) {
	tests := []struct {
		name   string
		deps   Dependencies
		wantIn error
	}{
		{
			name: "unknown registry",
			deps: Dependencies{PredicateRegistry: func() predicate.Registry {
				registry, err := predicate.New()
				if err != nil {
					panic(err)
				}
				return registry
			}},
			wantIn: task.ErrIncompatiblePredicate,
		},
		{
			name: "interpreter failure",
			deps: Dependencies{PredicateRegistry: func() predicate.Registry {
				registry, err := predicate.New(failingInterpreter{err: errInjectedPredicate})
				if err != nil {
					panic(err)
				}
				return registry
			}},
			wantIn: errInjectedPredicate,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, td, req := newAppTestTask(t, true)
			defer closeAppTestTask(t, store, td)

			var stdout, stderr bytes.Buffer
			code := Run([]string{"collect", req.TaskID, "--root", store.Root, "--json"}, &stdout, &stderr, test.deps)
			if code != 1 {
				t.Fatalf("collect exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			var response Response
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(response.Error, test.wantIn.Error()) {
				t.Fatalf("predicate failure missing from response: %+v", response)
			}
			if response.Publication != task.PublicationPending.String() {
				t.Fatalf("collect lost independently observed pending state: %+v", response)
			}
		})
	}
}

func TestCollectionPendingClassificationRejectsJoinedInfrastructureFault(t *testing.T) {
	if !isPureCollectionPendingError(errors.Join(task.ErrNoSeal, task.ErrLockBusy)) {
		t.Fatal("pending sentinels were not recognized")
	}
	if isPureCollectionPendingError(errors.Join(task.ErrNoSeal, errInjectedPredicate)) {
		t.Fatal("infrastructure fault was downgraded to pending")
	}
}

func TestCollectionInspectionWinnerUsesVerdictAfterPendingRace(t *testing.T) {
	cases := []struct {
		name        string
		verdict     string
		publication task.Publication
		code        int
	}{
		{name: "committed", verdict: task.VerdictCommitted, publication: task.PublicationCommitted, code: 0},
		{name: "rejected", verdict: task.VerdictRejected, publication: task.PublicationRejected, code: 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := newResponse("collect")
			outcome := &task.OutcomeRecord{Verdict: tc.verdict, Payload: task.PayloadDescriptor{Basename: "result.txt"}}
			inspection := &taskdir.TaskInspection{Publication: tc.publication, Outcome: outcome}
			result, handled := collectionInspectionResult(response, inspection, errors.Join(task.ErrNoSeal, task.ErrLockBusy), nil, nil)
			if !handled || result.code != tc.code || result.err != nil || result.response.Outcome != outcome {
				t.Fatalf("pending race lost terminal verdict: handled=%t result=%+v", handled, result)
			}
		})
	}
	infra := errors.New("inspection storage failed")
	response := newResponse("collect")
	outcome := &task.OutcomeRecord{Verdict: task.VerdictRejected, Payload: task.PayloadDescriptor{Basename: "publish.reject"}}
	inspection := &taskdir.TaskInspection{Publication: task.PublicationRejected, Outcome: outcome}
	result, handled := collectionInspectionResult(response, inspection, errors.Join(task.ErrNoSeal, infra), nil, nil)
	if !handled || result.code != 1 || !errors.Is(result.err, infra) || result.response.Outcome != outcome {
		t.Fatalf("joined infrastructure fault was downgraded: handled=%t result=%+v", handled, result)
	}
}

func TestStatusUncertaintyDoesNotHideStorageFault(t *testing.T) {
	if !isPureSupervisorUncertaintyError(errors.Join(pueue.ErrUnknown, pueue.ErrInFlight)) {
		t.Fatal("pure supervisor uncertainty was not recognized")
	}
	if isPureSupervisorUncertaintyError(errors.Join(pueue.ErrUnknown, errInjectedPredicate)) {
		t.Fatal("storage fault was downgraded to supervisor uncertainty")
	}
}

func TestLogsReturnsLiveAndSealedDescriptors(t *testing.T) {
	t.Run("live", func(t *testing.T) { assertLogDescriptors(t, false) })
	t.Run("sealed", func(t *testing.T) { assertLogDescriptors(t, true) })
}

func TestRunNonJSONEmitsTaskIdentityAndDescriptors(t *testing.T) {
	store, td, req := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"logs", req.TaskID, "--root", store.Root}, &stdout, &stderr, Dependencies{})
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("non-JSON logs failed: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "task_id="+req.TaskID) || !strings.Contains(output, "publication=unknown") || !strings.Contains(output, "raw path="+filepath.Join(td.Dir, "raw", "stderr")) {
		t.Fatalf("non-JSON result omitted task identity or descriptors: %q", output)
	}
}

func TestWriteHumanResponsePropagatesWriterFailure(t *testing.T) {
	errWrite := errors.New("human output failed")
	if err := writeHumanResponse(errorWriter{err: errWrite}, newResponse("status")); !errors.Is(err, errWrite) {
		t.Fatalf("human writer error=%v", err)
	}
}

func assertLogDescriptors(t *testing.T, sealed bool) {
	t.Helper()
	store, td, req := newAppTestTask(t, sealed)
	defer closeAppTestTask(t, store, td)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"logs", req.TaskID, "--root", store.Root, "--json"}, &stdout, &stderr, Dependencies{})
	if code != 0 {
		t.Fatalf("logs exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Raw) != 2 {
		t.Fatalf("unexpected descriptor count: %+v", response.Raw)
	}
	for i, name := range []string{"stderr", "stdout"} {
		descriptor := response.Raw[i]
		if descriptor.Path != filepath.Join(td.Dir, "raw", name) || descriptor.Sealed != sealed {
			t.Fatalf("wrong descriptor: %+v", descriptor)
		}
		if !sealed && (descriptor.Size != nil || descriptor.SHA256 != "") {
			t.Fatalf("live descriptor invented final identity: %+v", descriptor)
		}
		if sealed && (descriptor.Size == nil || descriptor.SHA256 == "") {
			t.Fatalf("sealed descriptor omitted final identity: %+v", descriptor)
		}
	}
}

func TestDispatchExistingRejectsChangedExplicitSupervisorConfig(t *testing.T) {
	store, td, req := newAppTestTask(t, false)
	response := newResponse("dispatch")
	result := dispatchExisting(Arguments{PueueConfig: "/tmp/other-pueue.yml"}, Dependencies{}, store, td, *req, response)
	if result.code != 2 || !errors.Is(result.err, task.ErrRequestConflict) {
		t.Fatalf("changed explicit config was not refused: %+v", result)
	}
	if _, err := td.ReadSubmission(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("changed explicit config mutated admission: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDispatchExistingAcceptsMatchingExplicitSupervisorConfig(t *testing.T) {
	store, td, req := newAppTestTask(t, true)
	if _, cleanupErr, collectErr := td.Collect(task.FixturePredicateRef()); cleanupErr != nil || collectErr != nil {
		t.Fatalf("preparing terminal task cleanup=%v collect=%v", cleanupErr, collectErr)
	}
	result := dispatchExisting(Arguments{PueueConfig: "/tmp/app-pueue.yml"}, Dependencies{}, store, td, *req, newResponse("dispatch"))
	if result.code != 0 || result.err != nil || result.response.Publication != task.PublicationCommitted.String() {
		t.Fatalf("matching explicit config was not accepted: %+v", result)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisorResponseDoesNotInventUnmatchedNumericID(t *testing.T) {
	unknown := supervisorResponse(pueue.Observation{Matched: false, State: pueue.StateUnknown, NumericTaskID: 99})
	if unknown.NumericTaskID != nil {
		t.Fatalf("unmatched observation fabricated numeric ID: %+v", unknown)
	}
	id := int64(17)
	bound := supervisorResponse(pueue.Observation{Matched: false, State: pueue.StateUnknown, NumericTaskID: 99, Identity: pueue.Identity{NumericTaskID: &id}})
	if bound.NumericTaskID == nil || *bound.NumericTaskID != id {
		t.Fatalf("explicit bound ID was not retained: %+v", bound)
	}
}

func TestMergeCommandCloseMakesAllResultsOperationalFailures(t *testing.T) {
	injected := errors.New("close failed")
	for _, code := range []int{0, 2, 3, 4} {
		result := commandResult{response: newResponse("test"), code: code}
		mergeCommandClose(&result, func() error { return injected })
		if result.code != 1 || !errors.Is(result.err, injected) || !strings.Contains(result.response.Error, injected.Error()) {
			t.Fatalf("close failure code=%d was not retained: %+v", code, result)
		}
	}
}

func TestWriteJSONBoundsAndFlagsDiagnosticTruncation(t *testing.T) {
	response := newResponse("logs")
	response.RootID = strings.Repeat("a", 32)
	response.TaskID = strings.Repeat("b", 32)
	response.Outcome = &task.OutcomeRecord{SchemaVersion: task.SchemaVersion, RootID: response.RootID, TaskID: response.TaskID, Verdict: task.VerdictCommitted, Payload: task.PayloadDescriptor{Basename: "result.txt"}}
	response.Raw = []taskdir.LogDescriptor{{Path: "/tmp/raw/stdout", Available: true, Sealed: true}}
	response.Error = strings.Repeat("\\\"", MaxJSONResponseBytes)
	response.Stop = &StopResponse{Message: strings.Repeat("\\\"", MaxJSONResponseBytes)}
	response.Stops = []StopResponse{{Message: strings.Repeat("\\\"", MaxJSONResponseBytes)}}

	var output bytes.Buffer
	if err := writeJSON(&output, response); err != nil {
		t.Fatal(err)
	}
	if output.Len() > MaxJSONResponseBytes {
		t.Fatalf("JSON response exceeded bound: %d", output.Len())
	}
	var decoded Response
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatalf("response was not complete JSON: %v", err)
	}
	if !decoded.ErrorTruncated || decoded.Stop == nil || !decoded.Stop.MessageTruncated || len(decoded.Stops) != 1 || !decoded.Stops[0].MessageTruncated {
		t.Fatalf("diagnostic truncation was not explicit: %+v", decoded)
	}
	if decoded.Outcome == nil || len(decoded.Raw) != 1 {
		t.Fatalf("authority or descriptors were dropped while bounding response: %+v", decoded)
	}
}

func TestRequestHashesUseExactPreparedRecordBytes(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	metaPath := filepath.Join(td.Dir, "meta.json")
	original, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	noncanonical := append([]byte(" \n"), original...)
	if err = os.WriteFile(metaPath, noncanonical, 0o600); err != nil {
		t.Fatal(err)
	}
	_, meta, _, gotMetaHash, err := requestHashes(td)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := task.MarshalCanonical(meta)
	if err != nil {
		t.Fatal(err)
	}
	if gotMetaHash != task.ComputeSHA256(noncanonical) || gotMetaHash == task.ComputeSHA256(canonical) {
		t.Fatalf("request hash was reconstructed canonically: got=%s", gotMetaHash)
	}
}

func TestMissingInitialSupervisorConfigUsesPrivateSupervisorPath(t *testing.T) {
	t.Setenv("DELEGATE_PUEUE_CONFIG", "")
	path, err := resolveInitialConfig(Arguments{})
	if err != nil || path != "" {
		t.Fatalf("missing initial config was not left for private supervisor resolution: path=%q err=%v", path, err)
	}
}

func TestReadBriefUsesNoFollowNonblockingOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "brief.md")
	want := []byte("brief\nwith exact bytes")
	writeAppTestFile(t, path, want)
	f, err := openBriefForRead(path)
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(f)
	closeErr := f.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(got, want) {
		t.Fatalf("valid brief read changed: bytes=%q read=%v close=%v", got, readErr, closeErr)
	}
	link := filepath.Join(dir, "brief-link")
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err = openBriefForRead(link); !errors.Is(err, config.ErrBriefNotRegular) {
		t.Fatalf("symlink brief was followed: %v", err)
	}
	fifo := filepath.Join(dir, "brief.fifo")
	if err = unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, openErr := openBriefForRead(fifo)
		result <- openErr
	}()
	select {
	case err = <-result:
		if !errors.Is(err, config.ErrBriefNotRegular) {
			t.Fatalf("FIFO result=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO open blocked")
	}
}

func TestRunProviderReturnsFailureForReceiptErrorWithOutcome(t *testing.T) {
	store, td, req := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	metaPath := filepath.Join(td.Dir, "meta.json")
	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	var meta task.MetaRecord
	if err = json.Unmarshal(metaData, &meta); err != nil {
		t.Fatal(err)
	}
	meta.ProviderExecutable = "/bin/true"
	metaData, err = task.MarshalCanonical(&meta)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(metaPath, metaData, 0o600); err != nil {
		t.Fatal(err)
	}
	profile := PreparedProfile{
		Plan:            execution.Plan{Executable: "/bin/true", Directory: req.CanonicalCwd, Predicate: task.FixturePredicateRef()},
		ObservedVersion: "fixture-v2",
		Effective:       task.EffectiveConfig{Containment: "fixture-only", Approval: "never", Digest: task.ComputeSHA256([]byte("app-test"))},
		Identity: func(task.SessionExpectation, func(task.SessionIdentity) error) (execution.IdentityObserver, error) {
			return receiptErrorObserver{}, nil
		},
	}
	result := runProvider(newResponse("dispatch"), td, req, profile, nil, Dependencies{PrepareProfile: func(task.TaskRecord) (PreparedProfile, error) {
		return profile, nil
	}})
	if result.code != 1 || !errors.Is(result.err, errInjectedReceipt) {
		t.Fatalf("receipt failure was not operational: %+v", result)
	}
	if result.response.Outcome == nil || result.response.Publication != task.PublicationRejected.String() {
		t.Fatalf("provider outcome was not preserved with receipt failure: %+v", result.response)
	}
}

var (
	errInjectedPredicate = errors.New("predicate evaluation failed")
	errInjectedReceipt   = errors.New("receipt completion failed")
)

type errorWriter struct{ err error }

func (w errorWriter) Write([]byte) (int, error) { return 0, w.err }

type failingInterpreter struct {
	err error
}

func (failingInterpreter) Reference() task.PredicateRef { return task.FixturePredicateRef() }

func (f failingInterpreter) Evaluate(predicate.Input, predicate.Evidence, io.Writer) (task.Interpretation, error) {
	return task.Interpretation{}, f.err
}

type receiptErrorObserver struct{}

func (receiptErrorObserver) Observe([]byte)  {}
func (receiptErrorObserver) Complete() error { return errInjectedReceipt }

func TestDispatchExistingIDConflictStopsBeforeAdmission(t *testing.T) {
	store, td, req := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	briefPath := filepath.Join(t.TempDir(), "changed.md")
	writeAppTestFile(t, briefPath, []byte("changed brief"))

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"dispatch", "--json", "--root", store.Root, "--id", req.TaskID,
		"--pueue-config", filepath.Join(t.TempDir(), "missing.yml"),
		"--provider", req.Provider, "--brief", briefPath, "--cwd", req.CanonicalCwd,
		"--runner", filepath.Join(t.TempDir(), "runner"),
	}, &stdout, &stderr, Dependencies{InitialSupervisorExecutable: filepath.Join(t.TempDir(), "missing-pueue")})
	if code != 2 {
		t.Fatalf("conflict exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Error, task.ErrRequestConflict.Error()) {
		t.Fatalf("existing identity conflict missing: %+v", response)
	}
	if _, err := td.ReadSubmission(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("conflicting dispatch admitted existing task: %v", err)
	}
}

func TestRunnerArgumentValidationRejectsProviderArguments(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	id := strings.Repeat("b", 32)
	for _, args := range [][]string{
		{"--root", root, id, "provider-argv"},
		{"--root", root, "--json", id, id},
		{"--root", root + "/..", id},
	} {
		if _, err := parseRunnerArguments(args); err == nil {
			t.Fatalf("runner accepted unsafe arguments: %q", args)
		}
	}
}

func TestStatusTerminalWinnerSkipsSupervisorReconciliation(t *testing.T) {
	store, td, req := newAppTestTask(t, true)
	defer closeAppTestTask(t, store, td)
	if _, cleanupErr, collectErr := td.Collect(task.FixturePredicateRef()); cleanupErr != nil || collectErr != nil {
		t.Fatalf("preparing terminal winner cleanup=%v collect=%v", cleanupErr, collectErr)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"status", req.TaskID, "--root", store.Root, "--json"}, &stdout, &stderr, Dependencies{SupervisorOptions: pueue.Options{Observer: func(pueue.CommandEvent) { t.Fatal("terminal status invoked supervisor") }}})
	if code != 0 {
		t.Fatalf("terminal status exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Publication != task.PublicationCommitted.String() || response.Outcome == nil {
		t.Fatalf("terminal winner was not preserved: %+v", response)
	}
}

func newAppTestTask(t *testing.T, sealed bool) (*taskdir.Store, *taskdir.TaskDir, *task.TaskRecord) {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	td, req := newAppTaskInStore(t, store, strings.Repeat("c", 32), sealed, nil)
	return store, td, req
}

func newAppTestTaskWithPrior(t *testing.T, sealed bool, prior *task.PriorSession) (*taskdir.Store, *taskdir.TaskDir, *task.TaskRecord) {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	td, req := newAppTaskInStore(t, store, strings.Repeat("c", 32), sealed, prior)
	return store, td, req
}

func newAppTaskInStore(t *testing.T, store *taskdir.Store, id string, sealed bool, prior *task.PriorSession) (*taskdir.TaskDir, *task.TaskRecord) {
	t.Helper()
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	brief := []byte("brief")
	requested := task.TaskConfig{Permission: "read-only", Budget: "1m0s"}
	digest := task.ComputeSHA256([]byte("app-test"))
	req := &task.TaskRecord{SchemaVersion: task.SchemaVersion, RootID: store.RootID, TaskID: id, Provider: "fixture:test", Mode: "read-only", CanonicalCwd: cwd, RequestedConfig: requested, BudgetNanos: int64(time.Minute), PriorSession: prior, BriefSHA256: task.ComputeSHA256(brief), BriefLength: int64(len(brief))}
	meta := &task.MetaRecord{SchemaVersion: task.SchemaVersion, RootID: store.RootID, TaskID: id, RequestedConfig: requested, EffectiveConfig: task.EffectiveConfig{Containment: "fixture-only", Approval: "never", Digest: digest}, Containment: "fixture-only", Approval: "never", ProviderExecutable: "/tmp/app-provider", ProviderVersion: "fixture-v2", PublisherBuild: "app-test", PublisherVersion: "app-test", Predicate: task.FixturePredicateRef(), SupervisorConfig: task.SupervisorRef{ClientExecutable: "/tmp/app-pueue", ClientSHA256: digest, ResolvedConfigSHA256: digest, ConfigPath: "/tmp/app-pueue.yml", ConfigDigest: digest, Endpoint: "unix:/tmp/app-pueue.sock", ObservedVersion: pueue.FixtureVersion}, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	td, err := store.CreateTask(id, req, brief, meta)
	if err != nil {
		t.Fatal(err)
	}
	if sealed {
		permit, permitErr := td.PrepareStart(0)
		if permitErr != nil {
			t.Fatal(permitErr)
		}
		if err = permit.Consume(); err != nil {
			t.Fatal(err)
		}
		if err = td.RecordStarted(0); err != nil {
			t.Fatal(err)
		}
		if err = td.WriteRawFiles("answer\n", ""); err != nil {
			t.Fatal(err)
		}
		if _, err = td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef()); err != nil {
			t.Fatal(err)
		}
		if err = permit.Release(); err != nil {
			t.Fatal(err)
		}
	}
	return td, req
}

func closeAppTestTask(t *testing.T, store *taskdir.Store, td *taskdir.TaskDir) {
	t.Helper()
	if err := td.Close(); err != nil {
		t.Error(err)
	}
	if err := store.Close(); err != nil {
		t.Error(err)
	}
}

func writeAppTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPiContinuationRequiresMatchingWorkspace(t *testing.T) {
	request := task.TaskRecord{Provider: config.ProviderPiJSON, Mode: config.ModeReadOnly, CanonicalCwd: "/workspace/current"}
	predecessor := request
	if err := validatePredecessorCompatibility(request, predecessor); err != nil {
		t.Fatalf("same workspace rejected: %v", err)
	}
	predecessor.CanonicalCwd = "/workspace/other"
	if err := validatePredecessorCompatibility(request, predecessor); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("cross-workspace Pi continuation error=%v", err)
	}
}
