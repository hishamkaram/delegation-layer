package claude

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const successorPredecessorTask = "0123456789abcdef0123456789abcdef"

// successorFixture is the admitted native record of a predecessor and the
// continuation request built from it. Only the stored predicate varies.
type successorFixture struct {
	request task.TaskRecord
	meta    task.MetaRecord
}

func newSuccessorFixture(t *testing.T, mode string) successorFixture {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, "claude"), []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", directory)
	t.Setenv("PATH", directory)
	request := profileRequest()
	request.CanonicalCwd, request.Mode, request.RequestedConfig.Permission = directory, mode, mode
	session, err := FreshSessionID(request.RootID, successorPredecessorTask)
	if err != nil {
		t.Fatal(err)
	}
	request.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: session, PredecessorTaskID: successorPredecessorTask}
	candidate, err := PrepareCandidate(request, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	admitted := requireStaticAdmission(t, candidate)
	return successorFixture{request: request, meta: task.MetaRecord{
		EffectiveConfig: admitted.Effective, RuntimePreflightSHA256: admitted.RuntimePreflightSHA256,
		Environment: slices.Clone(admitted.Plan.Environment), EnvironmentRecorded: true, Predicate: admitted.Plan.Predicate,
	}}
}

func (f successorFixture) admit(t *testing.T, prepare commonprovider.PrepareExistingCandidate, reference task.PredicateRef, environment []string) commonprovider.PreparedProfile {
	t.Helper()
	meta := f.meta
	meta.Predicate = reference
	candidate, err := prepare(f.request, meta, environment)
	if err != nil {
		t.Fatal(err)
	}
	return requireStaticAdmission(t, candidate)
}

// eligibleSuccessorSources are the exact print predicates the current
// predicate of the same mode corrects: pre-attribution and the retained
// unscoped and scoped revisions.
func eligibleSuccessorSources(mode string) []task.PredicateRef {
	return []task.PredicateRef{preAttributionReferenceForMode(mode), unscopedReference(mode), scopedReference(mode)}
}

// TestSuccessorCandidateSelectsCurrentPredicateUnderCallerPreparation pairs
// each correctable stored predicate with the reconstruction of the same
// record: reconstruction keeps the source predicate and the record's own
// launch, while the successor is exactly the current admission under its
// caller.
func TestSuccessorCandidateSelectsCurrentPredicateUnderCallerPreparation(t *testing.T) {
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		fixture := newSuccessorFixture(t, mode)
		caller := newSuccessorCaller(t, "successor")
		fresh := freshCallerAdmission(t, fixture.request, caller)
		for _, source := range eligibleSuccessorSources(mode) {
			t.Run(mode+"/"+source.Version+"/"+source.SHA256[:8], func(t *testing.T) {
				successor := fixture.admit(t, PrepareSuccessorCandidate, source, caller.values)
				reconstructed := fixture.admit(t, PrepareExistingCandidate, source, os.Environ())
				if !successor.Plan.Predicate.Equal(ReferenceForMode(mode)) {
					t.Errorf("successor predicate %+v, want %+v", successor.Plan.Predicate, ReferenceForMode(mode))
				}
				if !reconstructed.Plan.Predicate.Equal(source) {
					t.Errorf("reconstruction predicate %+v, want its source %+v", reconstructed.Plan.Predicate, source)
				}
				requireSameAdmission(t, "successor", successor, fresh)
				if !slices.Equal(reconstructed.Plan.Environment, fixture.meta.Environment) || !slices.Equal(reconstructed.WritableRoots, fixture.meta.EffectiveConfig.Policy.WritableRoots) ||
					!task.CompareEffectiveConfigs(reconstructed.Effective, fixture.meta.EffectiveConfig) {
					t.Error("reconstruction did not keep the record's own launch environment, roots, and policy")
				}
			})
		}
	}
}

func TestSuccessorCandidateDoesNotModifyThePredecessorRecord(t *testing.T) {
	fixture := newSuccessorFixture(t, WorkspaceWriteMode)
	fixture.meta.Predicate = preAttributionReferenceForMode(WorkspaceWriteMode)
	before := fixture.meta
	before.Environment = slices.Clone(fixture.meta.Environment)
	encoded := canonicalRecord(t, fixture.meta)
	if _, err := PrepareSuccessorCandidate(fixture.request, fixture.meta, newSuccessorCaller(t, "successor").values); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, fixture.meta) || !bytes.Equal(encoded, canonicalRecord(t, fixture.meta)) {
		t.Error("predecessor record changed")
	}
}

// TestSuccessorCandidateLeavesEveryOtherRecordAsReconstructed pairs each
// refused or unrelated stored predicate with the reconstruction of the same
// record: the successor must reach the same eligibility and predicate, error
// included, while launching from its caller.
func TestSuccessorCandidateLeavesEveryOtherRecordAsReconstructed(t *testing.T) {
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		fixture := newSuccessorFixture(t, mode)
		caller := newSuccessorCaller(t, "successor")
		altered := preAttributionReferenceForMode(mode)
		altered.SHA256 = strings.Repeat("0", 64)
		alteredCurrent := ReferenceForMode(mode)
		alteredCurrent.SHA256 = strings.Repeat("1", 64)
		cases := map[string]task.PredicateRef{
			"current":                ReferenceForMode(mode),
			"previous probe":         previousNativeProbeReferenceForMode(mode),
			"historical":             historicalNativeReferenceForMode(mode),
			"legacy native":          legacyNativeReferenceForMode(mode),
			"portable":               legacyPortableReferenceForMode(mode),
			"interactive":            interactionReference(mode),
			"scoped interactive":     scopedInteractionReference(mode),
			"other mode scoped":      scopedReference(otherMode(mode)),
			"altered digest":         altered,
			"altered current digest": alteredCurrent,
			"unknown":                {Adapter: Provider, Mode: mode, Version: "native-permissions-v9", SHA256: strings.Repeat("2", 64)},
			"zero":                   {},
			"other mode unscoped":    unscopedReference(otherMode(mode)),
		}
		for name, source := range cases {
			t.Run(mode+"/"+name, func(t *testing.T) {
				meta := fixture.meta
				meta.Predicate = source
				requireSameCandidateOutcome(t, fixture.request, meta, caller)
			})
		}
		t.Run(mode+"/non-native profile", func(t *testing.T) {
			meta := fixture.meta
			policy := *meta.EffectiveConfig.Policy
			policy.ProfileRevision = ProfileRevision
			meta.EffectiveConfig.Policy = &policy
			meta.Predicate = preAttributionReferenceForMode(mode)
			requireSameCandidateOutcome(t, fixture.request, meta, caller)
		})
		t.Run(mode+"/interactive request", func(t *testing.T) {
			request := fixture.request
			request.RequestedConfig.Interactive = true
			meta := fixture.meta
			meta.Predicate = preAttributionReferenceForMode(mode)
			requireSameCandidateOutcome(t, request, meta, caller)
		})
	}
}

// requireSameCandidateOutcome requires the successor of meta under caller to
// reach the outcome and predicate of reconstructing meta under the same
// hermetic caller, and to prepare exactly what reconstructing the same record
// admitted under caller would.
func requireSameCandidateOutcome(t *testing.T, request task.TaskRecord, meta task.MetaRecord, caller successorCaller) {
	t.Helper()
	reconstructed, existingErr := PrepareExistingCandidate(request, meta, caller.values)
	successor, successorErr := PrepareSuccessorCandidate(request, meta, caller.values)
	if (existingErr == nil) != (successorErr == nil) || (existingErr != nil && existingErr.Error() != successorErr.Error()) {
		t.Fatalf("successor error=%v, reconstruction error=%v", successorErr, existingErr)
	}
	if existingErr != nil {
		return
	}
	callerRecord, err := PrepareExistingCandidate(request, recordAdmittedUnder(t, meta, caller), caller.values)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(callerRecord.Inspection, successor.Inspection) || !slices.Equal(callerRecord.WritableRoots, successor.WritableRoots) ||
		(reconstructed.Admit == nil) != (successor.Admit == nil) {
		t.Fatal("successor candidate differs from the same record admitted under its caller")
	}
	if reconstructed.Admit == nil {
		return
	}
	want, wantErr := reconstructed.Admit()
	got, gotErr := successor.Admit()
	if wantErr != nil || gotErr != nil || !got.Plan.Predicate.Equal(want.Plan.Predicate) {
		t.Fatalf("successor admission predicate %+v err=%v, reconstructed %+v err=%v", got.Plan.Predicate, gotErr, want.Plan.Predicate, wantErr)
	}
}

// recordAdmittedUnder is meta as its own admission under caller would have
// saved it: the caller's filtered launch environment and writable roots.
func recordAdmittedUnder(t *testing.T, meta task.MetaRecord, caller successorCaller) task.MetaRecord {
	t.Helper()
	environment, err := prepareNativeEnvironment(caller.values)
	if err != nil {
		t.Fatal(err)
	}
	record := meta
	record.Environment, record.EnvironmentRecorded = environment.Values, true
	if meta.EffectiveConfig.Policy != nil {
		policy := *meta.EffectiveConfig.Policy
		policy.WritableRoots = environment.WritableRoots
		record.EffectiveConfig.Policy = &policy
	}
	return record
}
