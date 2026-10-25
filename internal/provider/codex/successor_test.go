package codex

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// successorFixture is one predecessor admitted under caller A's launch
// environment and the environment of caller B that continues it. The two
// callers differ in every launch selector and in the executable on PATH.
type successorFixture struct {
	request     task.TaskRecord
	meta        task.MetaRecord
	saved       launchHome
	provided    launchHome
	recorded    []string
	providedEnv []string
	providedCLI string
}

func newSuccessorFixture(t *testing.T, mode string) successorFixture {
	t.Helper()
	request, _ := profileReconstructionFixture(t)
	request.Mode, request.RequestedConfig.Permission = mode, mode
	saved, provided := newLaunchHome(t, "saved"), newLaunchHome(t, "provided")
	recorded, _ := saved.selectedEnvironment(t)
	providedEnv, providedCLI := provided.selectedEnvironment(t)
	candidate, err := PrepareCandidate(request, recorded)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := candidate.Admit()
	if err != nil {
		t.Fatal(err)
	}
	request.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: testThreadID, PredecessorTaskID: testTaskID}
	newLaunchHome(t, "ambient").apply(t)
	return successorFixture{
		request: request,
		meta: task.MetaRecord{
			EffectiveConfig: admitted.Effective, RuntimePreflightSHA256: admitted.RuntimePreflightSHA256,
			Environment: slices.Clone(admitted.Plan.Environment), EnvironmentRecorded: true, Predicate: admitted.Plan.Predicate,
		},
		saved: saved, provided: provided, recorded: recorded, providedEnv: providedEnv, providedCLI: providedCLI,
	}
}

// snapshotMeta deep-copies the predecessor record so a test can prove the
// preparation under test never changed it.
func snapshotMeta(meta task.MetaRecord) task.MetaRecord {
	meta.Environment = slices.Clone(meta.Environment)
	meta.EffectiveConfig = task.CloneEffectiveConfig(meta.EffectiveConfig)
	return meta
}

func requireMetaUnchanged(t *testing.T, got, want task.MetaRecord) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("predecessor record changed by successor preparation")
	}
}

func successorViaCatalog(t *testing.T, request task.TaskRecord, meta task.MetaRecord, environment []string) (commonprovider.ProfileCandidate, error) {
	t.Helper()
	catalog, err := commonprovider.NewCatalog(Registration())
	if err != nil {
		t.Fatal(err)
	}
	return catalog.SuccessorCandidate(request, meta, environment)
}

func TestCatalogSuccessorUsesCurrentEnvironmentNotPredecessorRecord(t *testing.T) {
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		t.Run(mode, func(t *testing.T) {
			fixture := newSuccessorFixture(t, mode)
			before := snapshotMeta(fixture.meta)
			candidate, err := successorViaCatalog(t, fixture.request, fixture.meta, fixture.providedEnv)
			if err != nil {
				t.Fatal(err)
			}
			requireMetaUnchanged(t, fixture.meta, before)
			requireNoPath(t, "successor inspection environment", candidate.Inspection.Environment, fixture.saved.paths())
			requireNoPath(t, "successor writable roots", candidate.WritableRoots, fixture.saved.paths())
			if !slices.Contains(candidate.Inspection.Environment, "HOME="+fixture.provided.home) || !slices.Contains(candidate.WritableRoots, fixture.provided.codexHome) {
				t.Fatalf("successor did not take the current HOME and CODEX_HOME (%s, %s)", summarizeValues(candidate.Inspection.Environment), summarizeValues(candidate.WritableRoots))
			}
			if candidate.Inspection.Executable != fixture.providedCLI {
				t.Fatalf("successor executable %q want the current scope's %q", candidate.Inspection.Executable, fixture.providedCLI)
			}
			profile, err := candidate.Admit()
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(profile.Plan.Environment, candidate.Inspection.Environment) || !slices.Equal(profile.WritableRoots, candidate.WritableRoots) {
				t.Fatal("admitted successor launch differs from its inspected environment")
			}
			requireNoPath(t, "admitted successor environment", profile.Plan.Environment, fixture.saved.paths())
		})
	}
}

func withoutKey(environment []string, key string) []string {
	return slices.DeleteFunc(slices.Clone(environment), func(entry string) bool { return strings.HasPrefix(entry, key+"=") })
}

func nativePolicyMeta(predicate task.PredicateRef, preflight string) task.MetaRecord {
	return task.MetaRecord{
		EffectiveConfig:        task.EffectiveConfig{Policy: &task.PolicyDetails{ProfileRevision: commonprovider.NativeProfileRevision}},
		RuntimePreflightSHA256: preflight, Predicate: predicate,
	}
}

func inspectionDigest(t *testing.T, candidate commonprovider.ProfileCandidate) string {
	t.Helper()
	_, digest, err := candidate.Inspection.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestSuccessorRegistrationDeclaresOnlyExecSuccessor(t *testing.T) {
	if Registration().PrepareSuccessor == nil {
		t.Fatal("codex:exec has no successor hook")
	}
	if ReviewRegistration().PrepareSuccessor != nil {
		t.Fatal("codex review declared a successor hook although it has no continuation")
	}
}

func TestSuccessorSelectsDefaultOrExplicitCodexHomeFromCurrentEnvironment(t *testing.T) {
	fixture := newSuccessorFixture(t, Mode)
	defaultHome := filepath.Join(fixture.provided.home, ".codex")
	if err := os.MkdirAll(defaultHome, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, testCase := range map[string]struct {
		environment []string
		codexHome   string
		listed      bool
	}{
		"default selector":  {environment: withoutKey(fixture.providedEnv, "CODEX_HOME"), codexHome: defaultHome},
		"explicit selector": {environment: fixture.providedEnv, codexHome: fixture.provided.codexHome, listed: true},
	} {
		t.Run(name, func(t *testing.T) {
			candidate, err := PrepareSuccessorCandidate(fixture.request, fixture.meta, testCase.environment)
			if err != nil {
				t.Fatal(err)
			}
			if listed := slices.Contains(candidate.Inspection.Environment, "CODEX_HOME="+testCase.codexHome); listed != testCase.listed {
				t.Fatalf("CODEX_HOME listed=%t want %t (%s)", listed, testCase.listed, summarizeValues(candidate.Inspection.Environment))
			}
			if !slices.Contains(candidate.WritableRoots, testCase.codexHome) {
				t.Fatalf("writable roots (%s) lack the current codex home", summarizeValues(candidate.WritableRoots))
			}
			requireNoPath(t, "successor environment", candidate.Inspection.Environment, fixture.saved.paths())
			requireNoPath(t, "successor roots", candidate.WritableRoots, fixture.saved.paths())
		})
	}
}

func TestSuccessorEnvironmentIsIndependentOfPredecessorRecording(t *testing.T) {
	for name, record := range map[string]func(task.MetaRecord) task.MetaRecord{
		"recorded": func(meta task.MetaRecord) task.MetaRecord { return meta },
		"recorded without PATH": func(meta task.MetaRecord) task.MetaRecord {
			meta.Environment = withoutKey(meta.Environment, "PATH")
			return meta
		},
		"unrecorded legacy": func(meta task.MetaRecord) task.MetaRecord {
			meta.Environment, meta.EnvironmentRecorded = nil, false
			return meta
		},
		"recorded empty": func(meta task.MetaRecord) task.MetaRecord {
			meta.Environment, meta.EnvironmentRecorded = []string{}, true
			return meta
		},
		"recorded nil with flag": func(meta task.MetaRecord) task.MetaRecord {
			meta.Environment, meta.EnvironmentRecorded = nil, true
			return meta
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newSuccessorFixture(t, Mode)
			meta := record(fixture.meta)
			before := snapshotMeta(meta)
			candidate, err := PrepareSuccessorCandidate(fixture.request, meta, fixture.providedEnv)
			if err != nil {
				t.Fatal(err)
			}
			requireMetaUnchanged(t, meta, before)
			if !slices.Contains(candidate.Inspection.Environment, "HOME="+fixture.provided.home) || candidate.Inspection.Executable != fixture.providedCLI {
				t.Fatalf("successor ignored the current environment (%s) executable %q", summarizeValues(candidate.Inspection.Environment), candidate.Inspection.Executable)
			}
			requireNoPath(t, "successor environment", candidate.Inspection.Environment, fixture.saved.paths())
		})
	}
}

func TestSuccessorInvalidCurrentEnvironmentNeverFallsBackToPredecessor(t *testing.T) {
	fixture := newSuccessorFixture(t, Mode)
	for name, environment := range map[string][]string{
		"empty":           {},
		"malformed entry": append(slices.Clone(fixture.providedEnv), "BROKEN"),
		"duplicate key":   append(slices.Clone(fixture.providedEnv), "HOME="+fixture.provided.home),
		"missing HOME":    withoutKey(fixture.providedEnv, "HOME"),
		"relative HOME":   append(withoutKey(fixture.providedEnv, "HOME"), "HOME=relative/home"),
		"missing PATH":    withoutKey(fixture.providedEnv, "PATH"),
	} {
		t.Run(name, func(t *testing.T) {
			// The recorded predecessor environment is complete and valid, so any
			// success here would be a silent fallback to it.
			if _, err := PrepareSuccessorCandidate(fixture.request, fixture.meta, environment); !errors.Is(err, ErrUnsupportedProfile) {
				t.Fatalf("direct error=%v want ErrUnsupportedProfile", err)
			}
			if _, err := successorViaCatalog(t, fixture.request, fixture.meta, environment); !errors.Is(err, commonprovider.ErrProfileUnavailable) {
				t.Fatalf("catalog error=%v want ErrProfileUnavailable", err)
			}
		})
	}
	if _, err := successorViaCatalog(t, fixture.request, fixture.meta, nil); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("uncaptured environment error=%v want ErrEvidenceFault", err)
	}
	unrecorded := fixture.meta
	unrecorded.Environment, unrecorded.EnvironmentRecorded = nil, false
	if _, err := PrepareSuccessorCandidate(fixture.request, unrecorded, withoutKey(fixture.providedEnv, "HOME")); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("unrecorded predecessor with invalid current error=%v want ErrUnsupportedProfile", err)
	}
}

// Reconstruction of an admitted task stays on its own saved metadata while a
// new successor of that same record takes the caller's environment.
func TestExistingReconstructionStillUsesOwnRecordedMetadata(t *testing.T) {
	fixture := newSuccessorFixture(t, Mode)
	before := snapshotMeta(fixture.meta)
	existing, err := PrepareExistingCandidate(fixture.request, fixture.meta, fixture.providedEnv)
	if err != nil {
		t.Fatal(err)
	}
	successor, err := PrepareSuccessorCandidate(fixture.request, fixture.meta, fixture.providedEnv)
	if err != nil {
		t.Fatal(err)
	}
	requireMetaUnchanged(t, fixture.meta, before)
	if !slices.Equal(existing.Inspection.Environment, fixture.meta.Environment) || !slices.Contains(existing.WritableRoots, fixture.saved.codexHome) {
		t.Fatalf("reconstruction left the recorded environment (%s) roots (%s)", summarizeValues(existing.Inspection.Environment), summarizeValues(existing.WritableRoots))
	}
	requireNoPath(t, "reconstruction environment", existing.Inspection.Environment, fixture.provided.paths())
	if existing.Inspection.Executable != fixture.providedCLI {
		t.Fatalf("reconstruction executable %q want the caller scope's %q", existing.Inspection.Executable, fixture.providedCLI)
	}
	if slices.Equal(existing.Inspection.Environment, successor.Inspection.Environment) || inspectionDigest(t, existing) == inspectionDigest(t, successor) {
		t.Fatal("successor and reconstruction of one record share a launch environment")
	}
}

func TestSuccessorDerivesFreshCapabilityOnlyForEligibleRecords(t *testing.T) {
	fixture := newSuccessorFixture(t, Mode)
	candidate, err := PrepareSuccessorCandidate(fixture.request, fixture.meta, fixture.providedEnv)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := candidate.Admit()
	if err != nil {
		t.Fatal(err)
	}
	capability, err := commonprovider.RuntimeCapabilityDigest(*candidate.Inspection)
	if err != nil {
		t.Fatal(err)
	}
	if profile.RuntimePreflightSHA256 != capability || profile.RuntimePreflightSHA256 == fixture.meta.RuntimePreflightSHA256 {
		t.Fatalf("successor proof %q want a fresh digest %q distinct from the predecessor's %q", profile.RuntimePreflightSHA256, capability, fixture.meta.RuntimePreflightSHA256)
	}
	if !slices.Equal(profile.Effective.Policy.WritableRoots, candidate.WritableRoots) || slices.Contains(profile.Effective.Policy.WritableRoots, fixture.saved.codexHome) {
		t.Fatalf("successor effective roots (%s) are not derived from the current environment", summarizeValues(profile.Effective.Policy.WritableRoots))
	}

	runtimeOnly := fixture.meta
	runtimeOnly.RuntimePreflightSHA256 = ""
	candidate, err = PrepareSuccessorCandidate(fixture.request, runtimeOnly, fixture.providedEnv)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Admit != nil {
		t.Fatal("runtime-only predecessor gained static admission")
	}
}

func TestSuccessorMatchesReconstructionWhereTheRecordHoldsNoEnvironment(t *testing.T) {
	fixture := newSuccessorFixture(t, Mode)
	fixture.request.RequestedConfig.Model, fixture.request.RequestedConfig.Effort = "custom-model", "high"
	for name, predicateFor := range map[string]func(string) task.PredicateRef{
		"current":            ReferenceForMode,
		"historical v3":      historicalV3ReferenceForMode,
		"historical current": historicalCurrentRuntimeReferenceForMode,
		"historical runtime": historicalRuntimeReferenceForMode,
	} {
		for _, mode := range []string{Mode, WorkspaceWriteMode} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				request := fixture.request
				request.Mode, request.RequestedConfig.Permission = mode, mode
				requireSuccessorMatchesReconstruction(t, request, predicateFor(mode), fixture.providedEnv)
			})
		}
	}
}

// requireSuccessorMatchesReconstruction proves that, for a record holding no
// environment, a successor differs from reconstruction of that record in no
// compatibility choice: predicate, launch dialect, eligibility, or options.
func requireSuccessorMatchesReconstruction(t *testing.T, request task.TaskRecord, reference task.PredicateRef, environment []string) {
	t.Helper()
	meta := nativePolicyMeta(reference, testSpecHash)
	successor, err := PrepareSuccessorCandidate(request, meta, environment)
	if err != nil {
		t.Fatal(err)
	}
	existing, err := PrepareExistingCandidate(request, meta, environment)
	if err != nil {
		t.Fatal(err)
	}
	if inspectionDigest(t, successor) != inspectionDigest(t, existing) || !slices.Equal(successor.WritableRoots, existing.WritableRoots) {
		t.Fatal("successor and reconstruction differ although the record holds no environment")
	}
	got, err := successor.Admit()
	if err != nil {
		t.Fatal(err)
	}
	want, err := existing.Admit()
	if err != nil {
		t.Fatal(err)
	}
	if !got.Plan.Predicate.Equal(reference) || !slices.Equal(got.Plan.Arguments, want.Plan.Arguments) || got.RuntimePreflightSHA256 != want.RuntimePreflightSHA256 {
		t.Fatalf("successor predicate=%+v args=%q", got.Plan.Predicate, got.Plan.Arguments)
	}
	for _, argument := range []string{"--model", "custom-model", "model_reasoning_effort=\"high\"", "resume", testThreadID} {
		if !slices.Contains(got.Plan.Arguments, argument) {
			t.Fatalf("successor argv %q lost %q", got.Plan.Arguments, argument)
		}
	}
	if skip := slices.Contains(got.Plan.Arguments, "--skip-git-repo-check"); skip != reference.Equal(ReferenceForMode(request.Mode)) {
		t.Fatalf("git-check flag=%t for predicate %+v", skip, reference)
	}
}

func TestSuccessorRequiresTheExactRequestedPriorSession(t *testing.T) {
	fixture := newSuccessorFixture(t, Mode)
	for name, prior := range map[string]*task.PriorSession{
		"foreign provider":  {Provider: "other", ConversationID: testThreadID, PredecessorTaskID: testTaskID},
		"invalid thread":    {Provider: Provider, ConversationID: "not-a-thread", PredecessorTaskID: testTaskID},
		"invalid predecess": {Provider: Provider, ConversationID: testThreadID, PredecessorTaskID: "bad"},
	} {
		request := fixture.request
		request.PriorSession = prior
		if _, err := PrepareSuccessorCandidate(request, fixture.meta, fixture.providedEnv); !errors.Is(err, ErrUnsupportedProfile) {
			t.Fatalf("%s: error=%v want ErrUnsupportedProfile", name, err)
		}
	}
	other := "0199a213-81c0-7800-8aa1-bbab2a035a54"
	request := fixture.request
	request.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: other, PredecessorTaskID: testTaskID}
	candidate, err := PrepareSuccessorCandidate(request, fixture.meta, fixture.providedEnv)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := candidate.Admit()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(profile.Plan.Arguments, other) || slices.Contains(profile.Plan.Arguments, testThreadID) {
		t.Fatalf("successor argv %q is not bound to the requested session", profile.Plan.Arguments)
	}
}

func TestSuccessorOfLegacyRecordUsesCurrentEnvironment(t *testing.T) {
	fixture := newSuccessorFixture(t, Mode)
	fixture.provided.writeAuth(t)
	meta := task.MetaRecord{
		EffectiveConfig: task.EffectiveConfig{Policy: &task.PolicyDetails{ProfileRevision: ProfileRevision}},
		Predicate:       LegacyReference(), Environment: slices.Clone(fixture.recorded), EnvironmentRecorded: true,
	}
	before := snapshotMeta(meta)
	candidate, err := PrepareSuccessorCandidate(fixture.request, meta, fixture.providedEnv)
	if err != nil {
		t.Fatal(err)
	}
	requireMetaUnchanged(t, meta, before)
	requireLegacyInspection(t, candidate)
	if candidate.Admit != nil {
		t.Fatal("legacy successor gained static admission")
	}
	now := time.Now()
	profile := finalizeProfile(t, candidate, now)
	wantArguments, _, err := legacyExecArguments(fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(profile.Plan.Arguments, wantArguments) || !profile.Plan.Predicate.Equal(LegacyReference()) {
		t.Fatalf("legacy successor argv=%q predicate=%+v", profile.Plan.Arguments, profile.Plan.Predicate)
	}
	if !slices.Contains(profile.Plan.Environment, "HOME="+fixture.provided.home) {
		t.Fatalf("legacy successor left the current HOME (%s)", summarizeValues(profile.Plan.Environment))
	}
	requireNoPath(t, "legacy successor environment", profile.Plan.Environment, fixture.saved.paths())
	if profile.Effective.Policy == nil || profile.Effective.Policy.ProfileRevision != ProfileRevision {
		t.Fatalf("legacy successor effective policy=%+v", profile.Effective.Policy)
	}

	rejected := append(slices.Clone(fixture.providedEnv), "OPENAI_API_KEY=redacted")
	if _, err = PrepareSuccessorCandidate(fixture.request, meta, rejected); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("legacy successor with alternate authentication error=%v want ErrUnsupportedProfile", err)
	}
}

func TestSuccessorKeepsInteractivePredicateGuard(t *testing.T) {
	fixture := newSuccessorFixture(t, Mode)
	fixture.request.RequestedConfig.Interactive = true
	if _, err := PrepareSuccessorCandidate(fixture.request, nativePolicyMeta(Reference(), ""), fixture.providedEnv); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("noninteractive predecessor predicate error=%v want ErrUnsupportedProfile", err)
	}
	reference := interactionReference(Mode)
	candidate, err := PrepareSuccessorCandidate(fixture.request, nativePolicyMeta(reference, testSpecHash), fixture.providedEnv)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := candidate.Admit()
	if err != nil {
		t.Fatal(err)
	}
	if !profile.Plan.Predicate.Equal(reference) || profile.Interaction == nil {
		t.Fatalf("interactive successor predicate=%+v", profile.Plan.Predicate)
	}
	requireNoPath(t, "interactive successor environment", profile.Plan.Environment, fixture.saved.paths())
}

// The filtered inspection environment is what the supervisor later replays, so
// preparing from it must reproduce the definition prepared from the raw one.
func TestSuccessorInspectionIsStableAcrossFilteredEnvironmentRoundTrip(t *testing.T) {
	t.Run("native", func(t *testing.T) {
		fixture := newSuccessorFixture(t, Mode)
		first := requireStableRoundTrip(t, fixture, fixture.meta)
		if !slices.Contains(first.WritableRoots, filepath.Join(fixture.provided.tmpDir, "cache")) {
			t.Fatalf("native successor roots (%s) lack the selected cache home", summarizeValues(first.WritableRoots))
		}
	})
	t.Run("legacy", func(t *testing.T) {
		fixture := newSuccessorFixture(t, Mode)
		fixture.provided.writeAuth(t)
		requireStableRoundTrip(t, fixture, task.MetaRecord{
			EffectiveConfig: task.EffectiveConfig{Policy: &task.PolicyDetails{ProfileRevision: ProfileRevision}},
			Predicate:       LegacyReference(),
		})
	})
}

// requireStableRoundTrip prepares a successor from a raw environment holding
// unselected variables, rebuilds it from the filtered inspection environment,
// and requires the same inspection definition and roots. It returns the first.
func requireStableRoundTrip(t *testing.T, fixture successorFixture, meta task.MetaRecord) commonprovider.ProfileCandidate {
	t.Helper()
	cacheHome := filepath.Join(fixture.provided.tmpDir, "cache")
	if err := os.MkdirAll(cacheHome, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := append(slices.Clone(fixture.providedEnv), "SECRET_TOKEN=do-not-forward", "XDG_CACHE_HOME="+cacheHome, "UNRELATED=value")
	first, err := PrepareSuccessorCandidate(fixture.request, meta, raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range first.Inspection.Environment {
		if strings.HasPrefix(entry, "SECRET_TOKEN=") || strings.HasPrefix(entry, "UNRELATED=") {
			t.Fatal("inspection environment retained an unselected variable")
		}
	}
	second, err := PrepareSuccessorCandidate(fixture.request, meta, first.Inspection.Environment)
	if err != nil {
		t.Fatalf("rebuild from the filtered inspection environment: %v", err)
	}
	if inspectionDigest(t, first) != inspectionDigest(t, second) || !slices.Equal(first.WritableRoots, second.WritableRoots) {
		t.Fatal("filtered-environment rebuild changed the inspection definition or roots")
	}
	return first
}
