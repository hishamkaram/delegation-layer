package app

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func authorizeAppWorkspaceRemoval(t *testing.T, resource *workspace.Resource) {
	t.Helper()
	if err := resource.RecordRelease(t.Context(), workspace.ReleaseRecord{SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: strings.Repeat("d", 32), Discard: true}); err != nil {
		t.Fatal(err)
	}
}

func TestUnprovisionedWorkspaceRemovalRequiresNoGit(t *testing.T) {
	_, resource := newAppWorkspaceResource(t)
	authorizeAppWorkspaceRemoval(t, resource)
	// A zero Git has no executable runner. Any Git command would fail.
	if err := removeManagedCheckout(t.Context(), resource, workspace.Git{}); err != nil {
		t.Fatalf("source-absent cleanup required Git: %v", err)
	}
}

func TestSourceOnlyWorkspaceRemovalDoesNotAdoptFilesystem(t *testing.T) {
	_, resource := newAppWorkspaceResource(t)
	request := resource.Request()
	source := workspace.Source{Root: request.SourceCwd, CommonDir: filepath.Join(request.SourceCwd, ".git"), GitDir: filepath.Join(request.SourceCwd, ".git"), BaseOID: strings.Repeat("e", 40), ObjectFormat: "sha1", RelativeCwd: "."}
	if err := resource.RecordSource(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	authorizeAppWorkspaceRemoval(t, resource)
	if err := removeManagedCheckout(t.Context(), resource, workspace.Git{}); err != nil {
		t.Fatalf("pre-anchor cleanup performed native Git work: %v", err)
	}
}

func TestWorkspaceRemovalRequiresLeadAuthorization(t *testing.T) {
	_, resource := newAppWorkspaceResource(t)
	if err := removeManagedCheckout(t.Context(), resource, workspace.Git{}); !workspace.OperationAbsent(err) {
		t.Fatalf("cleanup accepted missing release authority: %v", err)
	}
}

func TestUnprovisionedRemovalRejectsContradictoryAnchor(t *testing.T) {
	_, resource := newAppWorkspaceResource(t)
	authorizeAppWorkspaceRemoval(t, resource)
	if err := resource.RecordAnchor(t.Context(), workspace.AnchorRecord{SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, Path: filepath.Join(resource.Request().Container, resource.Request().WorkspaceID), Identity: workspace.DirectoryIdentity{Inode: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := removeManagedCheckout(t.Context(), resource, workspace.Git{}); !errors.Is(err, workspace.ErrIdentity) {
		t.Fatalf("source-absent cleanup ignored contradictory later facts: %v", err)
	}
}

func TestWorkspaceSourceRetryRejectsSurvivingAnchor(t *testing.T) {
	_, resource := newAppWorkspaceResource(t)
	request := resource.Request()
	if err := resource.RecordAnchor(t.Context(), workspace.AnchorRecord{
		SchemaVersion: task.SchemaVersion, WorkspaceID: request.WorkspaceID,
		Path:     filepath.Join(request.Container, request.WorkspaceID),
		Identity: workspace.DirectoryIdentity{Inode: 1},
	}); err != nil {
		t.Fatal(err)
	}
	// A surviving anchor proves selection already happened. No new Git
	// resolution is allowed, even if the requested branch has since moved.
	if _, err := resolveManagedSource(t.Context(), resource, workspace.Git{}); !errors.Is(err, workspace.ErrIdentity) {
		t.Fatalf("source-absent retry did not reject its later anchor before Git: %v", err)
	}
	if _, err := resource.Source(t.Context()); !workspace.OperationAbsent(err) {
		t.Fatalf("source-absent retry published another selection: %v", err)
	}
}
