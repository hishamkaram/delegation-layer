package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func (f *connectorFixture) publicHash(t *testing.T, args ...string) (Response, int) {
	t.Helper()
	data, err := task.MarshalCanonical(f.origin)
	if err != nil {
		t.Fatal(err)
	}
	deps := f.deps
	deps.Input = bytes.NewReader(data)
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(f.projectToken)))
	args = append(args, "--root", f.store.Root, "--expected-root", f.rootToken, "--expected-project-sha256", digest, "--expected-origin", "-", "--json")
	var stdout, stderr bytes.Buffer
	code := Run(args, &stdout, &stderr, deps)
	var response Response
	if err = json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("hash control exit=%d diagnostics=%q output=%q: %v", code, stderr.String(), stdout.String(), err)
	}
	return response, code
}

func historicalHashControlArguments(command, id, descriptor string) []string {
	switch command {
	case "result":
		return []string{"result", "read", id, "--expected-descriptor", descriptor, "--limit", "64"}
	case "cancel":
		return []string{"cancel", id, "--id", strings.Repeat("f", 32)}
	default:
		return []string{command, id}
	}
}

func connectorResultDescriptor(t *testing.T, outcome *task.OutcomeRecord) string {
	t.Helper()
	descriptor, err := task.MarshalCanonical(outcome.Payload)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "descriptor.json")
	writeAppTestFile(t, path, descriptor)
	return path
}

func TestConnectorHashControlsReadSavedEvidenceAfterProjectDeletion(t *testing.T) {
	for _, binding := range []struct {
		name string
		new  func(*testing.T) *connectorFixture
	}{{"legacy physical", newConnectorFixture}, {"canonical path", newPathConnectorFixture}} {
		t.Run(binding.name, func(t *testing.T) {
			checkHistoricalHashReadControls(t, binding.new(t))
		})
	}
}

func checkHistoricalHashReadControls(t *testing.T, fixture *connectorFixture) {
	t.Helper()
	outcome := commitConnectorFixtureResult(t, fixture, "saved answer")
	descriptor := connectorResultDescriptor(t, outcome)
	_, _, spec, _, err := fixture.task.PreparedIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(fixture.request.CanonicalCwd); err != nil {
		t.Fatal(err)
	}
	before := snapshotRebootTaskFiles(t, fixture.task.Dir)
	for _, command := range []string{"status", "collect", "result", "resolve"} {
		args := historicalHashControlArguments(command, fixture.request.TaskID, descriptor)
		args = append(args, "--expected-spec-sha256", spec)
		response, code := fixture.publicHash(t, args...)
		requireHistoricalHashReadResult(t, command, response, code, outcome)
	}
	if !reflect.DeepEqual(before, snapshotRebootTaskFiles(t, fixture.task.Dir)) {
		t.Fatal("historical reads changed saved task evidence")
	}
	hashed, hashCode := fixture.publicHash(t, "resolve", fixture.request.TaskID)
	legacy, legacyCode := fixture.public(t, "resolve", fixture.request.TaskID)
	if hashCode != 0 || legacyCode != 0 || !reflect.DeepEqual(hashed.Connector.Receipt, legacy.Connector.Receipt) {
		t.Fatalf("hash and legacy guards projected different receipts: hash=%+v raw=%+v", hashed, legacy)
	}
}

func requireHistoricalHashReadResult(t *testing.T, command string, response Response, code int, outcome *task.OutcomeRecord) {
	t.Helper()
	if code != 0 || response.Outcome == nil || response.Outcome.Payload != outcome.Payload {
		t.Fatalf("%s could not read saved evidence after deletion: code=%d %+v", command, code, response)
	}
	if command == "result" && (response.Connector == nil || response.Connector.ResultRange == nil || string(response.Connector.ResultRange.Data) != "saved answer") {
		t.Fatalf("result bytes changed after project deletion: %+v", response)
	}
}

func TestConnectorHashCancelAfterProjectDeletionKeepsStableControl(t *testing.T) {
	fixture := newConnectorFixture(t)
	before, err := os.ReadFile(filepath.Join(fixture.task.Dir, "task.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(fixture.request.CanonicalCwd); err != nil {
		t.Fatal(err)
	}
	requireHistoricalHashCancellation(t, fixture)
	stops, err := fixture.task.ReadStopRecords()
	if err != nil || len(stops) != 1 {
		t.Fatalf("stable cancellation created multiple stops: %+v, %v", stops, err)
	}
	after, err := os.ReadFile(filepath.Join(fixture.task.Dir, "task.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("cancellation rewrote saved project identity: %v", err)
	}
	resolved, code := fixture.publicHash(t, "resolve", fixture.request.TaskID)
	if code != 0 || resolved.Connector == nil || resolved.Connector.ExecutionClosure == nil {
		t.Fatalf("removed project blocked canonical cancellation closure: code=%d %+v", code, resolved)
	}
}

func requireHistoricalHashCancellation(t *testing.T, fixture *connectorFixture) {
	t.Helper()
	response, code := fixture.publicHash(t, "cancel", fixture.request.TaskID, "--id", strings.Repeat("f", 32))
	if code != 0 || response.Stop == nil || response.Stop.RequestID != strings.Repeat("f", 32) || !response.Stop.Requested || !response.Stop.Terminated {
		t.Fatalf("removed project blocked exact cancellation: code=%d %+v", code, response)
	}
	duplicate, duplicateCode := fixture.publicHash(t, "cancel", fixture.request.TaskID, "--id", strings.Repeat("f", 32))
	if duplicateCode != 0 || duplicate.Stop != nil || len(duplicate.Stops) != 1 || duplicate.Stops[0].RequestID != strings.Repeat("f", 32) || !duplicate.Stops[0].Requested {
		t.Fatalf("duplicate cancellation lost saved request: code=%d %+v", duplicateCode, duplicate)
	}
}

func TestConnectorHistoricalHashControlsRejectWrongGuardsBeforeEffects(t *testing.T) {
	fixture := newConnectorFixture(t)
	outcome := commitConnectorFixtureResult(t, fixture, "saved answer")
	descriptor := connectorResultDescriptor(t, outcome)
	_, _, spec, _, err := fixture.task.PreparedIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(fixture.request.CanonicalCwd); err != nil {
		t.Fatal(err)
	}
	before := snapshotRebootTaskFiles(t, fixture.task.Dir)
	for _, command := range []string{"status", "collect", "cancel", "result", "resolve"} {
		for _, field := range []string{"root", "project", "origin", "spec"} {
			t.Run(command+"/"+field, func(t *testing.T) {
				wrong := *fixture
				expectedSpec := spec
				switch field {
				case "root":
					wrong.rootToken = strings.Repeat("b", 64)
				case "project":
					wrong.projectToken = "different saved token"
				case "origin":
					wrong.origin.OperationID = "other-operation"
				case "spec":
					expectedSpec = strings.Repeat("b", 64)
				}
				args := historicalHashControlArguments(command, fixture.request.TaskID, descriptor)
				args = append(args, "--expected-spec-sha256", expectedSpec)
				response, code := wrong.publicHash(t, args...)
				if code == 0 || response.Outcome != nil || response.Stop != nil || (response.Connector != nil && response.Connector.ResultRange != nil) {
					t.Fatalf("%s accepted incorrect %s guard: code=%d %+v", command, field, code, response)
				}
				if !reflect.DeepEqual(before, snapshotRebootTaskFiles(t, fixture.task.Dir)) {
					t.Fatal("incorrect historical guard mutated task evidence")
				}
			})
		}
	}
}

func TestConnectorHashResolveAbsentTaskAfterProjectDeletion(t *testing.T) {
	fixture := newConnectorFixture(t)
	if err := os.Remove(fixture.request.CanonicalCwd); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("e", 32)
	resolved, code := fixture.publicHash(t, "resolve", id)
	if code != 0 || resolved.Connector == nil || resolved.Connector.Resolution != "absent" {
		t.Fatalf("hash guard could not observe absence: code=%d %+v", code, resolved)
	}
	if refusal, err := fixture.store.ReadNamespaceRefusalContext(t.Context(), id); err != nil || refusal != nil {
		t.Fatalf("passive hash guard fenced absent ID: %+v, %v", refusal, err)
	}
	fenced, code := fixture.publicHash(t, "resolve", id, "--fence-if-absent")
	if code != 0 || fenced.Connector == nil || fenced.Connector.Resolution != "permanently_refused" {
		t.Fatalf("hash guard could not fence absence: code=%d %+v", code, fenced)
	}
}
