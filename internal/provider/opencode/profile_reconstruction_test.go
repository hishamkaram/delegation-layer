package opencode

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestExistingNativeProfilePreservesPersistedPredicate(t *testing.T) {
	for _, mode := range []string{ModeReadOnly, ModeWorkspaceWrite} {
		t.Run(mode, func(t *testing.T) {
			for _, revision := range []struct {
				name string
				ref  task.PredicateRef
			}{
				{name: "v2", ref: historicalReferenceForMode(mode)},
				{name: "v3", ref: historicalCurrentReferenceForMode(mode)},
				{name: "v4", ref: historicalCurrentV4ReferenceForMode(mode)},
				{name: "v5", ref: runReferenceForMode(mode)},
			} {
				t.Run(revision.name, func(t *testing.T) {
					testPersistedProfile(t, mode, revision.ref)
				})
			}
		})
	}
}

func TestCurrentNativeProfileAdmitsWithoutRuntimeFacts(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, "opencode"), []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", directory)
	t.Setenv("PATH", directory)
	request := profileRequest(ModeReadOnly)
	request.CanonicalCwd = directory
	candidate, err := PrepareCandidate(request, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	static := requireStaticAdmission(t, candidate)
	assertFinalizedAdmission(t, candidate, static)
}

func requireStaticAdmission(t *testing.T, candidate commonprovider.ProfileCandidate) commonprovider.PreparedProfile {
	t.Helper()
	if candidate.Admit == nil {
		t.Fatal("current native candidate does not expose static admission")
	}
	static, err := candidate.Admit()
	if err != nil {
		t.Fatal(err)
	}
	if static.ObservedVersion != "" || static.Plan.ProviderRuntime != (task.ProviderRuntimeIdentity{}) {
		t.Fatalf("static admission claimed runtime facts: version=%q runtime=%+v", static.ObservedVersion, static.Plan.ProviderRuntime)
	}
	err = task.ValidateSHA256(static.RuntimePreflightSHA256)
	if err != nil {
		t.Fatalf("static capability digest=%q: %v", static.RuntimePreflightSHA256, err)
	}
	if candidate.Inspection == nil {
		t.Fatal("current native candidate omitted runtime inspection")
	}
	capability, err := commonprovider.RuntimeCapabilityDigest(*candidate.Inspection)
	if err != nil {
		t.Fatal(err)
	}
	if static.RuntimePreflightSHA256 != capability {
		t.Fatalf("static capability digest=%q want=%q", static.RuntimePreflightSHA256, capability)
	}
	if static.Plan.RuntimePreflightSHA256 != "" {
		t.Fatalf("provider static admission populated plan marker=%q", static.Plan.RuntimePreflightSHA256)
	}
	return static
}

func assertFinalizedAdmission(t *testing.T, candidate commonprovider.ProfileCandidate, static commonprovider.PreparedProfile) {
	t.Helper()
	if _, err := candidate.Finalize(nil, time.Now()); err == nil {
		t.Fatal("finalizer accepted missing runtime facts")
	}
	facts, err := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{
		Executable: candidate.Inspection.Executable, Version: "OpenCode fixture", SHA256: candidate.Inspection.ExecutableSHA256,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	finalized, err := candidate.Finalize(facts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if finalized.RuntimePreflightSHA256 != "" || finalized.ObservedVersion == "" {
		t.Fatalf("finalized profile retained pending runtime state: %+v", finalized)
	}
	if !slices.Equal(static.Plan.Arguments, finalized.Plan.Arguments) ||
		!slices.Equal(static.Plan.Environment, finalized.Plan.Environment) ||
		!task.CompareEffectiveConfigs(static.Effective, finalized.Effective) {
		t.Fatalf("static and finalized launch contracts differ: static=%+v finalized=%+v", static, finalized)
	}
}

func TestPendingCurrentNativeProfileReconstructsStaticAdmission(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, "opencode"), []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", directory)
	t.Setenv("PATH", directory)
	request := profileRequest(ModeReadOnly)
	request.CanonicalCwd = directory
	candidate, err := PrepareCandidate(request, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := candidate.Admit()
	if err != nil {
		t.Fatal(err)
	}
	pending, err := PrepareExistingCandidate(request, task.MetaRecord{
		EffectiveConfig:        admitted.Effective,
		RuntimePreflightSHA256: admitted.RuntimePreflightSHA256,
		Environment:            slices.Clone(admitted.Plan.Environment),
		EnvironmentRecorded:    true,
		Predicate:              admitted.Plan.Predicate,
	}, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	if pending.Admit == nil {
		t.Fatal("pending current native profile lost static admission")
	}
	rebuilt, err := pending.Admit()
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.ObservedVersion != "" || rebuilt.RuntimePreflightSHA256 != admitted.RuntimePreflightSHA256 {
		t.Fatalf("pending reconstruction claimed unexpected runtime state: rebuilt=%+v admitted=%+v", rebuilt, admitted)
	}
	if !slices.Equal(rebuilt.Plan.Arguments, admitted.Plan.Arguments) ||
		!slices.Equal(rebuilt.Plan.Environment, admitted.Plan.Environment) ||
		!task.CompareEffectiveConfigs(rebuilt.Effective, admitted.Effective) {
		t.Fatalf("pending reconstruction changed static launch contract: rebuilt=%+v admitted=%+v", rebuilt, admitted)
	}
}

// testPersistedProfile reconstructs a native run record stored under
// reference: the record keeps its run argv, identity observer, effective
// configuration and predicate.
func testPersistedProfile(t *testing.T, mode string, reference task.PredicateRef) {
	t.Helper()
	directory := installFixtureExecutable(t)
	request := profileRequest(mode)
	request.CanonicalCwd = directory
	meta, _ := historicalRunRecord(t, request, dialectNativeV2)
	meta.Predicate = reference
	existing, err := PrepareExistingCandidate(request, meta, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	wantArguments, err := runArguments(request, dialectNativeV2)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt := finalizeWithFixtureRuntime(t, existing)
	if !rebuilt.Plan.Predicate.Equal(reference) || !slices.Equal(rebuilt.Plan.Arguments, wantArguments) || !task.CompareEffectiveConfigs(meta.EffectiveConfig, rebuilt.Effective) {
		t.Fatalf("predicate or launch contract changed: rebuilt=%+v argv=%q", rebuilt.Plan.Predicate, rebuilt.Plan.Arguments)
	}
	if rebuilt.Identity == nil || rebuilt.Protocol != nil {
		t.Fatal("a run record must keep its identity observer and no protocol")
	}
}

func TestCurrentACPProfileLaunchesInPlanDirectory(t *testing.T) {
	directory := installFixtureExecutable(t)
	request := profileRequest(ModeWorkspaceWrite)
	request.CanonicalCwd = directory
	request.RequestedConfig.Model, request.RequestedConfig.Effort = "anthropic/claude-sonnet", "high"
	candidate, err := PrepareCandidate(request, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	requireACPProfile(t, candidate, requireStaticAdmission(t, candidate), directory)
	// ACP sets an explicit effort on the current model and refuses the turn
	// when OpenCode does not acknowledge it, so no model is required here.
	request.RequestedConfig.Model = ""
	if _, err = PrepareCandidate(request, os.Environ()); err != nil {
		t.Fatalf("explicit effort without a model: %v", err)
	}
}

func requireACPProfile(t *testing.T, candidate commonprovider.ProfileCandidate, admitted commonprovider.PreparedProfile, directory string) {
	t.Helper()
	if !slices.Equal(admitted.Plan.Arguments, []string{"acp"}) || admitted.Plan.Directory != directory || candidate.Directory != directory {
		t.Fatalf("argv=%q directory=%q candidate directory=%q want argv [acp] in %q", admitted.Plan.Arguments, admitted.Plan.Directory, candidate.Directory, directory)
	}
	if policy := admitted.Effective.Policy; policy.ProfileRevision != acpProfileRevision || admitted.Effective.Approval != "build:allow_once" {
		t.Fatalf("profile revision=%q approval=%q", policy.ProfileRevision, admitted.Effective.Approval)
	}
	if runtime := candidate.Inspection.Runtime; candidate.Inspection.Directory != directory || !slices.Equal(runtime.HelpArgs, []string{"acp"}) || len(runtime.RequiredFlags) != 0 {
		t.Fatalf("inspection directory=%q help=%q flags=%q", candidate.Inspection.Directory, runtime.HelpArgs, runtime.RequiredFlags)
	}
	if admitted.Protocol == nil || admitted.Identity != nil || admitted.Interaction != nil {
		t.Fatal("the ACP profile must supply only the machine protocol factory")
	}
	if !admitted.Plan.Predicate.Equal(ReferenceForMode(ModeWorkspaceWrite)) {
		t.Fatalf("predicate=%+v want the ACP reference", admitted.Plan.Predicate)
	}
	if commonprovider.HasNativeProfile(task.MetaRecord{EffectiveConfig: admitted.Effective}) {
		t.Fatal("current OpenCode profile was reported as the shared native revision")
	}
}

// TestHistoricalRunProfilesReconstructUnchanged binds a record admitted
// under each native run dialect to its original argv, effective digest, and
// static runtime capability digest, both pending and finalized. The current
// ACP dialect never replaces a recorded run launch.
func TestHistoricalRunProfilesReconstructUnchanged(t *testing.T) {
	for _, dialect := range []launchDialect{dialectNativeV1, dialectNativeV2} {
		for _, mode := range []string{ModeReadOnly, ModeWorkspaceWrite} {
			t.Run(fmt.Sprintf("%d/%s", dialect, mode), func(t *testing.T) {
				requireHistoricalRunReconstruction(t, dialect, mode)
			})
		}
	}
}

func requireHistoricalRunReconstruction(t *testing.T, dialect launchDialect, mode string) {
	t.Helper()
	directory := installFixtureExecutable(t)
	request := profileRequest(mode)
	request.CanonicalCwd = directory
	request.RequestedConfig.Model, request.RequestedConfig.Effort = "anthropic/claude-sonnet", "high"
	meta, wantCapability := historicalRunRecord(t, request, dialect)
	wantArguments, err := runArguments(request, dialect)
	if err != nil {
		t.Fatal(err)
	}
	requirements, err := dialect.runtimeRequirements()
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := PrepareExistingCandidate(request, meta, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	if wantArguments[0] != "run" || !slices.Equal(candidate.Inspection.Runtime.RequiredFlags, requirements.RequiredFlags) {
		t.Fatalf("historical argv=%q inspection flags=%q", wantArguments, candidate.Inspection.Runtime.RequiredFlags)
	}
	pending := requireStaticAdmission(t, candidate)
	if pending.RuntimePreflightSHA256 != wantCapability {
		t.Fatalf("capability digest=%q want %q", pending.RuntimePreflightSHA256, wantCapability)
	}
	requireRecordedLaunch(t, "pending", pending, meta, wantArguments)
	requireRecordedLaunch(t, "finalized", finalizeWithFixtureRuntime(t, candidate), meta, wantArguments)
}

func finalizeWithFixtureRuntime(t *testing.T, candidate commonprovider.ProfileCandidate) commonprovider.PreparedProfile {
	t.Helper()
	facts, err := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{Executable: candidate.Inspection.Executable, SHA256: candidate.Inspection.ExecutableSHA256}, nil)
	if err != nil {
		t.Fatal(err)
	}
	finalized, err := candidate.Finalize(facts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return finalized
}

func requireRecordedLaunch(t *testing.T, name string, rebuilt commonprovider.PreparedProfile, meta task.MetaRecord, wantArguments []string) {
	t.Helper()
	if !slices.Equal(rebuilt.Plan.Arguments, wantArguments) || rebuilt.Effective.Digest != meta.EffectiveConfig.Digest ||
		!task.CompareEffectiveConfigs(rebuilt.Effective, meta.EffectiveConfig) || !rebuilt.Plan.Predicate.Equal(meta.Predicate) {
		t.Fatalf("%s reconstruction changed: argv=%q effective=%+v predicate=%+v", name, rebuilt.Plan.Arguments, rebuilt.Effective, rebuilt.Plan.Predicate)
	}
}

// historicalRunRecord returns a pending record as a historical native run
// dialect admitted it, and that record's static runtime capability digest.
func historicalRunRecord(t *testing.T, request task.TaskRecord, dialect launchDialect) (task.MetaRecord, string) {
	t.Helper()
	current, err := PrepareCandidate(request, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	admitted := requireStaticAdmission(t, current)
	approval, err := dialect.approval(request.Mode)
	if err != nil {
		t.Fatal(err)
	}
	policy := admitted.Effective.Policy
	effective, err := dialect.nativeEffectiveConfig(request, policy.WritableRoots, policy.RuntimeSHA256, approval)
	if err != nil {
		t.Fatal(err)
	}
	if effective.Digest == admitted.Effective.Digest {
		t.Fatalf("historical effective config %+v is not distinct from current %+v", effective, admitted.Effective)
	}
	requirements, err := dialect.runtimeRequirements()
	if err != nil {
		t.Fatal(err)
	}
	cli := commonprovider.CLIInfo{Path: current.Inspection.Executable, SHA256: current.Inspection.ExecutableSHA256}
	definition, err := commonprovider.NewRuntimeInspectionDefinition(cli, request.CanonicalCwd, admitted.Plan.Environment, requirements)
	if err != nil {
		t.Fatal(err)
	}
	capability, err := commonprovider.RuntimeCapabilityDigest(definition)
	if err != nil {
		t.Fatal(err)
	}
	if capability == admitted.RuntimePreflightSHA256 {
		t.Fatal("historical and current capability digests are identical")
	}
	return task.MetaRecord{
		EffectiveConfig: effective, RuntimePreflightSHA256: capability,
		Environment: slices.Clone(admitted.Plan.Environment), EnvironmentRecorded: true,
		Predicate: historicalCurrentV4ReferenceForMode(request.Mode),
	}, capability
}

func installFixtureExecutable(t *testing.T) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, "opencode"), []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", directory)
	t.Setenv("PATH", directory)
	return directory
}
