package claude

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// successorCaller is one complete caller-selected environment below its own
// canonical root: a HOME, a TMPDIR, and a PATH holding a fixture claude whose
// content, and so whose fingerprint, differs from every other caller's.
type successorCaller struct {
	root, home, temporary, cli string
	values                     []string
}

func newSuccessorCaller(t *testing.T, label string) successorCaller {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	caller := successorCaller{root: root, home: filepath.Join(root, "home"), temporary: filepath.Join(root, "tmp"), cli: filepath.Join(root, "bin", "claude")}
	for _, directory := range []string{caller.home, caller.temporary, filepath.Dir(caller.cli)} {
		if err = os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(caller.cli, []byte("claude fixture "+label), 0o700); err != nil {
		t.Fatal(err)
	}
	caller.values = []string{"HOME=" + caller.home, "PATH=" + filepath.Dir(caller.cli), "TMPDIR=" + caller.temporary, "DELEGATE_UNRELATED=" + label}
	return caller
}

// withExplicitSelectors adds absolute native configuration selectors below
// the caller's own root.
func (c successorCaller) withExplicitSelectors() successorCaller {
	c.values = append(slices.Clone(c.values), "CLAUDE_CONFIG_DIR="+filepath.Join(c.root, "claude-config"), "XDG_CONFIG_HOME="+filepath.Join(c.root, "xdg"))
	return c
}

func successorRequest(t *testing.T, mode string) task.TaskRecord {
	t.Helper()
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	request := profileRequest()
	request.CanonicalCwd, request.Mode, request.RequestedConfig.Permission = workspace, mode, mode
	session, err := FreshSessionID(request.RootID, successorPredecessorTask)
	if err != nil {
		t.Fatal(err)
	}
	request.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: session, PredecessorTaskID: successorPredecessorTask}
	return request
}

// admittedRecord is the metadata a task admitted with profile saves.
func admittedRecord(profile commonprovider.PreparedProfile) task.MetaRecord {
	return task.MetaRecord{
		EffectiveConfig: profile.Effective, RuntimePreflightSHA256: profile.RuntimePreflightSHA256,
		Environment: slices.Clone(profile.Plan.Environment), EnvironmentRecorded: true, Predicate: profile.Plan.Predicate,
	}
}

func canonicalRecord(t *testing.T, meta task.MetaRecord) []byte {
	t.Helper()
	encoded, err := task.MarshalCanonical(meta)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func freshCallerAdmission(t *testing.T, request task.TaskRecord, caller successorCaller) commonprovider.PreparedProfile {
	t.Helper()
	candidate, err := PrepareCandidate(request, caller.values)
	if err != nil {
		t.Fatal(err)
	}
	return requireStaticAdmission(t, candidate)
}

func inspectionDigest(t *testing.T, candidate commonprovider.ProfileCandidate) string {
	t.Helper()
	if candidate.Inspection == nil {
		t.Fatal("candidate has no runtime inspection")
	}
	_, digest, err := candidate.Inspection.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

// requireCallerLaunch asserts that candidate and its admission launch the
// caller's claude with values and writable roots derived from the caller
// alone, and that nothing below another caller's root or outside the native
// filter reaches them.
func requireCallerLaunch(t *testing.T, label string, candidate commonprovider.ProfileCandidate, admitted commonprovider.PreparedProfile, caller successorCaller, others ...successorCaller) {
	t.Helper()
	if candidate.Inspection == nil || !slices.Equal(candidate.Inspection.Environment, admitted.Plan.Environment) {
		t.Fatalf("%s inspection and launch environments differ", label)
	}
	if admitted.Plan.Executable != caller.cli || admitted.Plan.ExecutableSHA256 != fixtureSHA256(t, caller.cli) || candidate.Inspection.Executable != caller.cli {
		t.Fatalf("%s executable %q want the caller's %q", label, admitted.Plan.Executable, caller.cli)
	}
	for _, want := range []string{"HOME=" + caller.home, "TMPDIR=" + caller.temporary} {
		if !slices.Contains(admitted.Plan.Environment, want) {
			t.Fatalf("%s launch environment lacks the caller's %s", label, strings.SplitN(want, "=", 2)[0])
		}
	}
	for _, want := range []string{caller.temporary, filepath.Join(caller.home, ".claude")} {
		if !slices.Contains(admitted.WritableRoots, want) {
			t.Fatalf("%s writable roots %q lack the caller's %q", label, admitted.WritableRoots, want)
		}
	}
	if !slices.Equal(candidate.WritableRoots, admitted.WritableRoots) || admitted.Effective.Policy == nil || !slices.Equal(admitted.Effective.Policy.WritableRoots, admitted.WritableRoots) {
		t.Fatalf("%s candidate, admission, and policy writable roots differ", label)
	}
	requireNoForeignLaunchEntries(t, label, admitted, others)
}

func requireNoForeignLaunchEntries(t *testing.T, label string, admitted commonprovider.PreparedProfile, others []successorCaller) {
	t.Helper()
	for _, entry := range admitted.Plan.Environment {
		if strings.HasPrefix(entry, "DELEGATE_UNRELATED=") || strings.HasPrefix(entry, "ANTHROPIC_API_KEY=") {
			t.Fatalf("%s launch environment kept unfiltered %q", label, strings.SplitN(entry, "=", 2)[0])
		}
	}
	for _, other := range others {
		for _, entry := range admitted.Plan.Environment {
			if strings.Contains(entry, other.root) {
				t.Fatalf("%s launch environment leaked another caller into %q", label, strings.SplitN(entry, "=", 2)[0])
			}
		}
		for _, root := range admitted.WritableRoots {
			if strings.HasPrefix(root, other.root) {
				t.Fatalf("%s writable root %q belongs to another caller", label, root)
			}
		}
	}
}

func requireSameAdmission(t *testing.T, label string, got, want commonprovider.PreparedProfile) {
	t.Helper()
	if !reflect.DeepEqual(got.Plan, want.Plan) || !task.CompareEffectiveConfigs(got.Effective, want.Effective) ||
		!slices.Equal(got.WritableRoots, want.WritableRoots) || got.RuntimePreflightSHA256 != want.RuntimePreflightSHA256 || got.ObservedVersion != want.ObservedVersion {
		t.Fatalf("%s admission differs from the fresh caller admission: plan equal=%t effective equal=%t roots equal=%t preflight equal=%t",
			label, reflect.DeepEqual(got.Plan, want.Plan), task.CompareEffectiveConfigs(got.Effective, want.Effective),
			slices.Equal(got.WritableRoots, want.WritableRoots), got.RuntimePreflightSHA256 == want.RuntimePreflightSHA256)
	}
}

// requireRecordReconstruction asserts that reconstructing an admitted record
// under another caller keeps the record's own launch environment and roots
// while the reconstructing caller resolves the executable.
func requireRecordReconstruction(t *testing.T, label string, request task.TaskRecord, meta task.MetaRecord, admitted commonprovider.PreparedProfile, caller successorCaller) {
	t.Helper()
	candidate, err := PrepareExistingCandidate(request, meta, caller.values)
	if err != nil {
		t.Fatalf("%s reconstruction: %v", label, err)
	}
	rebuilt := requireStaticAdmission(t, candidate)
	if !slices.Equal(rebuilt.Plan.Environment, meta.Environment) || !slices.Equal(rebuilt.WritableRoots, admitted.WritableRoots) ||
		!task.CompareEffectiveConfigContract(rebuilt.Effective, admitted.Effective) || !rebuilt.Plan.Predicate.Equal(meta.Predicate) {
		t.Fatalf("%s reconstruction did not keep its own saved launch and policy", label)
	}
	if rebuilt.Plan.Executable != caller.cli {
		t.Fatalf("%s reconstruction executable %q want the reconstructing caller's %q", label, rebuilt.Plan.Executable, caller.cli)
	}
}

// TestSuccessorLaunchesFromCallerWhileAdmittedRecordsKeepTheirOwn is the
// central continuation regression: A is admitted under caller A, caller B
// creates successor B, and caller C later reconstructs A from A's record and B
// from B's record.
func TestSuccessorLaunchesFromCallerWhileAdmittedRecordsKeepTheirOwn(t *testing.T) {
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		t.Run(mode, func(t *testing.T) {
			a, b, c := newSuccessorCaller(t, "a"), newSuccessorCaller(t, "b"), newSuccessorCaller(t, "c")
			request := successorRequest(t, mode)
			predecessor := freshCallerAdmission(t, request, a)
			predecessorMeta := admittedRecord(predecessor)
			before := canonicalRecord(t, predecessorMeta)

			candidate, err := PrepareSuccessorCandidate(request, predecessorMeta, b.values)
			if err != nil {
				t.Fatal(err)
			}
			successor := requireStaticAdmission(t, candidate)
			if !bytes.Equal(before, canonicalRecord(t, predecessorMeta)) {
				t.Fatal("predecessor record changed")
			}
			requireCallerLaunch(t, "successor", candidate, successor, b, a, c)
			requireSameAdmission(t, "successor", successor, freshCallerAdmission(t, request, b))
			if !successor.Plan.Predicate.Equal(predecessor.Plan.Predicate) || !slices.Equal(successor.Plan.Arguments, predecessor.Plan.Arguments) ||
				successor.Effective.Approval != predecessor.Effective.Approval {
				t.Fatal("successor changed the predecessor's predicate, session argv, or permission mode")
			}

			requireRecordReconstruction(t, "predecessor", request, predecessorMeta, predecessor, c)
			requireRecordReconstruction(t, "successor", request, admittedRecord(successor), successor, c)
		})
	}
}

// TestSuccessorRefusesInvalidCallerEnvironmentWithoutPredecessorFallback
// pairs each refused caller with the predecessor that still reconstructs:
// a new successor applies current native validation to the caller, while an
// admitted record keeps its historical environment tolerance.
func TestSuccessorRefusesInvalidCallerEnvironmentWithoutPredecessorFallback(t *testing.T) {
	a, b := newSuccessorCaller(t, "a"), newSuccessorCaller(t, "b")
	request := successorRequest(t, Mode)
	predecessor := freshCallerAdmission(t, request, a)
	strict := admittedRecord(predecessor)
	tolerated := admittedRecord(predecessor)
	tolerated.Environment = append(tolerated.Environment, "XDG_STATE_HOME=relative/state")
	slices.Sort(tolerated.Environment)

	without := func(key string) []string {
		return slices.DeleteFunc(slices.Clone(b.values), func(entry string) bool { return strings.HasPrefix(entry, key+"=") })
	}
	invalid := map[string][]string{
		"empty":                    {},
		"missing HOME":             without("HOME"),
		"relative HOME":            append(without("HOME"), "HOME=relative/home"),
		"missing PATH":             without("PATH"),
		"malformed entry":          append(slices.Clone(b.values), "MALFORMED"),
		"duplicate key":            append(slices.Clone(b.values), "HOME="+a.home),
		"relative native selector": append(slices.Clone(b.values), "CLAUDE_CONFIG_DIR=relative/claude"),
		"relative XDG selector":    append(slices.Clone(b.values), "XDG_STATE_HOME=relative/state"),
	}
	for name, values := range invalid {
		for recordName, meta := range map[string]task.MetaRecord{"strict": strict, "tolerated": tolerated} {
			t.Run(name+"/"+recordName, func(t *testing.T) {
				before := canonicalRecord(t, meta)
				if _, err := PrepareSuccessorCandidate(request, meta, values); !errors.Is(err, ErrUnsupportedProfile) {
					t.Fatalf("successor error=%v want ErrUnsupportedProfile without falling back to the predecessor environment", err)
				}
				if !bytes.Equal(before, canonicalRecord(t, meta)) {
					t.Fatal("predecessor record changed")
				}
			})
		}
	}

	reconstructed, err := PrepareExistingCandidate(request, tolerated, b.values)
	if err != nil {
		t.Fatalf("tolerated predecessor no longer reconstructs: %v", err)
	}
	if !slices.Equal(reconstructed.Inspection.Environment, tolerated.Environment) || !slices.Equal(reconstructed.WritableRoots, predecessor.WritableRoots) {
		t.Fatal("tolerated predecessor reconstruction changed its launch or roots")
	}
	candidate, err := PrepareSuccessorCandidate(request, tolerated, b.values)
	if err != nil {
		t.Fatalf("valid caller cannot continue a tolerated predecessor: %v", err)
	}
	requireCallerLaunch(t, "successor of tolerated predecessor", candidate, requireStaticAdmission(t, candidate), b, a)
}

// TestSuccessorIgnoresHowThePredecessorRecordedItsEnvironment covers legacy
// records without an environment and records that saved an empty one. The
// successor launches from the caller with the caller's roots; reconstruction
// of the same records keeps its existing policy.
func TestSuccessorIgnoresHowThePredecessorRecordedItsEnvironment(t *testing.T) {
	a, b, c := newSuccessorCaller(t, "a"), newSuccessorCaller(t, "b"), newSuccessorCaller(t, "c")
	request := successorRequest(t, WorkspaceWriteMode)
	predecessor := freshCallerAdmission(t, request, a)
	unrecorded, decodedEmpty, savedEmpty := admittedRecord(predecessor), admittedRecord(predecessor), admittedRecord(predecessor)
	unrecorded.Environment, unrecorded.EnvironmentRecorded = nil, false
	decodedEmpty.Environment = nil
	savedEmpty.Environment = []string{}
	for name, meta := range map[string]task.MetaRecord{"unrecorded": unrecorded, "decoded empty": decodedEmpty, "saved empty": savedEmpty} {
		t.Run(name, func(t *testing.T) {
			candidate, err := PrepareSuccessorCandidate(request, meta, b.values)
			if err != nil {
				t.Fatal(err)
			}
			requireCallerLaunch(t, name+" successor", candidate, requireStaticAdmission(t, candidate), b, a, c)
		})
	}

	// Existing policy: a record without a saved environment launches from the
	// reconstructing caller within its recorded roots; a saved empty one refuses.
	for name, meta := range map[string]task.MetaRecord{"unrecorded": unrecorded, "decoded empty": decodedEmpty} {
		reconstructed, err := PrepareExistingCandidate(request, meta, c.values)
		if err != nil {
			t.Fatalf("%s reconstruction: %v", name, err)
		}
		if !slices.Contains(reconstructed.Inspection.Environment, "HOME="+c.home) || !slices.Equal(reconstructed.WritableRoots, predecessor.WritableRoots) {
			t.Fatalf("%s reconstruction changed its existing caller-launch, recorded-roots policy", name)
		}
	}
	if _, err := PrepareExistingCandidate(request, savedEmpty, c.values); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("saved empty reconstruction error=%v want ErrUnsupportedProfile", err)
	}
}

// TestSuccessorPreservesHistoricalPermissionUnderCallerEnvironment keeps a
// predecessor's historical permission mode and its exact legacy predicate
// while the launch values and roots come from the caller.
func TestSuccessorPreservesHistoricalPermissionUnderCallerEnvironment(t *testing.T) {
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		t.Run(mode, func(t *testing.T) {
			a, b, c := newSuccessorCaller(t, "a"), newSuccessorCaller(t, "b"), newSuccessorCaller(t, "c")
			request := successorRequest(t, mode)
			historical, err := prepareNativeCandidateWithContract(request, a.values, nil, nil, nativePreparationOptions{permissionMode: nativeHistoricalPermission(mode)}, true)
			if err != nil {
				t.Fatal(err)
			}
			predecessor := requireStaticAdmission(t, historical)
			predecessorMeta := admittedRecord(predecessor)
			if !predecessorMeta.Predicate.Equal(legacyNativeReferenceForMode(mode)) || predecessorMeta.EffectiveConfig.Approval != nativeHistoricalPermission(mode) {
				t.Fatalf("fixture is not a historical-permission record: %+v", predecessorMeta.Predicate)
			}

			candidate, err := PrepareSuccessorCandidate(request, predecessorMeta, b.values)
			if err != nil {
				t.Fatal(err)
			}
			successor := requireStaticAdmission(t, candidate)
			requireCallerLaunch(t, "historical successor", candidate, successor, b, a, c)
			if !successor.Plan.Predicate.Equal(predecessorMeta.Predicate) || successor.Effective.Approval != nativeHistoricalPermission(mode) ||
				!slices.Equal(successor.Plan.Arguments, predecessor.Plan.Arguments) {
				t.Fatal("historical successor changed its predicate, permission mode, or argv")
			}
			requireRecordReconstruction(t, "historical predecessor", request, predecessorMeta, predecessor, c)
			requireRecordReconstruction(t, "historical successor", request, admittedRecord(successor), successor, c)
		})
	}
}

// TestSuccessorInspectionDefinitionRoundTripsThroughItsFilteredEnvironment
// prepares a successor from a raw caller environment, then prepares it again
// from the filtered environment its inspection records, for the default and
// explicit native configuration selectors.
func TestSuccessorInspectionDefinitionRoundTripsThroughItsFilteredEnvironment(t *testing.T) {
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		t.Run(mode+"/default", func(t *testing.T) {
			testSuccessorFilteredRoundTrip(t, mode, newSuccessorCaller(t, "a"), newSuccessorCaller(t, "b"), nil)
		})
		t.Run(mode+"/explicit", func(t *testing.T) {
			b := newSuccessorCaller(t, "b").withExplicitSelectors()
			selectedRoots := []string{filepath.Join(b.root, "claude-config"), filepath.Join(b.root, "xdg", "claude")}
			testSuccessorFilteredRoundTrip(t, mode, newSuccessorCaller(t, "a").withExplicitSelectors(), b, selectedRoots)
		})
	}
}

func testSuccessorFilteredRoundTrip(t *testing.T, mode string, a, b successorCaller, selectedRoots []string) {
	t.Helper()
	raw := append(slices.Clone(b.values), "ANTHROPIC_API_KEY=fixture-secret")
	request := successorRequest(t, mode)
	predecessorMeta := admittedRecord(freshCallerAdmission(t, request, a))

	first, err := PrepareSuccessorCandidate(request, predecessorMeta, raw)
	if err != nil {
		t.Fatal(err)
	}
	firstAdmission := requireStaticAdmission(t, first)
	requireCallerLaunch(t, "raw successor", first, firstAdmission, b, a)
	for _, want := range selectedRoots {
		if !slices.Contains(firstAdmission.WritableRoots, want) {
			t.Fatalf("explicit selector root %q missing from %q", want, firstAdmission.WritableRoots)
		}
	}
	second, err := PrepareSuccessorCandidate(request, predecessorMeta, slices.Clone(first.Inspection.Environment))
	if err != nil {
		t.Fatalf("filtered successor environment no longer prepares: %v", err)
	}
	if inspectionDigest(t, first) != inspectionDigest(t, second) || !slices.Equal(first.WritableRoots, second.WritableRoots) {
		t.Fatal("filtered successor environment changed the inspection definition")
	}
	requireSameAdmission(t, "filtered successor", requireStaticAdmission(t, second), firstAdmission)

	reconstructed, err := PrepareExistingCandidate(request, predecessorMeta, raw)
	if err != nil {
		t.Fatal(err)
	}
	if inspectionDigest(t, reconstructed) == inspectionDigest(t, first) {
		t.Fatal("successor reused the predecessor's inspection definition")
	}
}
