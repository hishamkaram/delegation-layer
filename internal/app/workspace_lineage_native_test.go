package app

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
)

func TestNativeSiblingProvisionFromDetachedLinkedSource(t *testing.T) {
	lineage := newNativeLineage(t)
	parent := lineage.linkedSource(t, filepath.Join(lineage.container, strings.Repeat("a", 32), "checkout"))
	resource, registration := lineage.provisionedChild(t, parent)
	if registration.SourceRoot != parent || registration.CommonDir != filepath.Join(lineage.main, ".git") {
		t.Fatalf("registration source = %s/%s, want linked parent and shared common directory", registration.SourceRoot, registration.CommonDir)
	}
	if registration.CheckoutPath != filepath.Join(lineage.container, resource.Request().WorkspaceID, "checkout") {
		t.Fatalf("child checkout %s is not the exact sibling anchor", registration.CheckoutPath)
	}
	source, err := resource.Source(lineage.scope.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, recorded := source.CommonDirIdentity(); !recorded {
		t.Fatal("new source did not pin the common-directory identity")
	}
	if status := lineage.gitIn(t, parent, "status", "--porcelain"); status != "" {
		t.Fatalf("provisioning modified the parent source checkout: %q", status)
	}
}

func TestNativeProvisionRejectsOwnedPlacementOverlaps(t *testing.T) {
	t.Run("self", func(t *testing.T) {
		lineage := newNativeLineage(t)
		workspaceID := strings.Repeat("c", 32)
		source := lineage.linkedSource(t, filepath.Join(lineage.container, workspaceID))
		resource := lineage.resourceWithID(t, lineage.store, source, lineage.container, workspaceID)
		assertOwnedPlacementRefused(t, lineage, resource, source)
		assertNoManagedMarker(t, source)
	})
	t.Run("ancestor", func(t *testing.T) {
		lineage := newNativeLineage(t)
		nested := openLineageStore(t, filepath.Join(lineage.main, "nested-state"))
		container := workspace.ContainerPath(nested.Root, nested.RootID)
		resource := lineage.resource(t, nested, lineage.main, container)
		if err := lineage.action(t, nested, resource, workspace.OperationProvision); !errors.Is(err, workspace.ErrIdentity) {
			t.Fatalf("anchor inside the source checkout error = %v", err)
		}
		assertPathAbsent(t, container)
	})
	t.Run("symlink alias", func(t *testing.T) {
		lineage := newNativeLineage(t)
		alias := filepath.Join(lineage.root, "alias")
		if err := os.Symlink(lineage.main, alias); err != nil {
			t.Fatal(err)
		}
		resource := lineage.resource(t, lineage.store, lineage.main, filepath.Join(alias, "container"))
		assertOwnedPlacementRefused(t, lineage, resource, filepath.Join(lineage.main, "container"))
	})
}

func assertOwnedPlacementRefused(t *testing.T, lineage nativeLineage, resource *workspace.Resource, forbidden string) {
	t.Helper()
	if err := lineage.action(t, lineage.store, resource, workspace.OperationProvision); !errors.Is(err, workspace.ErrIdentity) {
		t.Fatalf("overlapping owned placement error = %v, want workspace identity refusal", err)
	}
	assertPathAbsent(t, filepath.Join(forbidden, "checkout"))
	if _, err := resource.Anchor(lineage.scope.ctx); !workspace.OperationAbsent(err) {
		t.Fatalf("refused placement recorded an anchor: %v", err)
	}
}

func assertPathAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused provisioning created %s: %v", path, err)
	}
}

func assertNoManagedMarker(t *testing.T, path string) {
	t.Helper()
	assertPathAbsent(t, filepath.Join(path, ".delegate-workspace"))
}

func TestNativeChildSurvivesTransientSourceRemoval(t *testing.T) {
	lineage := newNativeLineage(t)
	parent := lineage.linkedSource(t, filepath.Join(lineage.container, strings.Repeat("a", 32), "checkout"))
	resource, registration := lineage.provisionedChild(t, parent)
	writeLineageFile(t, filepath.Join(registration.CheckoutPath, "child.txt"), "child edit\n")
	if err := os.RemoveAll(parent); err != nil {
		t.Fatal(err)
	}
	candidate := lineage.capture(t, resource, lineageOperationSubject("after source removal"))
	if candidate.BaseOID != registration.BaseOID {
		t.Fatalf("capture base %s, want original base %s", candidate.BaseOID, registration.BaseOID)
	}
	refs := lineage.ownedRefs(t, resource)
	if !strings.Contains(refs, registration.BaseRef+" "+registration.BaseOID) || !strings.Contains(refs, candidate.SnapshotRef+" "+candidate.TreeOID) {
		t.Fatalf("child base pin or capture ref missing after source removal:\n%s", refs)
	}
	releaseAndForgetNativeChild(t, lineage, resource)
	assertPathAbsent(t, registration.CheckoutPath)
	if refs = lineage.ownedRefs(t, resource); refs != "" {
		t.Fatalf("forget left owned refs behind:\n%s", refs)
	}
}

func releaseAndForgetNativeChild(t *testing.T, lineage nativeLineage, resource *workspace.Resource) {
	t.Helper()
	ctx := lineage.scope.ctx
	release := workspace.ReleaseRecord{SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: strings.Repeat("5", 32)}
	if err := resource.RecordRelease(ctx, release); err != nil {
		t.Fatal(err)
	}
	if err := lineage.action(t, lineage.store, resource, workspace.OperationRemove); err != nil {
		t.Fatalf("release cleanup: %v", err)
	}
	if err := resource.RecordRemoved(ctx, release.OperationID); err != nil {
		t.Fatal(err)
	}
	intent, err := workspacescore.EnsureForgetIntent(ctx, lineage.store, resource, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = lineage.action(t, lineage.store, resource, workspace.OperationForget); err != nil {
		t.Fatalf("forget %d refs: %v", len(intent.Refs), err)
	}
}

func TestNativeChangedCommonDirectoryIsRefused(t *testing.T) {
	t.Run("pinned identity", func(t *testing.T) { assertChangedCommonDirectoryRefused(t, false) })
	t.Run("legacy source", func(t *testing.T) { assertChangedCommonDirectoryRefused(t, true) })
}

// A legacy source has no recorded inode; the child's own Git topology must
// still refuse a repository replaced at the same common-directory path.
func assertChangedCommonDirectoryRefused(t *testing.T, legacy bool) {
	t.Helper()
	lineage := newNativeLineage(t)
	resource := lineage.resource(t, lineage.store, lineage.main, lineage.container)
	if legacy {
		source, err := lineage.git.ResolveSource(lineage.scope.ctx, lineage.main, "")
		if err == nil {
			err = resource.RecordSource(lineage.scope.ctx, source)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := lineage.action(t, lineage.store, resource, workspace.OperationProvision); err != nil {
		t.Fatal(err)
	}
	common := filepath.Join(lineage.main, ".git")
	if err := os.Rename(common, common+"-moved"); err != nil {
		t.Fatal(err)
	}
	lineage.gitIn(t, lineage.main, "init", "--quiet")
	operation := lineage.captureOperation(t, resource, lineageOperationSubject("changed common"))
	_, err := lineage.runOperation(operation, os.Environ())
	if !errors.Is(err, workspace.ErrIdentity) && !errors.Is(err, workspace.ErrIdentityMismatch) {
		t.Fatalf("capture against a replaced common directory error = %v", err)
	}
	if _, err = resource.CaptureCandidate(lineage.scope.ctx, operation.Request().OperationID); !workspace.OperationAbsent(err) {
		t.Fatalf("refused capture recorded a candidate: %v", err)
	}
}

func TestNativeCaptureKeepsCumulativeBaseAndLeavesRealIndexAndHEAD(t *testing.T) {
	lineage := newNativeLineage(t)
	resource, registration := lineage.provisionedChild(t, lineage.main)
	checkout := registration.CheckoutPath
	writeLineageFile(t, filepath.Join(checkout, "first.txt"), "first worker commit\n")
	lineage.gitIn(t, checkout, "add", "first.txt")
	lineage.commit(t, checkout, "worker checkpoint one")
	writeLineageFile(t, filepath.Join(checkout, "second.txt"), "second worker commit\n")
	lineage.gitIn(t, checkout, "add", "second.txt")
	head := lineage.commit(t, checkout, "worker checkpoint two")
	writeLineageFile(t, filepath.Join(checkout, "tracked.txt"), "dirty tracked edit\n")
	writeLineageFile(t, filepath.Join(checkout, "staged.txt"), "staged addition\n")
	lineage.gitIn(t, checkout, "add", "staged.txt")
	writeLineageFile(t, filepath.Join(checkout, "untracked.txt"), "untracked addition\n")
	if err := os.WriteFile(filepath.Join(checkout, "binary.bin"), []byte{0, 1, 2, 0, 255, 0}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(checkout, "removed.txt")); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(registration.AdminDir, "index")
	indexBefore, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	candidate := lineage.capture(t, resource, lineageOperationSubject("cumulative"))
	indexAfter, err := os.ReadFile(indexPath)
	if err != nil || !bytes.Equal(indexBefore, indexAfter) || lineage.gitIn(t, checkout, "rev-parse", "HEAD") != head {
		t.Fatalf("capture changed the real index or HEAD: %v", err)
	}
	if candidate.ObservedHEAD != head || candidate.BaseOID != registration.BaseOID || candidate.IndexSHA256 == "" {
		t.Fatalf("candidate = %+v, want worker HEAD over original base with index identity", candidate)
	}
	changes := lineage.gitIn(t, checkout, "diff", "--name-status", "--no-renames", candidate.BaseOID, candidate.TreeOID)
	for _, want := range []string{"A\tfirst.txt", "A\tsecond.txt", "M\ttracked.txt", "A\tstaged.txt", "A\tuntracked.txt", "A\tbinary.bin", "D\tremoved.txt"} {
		if !strings.Contains(changes, want) {
			t.Fatalf("cumulative capture lacks %q:\n%s", want, changes)
		}
	}
	patch, err := os.ReadFile(filepath.Join(workspace.ScratchPath(lineage.store.Root, candidate.OperationID), "diff.patch"))
	if err != nil || !bytes.Contains(patch, []byte("GIT binary patch")) {
		t.Fatalf("cumulative patch lost binary content: %v", err)
	}
}
