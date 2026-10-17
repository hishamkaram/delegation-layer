package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
	"golang.org/x/sys/unix"
)

func runWorkspaceAction(scope execution.PreflightScope, store *taskdir.Store, operation *workspace.Operation) (candidate workspace.OperationResult, resultErr error) {
	request := operation.Request()
	candidate = workspace.OperationResult{SchemaVersion: task.SchemaVersion, OperationID: request.OperationID, RequestSHA256: operation.Digest(), Status: "succeeded"}
	resource, err := workspace.LoadResource(scope.Context(), store, request.WorkspaceID)
	if err != nil {
		return candidate, err
	}
	defer func() { resultErr = errors.Join(resultErr, resource.Close()) }()
	if err = workspacescore.ValidateActionRequest(scope.Context(), resource, request); err != nil {
		return candidate, err
	}
	git := workspace.Git{Runner: workspaceCommandRunner{scope: scope}}
	switch request.Kind {
	case workspace.OperationProvision:
		resultErr = provisionManagedCheckout(scope.Context(), store, resource, git)
	case workspace.OperationCapture:
		candidate.Artifacts, resultErr = captureManagedCheckout(scope.Context(), store, resource, operation, git)
	case workspace.OperationVerify:
		candidate.CommandExit, candidate.Artifacts, resultErr = verifyManagedCheckout(scope, store, resource, request)
		if resultErr == nil && candidate.CommandExit != nil && *candidate.CommandExit < 0 {
			candidate.Status, candidate.ReasonCode, candidate.CommandExit = "failed", "verification_command_signaled", nil
		}
	case workspace.OperationRemove:
		resultErr = removeManagedCheckout(scope.Context(), resource, git)
	case workspace.OperationForget:
		resultErr = forgetManagedWorkspace(scope.Context(), store, resource, git)
	default:
		resultErr = task.ErrIdentityMismatch
	}
	return candidate, resultErr
}

func provisionManagedCheckout(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, git workspace.Git) error {
	request := resource.Request()
	source, err := resolveManagedSource(ctx, resource, git)
	if err != nil {
		return err
	}
	if config.PathsOverlap(request.Container, source.Root) || config.PathsOverlap(request.Container, source.CommonDir) || config.PathsOverlap(request.Container, store.Root) {
		return fmt.Errorf("%w: managed container overlaps the source or state root", workspace.ErrIdentity)
	}
	anchor, err := prepareManagedAnchor(ctx, store.Root, resource)
	if err != nil {
		return err
	}
	if err = git.PinBase(ctx, source, workspacescore.ManagedBaseRef(request.RootID, request.WorkspaceID)); err != nil {
		return err
	}
	checkout, err := provisionOrInspectCheckout(ctx, git, source, filepath.Join(anchor, "checkout"))
	if err != nil {
		return err
	}
	if err = resource.RecordCheckout(ctx, checkout); err != nil {
		return err
	}
	return recordManagedRegistration(ctx, resource, checkout)
}

func resolveManagedSource(ctx context.Context, resource *workspace.Resource, git workspace.Git) (workspace.Source, error) {
	request := resource.Request()
	source, err := resource.Source(ctx)
	if workspace.OperationAbsent(err) {
		if err = requireUnprovisionedWorkspace(ctx, resource, true); err != nil {
			return source, err
		}
		source, err = git.ResolveSource(ctx, request.SourceCwd, request.BaseRef)
		if err == nil {
			err = resource.RecordSource(ctx, source)
		}
	}
	return source, err
}

func prepareManagedAnchor(ctx context.Context, root string, resource *workspace.Resource) (string, error) {
	request := resource.Request()
	anchor := filepath.Join(request.Container, request.WorkspaceID)
	if err := os.MkdirAll(anchor, 0o700); err != nil {
		return "", err
	}
	if err := workspacescore.VerifyCanonicalDirectory(anchor); err != nil {
		return "", err
	}
	if err := ensureWorkspaceMarker(root, request.WorkspaceID, anchor); err != nil {
		return "", err
	}
	anchorRecord, err := resource.Anchor(ctx)
	if workspace.OperationAbsent(err) {
		identity, identityErr := workspacescore.DirectoryIdentity(anchor)
		if identityErr != nil {
			return "", identityErr
		}
		anchorRecord = workspace.AnchorRecord{SchemaVersion: task.SchemaVersion, WorkspaceID: request.WorkspaceID, Path: anchor, Identity: identity}
		err = resource.RecordAnchor(ctx, anchorRecord)
	}
	if err != nil {
		return "", err
	}
	if err = workspacescore.VerifyDirectoryIdentity(anchor, anchorRecord.Identity); err != nil {
		return "", err
	}
	return anchor, nil
}

func provisionOrInspectCheckout(ctx context.Context, git workspace.Git, source workspace.Source, checkoutPath string) (workspace.Checkout, error) {
	if _, statErr := os.Lstat(checkoutPath); statErr == nil {
		return git.InspectProvisioned(ctx, source, checkoutPath)
	} else if errors.Is(statErr, os.ErrNotExist) {
		return git.Provision(ctx, source, checkoutPath)
	} else {
		return workspace.Checkout{}, statErr
	}
}

func recordManagedRegistration(ctx context.Context, resource *workspace.Resource, checkout workspace.Checkout) error {
	request, source := resource.Request(), checkout.Source
	anchor, err := resource.Anchor(ctx)
	if err != nil {
		return err
	}
	if err = workspacescore.VerifyDirectoryIdentity(anchor.Path, anchor.Identity); err != nil {
		return err
	}
	checkoutIdentity, err := workspacescore.DirectoryIdentity(checkout.CheckoutPath)
	if err != nil {
		return err
	}
	return resource.RecordRegistration(ctx, workspace.Registration{
		SchemaVersion: task.SchemaVersion, WorkspaceID: request.WorkspaceID,
		SourceRoot: source.Root, CommonDir: source.CommonDir, GitDir: source.GitDir,
		AdminDir: checkout.AdminDir, AdminBacklink: checkout.AdminBacklink,
		CheckoutPath: checkout.CheckoutPath, CanonicalCwd: checkout.Workdir,
		RelativeCwd: source.RelativeCwd, BaseOID: source.BaseOID, ObjectFormat: source.ObjectFormat,
		BaseRef: workspacescore.ManagedBaseRef(request.RootID, request.WorkspaceID), SourceChangesExcluded: source.Dirty,
		ContainerIdentity: anchor.Identity, CheckoutIdentity: checkoutIdentity,
	})
}

func captureManagedCheckout(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, operation *workspace.Operation, git workspace.Git) ([]workspace.Artifact, error) {
	checkout, err := workspacescore.RegisteredCheckout(ctx, resource)
	if err != nil {
		return nil, err
	}
	request := operation.Request()
	scratch := workspace.ScratchPath(store.Root, request.OperationID)
	if err = os.MkdirAll(scratch, 0o700); err != nil {
		return nil, err
	}
	candidate, err := resource.CaptureCandidate(ctx, request.OperationID)
	if workspace.OperationAbsent(err) {
		candidate, err = git.BuildCapture(ctx, checkout, scratch)
		if err == nil {
			candidate.SchemaVersion, candidate.WorkspaceID, candidate.OperationID = task.SchemaVersion, request.WorkspaceID, request.OperationID
			candidate.Subject = request.Subject
			candidate.SnapshotRef = managedCaptureRef(request.RootID, request.WorkspaceID, request.OperationID)
			err = resource.RecordCaptureCandidate(ctx, candidate)
		}
	}
	if err != nil {
		return nil, err
	}
	if candidate.Subject != request.Subject {
		return nil, task.ErrIdentityMismatch
	}
	if err = git.PinCapture(ctx, checkout, candidate.SnapshotRef, candidate); err != nil {
		return nil, err
	}
	exported, err := git.ExportCapture(ctx, checkout, candidate, scratch)
	if err != nil {
		return nil, err
	}
	return []workspace.Artifact{captureArtifact("diff.patch", exported.Patch), captureArtifact("name-status.z", exported.NameStatus)}, nil
}

func captureArtifact(name string, artifact workspace.CaptureArtifact) workspace.Artifact {
	if artifact.OmittedReason != "" {
		return workspace.Artifact{Name: name, Omitted: artifact.OmittedReason}
	}
	return workspace.Artifact{Name: filepath.Base(artifact.Path), Length: artifact.Length, SHA256: artifact.SHA256}
}

func removeManagedCheckout(ctx context.Context, resource *workspace.Resource, git workspace.Git) (resultErr error) {
	if _, err := resource.Release(ctx); err != nil {
		return err
	}
	source, err := resource.Source(ctx)
	if workspace.OperationAbsent(err) {
		return requireUnprovisionedWorkspace(ctx, resource, true)
	}
	if err != nil {
		return err
	}
	anchor, err := resource.Anchor(ctx)
	if workspace.OperationAbsent(err) {
		return requireUnprovisionedWorkspace(ctx, resource, false)
	}
	if err != nil {
		return err
	}
	parent, err := pinManagedDirectory(anchor.Path, anchor.Identity)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, parent.Close()) }()
	if err = workspacescore.VerifyMarker(resource, anchor.Path); err != nil {
		return err
	}
	checkout, err := recoverWorkspaceCleanupTarget(ctx, resource, git, source, anchor)
	if err != nil {
		return err
	}
	if _, statErr := os.Lstat(checkout.CheckoutPath); errors.Is(statErr, os.ErrNotExist) {
		return git.VerifyRemoved(ctx, checkout)
	} else if statErr != nil {
		return statErr
	}
	identity, err := workspaceCleanupIdentity(ctx, resource, checkout.CheckoutPath)
	if err != nil {
		return err
	}
	checkoutDirectory, err := pinManagedDirectory(checkout.CheckoutPath, identity)
	if errors.Is(err, os.ErrNotExist) {
		return git.VerifyRemoved(ctx, checkout)
	}
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, checkoutDirectory.Close()) }()
	return git.Remove(ctx, checkout)
}

func workspaceCleanupIdentity(ctx context.Context, resource *workspace.Resource, path string) (workspace.DirectoryIdentity, error) {
	registration, err := resource.Registration(ctx)
	if workspace.OperationAbsent(err) {
		// Interrupted initial provisioning has no published checkout inode.
		// Pin the current native-inspected checkout for this removal; the
		// durable Anchor and Checkout still supply its ownership coordinates.
		return workspacescore.DirectoryIdentity(path)
	}
	if err != nil {
		return workspace.DirectoryIdentity{}, err
	}
	return registration.CheckoutIdentity, nil
}

func requireUnprovisionedWorkspace(ctx context.Context, resource *workspace.Resource, sourceAbsent bool) error {
	if _, err := resource.Checkout(ctx); !workspace.OperationAbsent(err) {
		return errors.Join(workspace.ErrIdentity, err)
	}
	if _, err := resource.Registration(ctx); !workspace.OperationAbsent(err) {
		return errors.Join(workspace.ErrIdentity, err)
	}
	if sourceAbsent {
		if _, err := resource.Anchor(ctx); !workspace.OperationAbsent(err) {
			return errors.Join(workspace.ErrIdentity, err)
		}
	}
	// Source and Anchor are durable before any Git mutation. An unrecorded
	// directory is deliberately left untouched; it is not adopted for removal.
	return nil
}

func recoverWorkspaceCleanupTarget(ctx context.Context, resource *workspace.Resource, git workspace.Git, source workspace.Source, anchor workspace.AnchorRecord) (workspace.Checkout, error) {
	checkout, err := resource.Checkout(ctx)
	path := filepath.Join(anchor.Path, "checkout")
	if workspace.OperationAbsent(err) {
		if _, statErr := os.Lstat(path); errors.Is(statErr, os.ErrNotExist) {
			return workspace.Checkout{Source: source, CheckoutPath: path}, nil
		} else if statErr != nil {
			return checkout, statErr
		}
		checkout, err = git.InspectInterruptedProvision(ctx, source, path)
		if err == nil {
			err = resource.RecordCheckout(ctx, checkout)
		}
	}
	if err != nil {
		return checkout, err
	}
	if checkout.CheckoutPath != path || checkout.Source != source {
		return checkout, workspace.ErrIdentity
	}
	return checkout, nil
}

func managedCaptureRef(rootID, workspaceID, operationID string) string {
	return "refs/delegate/" + rootID + "/workspaces/" + workspaceID + "/captures/" + operationID
}

func pinManagedDirectory(path string, expected workspace.DirectoryIdentity) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if (workspace.DirectoryIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}) != expected {
		return nil, errors.Join(workspace.ErrIdentity, file.Close())
	}
	return file, nil
}

func ensureWorkspaceMarker(root, id, anchor string) error {
	target := filepath.Join(root, "workspaces", id)
	marker := filepath.Join(anchor, ".delegate-workspace")
	if err := os.Symlink(target, marker); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	winner, err := os.Readlink(marker)
	if err != nil {
		return err
	}
	if winner != target {
		return workspace.ErrIdentity
	}
	return taskdir.SyncDirectoryEntry(anchor)
}
