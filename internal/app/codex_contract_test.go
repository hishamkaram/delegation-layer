package app

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/admission/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/provider/codex"
	testsupportcore "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

type codexContractFixture struct {
	store   *taskdir.Store
	request task.TaskRecord
	meta    task.MetaRecord
	flags   []string
}

func newCodexContractFixture(t *testing.T, mode, version string) codexContractFixture {
	t.Helper()
	home, err := os.UserHomeDir()
	testsupport.RequireAppTest(t, err)
	base := mustCCRContinuationTempDir(t, home, ".delegate-codex-contract-")
	base, err = filepath.EvalSymlinks(base)
	testsupport.RequireAppTest(t, err)
	testsupport.RequireAppTest(t, os.MkdirAll(filepath.Join(base, "home", ".codex"), 0o700))
	cliDir := testsupportcore.CanonicalTempDir(t)
	testsupport.RequireAppTest(t, os.WriteFile(filepath.Join(cliDir, "codex"), []byte("#!/bin/sh\ncase $1 in\n--version) printf 'codex fixture\\n' ;;\n*) printf '%s\\n' '-c --sandbox --cd --output-last-message --json --color' ;;\nesac\n"), 0o700))
	t.Setenv("PATH", cliDir)
	t.Setenv("HOME", filepath.Join(base, "home"))
	t.Setenv("CODEX_HOME", filepath.Join(base, "home", ".codex"))
	catalog := NativeCatalog()
	store, err := taskdir.InitStoreWithPredicates(filepath.Join(base, "state"), catalog.Registry())
	testsupport.RequireAppTest(t, err)
	t.Cleanup(func() { testsupport.RequireAppTest(t, store.Close()) })
	request := newBareInspectionRequest(t, store)
	request.Provider, request.Mode, request.RequestedConfig.Permission = codex.Provider, mode, mode
	candidate, err := catalog.Candidate(request)
	testsupport.RequireAppTest(t, err)
	profile, err := admission.ProfileForAdmission(candidate, request, true)
	testsupport.RequireAppTest(t, err)
	meta := tasklifecycle.NewMeta(request, profile, testsupport.ContinuationSupervisorRef(store.Root, strings.Repeat("a", 64), filepath.Join(base, "supervisor.sock")), "", task.RunnerOwnershipManaged, "app-test")
	for _, interpreter := range codex.Registration().Interpreters {
		ref := interpreter.Reference()
		if ref.Adapter == codex.Provider && ref.Mode == mode && ref.Version == version {
			meta.Predicate = ref
		}
	}
	if meta.Predicate.Version != version {
		t.Fatalf("missing recorded Codex contract %s/%s", mode, version)
	}
	flags := []string{"-c", "--sandbox", "--cd", "--output-last-message", "--json", "--color"}
	if version == "runtime-reported-v4" {
		flags = append(flags, "--skip-git-repo-check")
	}
	cli, err := commonprovider.LocateCLIPath(profile.Plan.Executable)
	testsupport.RequireAppTest(t, err)
	definition, err := commonprovider.NewRuntimeInspectionDefinition(cli, request.CanonicalCwd, meta.Environment,
		commonprovider.RuntimeCapability{HelpArgs: []string{"exec"}, RequiredFlags: flags})
	testsupport.RequireAppTest(t, err)
	meta.RuntimePreflightSHA256, err = commonprovider.RuntimeCapabilityDigest(definition)
	testsupport.RequireAppTest(t, err)
	td, err := store.CreateTask(request.TaskID, &request, []byte("inspection brief"), &meta)
	testsupport.RequireAppTest(t, err)
	testsupport.RequireAppTest(t, td.Close())
	return codexContractFixture{store: store, request: request, meta: meta, flags: flags}
}

func TestCodexPendingRuntimeV3Reconstruction(t *testing.T) {
	for _, mode := range []string{codex.Mode, codex.WorkspaceWriteMode} {
		t.Run(mode, func(t *testing.T) {
			fixture := newCodexContractFixture(t, mode, "runtime-reported-v3")
			td, err := fixture.store.OpenTask(fixture.request.TaskID)
			testsupport.RequireAppTest(t, err)
			t.Cleanup(func() { testsupport.RequireAppTest(t, td.Close()) })
			request, meta, err := td.PreparedRecords()
			testsupport.RequireAppTest(t, err)
			before, err := os.ReadFile(filepath.Join(td.Dir, "meta.json"))
			testsupport.RequireAppTest(t, err)
			profile, err := prepareMatchedProfile(Dependencies{Catalog: NativeCatalog()}, fixture.store.Root, *request, *meta)
			testsupport.RequireAppTest(t, err)
			assertCodexRecordedProfile(t, fixture, profile, false)
			if profile.Plan.RuntimePreflightSHA256 != meta.RuntimePreflightSHA256 {
				t.Fatal("pending plan lost its recorded capability digest")
			}
			after, err := os.ReadFile(filepath.Join(td.Dir, "meta.json"))
			testsupport.RequireAppTest(t, err)
			if !bytes.Equal(before, after) {
				t.Fatal("reconstruction mutated metadata")
			}
			if _, err := os.Lstat(filepath.Join(fixture.store.Root, "inspections")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("static reconstruction created inspection state: %v", err)
			}
		})
	}
}

func TestCodexContinuationPreservesRecordedContract(t *testing.T) {
	for _, mode := range []string{codex.Mode, codex.WorkspaceWriteMode} {
		for _, version := range []string{"runtime-reported-v3", "runtime-reported-v4"} {
			t.Run(mode+"/"+version, func(t *testing.T) {
				fixture := newCodexContractFixture(t, mode, version)
				td, err := fixture.store.OpenTask(fixture.request.TaskID)
				testsupport.RequireAppTest(t, err)
				recordContinuationTestIdentity(t, td, codex.Provider, "11111111-1111-4111-8111-111111111111")
				prior, err := tasklifecycle.ContinuationPrior(td, &fixture.request)
				testsupport.RequireAppTest(t, err)
				testsupport.RequireAppTest(t, td.Close())
				options, _, err := tasklifecycle.ContinuationOptionsForTask(fixture.store, dispatchOptions(Dependencies{Catalog: NativeCatalog()}, true), fixture.request.TaskID)
				testsupport.RequireAppTest(t, err)
				next := fixture.request
				next.TaskID, next.PriorSession = strings.Repeat("b", 32), prior
				candidate, err := options.Creation.Admission.Profiles.Prepare(fixture.store.Root, next)
				testsupport.RequireAppTest(t, err)
				if !slices.Equal(candidate.Inspection.Runtime.RequiredFlags, fixture.flags) {
					t.Fatalf("continuation requirements drifted: %+v", candidate.Inspection.Runtime)
				}
				profile, err := admission.ProfileForAdmission(candidate, next, true)
				testsupport.RequireAppTest(t, err)
				assertCodexRecordedProfile(t, fixture, profile, true)
			})
		}
	}
}

func assertCodexRecordedProfile(t *testing.T, fixture codexContractFixture, profile commonprovider.PreparedProfile, resume bool) {
	t.Helper()
	want := []string{
		"exec", "--json", "--color", "never", "--sandbox", fixture.request.Mode,
		"-c", `approval_policy="never"`, "--cd", fixture.request.CanonicalCwd, "--output-last-message", "",
	}
	if fixture.meta.Predicate.Version == "runtime-reported-v4" {
		want = append(want, "--skip-git-repo-check")
	}
	if resume {
		want = append(want, "resume", "11111111-1111-4111-8111-111111111111")
	}
	want = append(want, "-")
	if !slices.Equal(profile.Plan.Arguments, want) || !profile.Plan.Predicate.Equal(fixture.meta.Predicate) ||
		profile.ObservedVersion != "" || profile.RuntimePreflightSHA256 != fixture.meta.RuntimePreflightSHA256 ||
		!slices.Equal(profile.Plan.OutputArtifacts, []task.OutputArtifact{{Name: codex.OutputName, ArgumentIndex: 11}}) {
		t.Fatalf("recorded Codex profile changed: %+v want argv=%q", profile, want)
	}
}

func TestCodexMissingNonGitCapabilityRejectsBeforeLaunch(t *testing.T) {
	fixture := newCodexContractFixture(t, codex.Mode, "runtime-reported-v4")
	td, err := fixture.store.OpenTask(fixture.request.TaskID)
	testsupport.RequireAppTest(t, err)
	t.Cleanup(func() { testsupport.RequireAppTest(t, td.Close()) })
	request, meta, err := td.PreparedRecords()
	testsupport.RequireAppTest(t, err)
	deps := Dependencies{Catalog: NativeCatalog()}
	profile, err := prepareMatchedProfile(deps, fixture.store.Root, *request, *meta)
	testsupport.RequireAppTest(t, err)
	permit, err := td.PrepareStart(0)
	testsupport.RequireAppTest(t, err)
	starts := 0
	result := execution.Run(td, permit, profile.Plan, execution.Options{
		PreflightPlan: func(scope execution.PreflightScope, _ execution.Plan) (execution.Plan, error) {
			return freshPreflightProfile(deps, fixture.store.Root, *request, *meta, scope)
		},
		Hooks: execution.Hooks{Start: func(command *exec.Cmd) error { starts++; return command.Start() }},
	})
	testsupport.RequireAppTest(t, errors.Join(result.Error, result.CleanupError, result.ReceiptError, result.StopError))
	if starts != 0 || result.Outcome == nil || result.Outcome.Verdict != task.VerdictRejected {
		t.Fatalf("missing capability launched provider: starts=%d outcome=%+v", starts, result.Outcome)
	}
}
