package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	testsupportcore "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
	"github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

func newMappedWorkspaceExecutionFixture(t *testing.T) (*workspace.Resource, string) {
	t.Helper()
	store, original := testsupport.NewResource(t)
	sourceRoot := filepath.Join(testsupportcore.CanonicalTempDir(t), "source")
	source := workspace.Source{Root: sourceRoot, CommonDir: filepath.Join(sourceRoot, ".git"), GitDir: filepath.Join(sourceRoot, ".git"), RelativeCwd: "nested", BaseOID: strings.Repeat("a", 40), ObjectFormat: "sha1"}
	request := original.Request()
	request.WorkspaceID, request.TaskID = strings.Repeat("d", 32), strings.Repeat("e", 32)
	request.SourceCwd = filepath.Join(sourceRoot, "nested")
	if err := os.MkdirAll(request.SourceCwd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(source.CommonDir, 0o700); err != nil {
		t.Fatal(err)
	}
	resource, err := workspace.EstablishResource(t.Context(), store, request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := resource.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = resource.RecordSource(t.Context(), source); err != nil {
		t.Fatalf("record source: %v", err)
	}
	anchor := filepath.Join(request.Container, request.WorkspaceID)
	if err = os.MkdirAll(anchor, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join(store.Root, "workspaces", request.WorkspaceID), filepath.Join(anchor, ".delegate-workspace")); err != nil {
		t.Fatal(err)
	}
	anchorIdentity, err := workspaces.DirectoryIdentity(anchor)
	if err != nil {
		t.Fatal(err)
	}
	if err = resource.RecordAnchor(t.Context(), workspace.AnchorRecord{SchemaVersion: task.SchemaVersion, WorkspaceID: request.WorkspaceID, Path: anchor, Identity: anchorIdentity}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(anchor, "checkout")
	checkout := workspace.Checkout{Source: source, CheckoutPath: path, Workdir: filepath.Join(path, "nested"), AdminDir: filepath.Join(source.CommonDir, "worktrees", "fixture"), AdminBacklink: filepath.Join(source.CommonDir, "worktrees", "fixture")}
	if err = os.MkdirAll(checkout.Workdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = resource.RecordCheckout(t.Context(), checkout); err != nil {
		t.Fatalf("record checkout: %v", err)
	}
	checkoutIdentity, err := workspaces.DirectoryIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = resource.RecordRegistration(t.Context(), workspace.Registration{
		SchemaVersion: task.SchemaVersion, WorkspaceID: request.WorkspaceID,
		SourceRoot: source.Root, CommonDir: source.CommonDir, GitDir: source.GitDir,
		AdminDir: checkout.AdminDir, AdminBacklink: checkout.AdminBacklink,
		CheckoutPath: path, CanonicalCwd: checkout.Workdir,
		RelativeCwd: source.RelativeCwd, BaseOID: source.BaseOID, ObjectFormat: source.ObjectFormat,
		BaseRef:           workspaces.ManagedBaseRef(request.RootID, request.WorkspaceID),
		ContainerIdentity: anchorIdentity, CheckoutIdentity: checkoutIdentity,
	}); err != nil {
		t.Fatalf("record registration: %v", err)
	}
	return resource, checkout.Workdir
}

func TestWorkspaceExecutionRejectsEscapedMappedDirectory(t *testing.T) {
	resource, cwd := newMappedWorkspaceExecutionFixture(t)
	if _, err := workspaces.RegisteredExecutionCheckout(t.Context(), resource); err != nil {
		t.Fatal(err)
	}
	outside := testsupportcore.CanonicalTempDir(t)
	if err := os.Remove(cwd); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, cwd); err != nil {
		t.Fatal(err)
	}
	if _, err := workspaces.RegisteredExecutionCheckout(t.Context(), resource); !errors.Is(err, workspace.ErrIdentity) {
		t.Fatalf("execution followed an external symlink in the retained checkout: %v", err)
	}
	if _, err := workspaces.RegisteredCheckout(t.Context(), resource); err != nil {
		t.Fatalf("checkout-root evidence was lost with its mapped cwd: %v", err)
	}
}

func TestWorkspaceExecutionRequiresCwdWhileCaptureRetainsDeletedMapping(t *testing.T) {
	resource, cwd := newMappedWorkspaceExecutionFixture(t)
	if err := os.Remove(cwd); err != nil {
		t.Fatal(err)
	}
	if _, err := workspaces.RegisteredExecutionCheckout(t.Context(), resource); !errors.Is(err, workspace.ErrIdentity) {
		t.Fatalf("execution accepted a removed mapped cwd: %v", err)
	}
	if _, err := workspaces.RegisteredCheckout(t.Context(), resource); err != nil {
		t.Fatalf("a removed mapped cwd blocked capture/removal of its owned root: %v", err)
	}
}
