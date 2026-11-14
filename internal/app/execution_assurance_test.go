package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestExecutionAssuranceArguments(t *testing.T) {
	for _, argv := range [][]string{
		{"status", strings.Repeat("a", 32), "--include-execution-assurance"},
		{"context", "inspect", "--include-execution-assurance"},
	} {
		parsed, err := ParseArguments(argv)
		if err != nil || !parsed.Connector.IncludeExecutionAssurance {
			t.Fatalf("parse %v: %+v %v", argv, parsed, err)
		}
	}
	for _, argv := range [][]string{
		{"status", "remote:" + strings.Repeat("a", 32), "--include-execution-assurance"},
		{"context", "initialize", "--include-execution-assurance"},
		{"context", "enable-connector", "--include-execution-assurance"},
		{"resolve", strings.Repeat("a", 32), "--include-execution-assurance"},
	} {
		if _, err := ParseArguments(argv); err == nil {
			t.Fatalf("accepted %v", argv)
		}
	}
}

func TestExecutionAssuranceContextFeatureIsOptIn(t *testing.T) {
	f := newConnectorFixture(t)
	for _, include := range []bool{false, true} {
		args := []string{"context", "inspect", "--root", f.store.Root, "--json"}
		if include {
			args = append(args, "--include-execution-assurance")
		}
		var output, diagnostic bytes.Buffer
		if code := Run(args, &output, &diagnostic, f.deps); code != 0 {
			t.Fatalf("context exit=%d output=%s diagnostic=%s", code, output.String(), diagnostic.String())
		}
		var response struct {
			Connector map[string]json.RawMessage `json:"connector"`
		}
		if err := json.Unmarshal(output.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		feature, present := response.Connector["execution_assurance_feature"]
		if present != include || (include && string(feature) != `"task-lifetime-assurance-v1"`) {
			t.Fatalf("include=%v feature=%s present=%v", include, feature, present)
		}
	}
}

func TestExecutionAssuranceUnregisteredAndLegacyOmission(t *testing.T) {
	f := newConnectorFixture(t)
	for _, guarded := range []bool{false, true} {
		response := runExecutionAssuranceStatus(t, f, guarded, false)
		if response.ExecutionAssurance != nil {
			t.Fatalf("legacy status gained diagnostic: %+v", response)
		}
		response = runExecutionAssuranceStatus(t, f, guarded, true)
		assurance := response.ExecutionAssurance
		if assurance == nil || assurance.Feature != "task-lifetime-assurance-v1" || assurance.State != ExecutionAssuranceUnregistered {
			t.Fatalf("missing explicit registration absence: %+v", response)
		}
		if assurance.Backend != "" || assurance.Assurance != "" || assurance.RegistrationSHA256 != "" {
			t.Fatalf("unregistered task invented runtime facts: %+v", assurance)
		}
	}
}

func TestExecutionAssuranceDoesNotInferFromIntent(t *testing.T) {
	created := time.Now().UTC().Format(time.RFC3339Nano)
	observation, err := describeExecutionAssurance(task.LifetimeEvidence{Intent: &task.LifetimeIntentRecord{
		SchemaVersion: task.SchemaVersion, RootID: strings.Repeat("a", 32), TaskID: strings.Repeat("b", 32),
		OwnerKind: task.LifetimeOwnerTask, OwnerSHA256: strings.Repeat("c", 64),
		ScopeKind: task.LifetimeScopeCgroupV2, Assurance: task.LifetimeAssuranceExact,
		ScopePlan: "planned-cgroup", BootID: "planned-boot", Deadline: created, CreatedAt: created,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if observation.State != ExecutionAssuranceUnregistered || observation.Backend != "" || observation.Assurance != "" || observation.RegistrationSHA256 != "" {
		t.Fatalf("planned scope became registered execution assurance: %+v", observation)
	}
}

func runExecutionAssuranceStatus(t *testing.T, f *connectorFixture, guarded, include bool) Response {
	t.Helper()
	args := []string{"status", f.request.TaskID}
	if include {
		args = append(args, "--include-execution-assurance")
	}
	deps := f.deps
	if guarded {
		data, err := task.MarshalCanonical(f.origin)
		if err != nil {
			t.Fatal(err)
		}
		deps.Input = bytes.NewReader(data)
		args = append(args, "--expected-root", f.rootToken, "--expected-project", f.projectToken, "--expected-origin", "-")
	}
	args = append(args, "--root", f.store.Root, "--json")
	var output, diagnostic bytes.Buffer
	if code := Run(args, &output, &diagnostic, deps); code != 0 {
		t.Fatalf("status exit=%d output=%s diagnostic=%s", code, output.String(), diagnostic.String())
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(output.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if _, present := fields["execution_assurance"]; present != include {
		t.Fatalf("include=%v changed legacy status shape: %s", include, output.String())
	}
	var response Response
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func recordExecutionAssuranceFixture(t *testing.T, f *connectorFixture, backend, assurance string) {
	t.Helper()
	deadline := time.Now().Add(time.Minute).UTC()
	_, err := f.task.RecordLifetimeIntentContext(t.Context(), taskdir.LifetimeIntentFacts{
		ScopeKind: backend, Assurance: assurance, ScopePlan: "assurance-test-plan", BootID: "assurance-test-boot", Deadline: deadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.task.RecordLifetimeRegistrationContext(t.Context(), taskdir.LifetimeRegistrationFacts{
		ScopeKind: backend, Assurance: assurance, ScopeID: "/private/native/scope", BootID: "assurance-test-boot", Deadline: deadline,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestExecutionAssuranceProjectsRegisteredBackend(t *testing.T) {
	for _, test := range []struct {
		backend   string
		assurance string
	}{
		{backend: "cgroup-v2", assurance: "exact"},
		{backend: "process-group", assurance: "cooperative"},
	} {
		t.Run(test.backend, func(t *testing.T) {
			f := newConnectorFixture(t)
			recordExecutionAssuranceFixture(t, f, test.backend, test.assurance)
			data, err := os.ReadFile(filepath.Join(f.task.Dir, "lifetime", "registration.json"))
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(data)
			for _, guarded := range []bool{false, true} {
				runExecutionAssuranceStatus(t, f, guarded, false)
				response := runExecutionAssuranceStatus(t, f, guarded, true)
				assertRegisteredExecutionAssurance(t, response.ExecutionAssurance, test.backend, test.assurance, hex.EncodeToString(digest[:]))
			}
		})
	}
}

func assertRegisteredExecutionAssurance(t *testing.T, observation *ExecutionAssuranceObservation, backend, assurance, digest string) {
	t.Helper()
	if observation == nil || observation.State != ExecutionAssuranceRegistered || string(observation.Backend) != backend || string(observation.Assurance) != assurance || observation.RegistrationSHA256 != digest {
		t.Fatalf("incorrect registered projection: %+v", observation)
	}
	data, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"/private/native/scope", "boot", "deadline", "scope_id", "pid", "process_start"} {
		if bytes.Contains(data, []byte(private)) {
			t.Fatalf("diagnostic disclosed private scope facts: %s", data)
		}
	}
}

func TestExecutionAssuranceRejectsMalformedAndForeignRegistration(t *testing.T) {
	for _, fault := range []string{"malformed", "foreign", "assurance"} {
		t.Run(fault, func(t *testing.T) {
			f := newConnectorFixture(t)
			recordExecutionAssuranceFixture(t, f, "process-group", "cooperative")
			path := filepath.Join(f.task.Dir, "lifetime", "registration.json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "malformed":
				data = []byte(`{"schema_version":`)
			case "foreign":
				data = bytes.ReplaceAll(data, []byte(f.store.RootID), []byte(strings.Repeat("e", 32)))
			case "assurance":
				data = bytes.ReplaceAll(data, []byte(`"cooperative"`), []byte(`"exact"`))
			}
			if err = os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			response, code := f.public(t, "status", f.request.TaskID, "--include-execution-assurance")
			if code == 0 || response.ExecutionAssurance != nil || response.Error == "" {
				t.Fatalf("fault %s became a runtime claim: code=%d response=%+v", fault, code, response)
			}
		})
	}
}
