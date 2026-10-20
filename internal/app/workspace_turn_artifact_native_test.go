package app

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

func (l nativeLineage) artifactBytes(t *testing.T, capture nativeCapture, name string) []byte {
	t.Helper()
	for _, artifact := range capture.artifacts {
		if artifact.Name != name {
			continue
		}
		if artifact.Omitted != "" {
			t.Fatalf("artifact %s was omitted: %+v", name, artifact)
		}
		data, err := os.ReadFile(filepath.Join(workspace.ScratchPath(l.store.Root, capture.OperationID), name))
		if err != nil || int64(len(data)) != artifact.Length || task.ComputeSHA256(data) != artifact.SHA256 {
			t.Fatalf("artifact %s bytes do not match %+v: %v", name, artifact, err)
		}
		return data
	}
	t.Fatalf("capture lacks artifact %s: %+v", name, capture.artifacts)
	return nil
}

func assertArtifactBytes(t *testing.T, lineage nativeLineage, capture nativeCapture, name, want string) {
	t.Helper()
	if got := lineage.artifactBytes(t, capture, name); string(got) != want {
		t.Fatalf("%s = %q, want %q", name, got, want)
	}
}

// Turn files come from the turn's own prelaunch baseline, so the second turn
// excludes the first turn and the Lead's edit made before its launch, while
// the cumulative artifacts keep every change since the workspace base.
func TestNativeTurnArtifactsAttributeOnlyCurrentTurnFiles(t *testing.T) {
	lineage := newNativeLineage(t)
	resource, registration := lineage.provisionedChild(t, lineage.main)
	checkout := registration.CheckoutPath
	first := lineage.beginTurn(t, resource, registration)
	lineage.baseline(t, resource, first)
	writeLineageFile(t, filepath.Join(checkout, "first.txt"), "first turn\n")
	lineage.gitIn(t, checkout, "add", "first.txt")
	lineage.commit(t, checkout, "first turn checkpoint")
	writeLineageFile(t, filepath.Join(checkout, "tracked.txt"), "first turn dirty\n")
	firstCapture := lineage.captureTurn(t, resource, first)
	assertArtifactBytes(t, lineage, firstCapture, workspace.TurnNameStatusName, "A\x00first.txt\x00M\x00tracked.txt\x00")
	lineage.sealTurn(t, resource, first, firstCapture)
	writeLineageFile(t, filepath.Join(checkout, "lead.txt"), "lead edit before the second launch\n")
	second := lineage.beginTurn(t, resource, registration)
	lineage.baseline(t, resource, second)
	writeLineageFile(t, filepath.Join(checkout, "second.txt"), "second turn\n")
	if err := os.Remove(filepath.Join(checkout, "removed.txt")); err != nil {
		t.Fatal(err)
	}
	secondCapture := lineage.captureTurn(t, resource, second)
	assertArtifactBytes(t, lineage, secondCapture, workspace.TurnNameStatusName, "D\x00removed.txt\x00A\x00second.txt\x00")
	assertArtifactBytes(t, lineage, secondCapture, workspace.CaptureNameStatusName,
		"A\x00first.txt\x00A\x00lead.txt\x00D\x00removed.txt\x00A\x00second.txt\x00M\x00tracked.txt\x00")
	turnPatch := lineage.artifactBytes(t, secondCapture, workspace.TurnPatchName)
	if !bytes.Contains(turnPatch, []byte("b/second.txt")) || bytes.Contains(turnPatch, []byte("first.txt")) || bytes.Contains(turnPatch, []byte("lead.txt")) {
		t.Fatalf("turn patch is not limited to the current turn:\n%s", turnPatch)
	}
	lineage.sealTurn(t, resource, second, secondCapture)
	assertTurnDescriptors(t, lineage, resource, second, secondCapture, true)
	for _, capture := range []nativeCapture{firstCapture, secondCapture} {
		lineage.publishCaptureResult(t, resource, capture)
	}
	releaseAndForgetNativeChild(t, lineage, resource)
	assertTurnDescriptors(t, lineage, resource, second, secondCapture, false)
}

// publishCaptureResult registers the capture operation with its workspace,
// closes it through the real guardian record fixture and records its result,
// as the scheduled worker would, so the forget inventory sees the exact
// artifacts the capture produced.
func (l nativeLineage) publishCaptureResult(t *testing.T, resource *workspace.Resource, capture nativeCapture) {
	t.Helper()
	operation := capture.operation
	if err := resource.RecordOperation(l.scope.ctx, operation.Request()); err != nil {
		t.Fatal(err)
	}
	workspacefixture.SeedRealOperationClosure(t, l.store, operation, workspacescore.OperationClosed)
	result := workspace.OperationResult{
		SchemaVersion: task.SchemaVersion, OperationID: operation.Request().OperationID, RequestSHA256: operation.Digest(),
		Status: "succeeded", Artifacts: capture.artifacts,
	}
	if err := operation.RecordResult(t.Context(), result); err != nil {
		t.Fatal(err)
	}
}

// assertTurnDescriptors checks the public task projection: while retained, a
// turn artifact has a readable path; after forget the path is hidden and the
// bytes are removed.
func assertTurnDescriptors(t *testing.T, lineage nativeLineage, resource *workspace.Resource, turn nativeTurn, capture nativeCapture, retained bool) {
	t.Helper()
	description, err := workspacescore.DescribeTask(lineage.scope.ctx, resource, turn.request)
	if err != nil || description.Capture == nil || !description.Capture.Candidate.ObservedTurn() {
		t.Fatalf("task projection = %+v, %v", description, err)
	}
	found := 0
	for _, descriptor := range description.Artifacts {
		if descriptor.Name != workspace.TurnPatchName && descriptor.Name != workspace.TurnNameStatusName {
			continue
		}
		found++
		assertTurnDescriptor(t, descriptor, filepath.Join(workspace.ScratchPath(lineage.store.Root, capture.OperationID), descriptor.Name), capture.OperationID, retained)
	}
	if found != 2 {
		t.Fatalf("task projection has %d turn descriptors: %+v", found, description.Artifacts)
	}
}

func assertTurnDescriptor(t *testing.T, descriptor workspacescore.ArtifactDescription, path, operationID string, retained bool) {
	t.Helper()
	_, statErr := os.Stat(path)
	if retained && (descriptor.Path != path || descriptor.EvidenceState != "retained" || descriptor.OperationID != operationID || statErr != nil) {
		t.Fatalf("retained turn descriptor = %+v, stat %v", descriptor, statErr)
	}
	if !retained && (descriptor.Path != "" || descriptor.EvidenceState == "retained" || !errors.Is(statErr, os.ErrNotExist)) {
		t.Fatalf("forgotten turn descriptor = %+v, stat %v", descriptor, statErr)
	}
}

// A flag-only index edit is a mutation even when content and HEAD are
// unchanged and the turn file list is empty.
func TestNativeFlagOnlyTurnReportsIndexChange(t *testing.T) {
	lineage := newNativeLineage(t)
	resource, registration := lineage.provisionedChild(t, lineage.main)
	turn := lineage.beginTurn(t, resource, registration)
	lineage.baseline(t, resource, turn)
	lineage.gitIn(t, registration.CheckoutPath, "update-index", "--assume-unchanged", "tracked.txt")
	capture := lineage.captureTurn(t, resource, turn)
	assertTurn(t, capture.Turn, workspace.TurnUnchanged, workspace.TurnUnchanged, workspace.TurnChanged)
	assertArtifactBytes(t, lineage, capture, workspace.TurnNameStatusName, "")
	assertArtifactBytes(t, lineage, capture, workspace.TurnPatchName, "")
	if flags := lineage.gitIn(t, registration.CheckoutPath, "ls-files", "-v", "tracked.txt"); flags != "h tracked.txt" {
		t.Fatalf("capture changed the real index flag: %q", flags)
	}
	lineage.sealTurn(t, resource, turn, capture)
}

// Staging an intent-to-add entry as an empty file changes only the index: the
// tree, HEAD and turn file list stay the same, and the turn still reports it.
func TestNativeIntentToAddOnlyTurnReportsIndexChange(t *testing.T) {
	lineage := newNativeLineage(t)
	resource, registration := lineage.provisionedChild(t, lineage.main)
	writeLineageFile(t, filepath.Join(registration.CheckoutPath, "empty-new.txt"), "")
	lineage.gitIn(t, registration.CheckoutPath, "add", "-N", "empty-new.txt")
	turn := lineage.beginTurn(t, resource, registration)
	lineage.baseline(t, resource, turn)
	lineage.gitIn(t, registration.CheckoutPath, "add", "empty-new.txt")
	capture := lineage.captureTurn(t, resource, turn)
	assertTurn(t, capture.Turn, workspace.TurnUnchanged, workspace.TurnUnchanged, workspace.TurnChanged)
	assertArtifactBytes(t, lineage, capture, workspace.TurnNameStatusName, "")
	lineage.sealTurn(t, resource, turn, capture)
}

// A turn may start at a non-base checkpoint commit and then rewrite history.
// Once that commit is expired and pruned, the baseline still pins its tree and
// the original base stays pinned, so the final capture compares with the
// recorded baseline instead of requiring the old HEAD commit.
func TestNativeTurnCaptureSurvivesPrunedBaselineHEAD(t *testing.T) {
	lineage := newNativeLineage(t)
	resource, registration := lineage.provisionedChild(t, lineage.main)
	checkout := registration.CheckoutPath
	writeLineageFile(t, filepath.Join(checkout, "checkpoint.txt"), "lead checkpoint\n")
	lineage.gitIn(t, checkout, "add", "checkpoint.txt")
	checkpoint := lineage.commit(t, checkout, "lead checkpoint before the turn")
	turn := lineage.beginTurn(t, resource, registration)
	baseline := lineage.baseline(t, resource, turn)
	if baseline.ObservedHEAD != checkpoint || checkpoint == registration.BaseOID {
		t.Fatalf("baseline HEAD = %s, want the non-base checkpoint %s", baseline.ObservedHEAD, checkpoint)
	}
	lineage.gitIn(t, checkout, "-c", "user.name=Delegate fixture", "-c", "user.email=fixture@example.invalid", "commit", "--quiet", "--amend", "-m", "worker rewrote the checkpoint")
	lineage.gitIn(t, lineage.main, "reflog", "expire", "--expire=now", "--all")
	lineage.gitIn(t, lineage.main, "prune", "--expire=now")
	if lineage.gitSucceeds(t, lineage.main, "cat-file", "-e", checkpoint+"^{commit}") {
		t.Fatal("fixture did not prune the baseline HEAD commit")
	}
	for _, object := range []string{baseline.Ref + "^{tree}", registration.BaseOID + "^{commit}"} {
		if !lineage.gitSucceeds(t, lineage.main, "cat-file", "-e", object) {
			t.Fatalf("fixture lost %s", object)
		}
	}
	writeLineageFile(t, filepath.Join(checkout, "worker.txt"), "after rewrite\n")
	indexPath := filepath.Join(registration.AdminDir, "index")
	indexBefore, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	head := lineage.gitIn(t, checkout, "rev-parse", "HEAD")
	capture := lineage.captureTurn(t, resource, turn)
	if indexAfter, readErr := os.ReadFile(indexPath); readErr != nil || !bytes.Equal(indexBefore, indexAfter) || lineage.gitIn(t, checkout, "rev-parse", "HEAD") != head {
		t.Fatalf("capture changed the real index or HEAD: %v", readErr)
	}
	assertTurn(t, capture.Turn, workspace.TurnChanged, workspace.TurnChanged, workspace.TurnUnchanged)
	if capture.Turn.StartHEAD != checkpoint || capture.Turn.StartTreeOID != baseline.TreeOID {
		t.Fatalf("turn did not keep its recorded baseline: %+v", capture.Turn)
	}
	assertArtifactBytes(t, lineage, capture, workspace.TurnNameStatusName, "A\x00worker.txt\x00")
	lineage.sealTurn(t, resource, turn, capture)
}
