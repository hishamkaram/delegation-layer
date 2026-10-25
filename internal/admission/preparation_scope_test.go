package admission

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type staticCandidateSource struct {
	candidate commonprovider.ProfileCandidate
}

func (source staticCandidateSource) Current(task.TaskRecord, []string) (commonprovider.ProfileCandidate, error) {
	return source.candidate, nil
}

func (source staticCandidateSource) ForTask(task.TaskRecord, *task.MetaRecord, []string) (commonprovider.ProfileCandidate, bool, error) {
	return source.candidate, false, nil
}

func TestPrepareBindsFreshInspectionControlToSelectedScope(t *testing.T) {
	base := supervisorfixture.CanonicalTempDir(t)
	selectedTmp, callerTmp, ambientTmp := filepath.Join(base, "selected-tmp"), filepath.Join(base, "caller-tmp"), filepath.Join(base, "ambient-tmp")
	for _, directory := range []string{selectedTmp, callerTmp, ambientTmp} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := os.Getenv("PATH")
	selected := []string{"PATH=" + path, "TMPDIR=" + selectedTmp}
	tests := []struct {
		name       string
		supervisor func(supervisorcore.AdmissionOptions) supervisorcore.AdmissionOptions
		want       string
	}{
		{name: "fresh admission uses the captured scope", supervisor: func(options supervisorcore.AdmissionOptions) supervisorcore.AdmissionOptions { return options }, want: selectedTmp},
		{name: "prebound continuation control is kept", supervisor: func(options supervisorcore.AdmissionOptions) supervisorcore.AdmissionOptions {
			return supervisorcore.AdmissionOptionsForScope(options, []string{"PATH=" + path, "TMPDIR=" + callerTmp}, selected)
		}, want: callerTmp},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			probe := writeTMPDIRProbeSupervisor(t)
			store := supervisorfixture.NewInspectionStore(t)
			request := supervisorfixture.NewInspectionRequest(t, store)
			t.Setenv("TMPDIR", ambientTmp)
			options := PreparationOptions{
				Profiles:   NewProfiles(staticCandidateSource{candidate: scopeInspectionCandidate(request)}, nil, selected),
				Supervisor: test.supervisor(supervisorcore.AdmissionOptions{InitialExecutable: probe.executable, Environment: selected}),
			}
			selection := supervisorcore.AdmissionSelection{ConfigPath: probe.configPath, Runner: writeScopeRunner(t), RunnerOwnership: task.RunnerOwnershipCustom}
			_, err := Prepare(selection, options, store, request)
			if err == nil {
				t.Fatal("probe supervisor unexpectedly completed admission")
			}
			if got := probe.seen(t, err); got != test.want {
				t.Fatalf("admission supervisor control launched with TMPDIR=%q, want %q (ambient %q)", got, test.want, ambientTmp)
			}
		})
	}
}

func TestWorkspaceRuntimeMaintenanceLaunchUsesSelectedScope(t *testing.T) {
	t.Setenv("LANG", "issue91-ambient")
	tests := []struct {
		name        string
		environment []string
		maintenance []string
		want        []string
	}{
		{name: "captured scope", environment: []string{"LANG=issue91-selected", "SECRET_TOKEN=hidden", "PATH=/selected-bin"}, want: []string{"LANG=issue91-selected", "PATH=/selected-bin"}},
		{name: "explicit maintenance override", environment: []string{"LANG=issue91-selected"}, maintenance: []string{"LANG=issue91-explicit"}, want: []string{"LANG=issue91-explicit"}},
		{name: "explicit empty maintenance", environment: []string{"LANG=issue91-selected"}, maintenance: []string{}, want: []string{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, operation, bound, job := admissionfixture.NewRecordedTerminalCleanupFixture(t, TerminalEvidence{})
			var received []string
			launches := 0
			control := pueue.Options{
				ObservationTimeout:            pueue.DefaultObservationTimeout,
				SuccessfulCleanupAuthorizer:   newInspectionTestCleanup().AuthorizeSuccessfulCleanup,
				PrivateMaintenanceEnvironment: test.maintenance,
				PrivateMaintenanceLauncher: func(_ context.Context, _, _ string, environment []string) error {
					launches++
					received = slices.Clone(environment)
					return nil
				},
			}
			runtime := NewWorkspaceRuntime(supervisorcore.AdmissionOptions{Environment: test.environment, Control: control})
			client, err := pueue.NewClient(bound.Binding(), runtime.controlOptions())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if closeErr := client.Close(); closeErr != nil {
					t.Error(closeErr)
				}
			})
			observation := pueue.InspectionObservation{Binding: client.Binding(), Identity: operation.Request().SupervisorIdentity(), Job: job, Matched: true}
			if _, terminal, acceptErr := acceptCompletedInspectionObservation(t.Context(), operation, client, observation); acceptErr != nil || !terminal {
				t.Fatalf("accepted terminal observation terminal=%t err=%v", terminal, acceptErr)
			}
			// Report only the probed key: a failure may carry the inherited environment.
			if launches != 1 || received == nil || !slices.Equal(received, test.want) {
				t.Fatalf("maintenance launches=%d entries=%d ambientLANG=%t, want %q", launches, len(received), slices.Contains(received, "LANG=issue91-ambient"), test.want)
			}
		})
	}
}

// scopeInspectionCandidate declares a nonempty provider control environment,
// so admission merges it into the supervisor control environment.
func scopeInspectionCandidate(request task.TaskRecord) commonprovider.ProfileCandidate {
	workspace := request.CanonicalCwd
	digest := task.ComputeSHA256([]byte("fake"))
	environment := []string{"PROVIDER_PROFILE=fixture"}
	definition := commonprovider.InspectionDefinition{
		Revision: commonprovider.RuntimeInspectionRevision, Executable: "/fake/provider", ExecutableSHA256: digest,
		Directory: workspace, Environment: environment, OutputLimit: commonprovider.MaxInspectionOutput,
		Runtime: &commonprovider.RuntimeProbeDefinition{Executable: "/fake/provider", ExecutableSHA256: digest, Directory: workspace, Environment: environment, RequiredFlags: []string{"--fixture"}},
	}
	return commonprovider.ProfileCandidate{Directory: workspace, Inspection: &definition, Finalize: func(json.RawMessage, time.Time) (commonprovider.PreparedProfile, error) {
		return commonprovider.PreparedProfile{}, commonprovider.ErrProfileUnavailable
	}}
}

type tmpdirProbeSupervisor struct {
	executable string
	configPath string
}

// writeTMPDIRProbeSupervisor writes the TMPDIR-recording probe supervisor
// with an external configuration and clears its bind-time observation.
func writeTMPDIRProbeSupervisor(t *testing.T) tmpdirProbeSupervisor {
	t.Helper()
	base := supervisorfixture.CanonicalTempDir(t)
	probe := tmpdirProbeSupervisor{executable: filepath.Join(base, "pueue"), configPath: filepath.Join(base, "pueue.json")}
	if err := os.WriteFile(probe.executable, []byte(tmpdirProbeScript), 0o700); err != nil {
		t.Fatal(err)
	}
	admissionfixture.WriteUncertainGroupSupervisorConfig(t, base, probe.configPath, "scope", filepath.Join(base, "status.json"), filepath.Join(base, "status-after.json"))
	return probe
}

func (probe tmpdirProbeSupervisor) seen(t *testing.T, admissionErr error) string {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(filepath.Dir(probe.executable), "tmpdir-seen"))
	if err != nil {
		t.Fatalf("probe supervisor was not launched (admission error %v): %v", admissionErr, err)
	}
	return string(got)
}

func writeScopeRunner(t *testing.T) string {
	t.Helper()
	runner := filepath.Join(supervisorfixture.CanonicalTempDir(t), "delegate-run")
	if err := os.WriteFile(runner, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return runner
}
