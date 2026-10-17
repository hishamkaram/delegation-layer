package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestSupervisorOptionsForCandidateKeepOrdinaryTimeout(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executable, err := filepath.EvalSymlinks("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := commonprovider.FingerprintExecutable(executable)
	if err != nil {
		t.Fatal(err)
	}
	candidate := commonprovider.ProfileCandidate{Inspection: &commonprovider.InspectionDefinition{
		Revision: commonprovider.RuntimeInspectionRevision, Executable: executable,
		ExecutableSHA256: digest, Arguments: []string{"--version"}, Directory: workspace,
		Environment: []string{}, OutputLimit: commonprovider.MaxInspectionOutput,
		Project: func([]byte) (json.RawMessage, error) { return json.RawMessage(`{}`), nil },
	}}
	options, err := admission.SupervisorOptionsForCandidate(pueue.Options{ObservationTimeout: time.Millisecond}, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if options.ObservationTimeout != time.Millisecond {
		t.Fatalf("ordinary observation timeout=%s, want %s", options.ObservationTimeout, time.Millisecond)
	}
	longer, err := admission.SupervisorOptionsForCandidate(pueue.Options{ObservationTimeout: time.Minute}, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if longer.ObservationTimeout != time.Minute {
		t.Fatalf("explicit longer observation timeout=%s was reduced", longer.ObservationTimeout)
	}
	if _, err = admission.SupervisorOptionsForCandidate(pueue.Options{ObservationTimeout: -time.Second}, candidate); !errors.Is(err, pueue.ErrConfiguration) {
		t.Fatalf("negative observation timeout was accepted: %v", err)
	}
}

func TestEnsureInspectionGroupRetainsUncertainCreate(t *testing.T) {
	store := newBareInspectionStore(t)
	bound, logPath, donePath := newUncertainGroupSupervisor(t, store.RootID)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan struct{}, 1)
	client := newObservedInspectionTestClient(t, bound, logPath, 10*time.Second, func(event pueue.CommandEvent) {
		if event.Stage == "started" && strings.Contains("\x00"+strings.Join(event.Argv, "\x00")+"\x00", "\x00group\x00") {
			select {
			case started <- struct{}{}:
			default:
			}
			cancel()
		}
	}, "FAKE_AFTER="+filepath.Join(filepath.Dir(logPath), "status-after.json"), "FAKE_DONE="+donePath)

	err := supervisor.EnsureInspectionGroupWithMigrationContext(ctx, store, client, inspection.AdmissionTimeout, false)
	pending := assertRetainedInspectionPending(t, err)
	settled := false
	t.Cleanup(func() {
		if !settled {
			drainInspectionPending(t, pending)
		}
	})
	select {
	case <-started:
	default:
		t.Fatal("group command did not start before the caller stopped observing")
	}
	drainInspectionPending(t, pending)
	settled = true
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if _, statErr := os.Stat(donePath); statErr != nil {
		t.Fatalf("uncertain create did not settle naturally: %v; log=%s", statErr, logData)
	}
	if count := strings.Count(string(logData), "arg=group\n"); count != 1 {
		t.Fatalf("group creation was launched %d times: err=%v log=%s", count, err, logData)
	}
	if count := strings.Count(string(logData), "arg=status\n"); count != 1 {
		t.Fatalf("unfinished creation caused %d status commands", count)
	}
	if err = supervisor.EnsureInspectionGroup(store, client, inspection.AdmissionTimeout); err != nil {
		t.Fatalf("settled uncertain creation was not observed by the next caller: %v", err)
	}
	logData, readErr = os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if count := strings.Count(string(logData), "arg=group\n"); count != 1 {
		t.Fatalf("settled creation was retried %d times: %s", count, logData)
	}
	if count := strings.Count(string(logData), "arg=status\n"); count != 2 {
		t.Fatalf("next caller did not take one fresh snapshot: %d: %s", count, logData)
	}
}

func TestStopLegacyInspectionSupervisorPreservesDispatchedOwnership(t *testing.T) {
	client, logPath, _ := newUncertainGroupSupervisor(t, "shutdown-ownership")
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	dispatched, err := client.ShutdownDispatched(ctx)
	if !dispatched {
		t.Fatalf("shutdown ownership was lost after dispatch: err=%v", err)
	}
	if err == nil {
		t.Fatal("timed-out shutdown was reported as successful")
	}
	admissionfixture.WaitForTestPath(t, filepath.Join(filepath.Dir(logPath), "shutdown-done"))
	invocations, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if count := strings.Count(string(invocations), "arg=shutdown\n"); count != 1 {
		t.Fatalf("shutdown command was dispatched %d times after ownership transferred: %s", count, invocations)
	}
}

func waitForTestPathWithin(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	admissionfixture.WaitForTestPathWithin(t, path, timeout)
}

func assertRetainedInspectionPending(t *testing.T, err error) *pueue.Pending {
	t.Helper()
	return admissionfixture.AssertRetainedInspectionPending(t, err)
}

func drainInspectionPending(t *testing.T, pending *pueue.Pending) {
	t.Helper()
	admissionfixture.DrainInspectionPending(t, pending)
}

func newBlockingInspectionSupervisor(t *testing.T, rootID string) (*pueue.Client, string) {
	t.Helper()
	return supervisorfixture.NewBlockingSupervisor(t, rootID)
}

func newObservedInspectionTestClient(t *testing.T, bound *pueue.Client, logPath string, timeout time.Duration, observer func(pueue.CommandEvent), extraEnvironment ...string) *pueue.Client {
	t.Helper()
	return admissionfixture.NewObservedInspectionTestClient(t, bound, logPath, timeout, observer, extraEnvironment...)
}

func newUncertainGroupSupervisor(t *testing.T, rootID string) (*pueue.Client, string, string) {
	t.Helper()
	return admissionfixture.NewUncertainGroupSupervisor(t, rootID)
}

func newUncertainGroupSupervisorWithConfig(t *testing.T, rootID, configuredPath string) (*pueue.Client, string, string) {
	t.Helper()
	return admissionfixture.NewUncertainGroupSupervisorWithConfig(t, rootID, configuredPath)
}

func newUncertainGroupSupervisorWithConfigAndTimeout(t *testing.T, rootID, configuredPath string, observationTimeout time.Duration) (*pueue.Client, string, string) {
	t.Helper()
	return admissionfixture.NewUncertainGroupSupervisorWithConfigAndTimeout(t, rootID, configuredPath, observationTimeout)
}

func TestInspectionAdmissionRejectsStaticCandidateBeforeSupervisorBinding(t *testing.T) {
	tests := []struct {
		name      string
		candidate func(task.TaskRecord, *taskdir.Store) commonprovider.ProfileCandidate
	}{
		{
			name: "candidate directory mismatch",
			candidate: func(request task.TaskRecord, _ *taskdir.Store) commonprovider.ProfileCandidate {
				return appInvalidInspectionCandidate(request, filepath.Join(request.CanonicalCwd, "unexpected"), nil, nil)
			},
		},
		{
			name: "writable state overlap",
			candidate: func(request task.TaskRecord, store *taskdir.Store) commonprovider.ProfileCandidate {
				return appInvalidInspectionCandidate(request, request.CanonicalCwd, []string{store.Root}, nil)
			},
		},
		{
			name: "malformed inspection definition",
			candidate: func(request task.TaskRecord, _ *taskdir.Store) commonprovider.ProfileCandidate {
				definition := &commonprovider.InspectionDefinition{
					Revision:    "",
					Executable:  "relative-helper",
					Directory:   request.CanonicalCwd,
					Environment: []string{},
					OutputLimit: 1024,
					Project: func([]byte) (json.RawMessage, error) {
						return json.RawMessage(`{"eligible":true}`), nil
					},
				}
				return appInvalidInspectionCandidate(request, request.CanonicalCwd, nil, definition)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newBareInspectionStore(t)
			request := newBareInspectionRequest(t, store)
			candidate := test.candidate(request, store)
			deps := Dependencies{
				PrepareCandidate: func(task.TaskRecord) (commonprovider.ProfileCandidate, error) {
					return candidate, nil
				},
				InitialSupervisorExecutable: filepath.Join(t.TempDir(), "missing-pueue"),
			}
			args := Arguments{PueueConfig: filepath.Join(t.TempDir(), "missing-pueue.yml")}
			if _, err := prepareAdmission(args, deps, store, request); !errors.Is(err, ErrProfileUnavailable) {
				t.Fatalf("static candidate crossed supervisor binding: %v", err)
			}
			assertAppInspectionNoOrdinaryTasks(t, store)
		})
	}
}

func appInvalidInspectionCandidate(request task.TaskRecord, directory string, writableRoots []string, definition *commonprovider.InspectionDefinition) commonprovider.ProfileCandidate {
	return commonprovider.ProfileCandidate{
		Directory:     directory,
		WritableRoots: writableRoots,
		Inspection:    definition,
		Finalize: func(json.RawMessage, time.Time) (commonprovider.PreparedProfile, error) {
			return appInspectionPreparedProfile(request, directory, writableRoots), nil
		},
	}
}

func newBareInspectionStore(t *testing.T) *taskdir.Store {
	t.Helper()
	return supervisorfixture.NewInspectionStore(t)
}

func publishAppTestGroupMigration(t *testing.T, store *taskdir.Store, legacy, successor inspection.GroupRequestRecord) {
	t.Helper()
	if err := inspection.PrepareGroupMigration(t.Context(), store, legacy, successor); err != nil {
		t.Fatal(err)
	}
	if err := inspection.CompleteGroupMigration(t.Context(), store, legacy, successor); err != nil {
		t.Fatal(err)
	}
}

func newBareInspectionRequest(t *testing.T, store *taskdir.Store) task.TaskRecord {
	t.Helper()
	return supervisorfixture.NewInspectionRequest(t, store)
}
