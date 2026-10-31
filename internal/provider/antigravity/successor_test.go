package antigravity

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
// continuation request built from it. Only the stored predicate and recorded
// approval vary.
type successorFixture struct {
	request task.TaskRecord
	meta    task.MetaRecord
}

func newSuccessorFixture(t *testing.T, mode string) successorFixture {
	t.Helper()
	home, workspace := profileFixture(t)
	executable := filepath.Join(t.TempDir(), "agy")
	if err := os.WriteFile(executable, []byte("fixture executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", filepath.Dir(executable))
	request := argumentRequest()
	request.Mode, request.RequestedConfig.Permission, request.CanonicalCwd = mode, mode, workspace
	request.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: testConversation, PredecessorTaskID: successorPredecessorTask}
	candidate, err := PrepareCandidate(request, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := candidate.Admit()
	if err != nil {
		t.Fatal(err)
	}
	return successorFixture{request: request, meta: task.MetaRecord{
		EffectiveConfig: admitted.Effective, RuntimePreflightSHA256: admitted.RuntimePreflightSHA256,
		Environment: slices.Clone(admitted.Plan.Environment), EnvironmentRecorded: true, Predicate: admitted.Plan.Predicate,
	}}
}

func (f successorFixture) admit(t *testing.T, prepare commonprovider.PrepareExistingCandidate, meta task.MetaRecord) commonprovider.PreparedProfile {
	t.Helper()
	candidate, err := prepare(f.request, meta, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	profile, err := candidate.Admit()
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

// eligibleSuccessorSources are the exact native envelope and record revisions
// the current record interpreter corrects.
func eligibleSuccessorSources(mode string) []task.PredicateRef {
	return []task.PredicateRef{
		newHistoricalCurrentPrintInterpreterForMode(mode).Reference(),
		NewCurrentPrintInterpreterForMode(mode).Reference(),
		newHistoricalRecordPrintInterpreter(mode).Reference(),
	}
}

func TestSuccessorCandidateSelectsCurrentRecordAndKeepsPreparation(t *testing.T) {
	for _, mode := range []string{ModeReadOnly, ModeWorkspaceWrite} {
		fixture := newSuccessorFixture(t, mode)
		for _, source := range eligibleSuccessorSources(mode) {
			t.Run(mode+"/"+source.Version, func(t *testing.T) {
				meta := fixture.meta
				meta.Predicate = source
				successor := fixture.admit(t, PrepareSuccessorCandidate, meta)
				reconstructed := fixture.admit(t, PrepareExistingCandidate, meta)
				if !successor.Plan.Predicate.Equal(NewRecordPrintInterpreter(mode).Reference()) {
					t.Errorf("successor predicate %+v, want the current record reference", successor.Plan.Predicate)
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

// TestSuccessorCandidateRetainsRecordedApproval keeps the approval the
// predecessor was admitted under; the predicate upgrade grants no permission.
func TestSuccessorCandidateRetainsRecordedApproval(t *testing.T) {
	fixture := newSuccessorFixture(t, ModeWorkspaceWrite)
	for _, approval := range []string{"accept-edits", unattendedApproval} {
		t.Run(approval, func(t *testing.T) {
			meta := fixture.meta
			meta.EffectiveConfig.Approval = approval
			meta.Predicate = NewCurrentPrintInterpreterForMode(ModeWorkspaceWrite).Reference()
			successor := fixture.admit(t, PrepareSuccessorCandidate, meta)
			reconstructed := fixture.admit(t, PrepareExistingCandidate, meta)
			skipsPermissions := slices.Contains(successor.Plan.Arguments, "--dangerously-skip-permissions")
			if successor.Effective.Approval != approval || skipsPermissions != (approval == unattendedApproval) {
				t.Errorf("successor approval=%q skip-permissions=%t, recorded %q", successor.Effective.Approval, skipsPermissions, approval)
			}
			if !slices.Equal(successor.Plan.Arguments, reconstructed.Plan.Arguments) || successor.Effective.Approval != reconstructed.Effective.Approval {
				t.Errorf("successor launch %q approval %q differs from reconstruction %q approval %q",
					successor.Plan.Arguments, successor.Effective.Approval, reconstructed.Plan.Arguments, reconstructed.Effective.Approval)
			}
		})
	}
}

func TestSuccessorCandidateDoesNotModifyThePredecessorRecord(t *testing.T) {
	fixture := newSuccessorFixture(t, ModeWorkspaceWrite)
	fixture.meta.Predicate = NewCurrentPrintInterpreterForMode(ModeWorkspaceWrite).Reference()
	before := fixture.meta
	before.Environment = slices.Clone(fixture.meta.Environment)
	if _, err := PrepareSuccessorCandidate(fixture.request, fixture.meta, os.Environ()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, fixture.meta) {
		t.Error("predecessor record changed")
	}
}

// TestSuccessorCandidateLeavesEveryOtherRecordAsReconstructed pairs each
// unrelated stored predicate with the reconstruction of the same record: the
// successor must behave identically, error included. The unproven 1.2.2 base
// revision is one of them.
func TestSuccessorCandidateLeavesEveryOtherRecordAsReconstructed(t *testing.T) {
	for _, mode := range []string{ModeReadOnly, ModeWorkspaceWrite} {
		fixture := newSuccessorFixture(t, mode)
		altered := NewCurrentPrintInterpreterForMode(mode).Reference()
		altered.SHA256 = strings.Repeat("0", 64)
		alteredCurrent := NewRecordPrintInterpreter(mode).Reference()
		alteredCurrent.SHA256 = strings.Repeat("1", 64)
		cases := map[string]task.PredicateRef{
			"current record":    NewRecordPrintInterpreter(mode).Reference(),
			"base 1.2.2":        NewPrintInterpreter().Reference(),
			"altered auth3":     altered,
			"altered current":   alteredCurrent,
			"zero":              {},
			"unknown revision":  {Adapter: Provider, Mode: mode, Version: "print-record-v9", SHA256: strings.Repeat("2", 64)},
			"other mode record": newHistoricalRecordPrintInterpreter(otherMode(mode)).Reference(),
		}
		for name, source := range cases {
			t.Run(mode+"/"+name, func(t *testing.T) {
				meta := fixture.meta
				meta.Predicate = source
				requireSameCandidateOutcome(t, fixture.request, meta)
			})
		}
		t.Run(mode+"/portable profile", func(t *testing.T) {
			meta := fixture.meta
			policy := *meta.EffectiveConfig.Policy
			policy.ProfileRevision = ProfileRevision
			meta.EffectiveConfig.Policy = &policy
			meta.Predicate = NewCurrentPrintInterpreterForMode(mode).Reference()
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
	reconstructed, existingErr := PrepareExistingCandidate(request, meta, os.Environ())
	successor, successorErr := PrepareSuccessorCandidate(request, meta, os.Environ())
	if (existingErr == nil) != (successorErr == nil) || (existingErr != nil && existingErr.Error() != successorErr.Error()) {
		t.Fatalf("successor error=%v, reconstruction error=%v", successorErr, existingErr)
	}
	if existingErr != nil {
		return
	}
	if !slices.Equal(reconstructed.WritableRoots, successor.WritableRoots) || (reconstructed.Admit == nil) != (successor.Admit == nil) {
		t.Fatal("successor candidate differs from the reconstructed candidate")
	}
	facts, err := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{
		Executable: reconstructed.Inspection.Executable, SHA256: reconstructed.Inspection.ExecutableSHA256,
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
