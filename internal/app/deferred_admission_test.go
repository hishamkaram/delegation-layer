package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestPendingRuntimeAdmissionSurvivesExecutableRefreshAndRechecksPayload(t *testing.T) {
	for _, name := range []string{"relinked executable", "unchanged launcher compatible payload", "unchanged launcher incompatible payload"} {
		t.Run(name, func(t *testing.T) { testPendingRuntimeUpgrade(t, name) })
	}
}

func testPendingRuntimeUpgrade(t *testing.T, name string) {
	t.Helper()
	store := newBareInspectionStore(t)
	request := newBareInspectionRequest(t, store)
	directory := canonicalAppTestTempDir(t)
	versionPath, helpPath := filepath.Join(directory, "version"), filepath.Join(directory, "help")
	writeAppTestFile(t, versionPath, []byte("provider-v1\n"))
	writeAppTestFile(t, helpPath, []byte("--run\n"))
	oldPath := filepath.Join(directory, "provider-old")
	writeRuntimeUpgradeLauncher(t, oldPath, versionPath, helpPath)
	admitted := runtimeUpgradeCandidate(t, request, oldPath)
	profile, err := admitCandidate(admitted, request)
	requireAppTest(t, err)
	supervisor, _, _ := newUncertainGroupSupervisor(t, store.RootID)
	meta := newMeta(request, profile, supervisor.Binding(), "", task.RunnerOwnershipManaged, "app-test")
	td, err := store.CreateTask(request.TaskID, &request, []byte("inspection brief"), &meta)
	requireAppTest(t, err)
	t.Cleanup(func() { requireAppTest(t, td.Close()) })
	persistedRequest, persistedMeta, err := td.PreparedRecords()
	requireAppTest(t, err)
	metadataBefore, err := os.ReadFile(filepath.Join(td.Dir, "meta.json"))
	requireAppTest(t, err)
	currentPath := oldPath
	if name == "relinked executable" {
		currentPath = filepath.Join(directory, "provider-new")
		writeRuntimeUpgradeLauncher(t, currentPath, versionPath, helpPath)
		requireAppTest(t, os.Remove(oldPath))
	}
	writeAppTestFile(t, versionPath, []byte("provider-v2\n"))
	incompatible := name == "unchanged launcher incompatible payload"
	if incompatible {
		writeAppTestFile(t, helpPath, []byte("--different\n"))
	}
	current := runtimeUpgradeCandidate(t, request, currentPath)
	deps := Dependencies{PrepareCandidate: func(task.TaskRecord) (commonprovider.ProfileCandidate, error) { return current, nil }}
	rebuilt, err := prepareMatchedProfile(deps, store.Root, *persistedRequest, *persistedMeta)
	requireAppTest(t, err)
	permit, err := td.PrepareStart(0)
	requireAppTest(t, err)
	starts := 0
	result := execution.Run(td, permit, rebuilt.Plan, execution.Options{
		PreflightPlan: func(scope execution.PreflightScope, _ execution.Plan) (execution.Plan, error) {
			return freshPreflightProfile(deps, store.Root, *persistedRequest, *persistedMeta, scope)
		},
		Hooks: execution.Hooks{Start: func(command *exec.Cmd) error { starts++; return command.Start() }},
	})
	requireAppTest(t, errors.Join(result.Error, result.CleanupError, result.ReceiptError, result.StopError))
	assertPendingRuntimeUpgradeOutcome(t, td.Dir, result, incompatible, starts, currentPath)
	metadataAfter, err := os.ReadFile(filepath.Join(td.Dir, "meta.json"))
	requireAppTest(t, err)
	if !bytes.Equal(metadataBefore, metadataAfter) {
		t.Fatal("runtime refresh mutated admission metadata")
	}
}

func assertPendingRuntimeUpgradeOutcome(t *testing.T, directory string, result execution.Result, incompatible bool, starts int, executable string) {
	t.Helper()
	verdict, expectedStarts := task.VerdictCommitted, 1
	if incompatible {
		verdict, expectedStarts = task.VerdictRejected, 0
	}
	if result.Outcome == nil || result.Outcome.Verdict != verdict || starts != expectedStarts {
		t.Fatalf("runtime upgrade outcome=%+v starts=%d, want %s/%d", result, starts, verdict, expectedStarts)
	}
	if !incompatible {
		data, err := os.ReadFile(filepath.Join(directory, "provider.started.json"))
		requireAppTest(t, err)
		var started task.ProviderStartedRecord
		requireAppTest(t, task.DecodeStrict(data, &started))
		if started.ProviderExecutable != executable || started.ProviderVersion != "provider-v2" {
			t.Fatalf("launch did not record refreshed runtime: %+v", started)
		}
	}
}

func writeRuntimeUpgradeLauncher(t *testing.T, executable, version, help string) {
	t.Helper()
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
	source := fmt.Sprintf("#!/bin/sh\ncase ${1-} in\n--version) exec /bin/cat %s ;;\n--help) exec /bin/cat %s ;;\n*) exec /bin/cat ;;\nesac\n", quote(version), quote(help))
	requireAppTest(t, os.WriteFile(executable, []byte(source), 0o700))
}

func runtimeUpgradeCandidate(t *testing.T, request task.TaskRecord, executable string) commonprovider.ProfileCandidate {
	t.Helper()
	cli, err := commonprovider.LocateCLIPath(executable)
	requireAppTest(t, err)
	definition, err := commonprovider.NewRuntimeInspectionDefinition(cli, request.CanonicalCwd, []string{"LANG=C"}, commonprovider.RuntimeCapability{RequiredFlags: []string{"--run"}})
	requireAppTest(t, err)
	capability, err := commonprovider.RuntimeCapabilityDigest(definition)
	requireAppTest(t, err)
	build := func(version, pending string) commonprovider.PreparedProfile {
		profile := appInspectionPreparedProfile(request, request.CanonicalCwd, nil)
		profile.Plan.Executable, profile.Plan.ExecutableSHA256 = cli.Path, cli.SHA256
		profile.Plan.Arguments = []string{"--run"}
		profile.Plan.Environment = []string{"LANG=C"}
		profile.ObservedVersion, profile.RuntimePreflightSHA256 = version, pending
		return profile
	}
	return commonprovider.ProfileCandidate{
		Directory: request.CanonicalCwd, Inspection: &definition,
		Admit: func() (commonprovider.PreparedProfile, error) { return build("", capability), nil },
		Finalize: func(data json.RawMessage, _ time.Time) (commonprovider.PreparedProfile, error) {
			facts, decodeErr := commonprovider.DecodeInspectionFacts(data)
			if decodeErr != nil || facts.Runtime == nil {
				return commonprovider.PreparedProfile{}, task.ErrEvidenceFault
			}
			return build(facts.Runtime.Version, ""), nil
		},
	}
}

func TestPendingRuntimeAdmissionReconstructsFromTaskMetadataWithoutInspection(t *testing.T) {
	store := newBareInspectionStore(t)
	request := newBareInspectionRequest(t, store)
	candidate := pendingRuntimeCandidate(t, request)
	profile, err := profileForAdmission(candidate, request, true)
	requireAppTest(t, err)
	supervisor, _, _ := newUncertainGroupSupervisor(t, store.RootID)
	meta := newMeta(request, profile, supervisor.Binding(), "", task.RunnerOwnershipManaged, "app-test")
	td, err := store.CreateTask(request.TaskID, &request, []byte("inspection brief"), &meta)
	requireAppTest(t, err)
	t.Cleanup(func() { requireAppTest(t, td.Close()) })
	persistedRequest, persistedMeta, err := td.PreparedRecords()
	requireAppTest(t, err)
	if persistedMeta.RuntimePreflightSHA256 == "" || persistedMeta.ProviderVersion != "" {
		t.Fatalf("admission fabricated runtime verification: %+v", persistedMeta)
	}
	deps := Dependencies{PrepareCandidate: func(task.TaskRecord) (commonprovider.ProfileCandidate, error) {
		return candidate, nil
	}}
	rebuilt, err := prepareMatchedProfile(deps, store.Root, *persistedRequest, *persistedMeta)
	requireAppTest(t, err)
	if rebuilt.RuntimePreflightSHA256 != persistedMeta.RuntimePreflightSHA256 || rebuilt.ObservedVersion != "" ||
		rebuilt.Plan.RuntimePreflightSHA256 != persistedMeta.RuntimePreflightSHA256 {
		t.Fatalf("reconstruction lost pending authority: %+v", rebuilt)
	}
	if _, err = os.Lstat(filepath.Join(store.Root, "inspections")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("static admission created inspection state: %v", err)
	}
}

func TestPendingRuntimeCandidateRejectsChangedContractBeforeProbe(t *testing.T) {
	store := newBareInspectionStore(t)
	request := newBareInspectionRequest(t, store)
	candidate := pendingRuntimeCandidate(t, request)
	profile, err := admitCandidate(candidate, request)
	requireAppTest(t, err)
	meta := task.MetaRecord{RuntimePreflightSHA256: profile.RuntimePreflightSHA256, Environment: profile.Plan.Environment, EnvironmentRecorded: true}
	requireAppTest(t, validatePendingRuntimeCandidate(candidate, meta))
	for _, test := range []struct {
		name   string
		mutate func(*commonprovider.InspectionDefinition)
	}{
		{"capability", func(definition *commonprovider.InspectionDefinition) {
			definition.Runtime.RequiredFlags = []string{"--changed"}
		}},
		{"environment", func(definition *commonprovider.InspectionDefinition) {
			definition.Environment = []string{"LANG=en_US.UTF-8"}
		}},
		{"native projection", func(definition *commonprovider.InspectionDefinition) {
			definition.Project = func([]byte) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := pendingRuntimeCandidate(t, request)
			test.mutate(changed.Inspection)
			if err := validatePendingRuntimeCandidate(changed, meta); !errors.Is(err, task.ErrIdentityMismatch) {
				t.Fatalf("changed contract passed before probe: %v", err)
			}
		})
	}
}

func pendingRuntimeCandidate(t *testing.T, request task.TaskRecord) commonprovider.ProfileCandidate {
	t.Helper()
	candidate := runtimeAdmissionCandidate(t, request, "provider", "--run")
	capability, err := commonprovider.RuntimeCapabilityDigest(*candidate.Inspection)
	requireAppTest(t, err)
	profile := appInspectionPreparedProfile(request, request.CanonicalCwd, nil)
	profile.Plan.Executable = candidate.Inspection.Executable
	profile.Plan.ExecutableSHA256 = candidate.Inspection.ExecutableSHA256
	profile.Plan.Environment = append([]string(nil), candidate.Inspection.Environment...)
	profile.ObservedVersion = ""
	profile.RuntimePreflightSHA256 = capability
	candidate.Admit = func() (commonprovider.PreparedProfile, error) { return profile, nil }
	candidate.Finalize = func(json.RawMessage, time.Time) (commonprovider.PreparedProfile, error) {
		return commonprovider.PreparedProfile{}, errors.New("unexpected inspection finalization")
	}
	return candidate
}
