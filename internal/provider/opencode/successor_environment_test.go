package opencode

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// namedPredecessor is an admitted predecessor record under one dialect.
type namedPredecessor struct {
	name string
	meta task.MetaRecord
}

// successorScenario separates two launch environments. The predecessors were
// admitted under the saved one (A). The successor's caller selects the
// provided one (B), whose PATH holds a different opencode executable. The
// process environment is a third, unrelated one.
type successorScenario struct {
	request     task.TaskRecord
	saved       launchHome
	provided    launchHome
	scope       []string
	providedCLI string
	// native lists the predecessors whose successor continues through ACP.
	native []namedPredecessor
	// pure is the historical adapter-owned pure-mode record.
	pure task.MetaRecord
}

func newSuccessorScenario(t *testing.T, mode string) successorScenario {
	t.Helper()
	request := recordedEnvironmentFixture(t, mode)
	// The pure-mode workspace-write policy requires the workspace to be a Git
	// worktree root.
	if err := os.Mkdir(filepath.Join(request.CanonicalCwd, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	request.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: testSession, PredecessorTaskID: successorPredecessorTask}
	saved, provided := newLaunchHome(t, "saved"), newLaunchHome(t, "provided")
	saved.apply(t)
	current, err := PrepareCandidate(request, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	admitted := requireStaticAdmission(t, current)
	acp := task.MetaRecord{
		EffectiveConfig: admitted.Effective, RuntimePreflightSHA256: admitted.RuntimePreflightSHA256,
		Environment: slices.Clone(admitted.Plan.Environment), EnvironmentRecorded: true, Predicate: admitted.Plan.Predicate,
	}
	runV1, _ := historicalRunRecord(t, request, dialectNativeV1)
	runV2, _ := historicalRunRecord(t, request, dialectNativeV2)
	newLaunchHome(t, "ambient").apply(t)
	scope, providedCLI := provided.selectedEnvironment(t)
	return successorScenario{
		request: request, saved: saved, provided: provided, scope: scope, providedCLI: providedCLI,
		native: []namedPredecessor{
			{"native v1 run", runV1},
			{"native v2 run", runV2},
			{"acp v1", withPredicate(acp, acpV1ReferenceForMode(mode))},
			{"acp v2", acp},
		},
		pure: withRevision(withPredicate(runV2, LegacyReferenceForMode(mode)), ProfileRevision),
	}
}

// snapshotMeta returns a deep copy of the parts of meta a successor could
// mutate, so reflect.DeepEqual detects any change to the original.
func snapshotMeta(meta task.MetaRecord) task.MetaRecord {
	meta.Environment = slices.Clone(meta.Environment)
	if meta.EffectiveConfig.Policy != nil {
		policy := *meta.EffectiveConfig.Policy
		policy.WritableRoots = slices.Clone(policy.WritableRoots)
		meta.EffectiveConfig.Policy = &policy
	}
	return meta
}

func inspectionDigest(t *testing.T, candidate commonprovider.ProfileCandidate) string {
	t.Helper()
	_, digest, err := candidate.Inspection.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

// requireFreshACPSuccessor requires the successor of meta under scope to be
// exactly the fresh ACP admission that scope selects, with the executable
// found through scope.
func requireFreshACPSuccessor(t *testing.T, request task.TaskRecord, meta task.MetaRecord, scope []string) commonprovider.ProfileCandidate {
	t.Helper()
	fresh, err := PrepareCandidate(request, scope)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := PrepareSuccessorCandidate(request, meta, scope)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := inspectionDigest(t, candidate), inspectionDigest(t, fresh); got != want {
		t.Fatalf("successor inspection definition %s differs from the one the current environment selects %s", got, want)
	}
	if candidate.Inspection.Executable != fresh.Inspection.Executable || candidate.Inspection.ExecutableSHA256 != fresh.Inspection.ExecutableSHA256 {
		t.Fatal("successor executable was not resolved through the current environment")
	}
	if !slices.Equal(candidate.WritableRoots, fresh.WritableRoots) {
		t.Fatalf("successor writable roots (%s) differ from the current environment's (%s)", summarizeValues(candidate.WritableRoots), summarizeValues(fresh.WritableRoots))
	}
	successor, want := requireStaticAdmission(t, candidate), requireStaticAdmission(t, fresh)
	if !slices.Equal(successor.Plan.Environment, want.Plan.Environment) || !slices.Equal(successor.Effective.Policy.WritableRoots, want.Effective.Policy.WritableRoots) ||
		successor.Effective.Policy.RuntimeSHA256 != want.Effective.Policy.RuntimeSHA256 || successor.RuntimePreflightSHA256 != want.RuntimePreflightSHA256 {
		t.Fatal("successor launch environment, recorded roots or capability proof differ from the current environment's")
	}
	return candidate
}

func TestSuccessorUsesCurrentEnvironmentAndLeavesPredecessorUntouched(t *testing.T) {
	for _, mode := range []string{ModeReadOnly, ModeWorkspaceWrite} {
		scenario := newSuccessorScenario(t, mode)
		for _, predecessor := range scenario.native {
			t.Run(mode+"/"+predecessor.name, func(t *testing.T) {
				before := snapshotMeta(predecessor.meta)
				candidate := requireFreshACPSuccessor(t, scenario.request, predecessor.meta, scenario.scope)
				requireSelectors(t, "successor inspection environment", candidate.Inspection.Environment, scenario.provided.selectors())
				requireNoPath(t, "successor inspection environment", candidate.Inspection.Environment, scenario.saved.paths())
				requireNoPath(t, "successor writable roots", candidate.WritableRoots, scenario.saved.paths())
				if candidate.Inspection.Executable != scenario.providedCLI {
					t.Fatalf("successor executable %q want the current PATH's %q", candidate.Inspection.Executable, scenario.providedCLI)
				}
				if !reflect.DeepEqual(before, predecessor.meta) {
					t.Fatal("successor preparation modified the predecessor record")
				}
			})
		}
	}
}

// An admitted record is still reconstructed from its own saved environment and
// roots after a successor was prepared from it under another environment.
func TestAdmittedRecordStillReconstructsFromItsOwnEnvironment(t *testing.T) {
	scenario := newSuccessorScenario(t, ModeWorkspaceWrite)
	for _, predecessor := range scenario.native {
		t.Run(predecessor.name, func(t *testing.T) {
			if _, err := PrepareSuccessorCandidate(scenario.request, predecessor.meta, scenario.scope); err != nil {
				t.Fatal(err)
			}
			existing, err := PrepareExistingCandidate(scenario.request, predecessor.meta, scenario.scope)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(existing.Inspection.Environment, predecessor.meta.Environment) ||
				!slices.Equal(existing.WritableRoots, predecessor.meta.EffectiveConfig.Policy.WritableRoots) {
				t.Fatal("reconstruction no longer uses the record's saved environment and writable roots")
			}
			requireSelectors(t, "reconstructed launch environment", existing.Inspection.Environment, scenario.saved.selectors())
			requireNoPath(t, "reconstructed launch environment", existing.Inspection.Environment, scenario.provided.paths())
			if existing.Inspection.Executable != scenario.providedCLI {
				t.Fatalf("reconstruction executable %q want the caller scope's %q", existing.Inspection.Executable, scenario.providedCLI)
			}
		})
	}
}

// The default selectors derive their roots from HOME alone. Explicit
// selectors name further roots. Both come from the current environment.
func TestSuccessorDerivesRootsFromDefaultAndExplicitSelectors(t *testing.T) {
	scenario := newSuccessorScenario(t, ModeReadOnly)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	configDir, stateHome := filepath.Join(root, "config-dir"), filepath.Join(root, "state")
	for _, path := range []string{configDir, stateHome} {
		if err = os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := slices.IndexFunc(scenario.scope, func(entry string) bool { return len(entry) > 5 && entry[:5] == "PATH=" })
	defaults := []string{"HOME=" + scenario.provided.home, scenario.scope[path]}
	explicit := append(slices.Clone(scenario.provided.selectors()), scenario.scope[path], "OPENCODE_CONFIG_DIR="+configDir, "XDG_STATE_HOME="+stateHome)
	for name, environment := range map[string][]string{"default": defaults, "explicit": explicit} {
		for _, predecessor := range scenario.native {
			t.Run(name+"/"+predecessor.name, func(t *testing.T) {
				candidate := requireFreshACPSuccessor(t, scenario.request, predecessor.meta, environment)
				if explicitRoots := slices.Contains(candidate.WritableRoots, configDir) && slices.Contains(candidate.WritableRoots, filepath.Join(stateHome, "opencode")); explicitRoots != (name == "explicit") {
					t.Fatalf("explicit selector roots present=%v in the %s environment", explicitRoots, name)
				}
				requireNoPath(t, "successor writable roots", candidate.WritableRoots, scenario.saved.paths())
			})
		}
	}
}

// A new successor whose caller environment is unusable fails; it never falls
// back to the predecessor's valid saved environment.
func TestSuccessorRefusesInvalidCurrentEnvironment(t *testing.T) {
	scenario := newSuccessorScenario(t, ModeReadOnly)
	withoutKey := func(prefix string) []string {
		return slices.DeleteFunc(slices.Clone(scenario.scope), func(entry string) bool { return len(entry) >= len(prefix) && entry[:len(prefix)] == prefix })
	}
	invalid := map[string][]string{
		"empty":         {},
		"no HOME":       withoutKey("HOME="),
		"no PATH":       withoutKey("PATH="),
		"malformed":     append(slices.Clone(scenario.scope), "NOT_AN_ASSIGNMENT"),
		"duplicate key": append(slices.Clone(scenario.scope), "TZ=UTC", "TZ=UTC"),
		"NUL value":     append(slices.Clone(scenario.scope), "TZ=U\x00TC"),
	}
	predecessors := append(slices.Clone(scenario.native), namedPredecessor{"pure", scenario.pure})
	for name, environment := range invalid {
		for _, predecessor := range predecessors {
			t.Run(name+"/"+predecessor.name, func(t *testing.T) {
				if _, err := PrepareSuccessorCandidate(scenario.request, predecessor.meta, environment); !errors.Is(err, ErrUnsupportedProfile) {
					t.Fatalf("successor with an invalid current environment error=%v want ErrUnsupportedProfile", err)
				}
			})
		}
	}
}

// A predecessor with no recorded environment or an empty recorded one does not
// change the successor, which reads the current environment either way. The
// empty record still fails closed for its own reconstruction.
func TestSuccessorIgnoresUnrecordedAndEmptyRecordedEnvironment(t *testing.T) {
	scenario := newSuccessorScenario(t, ModeReadOnly)
	for _, predecessor := range scenario.native {
		unrecorded := predecessor.meta
		unrecorded.Environment, unrecorded.EnvironmentRecorded = nil, false
		empty := predecessor.meta
		empty.Environment, empty.EnvironmentRecorded = []string{}, true
		for name, meta := range map[string]task.MetaRecord{"unrecorded": unrecorded, "recorded empty": empty} {
			t.Run(predecessor.name+"/"+name, func(t *testing.T) {
				requireFreshACPSuccessor(t, scenario.request, meta, scenario.scope)
			})
		}
		if _, err := PrepareExistingCandidate(scenario.request, empty, scenario.scope); !errors.Is(err, ErrUnsupportedProfile) {
			t.Fatalf("%s: reconstruction of a recorded empty environment error=%v want ErrUnsupportedProfile", predecessor.name, err)
		}
	}
}

// The pure-mode dialect is kept, with its recorded predicate. Its launch
// environment and writable roots are the ones the current environment selects
// under that dialect, not the record's.
func TestSuccessorOfPureRecordUsesCurrentEnvironment(t *testing.T) {
	for _, mode := range []string{ModeReadOnly, ModeWorkspaceWrite} {
		t.Run(mode, func(t *testing.T) {
			scenario := newSuccessorScenario(t, mode)
			before := snapshotMeta(scenario.pure)
			want, err := PrepareExistingCandidate(scenario.request, task.MetaRecord{Predicate: scenario.pure.Predicate}, scenario.scope)
			if err != nil {
				t.Fatal(err)
			}
			candidate, err := PrepareSuccessorCandidate(scenario.request, scenario.pure, scenario.scope)
			if err != nil {
				t.Fatal(err)
			}
			if got, wantDigest := inspectionDigest(t, candidate), inspectionDigest(t, want); got != wantDigest {
				t.Fatalf("pure successor inspection definition %s want the current environment's %s", got, wantDigest)
			}
			if !slices.Equal(candidate.WritableRoots, want.WritableRoots) {
				t.Fatal("pure successor writable roots are not the current environment's")
			}
			requireNoPath(t, "pure successor inspection environment", candidate.Inspection.Environment, scenario.saved.paths())
			requireNoPath(t, "pure successor writable roots", candidate.WritableRoots, scenario.saved.paths())
			if candidate.Admit != nil || candidate.Inspection.Executable != scenario.providedCLI || len(candidate.Inspection.Arguments) == 0 {
				t.Fatal("pure successor is not the pure-mode policy inspection under the current scope")
			}
			if !reflect.DeepEqual(before, scenario.pure) {
				t.Fatal("pure successor preparation modified the predecessor record")
			}
		})
	}
}

// Unknown revisions and unknown ACP predicates stay refused whatever the
// current environment, and the refusal precedes any use of it.
func TestSuccessorRefusesUnknownRecordsUnderCurrentEnvironment(t *testing.T) {
	scenario := newSuccessorScenario(t, ModeReadOnly)
	altered := ReferenceForMode(ModeReadOnly)
	altered.SHA256 = "1111111111111111111111111111111111111111111111111111111111111111"
	acp := scenario.native[3].meta
	for name, meta := range map[string]task.MetaRecord{
		"unknown revision":      withRevision(scenario.native[1].meta, "opencode-future-v9"),
		"acp altered predicate": withPredicate(acp, altered),
	} {
		t.Run(name, func(t *testing.T) {
			_, existingErr := PrepareExistingCandidate(scenario.request, meta, scenario.scope)
			_, successorErr := PrepareSuccessorCandidate(scenario.request, meta, scenario.scope)
			if !errors.Is(successorErr, ErrUnsupportedProfile) || existingErr == nil || existingErr.Error() != successorErr.Error() {
				t.Fatalf("successor error=%v, reconstruction error=%v", successorErr, existingErr)
			}
		})
	}
}

// Rebuilding a successor from the filtered inspection environment of one built
// from the raw caller environment gives the same inspection definition, as the
// inspection worker does when it re-derives the candidate.
func TestSuccessorInspectionEnvironmentRoundTrips(t *testing.T) {
	for _, mode := range []string{ModeReadOnly, ModeWorkspaceWrite} {
		scenario := newSuccessorScenario(t, mode)
		raw := append(slices.Clone(scenario.scope), "OPENAI_API_KEY=not-retained", "OPENCODE_CONFIG_CONTENT={}", "UNRELATED_SETTING=1",
			"OPENCODE_CONFIG_DIR="+scenario.provided.configHome)
		predecessors := append(slices.Clone(scenario.native), namedPredecessor{"pure", scenario.pure})
		for _, predecessor := range predecessors {
			t.Run(mode+"/"+predecessor.name, func(t *testing.T) {
				requireInspectionRoundTrip(t, scenario.request, predecessor.meta, raw)
			})
		}
	}
}

func requireInspectionRoundTrip(t *testing.T, request task.TaskRecord, predecessor task.MetaRecord, raw []string) {
	t.Helper()
	first, err := PrepareSuccessorCandidate(request, predecessor, raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{"OPENAI_API_KEY=not-retained", "UNRELATED_SETTING=1"} {
		if slices.Contains(first.Inspection.Environment, entry) {
			t.Fatal("inspection environment retained an unfiltered caller variable")
		}
	}
	rebuilt, err := PrepareSuccessorCandidate(request, predecessor, first.Inspection.Environment)
	if err != nil {
		t.Fatalf("successor from its own filtered inspection environment: %v", err)
	}
	if got, want := inspectionDigest(t, rebuilt), inspectionDigest(t, first); got != want {
		t.Fatalf("rebuilt inspection definition %s differs from the original %s", got, want)
	}
	if !slices.Equal(rebuilt.WritableRoots, first.WritableRoots) {
		t.Fatal("rebuilt writable roots differ from the original")
	}
}
