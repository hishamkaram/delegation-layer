package opencode

import (
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
	if err = os.WriteFile(filepath.Join(directory, "opencode"), []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", directory)
	t.Setenv("PATH", directory)
	request := profileRequest(mode)
	request.CanonicalCwd = directory
	request.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: testSession, PredecessorTaskID: successorPredecessorTask}
	candidate, err := PrepareCandidate(request)
	if err != nil {
		t.Fatal(err)
	}
	admitted := requireStaticAdmission(t, candidate)
	return successorFixture{request: request, meta: task.MetaRecord{
		EffectiveConfig: admitted.Effective, RuntimePreflightSHA256: admitted.RuntimePreflightSHA256,
		Environment: slices.Clone(admitted.Plan.Environment), EnvironmentRecorded: true, Predicate: admitted.Plan.Predicate,
	}}
}

func (f successorFixture) admit(t *testing.T, prepare func(task.TaskRecord, task.MetaRecord) (commonprovider.ProfileCandidate, error), reference task.PredicateRef) commonprovider.PreparedProfile {
	t.Helper()
	meta := f.meta
	meta.Predicate = reference
	candidate, err := prepare(f.request, meta)
	if err != nil {
		t.Fatal(err)
	}
	return requireStaticAdmission(t, candidate)
}

func TestSuccessorCandidateSelectsCurrentPredicateAndKeepsPreparation(t *testing.T) {
	for _, mode := range []string{ModeReadOnly, ModeWorkspaceWrite} {
		fixture := newSuccessorFixture(t, mode)
		for _, source := range []task.PredicateRef{historicalReferenceForMode(mode), historicalCurrentReferenceForMode(mode), historicalCurrentV4ReferenceForMode(mode)} {
			t.Run(mode+"/v"+source.Version, func(t *testing.T) {
				successor := fixture.admit(t, PrepareSuccessorCandidate, source)
				reconstructed := fixture.admit(t, PrepareExistingCandidate, source)
				if !successor.Plan.Predicate.Equal(ReferenceForMode(mode)) {
					t.Errorf("successor predicate %+v, want %+v", successor.Plan.Predicate, ReferenceForMode(mode))
				}
				if !reconstructed.Plan.Predicate.Equal(source) {
					t.Errorf("reconstruction predicate %+v, want its source %+v", reconstructed.Plan.Predicate, source)
				}
				successor.Plan.Predicate, reconstructed.Plan.Predicate = task.PredicateRef{}, task.PredicateRef{}
				planEqual := reflect.DeepEqual(successor.Plan, reconstructed.Plan)
				effectiveEqual := reflect.DeepEqual(successor.Effective, reconstructed.Effective)
				rootsEqual := slices.Equal(successor.WritableRoots, reconstructed.WritableRoots)
				preflightEqual := successor.RuntimePreflightSHA256 == reconstructed.RuntimePreflightSHA256
				if !planEqual || !effectiveEqual || !rootsEqual || !preflightEqual {
					t.Errorf("successor preparation differs beyond the predicate: plan equal=%t effective config equal=%t writable roots equal=%t runtime preflight equal=%t",
						planEqual, effectiveEqual, rootsEqual, preflightEqual)
				}
			})
		}
	}
}

func TestSuccessorCandidateDoesNotModifyThePredecessorRecord(t *testing.T) {
	fixture := newSuccessorFixture(t, ModeWorkspaceWrite)
	fixture.meta.Predicate = historicalCurrentV4ReferenceForMode(ModeWorkspaceWrite)
	before := fixture.meta
	before.Environment = slices.Clone(fixture.meta.Environment)
	if _, err := PrepareSuccessorCandidate(fixture.request, fixture.meta); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, fixture.meta) {
		t.Error("predecessor record changed")
	}
}

// TestSuccessorCandidateLeavesEveryOtherRecordAsReconstructed pairs each
// unrelated stored predicate with the reconstruction of the same record: the
// successor must behave identically, error included.
func TestSuccessorCandidateLeavesEveryOtherRecordAsReconstructed(t *testing.T) {
	for _, mode := range []string{ModeReadOnly, ModeWorkspaceWrite} {
		fixture := newSuccessorFixture(t, mode)
		altered := historicalCurrentV4ReferenceForMode(mode)
		altered.SHA256 = strings.Repeat("0", 64)
		alteredCurrent := ReferenceForMode(mode)
		alteredCurrent.SHA256 = strings.Repeat("1", 64)
		cases := map[string]task.PredicateRef{
			"current":             ReferenceForMode(mode),
			"legacy v1":           LegacyReferenceForMode(mode),
			"altered v4 digest":   altered,
			"altered current":     alteredCurrent,
			"zero":                {},
			"other mode revision": historicalCurrentV4ReferenceForMode(otherMode(mode)),
			"unknown revision":    {Adapter: Provider, Mode: mode, Version: "9", SHA256: strings.Repeat("2", 64)},
		}
		for name, source := range cases {
			t.Run(mode+"/"+name, func(t *testing.T) {
				meta := fixture.meta
				meta.Predicate = source
				requireSameCandidateOutcome(t, fixture.request, meta)
			})
		}
		t.Run(mode+"/legacy profile", func(t *testing.T) {
			meta := fixture.meta
			policy := *meta.EffectiveConfig.Policy
			policy.ProfileRevision = ProfileRevision
			meta.EffectiveConfig.Policy = &policy
			meta.Predicate = historicalCurrentV4ReferenceForMode(mode)
			requireSameCandidateOutcome(t, fixture.request, meta)
		})
	}
}

func otherMode(mode string) string {
	if mode == ModeReadOnly {
		return ModeWorkspaceWrite
	}
	return ModeReadOnly
}

func requireSameCandidateOutcome(t *testing.T, request task.TaskRecord, meta task.MetaRecord) {
	t.Helper()
	reconstructed, existingErr := PrepareExistingCandidate(request, meta)
	successor, successorErr := PrepareSuccessorCandidate(request, meta)
	if (existingErr == nil) != (successorErr == nil) || (existingErr != nil && existingErr.Error() != successorErr.Error()) {
		t.Fatalf("successor error=%v, reconstruction error=%v", successorErr, existingErr)
	}
	if existingErr != nil {
		return
	}
	if !slices.Equal(reconstructed.WritableRoots, successor.WritableRoots) || (reconstructed.Admit == nil) != (successor.Admit == nil) {
		t.Fatal("successor candidate differs from the reconstructed candidate")
	}
	if reconstructed.Admit != nil {
		want, wantErr := reconstructed.Admit()
		got, gotErr := successor.Admit()
		if wantErr != nil || gotErr != nil || !got.Plan.Predicate.Equal(want.Plan.Predicate) {
			t.Fatalf("successor admission predicate %+v err=%v, reconstructed %+v err=%v", got.Plan.Predicate, gotErr, want.Plan.Predicate, wantErr)
		}
		return
	}
	facts, err := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{
		Executable: reconstructed.Inspection.Executable, SHA256: reconstructed.Inspection.ExecutableSHA256, Version: "fixture",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want, wantErr := reconstructed.Finalize(facts, time.Now())
	got, gotErr := successor.Finalize(facts, time.Now())
	if (wantErr == nil) != (gotErr == nil) || !got.Plan.Predicate.Equal(want.Plan.Predicate) {
		t.Fatalf("successor finalized %+v err=%v, reconstruction %+v err=%v", got.Plan.Predicate, gotErr, want.Plan.Predicate, wantErr)
	}
}
