package opencode

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const successorPredecessorTask = "0123456789abcdef0123456789abcdef"

// successorFixture is a continuation request and the admitted records of a
// predecessor under each dialect: a native run record and an ACP record.
type successorFixture struct {
	request task.TaskRecord
	run     task.MetaRecord
	acp     task.MetaRecord
}

func newSuccessorFixture(t *testing.T, mode string, dialect launchDialect) successorFixture {
	t.Helper()
	directory := installFixtureExecutable(t)
	request := profileRequest(mode)
	request.CanonicalCwd = directory
	request.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: testSession, PredecessorTaskID: successorPredecessorTask}
	candidate, err := PrepareCandidate(request, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	admitted := requireStaticAdmission(t, candidate)
	run, _ := historicalRunRecord(t, request, dialect)
	// A recorded environment differs from the successor's ambient one, so the
	// successor must take it from the predecessor record.
	run.Environment = append(slices.Clone(run.Environment), "OPENCODE_CONFIG_DIR="+directory+"/recorded")
	slices.Sort(run.Environment)
	return successorFixture{request: request, run: run, acp: task.MetaRecord{
		EffectiveConfig: admitted.Effective, RuntimePreflightSHA256: admitted.RuntimePreflightSHA256,
		Environment: slices.Clone(admitted.Plan.Environment), EnvironmentRecorded: true, Predicate: admitted.Plan.Predicate,
	}}
}

func TestSuccessorOfRunPredecessorContinuesThroughACP(t *testing.T) {
	for _, dialect := range []launchDialect{dialectNativeV1, dialectNativeV2} {
		for _, mode := range []string{ModeReadOnly, ModeWorkspaceWrite} {
			fixture := newSuccessorFixture(t, mode, dialect)
			for _, source := range []task.PredicateRef{historicalReferenceForMode(mode), historicalCurrentReferenceForMode(mode), historicalCurrentV4ReferenceForMode(mode), runReferenceForMode(mode)} {
				t.Run(fmt.Sprintf("%d/%s/v%s", dialect, mode, source.Version), func(t *testing.T) {
					predecessor := fixture.run
					predecessor.Predicate = source
					requireACPSuccessor(t, fixture.request, predecessor, os.Environ())
					reconstructed, err := PrepareExistingCandidate(fixture.request, predecessor, os.Environ())
					if err != nil {
						t.Fatal(err)
					}
					if rebuilt := requireStaticAdmission(t, reconstructed); !rebuilt.Plan.Predicate.Equal(source) || rebuilt.Plan.Arguments[0] != "run" || rebuilt.Protocol != nil {
						t.Fatalf("the predecessor no longer reconstructs its run contract: predicate=%+v argv=%q", rebuilt.Plan.Predicate, rebuilt.Plan.Arguments)
					}
				})
			}
		}
	}
}

// requireACPSuccessor requires the successor of a run record to be a fresh ACP
// admission whose environment and writable roots come from current, never
// from the predecessor record, and whose protocol resumes exactly the
// recorded session.
func requireACPSuccessor(t *testing.T, request task.TaskRecord, predecessor task.MetaRecord, current []string) {
	t.Helper()
	candidate, err := PrepareSuccessorCandidate(request, predecessor, current)
	if err != nil {
		t.Fatal(err)
	}
	successor := requireStaticAdmission(t, candidate)
	if !slices.Equal(successor.Plan.Arguments, []string{"acp"}) || !successor.Plan.Predicate.Equal(ReferenceForMode(request.Mode)) ||
		successor.Effective.Policy.ProfileRevision != acpProfileRevision || successor.Identity != nil || successor.Protocol == nil {
		t.Fatalf("successor is not an ACP admission: argv=%q predicate=%+v revision=%q", successor.Plan.Arguments, successor.Plan.Predicate, successor.Effective.Policy.ProfileRevision)
	}
	requireCurrentLaunchEnvironment(t, request, predecessor, successor, current)
	protocol, err := successor.Protocol(task.SessionExpectation{Required: true, ID: testSession}, func(task.SessionIdentity) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = protocol.Start([]byte("follow-up brief")); err != nil {
		t.Fatal(err)
	}
	update, err := protocol.Receive([]byte(acpInitializeResult(1, true)))
	if err != nil || len(update.Outbound) != 1 || !bytes.Contains(update.Outbound[0], []byte(`"method":"session/resume","params":{"sessionId":"`+testSession+`"`)) {
		t.Fatalf("successor does not resume the exact recorded session: frames=%d err=%v", len(update.Outbound), err)
	}
	if _, err = successor.Protocol(task.SessionExpectation{}, func(task.SessionIdentity) error { return nil }); err == nil {
		t.Fatal("successor protocol accepted a fresh-session expectation")
	}
}

// requireCurrentLaunchEnvironment requires successor to launch with the
// environment and writable roots a fresh admission under current selects, and
// with no entry that only the predecessor record carried.
func requireCurrentLaunchEnvironment(t *testing.T, request task.TaskRecord, predecessor task.MetaRecord, successor commonprovider.PreparedProfile, current []string) {
	t.Helper()
	fresh, err := PrepareCandidate(request, current)
	if err != nil {
		t.Fatal(err)
	}
	want := requireStaticAdmission(t, fresh)
	if !slices.Equal(successor.Plan.Environment, want.Plan.Environment) || !slices.Equal(successor.WritableRoots, want.WritableRoots) {
		t.Fatalf("successor environment (%s) or writable roots differ from the current environment's (%s)", summarizeValues(successor.Plan.Environment), summarizeValues(want.Plan.Environment))
	}
	for _, entry := range predecessor.Environment {
		if !slices.Contains(want.Plan.Environment, entry) && slices.Contains(successor.Plan.Environment, entry) {
			t.Fatal("successor kept a launch entry that only the predecessor record carried")
		}
	}
}

func TestSuccessorOfRunPredecessorNeverOpensANewSession(t *testing.T) {
	fixture := newSuccessorFixture(t, ModeReadOnly, dialectNativeV1)
	request := fixture.request
	request.PriorSession = nil
	if _, err := PrepareSuccessorCandidate(request, fixture.run, os.Environ()); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("successor without the recorded session error=%v", err)
	}
}

func TestSuccessorCandidateDoesNotModifyThePredecessorRecord(t *testing.T) {
	for _, dialect := range []launchDialect{dialectNativeV1, dialectNativeV2} {
		fixture := newSuccessorFixture(t, ModeWorkspaceWrite, dialect)
		before := fixture.run
		before.Environment = slices.Clone(fixture.run.Environment)
		policy := *fixture.run.EffectiveConfig.Policy
		before.EffectiveConfig.Policy = &policy
		if _, err := PrepareSuccessorCandidate(fixture.request, fixture.run, os.Environ()); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, fixture.run) {
			t.Errorf("dialect %d: predecessor record changed", dialect)
		}
	}
}

// TestSuccessorCandidateLeavesEveryOtherRecordAsReconstructed pairs each ACP
// and unknown record with the reconstruction of the same record under the same
// caller environment: the successor must behave identically, error included.
// A pure-mode record is covered by TestSuccessorOfPureRecordUsesCurrentEnvironment.
func TestSuccessorCandidateLeavesEveryOtherRecordAsReconstructed(t *testing.T) {
	for _, mode := range []string{ModeReadOnly, ModeWorkspaceWrite} {
		fixture := newSuccessorFixture(t, mode, dialectNativeV2)
		altered := ReferenceForMode(mode)
		altered.SHA256 = strings.Repeat("1", 64)
		cases := map[string]task.MetaRecord{
			"acp":                      fixture.acp,
			"acp altered predicate":    withPredicate(fixture.acp, altered),
			"acp run predicate":        withPredicate(fixture.acp, runReferenceForMode(mode)),
			"acp other mode predicate": withPredicate(fixture.acp, ReferenceForMode(otherMode(mode))),
			"unknown revision":         withRevision(fixture.run, "opencode-future-v9"),
		}
		for name, meta := range cases {
			t.Run(mode+"/"+name, func(t *testing.T) {
				requireSameCandidateOutcome(t, fixture.request, meta)
			})
		}
	}
}

func withPredicate(meta task.MetaRecord, reference task.PredicateRef) task.MetaRecord {
	meta.Predicate = reference
	return meta
}

func withRevision(meta task.MetaRecord, revision string) task.MetaRecord {
	policy := *meta.EffectiveConfig.Policy
	policy.ProfileRevision = revision
	meta.EffectiveConfig.Policy = &policy
	return meta
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

// TestACPV1RecordReconstructsV1AndItsSuccessorUsesV2 keeps an admitted
// acp-v1 record on acp-v1 for reconstruction, while its new exact-session
// successor resumes the recorded session under the current acp-v2 contract.
// The predecessor record never changes.
func TestACPV1RecordReconstructsV1AndItsSuccessorUsesV2(t *testing.T) {
	for _, mode := range []string{ModeReadOnly, ModeWorkspaceWrite} {
		t.Run(mode, func(t *testing.T) {
			fixture := newSuccessorFixture(t, mode, dialectNativeV2)
			for _, recorded := range []task.PredicateRef{acpV1ReferenceForMode(mode), ReferenceForMode(mode)} {
				predecessor := withPredicate(fixture.acp, recorded)
				predecessor.Environment = slices.Clone(fixture.acp.Environment)
				before := predecessor
				before.Environment = slices.Clone(predecessor.Environment)
				policy := *predecessor.EffectiveConfig.Policy
				before.EffectiveConfig.Policy = &policy
				requireACPSuccessor(t, fixture.request, predecessor, os.Environ())
				reconstructed, err := PrepareExistingCandidate(fixture.request, predecessor, os.Environ())
				if err != nil {
					t.Fatal(err)
				}
				if rebuilt := requireStaticAdmission(t, reconstructed); !rebuilt.Plan.Predicate.Equal(recorded) || !slices.Equal(rebuilt.Plan.Arguments, []string{"acp"}) {
					t.Fatalf("%s record reconstructed predicate=%+v argv=%q", recorded.Version, rebuilt.Plan.Predicate, rebuilt.Plan.Arguments)
				}
				if !reflect.DeepEqual(before, predecessor) {
					t.Fatalf("%s predecessor record changed", recorded.Version)
				}
			}
		})
	}
}
