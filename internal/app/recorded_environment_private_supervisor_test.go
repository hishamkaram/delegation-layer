package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
)

// TestPrivateContinuationKeepsPredecessorBindingOverCallerSupervisorConfig
// runs both public commands for a predecessor bound to the state root's
// private supervisor while the caller's scope names another configuration in
// DELEGATE_PUEUE_CONFIG. That scope value cannot retarget the private binding:
// the successor is admitted on the predecessor's own private generation and
// the process environment is unchanged. The paired explicit --pueue-config
// keeps its unchanged authority: continue refuses it for a private
// predecessor, and dispatch --resume-task binds the named configuration.
func TestPrivateContinuationKeepsPredecessorBindingOverCallerSupervisorConfig(t *testing.T) {
	testCase := recordedEnvironmentCases()[0]
	for _, entryPoint := range []string{"continue --task", "dispatch --resume-task"} {
		t.Run(entryPoint+"/caller scope config", func(t *testing.T) {
			requireCallerScopeKeepsPrivateBinding(t, testCase, entryPoint)
		})
		t.Run(entryPoint+"/explicit external config", func(t *testing.T) {
			requireExplicitConfigAuthority(t, testCase, entryPoint)
		})
	}
}

// requireCallerScopeKeepsPrivateBinding admits the successor without
// --pueue-config while the caller scope's DELEGATE_PUEUE_CONFIG names an
// external configuration.
func requireCallerScopeKeepsPrivateBinding(t *testing.T, testCase recordedEnvironmentCase, entryPoint string) {
	t.Helper()
	fixture := newRecordedEnvironmentFixtureWith(t, testCase, recordedFixtureOptions{supervisor: recordedPrivateSupervisor})
	callerConfig := os.Getenv("DELEGATE_PUEUE_CONFIG")
	if callerConfig == "" || pueue.IsPrivateConfig(fixture.root, callerConfig) {
		t.Fatal("caller scope does not name an external supervisor configuration")
	}
	code, response := fixture.run(t, entryPoint, "")
	if code != 0 || response.TaskRecord != "created" || response.TaskID == "" {
		t.Fatalf("%s exit=%d record=%s failure=%+v", entryPoint, code, response.TaskRecord, response.Failure)
	}
	_, predecessorMeta := fixture.loadRecords(t, fixture.predecessorID)
	_, successorMeta := fixture.loadRecords(t, response.TaskID)
	if successorMeta.SupervisorConfig != predecessorMeta.SupervisorConfig || !pueue.IsPrivateConfig(fixture.root, successorMeta.SupervisorConfig.ConfigPath) {
		t.Fatal("successor left the predecessor's private supervisor binding")
	}
	if got := os.Getenv("DELEGATE_PUEUE_CONFIG"); got != callerConfig {
		t.Fatal("continuation changed the process supervisor configuration")
	}
	requirePrivateSubmissions(t, fixture, "add\n")
	fixture.assertPredecessorUnchanged(t)
}

// requireExplicitConfigAuthority passes a live external configuration as an
// explicit --pueue-config. continue keeps the canonical refusal of any
// explicit configuration for a private predecessor; dispatch --resume-task
// keeps the caller's explicit authority and binds the named configuration.
func requireExplicitConfigAuthority(t *testing.T, testCase recordedEnvironmentCase, entryPoint string) {
	t.Helper()
	// The explicit configuration is allocated before the fixture installs the
	// simulated caller scope, so its temporary directory comes from the real
	// native TMPDIR and the simulated scope does not lengthen its supervisor
	// endpoint.
	explicitConfig := writeExplicitExternalConfig(t)
	fixture := newRecordedEnvironmentFixtureWith(t, testCase, recordedFixtureOptions{supervisor: recordedPrivateSupervisor})
	requireOutsideCallerTmp(t, explicitConfig, fixture.caller.tmpDir)
	code, response := fixture.run(t, entryPoint, explicitConfig)
	requirePrivateSubmissions(t, fixture, "")
	fixture.assertPredecessorUnchanged(t)
	explicitSubmissions := readSubmissions(t, filepath.Join(filepath.Dir(explicitConfig), "submissions"))
	if entryPoint == "continue --task" {
		if code == 0 || response.TaskRecord != "not_created" || response.Failure == nil ||
			response.Failure.Code != "task_request_conflict" || response.Failure.Stage != "validate-continuation" {
			t.Fatalf("continue accepted an explicit override of a private predecessor: exit=%d record=%s failure=%+v", code, response.TaskRecord, response.Failure)
		}
		if explicitSubmissions != "" {
			t.Fatalf("refused continuation submitted to the explicit supervisor: %q", explicitSubmissions)
		}
		return
	}
	if code != 0 || response.TaskRecord != "created" || response.TaskID == "" {
		t.Fatalf("dispatch exit=%d record=%s failure=%+v", code, response.TaskRecord, response.Failure)
	}
	if _, successorMeta := fixture.loadRecords(t, response.TaskID); successorMeta.SupervisorConfig.ConfigPath != explicitConfig {
		t.Fatal("dispatch replaced the explicit supervisor configuration")
	}
	if explicitSubmissions != "add\n" {
		t.Fatalf("explicit supervisor submissions = %q, want one add", explicitSubmissions)
	}
}

// requireOutsideCallerTmp fails when path lies inside the simulated caller
// scope's temporary directory, which would lengthen every path allocated there.
func requireOutsideCallerTmp(t *testing.T, path, callerTmp string) {
	t.Helper()
	relative, err := filepath.Rel(callerTmp, path)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.IsLocal(relative) {
		t.Fatalf("explicit configuration %s was allocated inside the caller scope's temporary directory", path)
	}
}

// requirePrivateSubmissions compares the private pair's stashed adds with want.
func requirePrivateSubmissions(t *testing.T, fixture *recordedEnvironmentFixture, want string) {
	t.Helper()
	if got := readSubmissions(t, fixture.submissions); got != want {
		t.Fatalf("private supervisor submissions = %q, want %q", got, want)
	}
}

// readSubmissions returns a fake supervisor's submission log; an absent log
// means no submission.
func readSubmissions(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(data)
}

// writeExplicitExternalConfig writes a ready external configuration that the
// fixture's fake client serves from the configuration's directory, so an
// explicit --pueue-config names a live supervisor other than the private one.
func writeExplicitExternalConfig(t *testing.T) string {
	t.Helper()
	base := testsupport.CanonicalTempDir(t)
	configPath := filepath.Join(base, "pueue.json")
	config, err := json.Marshal(map[string]map[string]string{"shared": {
		"pueue_directory":    filepath.Join(base, "data"),
		"runtime_directory":  filepath.Join(base, "runtime"),
		"unix_socket_path":   filepath.Join(base, "runtime", "pueue.sock"),
		"alias_file":         filepath.Join(base, "aliases"),
		"pid_path":           filepath.Join(base, "runtime", "pueue.pid"),
		"shared_secret_path": filepath.Join(base, "secret"),
		"daemon_cert":        filepath.Join(base, "cert"),
		"daemon_key":         filepath.Join(base, "key"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	writeAppTestFile(t, configPath, config)
	writeAppTestFile(t, filepath.Join(base, "ready"), nil)
	return configPath
}
