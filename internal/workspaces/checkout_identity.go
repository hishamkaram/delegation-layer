package workspaces

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"

	"github.com/hishamkaram/delegation-layer/internal/workspace"

	"golang.org/x/sys/unix"
)

func RegisteredCheckout(ctx context.Context, resource *workspace.Resource) (workspace.Checkout, error) {
	registration, err := resource.Registration(ctx)
	if err != nil {
		return workspace.Checkout{}, err
	}
	source, err := resource.Source(ctx)
	if err != nil {
		return workspace.Checkout{}, err
	}
	if source.Root != registration.SourceRoot || source.CommonDir != registration.CommonDir || source.GitDir != registration.GitDir || source.BaseOID != registration.BaseOID || source.ObjectFormat != registration.ObjectFormat || source.RelativeCwd != registration.RelativeCwd {
		return workspace.Checkout{}, task.ErrIdentityMismatch
	}
	anchor := filepath.Join(resource.Request().Container, resource.Request().WorkspaceID)
	if registration.CheckoutPath != filepath.Join(anchor, "checkout") {
		return workspace.Checkout{}, workspace.ErrIdentity
	}
	if err = VerifyDirectoryIdentity(anchor, registration.ContainerIdentity); err != nil {
		return workspace.Checkout{}, err
	}
	if err = VerifyMarker(resource, anchor); err != nil {
		return workspace.Checkout{}, err
	}
	if err = VerifyDirectoryIdentity(registration.CheckoutPath, registration.CheckoutIdentity); err != nil {
		return workspace.Checkout{}, err
	}
	return workspace.Checkout{Source: source, CheckoutPath: registration.CheckoutPath, Workdir: registration.CanonicalCwd, AdminDir: registration.AdminDir, AdminBacklink: registration.AdminBacklink}, nil
}

// Execution requires the exact mapped directory. Capture and removal instead
// inspect the checkout root so deleting a mapped subdirectory remains capturable.
func RegisteredExecutionCheckout(ctx context.Context, resource *workspace.Resource) (workspace.Checkout, error) {
	checkout, err := RegisteredCheckout(ctx, resource)
	if err != nil {
		return checkout, err
	}
	if err = VerifyCanonicalDirectory(checkout.Workdir); err != nil {
		return checkout, errors.Join(workspace.ErrIdentity, err)
	}
	relative, err := filepath.Rel(checkout.CheckoutPath, checkout.Workdir)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return checkout, errors.Join(workspace.ErrIdentity, err)
	}
	return checkout, nil
}

func VerifyCanonicalDirectory(path string) error {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if canonical != path {
		return workspace.ErrIdentity
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return workspace.ErrIdentity
	}
	return nil
}

func DirectoryIdentity(path string) (workspace.DirectoryIdentity, error) {
	if err := VerifyCanonicalDirectory(path); err != nil {
		return workspace.DirectoryIdentity{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return workspace.DirectoryIdentity{}, err
	}
	var stat unix.Stat_t
	statErr := unix.Fstat(int(file.Fd()), &stat)
	if err = errors.Join(statErr, file.Close()); err != nil {
		return workspace.DirectoryIdentity{}, err
	}
	return workspace.DirectoryIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}, nil
}

func VerifyDirectoryIdentity(path string, expected workspace.DirectoryIdentity) error {
	current, err := DirectoryIdentity(path)
	if err != nil {
		return err
	}
	if current != expected {
		return workspace.ErrIdentity
	}
	return nil
}

func VerifyMarker(resource *workspace.Resource, anchor string) error {
	target, err := os.Readlink(filepath.Join(anchor, ".delegate-workspace"))
	if err != nil {
		return err
	}
	request := resource.Request()
	if target != filepath.Join(resource.StateRoot(), "workspaces", request.WorkspaceID) {
		return workspace.ErrIdentity
	}
	return nil
}

func RejectUnmanagedPath(cwd string) error {
	for path := cwd; path != filepath.Dir(path); path = filepath.Dir(path) {
		marker := filepath.Join(path, ".delegate-workspace")
		info, err := os.Lstat(marker)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("managed workspace requires --workspace ID with its owning state root")
		}
	}
	return nil
}
