//go:build linux

package lifetime

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const cgroupPlanPrefix = "cgroup2-plan:"

type cgroupPlanLocator struct {
	parent string
	dev    uint64
	ino    uint64
	name   string
}

func preparePlatformPlan(rootID, ownerKind, taskID string) (Plan, error) {
	location, err := discoverCgroupLocation(procSelfCgroupPath, procSelfMountinfoPath)
	if err != nil {
		return processGroupFallback(rootID, ownerKind, taskID, err)
	}
	if controlErr := checkCgroupControls(location.currentPath); controlErr != nil {
		return processGroupFallback(rootID, ownerKind, taskID, controlErr)
	}
	parent, err := openCgroupDirectory(location.currentPath)
	if err != nil {
		return processGroupFallback(rootID, ownerKind, taskID, err)
	}
	dev, ino, identityErr := cgroupIdentity(parent)
	closeErr := parent.Close()
	if identityErr != nil || closeErr != nil {
		return Plan{}, errors.Join(identityErr, closeErr)
	}
	name := "delegate-v1-" + deterministicScopeToken(rootID, ownerKind, taskID)
	identifier := encodeCgroupPlan(cgroupPlanLocator{parent: location.currentPath, dev: dev, ino: ino, name: name})
	return NewPlan(KindCgroupV2, AssuranceExact, identifier)
}

func processGroupFallback(rootID, ownerKind, taskID string, cgroupUnavailable error) (Plan, error) {
	if cgroupUnavailable == nil {
		return Plan{}, fmt.Errorf("%w: cgroup fallback has no cause", ErrInvalidRef)
	}
	if err := requireLinuxPidfd(); err != nil {
		return Plan{}, errors.Join(cgroupUnavailable, err)
	}
	return processGroupPlan(rootID, ownerKind, taskID), nil
}

func createPreparedPlatformScope(plan Plan) (*Scope, error) {
	if plan.kind == KindProcessGroup {
		if err := requireLinuxPidfd(); err != nil {
			return nil, err
		}
		ref, err := NewRef(plan.kind, plan.assurance, plan.identifier)
		if err != nil {
			return nil, err
		}
		return openProcessGroupScope(context.Background(), ref)
	}
	locator, parent, err := openPlannedCgroupParent(plan)
	if err != nil {
		return nil, err
	}
	if err = unix.Mkdirat(int(parent.Fd()), locator.name, 0o700); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return nil, closePlannedCgroupParent(parent, fmt.Errorf("%w: planned cgroup already exists", ErrScopeExists))
		}
		return nil, closePlannedCgroupParent(parent, fmt.Errorf("create planned cgroup: %w", err))
	}
	child, err := openCgroupDirectoryAt(parent, locator.name)
	if err != nil {
		return nil, closePlannedCgroupParent(parent, discardPreparedCgroup(parent, locator.name, nil, err))
	}
	path := filepath.Join(locator.parent, locator.name)
	if controlErr := checkCgroupControlsAt(child); controlErr != nil {
		return nil, closePlannedCgroupParent(parent, discardPreparedCgroup(parent, locator.name, child, controlErr))
	}
	dev, ino, identityErr := cgroupIdentity(child)
	if identityErr != nil {
		return nil, closePlannedCgroupParent(parent, discardPreparedCgroup(parent, locator.name, child, identityErr))
	}
	ref, refErr := NewRef(KindCgroupV2, AssuranceExact, encodeCgroupIdentifier(path, dev, ino))
	if refErr != nil {
		return nil, closePlannedCgroupParent(parent, discardPreparedCgroup(parent, locator.name, child, refErr))
	}
	scope := newScope(ref, &cgroupScope{path: path, file: child})
	if closeErr := parent.Close(); closeErr != nil {
		return nil, errors.Join(
			fmt.Errorf("close planned cgroup parent: %w", closeErr),
			scope.DestroyEmpty(),
			scope.Release(),
		)
	}
	return scope, nil
}

func openPreparedPlatformScope(plan Plan) (*Scope, error) {
	if plan.kind == KindProcessGroup {
		return nil, ErrScopeAbsent
	}
	locator, parent, err := openPlannedCgroupParent(plan)
	if err != nil {
		return nil, err
	}
	child, err := openCgroupDirectoryAt(parent, locator.name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOENT) {
			return nil, closePlannedCgroupParent(parent, ErrScopeAbsent)
		}
		return nil, closePlannedCgroupParent(parent, fmt.Errorf("open planned cgroup: %w", err))
	}
	path := filepath.Join(locator.parent, locator.name)
	if controlErr := checkCgroupControlsAt(child); controlErr != nil {
		return nil, closePlannedCgroupParent(parent, errors.Join(fmt.Errorf("inspect planned cgroup controls: %w", controlErr), child.Close()))
	}
	dev, ino, identityErr := cgroupIdentity(child)
	if identityErr != nil {
		return nil, closePlannedCgroupParent(parent, errors.Join(identityErr, child.Close()))
	}
	ref, refErr := NewRef(KindCgroupV2, AssuranceExact, encodeCgroupIdentifier(path, dev, ino))
	if refErr != nil {
		return nil, closePlannedCgroupParent(parent, errors.Join(refErr, child.Close()))
	}
	scope := newScope(ref, &cgroupScope{path: path, file: child})
	if closeErr := parent.Close(); closeErr != nil {
		return nil, errors.Join(fmt.Errorf("close planned cgroup parent: %w", closeErr), scope.Release())
	}
	return scope, nil
}

func openRegisteredPlatformScope(ctx context.Context, plan Plan, ref Ref) (*Scope, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if plan.kind == KindProcessGroup {
		return openRegisteredProcessGroupScope(ctx, plan, ref)
	}
	return openRegisteredCgroupScope(plan, ref)
}

func openRegisteredProcessGroupScope(ctx context.Context, plan Plan, ref Ref) (*Scope, error) {
	if ref.kind != plan.kind || ref.assurance != plan.assurance || ref.identifier != plan.identifier {
		return nil, ErrInvalidRef
	}
	if err := requireLinuxPidfd(); err != nil {
		return nil, err
	}
	return openProcessGroupScope(ctx, ref)
}

func openRegisteredCgroupScope(plan Plan, ref Ref) (*Scope, error) {
	if ref.kind != KindCgroupV2 || ref.assurance != AssuranceExact {
		return nil, ErrInvalidRef
	}
	locator, parent, err := openPlannedCgroupParent(plan)
	if err != nil {
		return nil, err
	}
	path, wantDev, wantIno, err := decodeCgroupIdentifier(ref.identifier)
	if err != nil || path != filepath.Join(locator.parent, locator.name) {
		return nil, closePlannedCgroupParent(parent, errors.Join(ErrInvalidRef, err))
	}
	child, err := openRegisteredCgroupChild(parent, locator, path, wantDev, wantIno)
	if err != nil {
		return nil, closePlannedCgroupParent(parent, err)
	}
	scope := newScope(ref, &cgroupScope{path: path, file: child})
	if closeErr := parent.Close(); closeErr != nil {
		return nil, errors.Join(fmt.Errorf("close registered cgroup parent: %w", closeErr), scope.Release())
	}
	return scope, nil
}

func openRegisteredCgroupChild(parent *os.File, locator cgroupPlanLocator, path string, wantDev, wantIno uint64) (*os.File, error) {
	child, err := openCgroupDirectoryAt(parent, locator.name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOENT) {
			return nil, ErrScopeAbsent
		}
		return nil, fmt.Errorf("open registered cgroup: %w", err)
	}
	dev, ino, identityErr := cgroupIdentity(child)
	if identityErr != nil || dev != wantDev || ino != wantIno {
		return nil, errors.Join(fmt.Errorf("%w: registered cgroup identity changed", ErrInvalidRef), identityErr, child.Close())
	}
	if controlErr := checkCgroupControlsAt(child); controlErr != nil {
		return nil, errors.Join(fmt.Errorf("registered cgroup controls: %w", controlErr), child.Close())
	}
	return child, nil
}

func validatePlatformPlan(plan Plan) error {
	_, err := decodeCgroupPlan(plan.identifier)
	return err
}

func openPlannedCgroupParent(plan Plan) (cgroupPlanLocator, *os.File, error) {
	locator, err := decodeCgroupPlan(plan.identifier)
	if err != nil {
		return cgroupPlanLocator{}, nil, err
	}
	location, err := discoverCgroupLocation(procSelfCgroupPath, procSelfMountinfoPath)
	if err != nil {
		return cgroupPlanLocator{}, nil, fmt.Errorf("discover cgroup mount for plan: %w", err)
	}
	if err = validatePlannedCgroupParent(location.mountPath, locator); err != nil {
		return cgroupPlanLocator{}, nil, fmt.Errorf("%w: planned parent is outside cgroup mount", ErrInvalidRef)
	}
	parent, err := openCgroupDirectory(locator.parent)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cgroupPlanLocator{}, nil, ErrScopeAbsent
		}
		return cgroupPlanLocator{}, nil, fmt.Errorf("open planned cgroup parent: %w", err)
	}
	dev, ino, identityErr := cgroupIdentity(parent)
	if identityErr != nil || dev != locator.dev || ino != locator.ino {
		return cgroupPlanLocator{}, nil, errors.Join(fmt.Errorf("%w: planned cgroup parent identity changed", ErrInvalidRef), identityErr, parent.Close())
	}
	return locator, parent, nil
}

func validatePlannedCgroupParent(mountPath string, locator cgroupPlanLocator) error {
	if !pathWithin(mountPath, locator.parent) {
		return ErrInvalidRef
	}
	return nil
}

func openCgroupDirectoryAt(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		return nil, errors.Join(errors.New("create planned cgroup file handle"), unix.Close(fd))
	}
	return file, nil
}

func discardPreparedCgroup(parent *os.File, name string, child *os.File, cause error) error {
	var closeErr error
	if child != nil {
		closeErr = child.Close()
	}
	removeErr := unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR)
	return errors.Join(cause, closeErr, removeErr)
}

func closePlannedCgroupParent(parent *os.File, cause error) error {
	return errors.Join(cause, parent.Close())
}

func encodeCgroupPlan(locator cgroupPlanLocator) string {
	parent := base64.RawURLEncoding.EncodeToString([]byte(locator.parent))
	return cgroupPlanPrefix + parent + ":" + strconv.FormatUint(locator.dev, 10) + ":" + strconv.FormatUint(locator.ino, 10) + ":" + locator.name
}

func decodeCgroupPlan(identifier string) (cgroupPlanLocator, error) {
	parts := strings.Split(identifier, ":")
	if len(parts) != 5 || parts[0]+":" != cgroupPlanPrefix || parts[1] == "" {
		return cgroupPlanLocator{}, fmt.Errorf("%w: cgroup plan is malformed", ErrInvalidRef)
	}
	parent, err := decodeCgroupPlanParent(parts[1])
	if err != nil {
		return cgroupPlanLocator{}, err
	}
	dev, err := parseCgroupPlanIdentity("device", parts[2], false)
	if err != nil {
		return cgroupPlanLocator{}, err
	}
	ino, err := parseCgroupPlanIdentity("inode", parts[3], true)
	if err != nil {
		return cgroupPlanLocator{}, err
	}
	name := parts[4]
	if err = validateCgroupPlanName(name); err != nil {
		return cgroupPlanLocator{}, err
	}
	return cgroupPlanLocator{parent: parent, dev: dev, ino: ino, name: name}, nil
}

func decodeCgroupPlanParent(encoded string) (string, error) {
	parentBytes, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("%w: cgroup plan parent is malformed", ErrInvalidRef)
	}
	parent := string(parentBytes)
	if parent == "" || !filepath.IsAbs(parent) || filepath.Clean(parent) != parent || strings.ContainsRune(parent, '\x00') {
		return "", fmt.Errorf("%w: cgroup plan parent is invalid", ErrInvalidRef)
	}
	return parent, nil
}

func parseCgroupPlanIdentity(name, value string, positive bool) (uint64, error) {
	identity, err := strconv.ParseUint(value, 10, 64)
	if err != nil || (positive && identity == 0) {
		return 0, fmt.Errorf("%w: cgroup plan %s is invalid", ErrInvalidRef, name)
	}
	return identity, nil
}

func validateCgroupPlanName(name string) error {
	const prefix = "delegate-v1-"
	if !strings.HasPrefix(name, prefix) || len(name) != len(prefix)+32 || filepath.Base(name) != name {
		return fmt.Errorf("%w: cgroup plan name is invalid", ErrInvalidRef)
	}
	for _, char := range name[len(prefix):] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return fmt.Errorf("%w: cgroup plan name is invalid", ErrInvalidRef)
		}
	}
	return nil
}
