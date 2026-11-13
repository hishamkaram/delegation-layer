package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestContinuationFailureReasonPreservesRefusalAndPrivacy(t *testing.T) {
	const privateDetail = "PRIVATE-session-env-token-/private/provider/account"
	opaque := &fs.PathError{Op: "private-operation", Path: privateDetail, Err: fs.ErrPermission}
	for _, tc := range []struct {
		name        string
		environment bool
		heldLease   bool
		cause       error
		exitCode    int
		code        string
		reason      string
	}{
		{name: "unrecorded environment", exitCode: 1, code: "continuation_unavailable", reason: "predecessor_environment_unrecorded"},
		{name: "held runner lease", environment: true, heldLease: true, exitCode: 1, code: "continuation_unavailable", reason: "predecessor_runner_lease_busy"},
		{name: "opaque admission fault", environment: true, cause: opaque, exitCode: 1, code: "continuation_unavailable", reason: "continuation_validation_failed"},
		{name: "identity cause keeps priority", environment: true, cause: errors.Join(opaque, task.ErrIdentityMismatch), exitCode: 2, code: "task_identity_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRebootFixture(t, rebootSetup{prior: testPriorSession(), identity: true, stage: rebootLifetimeClosedPreviousBoot, launchEnvironment: tc.environment, holdRunnerLease: tc.heldLease})
			before := snapshotRebootTaskFiles(t, fixture.td.Dir)
			ids, err := fixture.store.TaskIDs(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			deps := rebootDependencies(t, fixture)
			if tc.cause != nil {
				deps.Catalog = fixtureContinuationDependencies(t, func(task.TaskRecord, task.MetaRecord, task.TaskRecord) error { return tc.cause }, fixture.clientA).Catalog
			}
			result := dispatch(Arguments{Root: fixture.root, TaskID: strings.Repeat("7", 32), ResumeTask: fixture.req.TaskID, Provider: fixture.req.Provider, Cwd: fixture.req.CanonicalCwd, Config: fixture.req.RequestedConfig, BriefBytes: []byte("continue")}, deps)
			if result.code != tc.exitCode || result.err == nil {
				t.Fatalf("refusal = code %d error %v, want refusal code %d", result.code, result.err, tc.exitCode)
			}
			assertContinuationOriginalCauses(t, result.err, tc.cause, tc.heldLease, privateDetail)
			assertContinuationFailureJSON(t, result, tc.code, tc.reason, privateDetail)
			after, listErr := fixture.store.TaskIDs(t.Context())
			if listErr != nil || !reflect.DeepEqual(ids, after) {
				t.Fatalf("refused continuation created successor: before=%v after=%v error=%v", ids, after, listErr)
			}
			requireRebootTaskFilesUnchanged(t, before)
			requireNoRebootSessionRelease(t, fixture)
		})
	}
}

func assertContinuationOriginalCauses(t *testing.T, err, cause error, heldLease bool, privateDetail string) {
	t.Helper()
	if cause != nil {
		var original *fs.PathError
		if !errors.Is(err, cause) || !errors.As(err, &original) || original.Path != privateDetail {
			t.Fatalf("refusal lost original cause or type: %v", err)
		}
	}
	if heldLease && !errors.Is(err, task.ErrSessionBusy) {
		t.Fatalf("refusal lost runner lease cause: %v", err)
	}
}

func assertContinuationFailureJSON(t *testing.T, result commandResult, code, reason, privateDetail string) {
	t.Helper()
	var output bytes.Buffer
	if err := writeJSONResponse(&output, "dispatch", &result.response, result.err); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), privateDetail) {
		t.Fatal("public continuation failure exposed private cause")
	}
	var response Response
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	// Dispatch has not reached its successor lookup at this boundary, so its
	// existing public record state remains unknown; the store proves no creation.
	if response.TaskID != strings.Repeat("7", 32) || response.TaskRecord != "unknown" || response.Failure == nil {
		t.Fatalf("refusal changed its predecessor-resolution envelope: %+v", response)
	}
	failure := response.Failure
	if failure.Code != code || failure.ReasonCode != reason || failure.Stage != "resolve-predecessor" || failure.NextAction != "stop_and_report" {
		t.Fatalf("failure = %+v; want %s/%s at resolve-predecessor with stop_and_report", failure, code, reason)
	}
}
