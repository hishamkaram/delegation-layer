package claude

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// The projection revisions are the follow-up revisions sealed by completed
// tasks before the failed-request and quota clauses existed: read-only
// native-permissions-v6 and workspace-write native-permissions-v7. They share
// version strings with the current revisions and differ by digest. The
// digests are the ones recorded in those tasks' immutable metadata.
const (
	projectionReadDigest  = "d750a584e223e4c80f2cc165c6ee4cc6d7b664b26a740119671990c966cc36cb"
	projectionWriteDigest = "35853e32272b55b35b2276309ae7f57909d2d462ebed65b88e30f26f62616f11"
	// projectionCorrectionStart and projectionCorrectionEnd bound the
	// failed-request and quota clause that the current revisions append to the
	// frozen projection policy.
	projectionCorrectionStart = "; a result with subtype success and is_error true is the documented failed final request"
	projectionCorrectionEnd   = "or has any other status"
)

// projectionRevisions pairs each recorded projection reference with the
// current digest of the same version and the native permission its task
// recorded.
type projectionRevision struct {
	mode, version, digest, current, approval string
}

var projectionRevisions = []projectionRevision{
	{Mode, "native-permissions-v6", projectionReadDigest, followUpReadDigest, "plan"},
	{WorkspaceWriteMode, "native-permissions-v7", projectionWriteDigest, followUpWriteDigest, "bypassPermissions"},
}

func recordedProjectionReference(mode, version, digest string) task.PredicateRef {
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: version, SHA256: digest}
}

// TestProjectionRevisionsAreFrozenReplayOnlyIdentities proves the recorded
// digests are the current bytes without the correction clause, that the
// catalog resolves each exact reference to a replay-only interpreter, that
// the current references keep their exact digests, and that a changed digest,
// version, or mode resolves nothing.
func TestProjectionRevisionsAreFrozenReplayOnlyIdentities(t *testing.T) {
	catalog, err := commonprovider.NewCatalog(Registration())
	if err != nil {
		t.Fatal(err)
	}
	for _, revision := range projectionRevisions {
		current := ContractForMode(revision.mode)
		start, end := strings.Index(current, projectionCorrectionStart), strings.Index(current, projectionCorrectionEnd)
		if start < 0 || end < start || task.ComputeSHA256([]byte(current[:start]+current[end+len(projectionCorrectionEnd):])) != revision.digest {
			t.Fatalf("%s frozen projection bytes no longer derive from the current contract", revision.mode)
		}
		if reference := ReferenceForMode(revision.mode); reference != recordedProjectionReference(revision.mode, revision.version, revision.current) {
			t.Fatalf("%s current reference changed: %+v", revision.mode, reference)
		}
		requireReplayOnlyProjection(t, catalog.Registry(), revision)
		for name, changed := range projectionRefusedReferences(revision.mode) {
			if _, changedErr := catalog.Registry().Resolve(changed); changedErr == nil {
				t.Fatalf("%s %s reference resolved", revision.mode, name)
			}
		}
	}
}

// requireReplayOnlyProjection requires registry to resolve the recorded
// reference of revision to an interpreter that is neither a recovery source
// nor a recovery target.
func requireReplayOnlyProjection(t *testing.T, registry predicate.Registry, revision projectionRevision) {
	t.Helper()
	reference := recordedProjectionReference(revision.mode, revision.version, revision.digest)
	interpreter, err := registry.Resolve(reference)
	if err != nil || !interpreter.Reference().Equal(reference) {
		t.Fatalf("%s recorded projection reference is not registered: %v", revision.mode, err)
	}
	if _, err = registry.ResolveRecovery(reference); !errors.Is(err, predicate.ErrRecoveryUnsupported) {
		t.Fatalf("%s projection revision became a recovery source: %v", revision.mode, err)
	}
	frozen, ok := interpreter.(predicate.RecoveryInterpreter)
	if !ok {
		t.Fatalf("%s projection revision lost its recovery contract", revision.mode)
	}
	if _, err = frozen.Recover(revisionInput(nil, ReferenceForMode(revision.mode)), &claudeTestEvidence{}, new(strings.Builder)); !errors.Is(err, task.ErrIncompatiblePredicate) {
		t.Fatalf("%s projection revision became a recovery target: %v", revision.mode, err)
	}
}

// projectionRefusedReferences are near misses of mode's recorded projection
// reference that no interpreter owns.
func projectionRefusedReferences(mode string) map[string]task.PredicateRef {
	own, other := projectionRevisions[0], projectionRevisions[1]
	if mode == WorkspaceWriteMode {
		own, other = other, own
	}
	altered := []byte(own.digest)
	altered[len(altered)-1] ^= 1
	return map[string]task.PredicateRef{
		"altered digest":   recordedProjectionReference(mode, own.version, string(altered)),
		"other version":    recordedProjectionReference(mode, other.version, own.digest),
		"other mode label": recordedProjectionReference(other.mode, own.version, own.digest),
		"other digest":     recordedProjectionReference(mode, own.version, other.digest),
	}
}

// TestProjectionRevisionKeepsItsRecordedInterpretation replays sealed streams
// through the recorded projection references: follow-up answers are framed as
// under the current revision, but a failed final request stays malformed and
// never reports a quota, while the current revision keeps its corrected
// provider-failure and quota classification.
func TestProjectionRevisionKeepsItsRecordedInterpretation(t *testing.T) {
	registry := retainedRegistry(t)
	for _, revision := range projectionRevisions {
		frozen, err := registry.Resolve(recordedProjectionReference(revision.mode, revision.version, revision.digest))
		if err != nil {
			t.Fatalf("%s recorded projection reference is not registered: %v", revision.mode, err)
		}
		current := NewInterpreter(revision.mode)
		lines := followUpTraceForMode(revision.mode)
		for _, v := range []predicate.Interpreter{frozen, current} {
			if committed, answer := evaluateWriteTrace(t, v, lines); committed.Verdict != task.VerdictCommitted || answer != expectedResponses(t, lines) {
				t.Fatalf("%s %s did not frame follow-up answers: %+v answer=%q", revision.mode, v.Reference().SHA256, committed, answer)
			}
		}
		failed := []string{interactiveInitLine(revision.mode), rateLimitLine("rejected"), apiErrorResult(`,"result_index":0`)}
		historical, answer := evaluateQuotaTrace(t, frozen, failed, 0)
		if historical.Verdict != task.VerdictRejected || historical.Refusal != refusalMalformed || answer != "" ||
			len(historical.Diagnostics) == 0 || historical.Diagnostics[0] != claudeCurrentDiagnostic(refusalMalformed) {
			t.Fatalf("%s projection revision reclassified a failed final request: %+v", revision.mode, historical)
		}
		exited, answer := evaluateQuotaTrace(t, frozen, failed, 1)
		requireQuotaRejection(t, exited, answer, genericProviderFailure())
		for _, exitCode := range []int{0, 1} {
			corrected, correctedAnswer := evaluateQuotaTrace(t, current, failed, exitCode)
			requireQuotaRejection(t, corrected, correctedAnswer, claudeQuotaDiagnostic())
		}
	}
}

// TestProjectionPredecessorsContinueThroughTheCatalog prepares a successor and
// a reconstruction through the production catalog for each recorded
// projection reference, with the permission recorded by its task and with the
// permission derived from the reference alone. The successor is exactly the
// fresh catalog admission of the same exact-session request; reconstruction
// keeps the recorded reference, permission, environment, and relaxed probe.
func TestProjectionPredecessorsContinueThroughTheCatalog(t *testing.T) {
	catalog, err := commonprovider.NewCatalog(Registration())
	if err != nil {
		t.Fatal(err)
	}
	for _, revision := range projectionRevisions {
		continuation := projectionContinuation{catalog: catalog, fixture: newSuccessorFixture(t, revision.mode), caller: newSuccessorCaller(t, "successor"), revision: revision}
		continuation.fresh = catalogAdmission(t, "fresh", func() (commonprovider.ProfileCandidate, error) {
			return catalog.Candidate(continuation.fixture.request, continuation.caller.values)
		})
		for name, approval := range map[string]string{"recorded approval": revision.approval, "derived approval": ""} {
			t.Run(revision.mode+"/"+name, func(t *testing.T) { continuation.requireContinues(t, approval) })
		}
		for name, changed := range projectionRefusedReferences(revision.mode) {
			t.Run(revision.mode+"/refused "+name, func(t *testing.T) { continuation.requireRefused(t, changed) })
		}
	}
}

// projectionContinuation is one mode's admitted predecessor record, the
// hermetic successor caller, and the fresh catalog admission of the same
// exact-session request under that caller.
type projectionContinuation struct {
	catalog  commonprovider.Catalog
	fixture  successorFixture
	caller   successorCaller
	fresh    commonprovider.PreparedProfile
	revision projectionRevision
}

// requireContinues stores the recorded projection reference with approval as
// the recorded permission, or none, and requires the successor to be the fresh
// current admission while reconstruction keeps the recorded contract.
func (c projectionContinuation) requireContinues(t *testing.T, approval string) {
	t.Helper()
	revision, source := c.revision, recordedProjectionReference(c.revision.mode, c.revision.version, c.revision.digest)
	meta := c.fixture.meta
	meta.Predicate, meta.Approval, meta.EffectiveConfig.Approval = source, approval, approval
	successor := catalogAdmission(t, "successor", func() (commonprovider.ProfileCandidate, error) {
		return c.catalog.SuccessorCandidate(c.fixture.request, meta, c.caller.values)
	})
	if successor.Plan.Predicate != recordedProjectionReference(revision.mode, revision.version, revision.current) || successor.Effective.Approval != revision.approval {
		t.Fatalf("successor predicate=%+v approval=%q", successor.Plan.Predicate, successor.Effective.Approval)
	}
	requireSameAdmission(t, "successor", successor, c.fresh)
	existing, err := c.catalog.ExistingCandidate(c.fixture.request, meta, os.Environ())
	if err != nil {
		t.Fatalf("reconstruction refused the recorded projection reference: %v", err)
	}
	requireRuntimeProbeDefinition(t, existing.Inspection, []string{"--print"})
	rebuilt := catalogAdmission(t, "reconstruction", func() (commonprovider.ProfileCandidate, error) { return existing, nil })
	keptEnvironment := slices.Equal(rebuilt.Plan.Environment, c.fixture.meta.Environment)
	if !rebuilt.Plan.Predicate.Equal(source) || rebuilt.Effective.Approval != revision.approval || !keptEnvironment {
		t.Fatalf("reconstruction predicate=%+v approval=%q kept environment=%t", rebuilt.Plan.Predicate, rebuilt.Effective.Approval, keptEnvironment)
	}
}

// requireRefused requires both successor preparation and reconstruction of a
// record carrying changed to refuse it as an unsupported profile.
func (c projectionContinuation) requireRefused(t *testing.T, changed task.PredicateRef) {
	t.Helper()
	meta := c.fixture.meta
	meta.Predicate = changed
	if _, err := c.catalog.SuccessorCandidate(c.fixture.request, meta, c.caller.values); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("successor accepted %+v: %v", changed, err)
	}
	if _, err := c.catalog.ExistingCandidate(c.fixture.request, meta, os.Environ()); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("reconstruction accepted %+v: %v", changed, err)
	}
}

// catalogAdmission prepares a candidate through the catalog and returns its
// static admission, which the catalog binds to its registry.
func catalogAdmission(t *testing.T, label string, prepare func() (commonprovider.ProfileCandidate, error)) commonprovider.PreparedProfile {
	t.Helper()
	candidate, err := prepare()
	if err != nil {
		t.Fatalf("%s preparation refused the recorded projection reference: %v", label, err)
	}
	if candidate.Admit == nil {
		t.Fatalf("%s candidate does not expose static admission", label)
	}
	admitted, err := candidate.Admit()
	if err != nil {
		t.Fatalf("%s admission: %v", label, err)
	}
	return admitted
}
