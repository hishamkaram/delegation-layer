package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestConnectorWorkspaceArguments(t *testing.T) {
	for _, argv := range [][]string{
		{"context", "initialize", "--workspace-constrained"},
		{"context", "enable-connector", "--workspace-constrained", "--offline-cutover-confirmed"},
		{"context", "inspect", "--expected-workspace", "opaque", "--relative-project", "team/project"},
	} {
		if _, err := ParseArguments(argv); err != nil {
			t.Fatalf("%v: %v", argv, err)
		}
	}
	for _, argv := range [][]string{
		{"context", "inspect", "--expected-workspace", "opaque"},
		{"context", "inspect", "--relative-project", "team/project"},
		{"context", "inspect", "--expected-workspace", "opaque", "--relative-project", "../outside"},
		{"context", "initialize", "--expected-workspace", "opaque", "--relative-project", "."},
		{"context", "inspect", "--workspace-constrained"},
	} {
		if _, err := ParseArguments(argv); err == nil {
			t.Fatalf("accepted %v", argv)
		}
	}
}

func TestConnectorPublicCreateOnlyInitialize(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	var output, diagnostic bytes.Buffer
	code := Run([]string{"context", "initialize", "--root", root, "--workspace-constrained", "--json"}, &output, &diagnostic, Dependencies{})
	if code != 0 {
		t.Fatalf("initialize exit=%d output=%s diagnostics=%s", code, output.String(), diagnostic.String())
	}
	var response Response
	if decodeErr := json.Unmarshal(output.Bytes(), &response); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if response.Connector == nil || !slices.Contains(response.Connector.Features, task.WorkspaceConstrainedConnectorFeature) {
		t.Fatalf("missing feature: %+v", response)
	}
	before, err := os.ReadFile(filepath.Join(root, "root.json"))
	if err != nil {
		t.Fatal(err)
	}
	output.Reset()
	code = Run([]string{"context", "initialize", "--root", root, "--workspace-constrained", "--json"}, &output, &diagnostic, Dependencies{})
	if code == 0 {
		t.Fatal("existing state adopted")
	}
	if decodeErr := json.Unmarshal(output.Bytes(), &response); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if response.Failure == nil || response.Failure.Code != "root_exists" || response.Failure.NextAction != "inspect_existing_root" {
		t.Fatalf("missing typed create-only failure: %+v", response)
	}
	after, err := os.ReadFile(filepath.Join(root, "root.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("initialize modified existing root")
	}
}

func newWorkspaceConnectorFixture(t *testing.T) (*connectorFixture, *task.WorkspaceConstraint) {
	t.Helper()
	f := newConnectorFixture(t)
	if err := f.store.EnableWorkspaceConnectorContext(t.Context(), true, true); err != nil {
		t.Fatal(err)
	}
	workspace, err := taskdir.ObserveProjectContext(t.Context(), f.request.CanonicalCwd)
	if err != nil {
		t.Fatal(err)
	}
	token, err := taskdir.ProjectToken(workspace)
	if err != nil {
		t.Fatal(err)
	}
	constraint := &task.WorkspaceConstraint{Token: token, RelativeProject: "."}
	_, meta, err := f.task.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	request := *f.request
	request.TaskID = strings.Repeat("e", 32)
	request.ConnectorContext = request.ConnectorContext.Clone()
	request.ConnectorContext.WorkspaceConstraint = constraint.Clone()
	metadata := *meta
	metadata.TaskID = request.TaskID
	td, err := f.store.CreateTaskContext(t.Context(), request.TaskID, &request, []byte("brief"), &metadata)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := td.Close(); err != nil {
			t.Error(err)
		}
	})
	f.task, f.request = td, &request
	return f, constraint
}

func requireWorkspacePublicInspection(t *testing.T, f *connectorFixture, constraint *task.WorkspaceConstraint) {
	t.Helper()
	var output, diagnostic bytes.Buffer
	code := Run([]string{"context", "inspect", "--root", f.store.Root, "--expected-workspace", constraint.Token, "--relative-project", ".", "--json"}, &output, &diagnostic, f.deps)
	if code != 0 {
		t.Fatalf("inspection code=%d output=%s error=%s", code, output.String(), diagnostic.String())
	}
	var inspected Response
	if err := json.Unmarshal(output.Bytes(), &inspected); err != nil {
		t.Fatal(err)
	}
	if inspected.Connector.ProjectToken != f.projectToken || !reflect.DeepEqual(inspected.Connector.WorkspaceConstraint, constraint) {
		t.Fatalf("wrong context: %+v", inspected.Connector)
	}
}

func TestConnectorWorkspacePublicInspectionAndPassiveAfterRemoval(t *testing.T) {
	f, constraint := newWorkspaceConnectorFixture(t)
	requireWorkspacePublicInspection(t, f, constraint)
	if err := os.RemoveAll(f.request.CanonicalCwd); err != nil {
		t.Fatal(err)
	}
	result, code := f.public(t, "resolve", f.request.TaskID, "--expected-workspace", constraint.Token, "--relative-project", ".")
	if code != 0 || result.Connector == nil || result.Connector.Resolution != "existing" {
		t.Fatalf("missing workspace stranded passive control: code=%d %+v", code, result)
	}
	if !reflect.DeepEqual(result.Connector.Receipt.WorkspaceConstraint, constraint) {
		t.Fatal("receipt dropped frozen workspace")
	}
	legacy, code := f.public(t, "resolve", f.request.TaskID)
	if code != 0 || legacy.Connector.Receipt.WorkspaceConstraint != nil {
		t.Fatal("legacy passive control or projection changed")
	}
	cancelled, code := f.public(t, "cancel", f.request.TaskID, "--id", strings.Repeat("f", 32))
	if code != 0 || cancelled.Stop == nil || !cancelled.Stop.Requested || !cancelled.Stop.Terminated {
		t.Fatalf("removed workspace stranded legacy cancellation: code=%d %+v", code, cancelled)
	}
}

// These fields deliberately freeze the pre-workspace inventory decoder.
// Using ConnectorReceipt here would let new fields silently change the oracle.
type legacyConnectorReceipt struct {
	ExecutionOptions taskdir.ConnectorExecutionOptions `json:"execution_options"`
	RootToken        string                            `json:"root_token"`
	ProjectToken     string                            `json:"project_token"`
	TaskID           string                            `json:"task_id"`
	Origin           task.GatewayOrigin                `json:"origin"`
	SpecSHA256       string                            `json:"spec_sha256"`
	BriefSHA256      string                            `json:"brief_sha256"`
}

type legacyOriginEntry struct {
	Receipt     *legacyConnectorReceipt `json:"receipt,omitempty"`
	TaskID      string                  `json:"task_id"`
	Resolution  string                  `json:"resolution"`
	Origin      *task.GatewayOrigin     `json:"origin,omitempty"`
	SpecSHA256  string                  `json:"spec_sha256,omitempty"`
	BriefSHA256 string                  `json:"brief_sha256,omitempty"`
}

type legacyOriginInventory struct {
	Entries             []legacyOriginEntry `json:"entries"`
	NextCursor          string              `json:"next_cursor,omitempty"`
	Complete            bool                `json:"complete"`
	MembershipSHA256    string              `json:"membership_sha256"`
	CompletenessReceipt string              `json:"completeness_receipt,omitempty"`
}

func workspacePublicInventory(t *testing.T, f *connectorFixture, include bool) Response {
	t.Helper()
	args := []string{"origin", "inventory", "--root", f.store.Root, "--gateway-id", "gateway", "--generation", "generation", "--expected-root", f.rootToken, "--json"}
	if include {
		args = append(args, "--include-workspace-constraint")
	}
	var output, diagnostic bytes.Buffer
	if code := Run(args, &output, &diagnostic, f.deps); code != 0 {
		t.Fatalf("inventory code=%d out=%s error=%s", code, output.String(), diagnostic.String())
	}
	var response Response
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !include {
		var envelope struct {
			Connector struct {
				Inventory json.RawMessage `json:"inventory"`
			} `json:"connector"`
		}
		if err := json.Unmarshal(output.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		var legacy legacyOriginInventory
		if err := task.DecodeStrict(envelope.Connector.Inventory, &legacy); err != nil {
			t.Fatalf("mixed history broke strict legacy decoder: %v", err)
		}
		if len(legacy.Entries) != 2 {
			t.Fatalf("mixed history entries=%d", len(legacy.Entries))
		}
	}
	return response
}

func TestConnectorWorkspaceMixedHistoryInventoryLegacyAndOptIn(t *testing.T) {
	f, constraint := newWorkspaceConnectorFixture(t)
	legacy := workspacePublicInventory(t, f, false)
	modern := workspacePublicInventory(t, f, true)
	if legacy.Connector.Inventory.MembershipSHA256 != modern.Connector.Inventory.MembershipSHA256 {
		t.Fatal("projection changed canonical inventory commitment")
	}
	var found bool
	for _, entry := range modern.Connector.Inventory.Entries {
		if entry.TaskID == f.request.TaskID {
			found = true
			if entry.Receipt == nil || !reflect.DeepEqual(entry.Receipt.WorkspaceConstraint, constraint) {
				t.Fatal("opt-in inventory lost canonical constraint")
			}
		}
	}
	if !found {
		t.Fatal("dynamic task missing")
	}
	reader, err := taskdir.OpenReadOnlyStoreContext(t.Context(), f.store.Root, f.deps.storeDependencies().registry())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	receipt, err := reader.ReadConnectorReceiptContext(t.Context(), f.request.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(receipt.WorkspaceConstraint, constraint) {
		t.Fatal("output projection mutated canonical receipt")
	}
}

func TestConnectorWorkspaceResumeCannotStripConstraint(t *testing.T) {
	f, _ := newWorkspaceConnectorFixture(t)
	receipt, err := f.store.OpenTask(f.request.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	_, _, spec, _, err := receipt.PreparedIdentity()
	if closeErr := receipt.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	result, code := f.public(t, "resolve", f.request.TaskID, "--resume-admission", "--expected-spec-sha256", spec)
	if code == 0 || result.Connector == nil {
		t.Fatalf("resume stripped required constraint: code=%d %+v", code, result)
	}
	observed, code := f.public(t, "resolve", f.request.TaskID, "--include-workspace-constraint")
	if code != 0 || observed.Connector.Receipt == nil || observed.Connector.Receipt.WorkspaceConstraint == nil {
		t.Fatalf("passive opt-in failed: code=%d %+v", code, observed)
	}
}

func TestConnectorWorkspaceLegacyResultAfterRemoval(t *testing.T) {
	f, _ := newWorkspaceConnectorFixture(t)
	outcome := commitConnectorFixtureResult(t, f, "accepted dynamic result")
	descriptor, err := task.MarshalCanonical(outcome.Payload)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "descriptor.json")
	writeAppTestFile(t, path, descriptor)
	if err = os.RemoveAll(f.request.CanonicalCwd); err != nil {
		t.Fatal(err)
	}
	response, code := f.public(t, "result", "read", f.request.TaskID, "--expected-descriptor", path)
	if code != 0 || response.Connector == nil || response.Connector.ResultRange == nil || string(response.Connector.ResultRange.Data) != "accepted dynamic result" {
		t.Fatalf("legacy result lost after removal: code=%d %+v", code, response)
	}
	if response.Connector.Receipt.WorkspaceConstraint != nil {
		t.Fatal("legacy result projection exposed new field")
	}
}

func TestConnectorWorkspaceCutoverFailureIsTyped(t *testing.T) {
	f := newConnectorFixture(t)
	var output, diagnostic bytes.Buffer
	code := Run([]string{"context", "inspect", "--root", f.store.Root, "--expected-workspace", f.projectToken, "--relative-project", ".", "--json"}, &output, &diagnostic, f.deps)
	if code != 1 {
		t.Fatalf("cutover missing: code=%d out=%s diagnostics=%s", code, output.String(), diagnostic.String())
	}
	var response Response
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Failure == nil || response.Failure.Code != "root_cutover_required" || response.Failure.NextAction != "run_explicit_offline_cutover" {
		t.Fatalf("untyped upgrade failure: %+v", response)
	}
}
