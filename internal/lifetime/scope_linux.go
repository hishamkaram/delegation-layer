//go:build linux

package lifetime

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	procSelfCgroupPath    = "/proc/self/cgroup"
	procSelfMountinfoPath = "/proc/self/mountinfo"
	cgroupEventsName      = "cgroup.events"
	cgroupKillName        = "cgroup.kill"
)

type cgroupMount struct {
	root      string
	mountPath string
}

type cgroupLocation struct {
	mountRoot   string
	mountPath   string
	currentPath string
}

type cgroupScope struct {
	path string
	file *os.File
}

func openPlatformScope(ctx context.Context, ref Ref) (*Scope, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch ref.kind {
	case KindCgroupV2:
		return openCgroupScope(ref)
	case KindProcessGroup:
		if err := requireLinuxPidfd(); err != nil {
			return nil, err
		}
		return openProcessGroupScope(ctx, ref)
	default:
		return nil, fmt.Errorf("%w: unknown scope kind %q", ErrInvalidRef, ref.kind)
	}
}

func openCgroupScope(ref Ref) (*Scope, error) {
	path, wantDev, wantIno, err := decodeCgroupIdentifier(ref.identifier)
	if err != nil {
		return nil, err
	}
	location, err := discoverCgroupLocation(procSelfCgroupPath, procSelfMountinfoPath)
	if err != nil {
		return nil, fmt.Errorf("%w: discover current cgroup: %w", ErrInvalidRef, err)
	}
	if path == location.currentPath || !pathWithin(location.currentPath, path) || !pathWithin(location.mountPath, path) {
		return nil, fmt.Errorf("%w: cgroup reference is outside the current delegated subtree", ErrInvalidRef)
	}
	file, err := openCgroupDirectory(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: cgroup scope %q: %w", ErrScopeAbsent, path, err)
		}
		return nil, fmt.Errorf("%w: open cgroup scope: %w", ErrInvalidRef, err)
	}
	dev, ino, identityErr := cgroupIdentity(file)
	if identityErr != nil {
		closeErr := file.Close()
		return nil, errors.Join(fmt.Errorf("%w: inspect cgroup identity: %w", ErrInvalidRef, identityErr), closeErr)
	}
	if dev != wantDev || ino != wantIno {
		closeErr := file.Close()
		return nil, errors.Join(fmt.Errorf("%w: cgroup identity changed", ErrInvalidRef), closeErr)
	}
	if controlErr := checkCgroupControlsAt(file); controlErr != nil {
		closeErr := file.Close()
		return nil, errors.Join(fmt.Errorf("%w: cgroup controls are unavailable: %w", ErrInvalidRef, controlErr), closeErr)
	}
	return newScope(ref, &cgroupScope{path: path, file: file}), nil
}

func (s *cgroupScope) configure(cmd *exec.Cmd) error {
	if cmd == nil {
		return fmt.Errorf("%w: nil command", ErrInvalidRef)
	}
	if cmd.Process != nil || cmd.ProcessState != nil {
		return fmt.Errorf("%w: command has already started", ErrConflictingSysProc)
	}
	fd := int(s.file.Fd())
	attr := syscall.SysProcAttr{}
	if cmd.SysProcAttr != nil {
		attr = *cmd.SysProcAttr
		if attr.UseCgroupFD || attr.CgroupFD != 0 {
			if attr.UseCgroupFD && attr.CgroupFD == fd {
				return nil
			}
			return fmt.Errorf("%w: command already has cgroup containment", ErrConflictingSysProc)
		}
	}
	attr.UseCgroupFD = true
	attr.CgroupFD = fd
	cmd.SysProcAttr = &attr
	return nil
}

func (s *cgroupScope) observeStarted(pid int) (LaunchRef, error) {
	present, token, err := observeStartedProcessIdentity(pid)
	if err != nil {
		return LaunchRef{}, fmt.Errorf("observe cgroup launch identity: %w", err)
	}
	if !present {
		return LaunchRef{}, fmt.Errorf("%w: process %d is absent", ErrUnverifiableLaunch, pid)
	}
	memberPath, err := processCgroupPath(pid)
	if err != nil {
		return LaunchRef{}, fmt.Errorf("observe cgroup membership for PID %d: %w", pid, err)
	}
	if !pathWithin(s.path, memberPath) {
		return LaunchRef{}, fmt.Errorf("%w: PID %d is outside cgroup scope", ErrUnverifiableLaunch, pid)
	}
	return NewLaunchRef(pid, token, 0)
}

func (s *cgroupScope) terminate(ctx context.Context, launchRefs []LaunchRef) error {
	if err := validateCgroupLaunchRefs(launchRefs); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := writeCgroupKillAt(s.file); err != nil {
		return fmt.Errorf("terminate cgroup scope: %w", err)
	}
	if err := waitForEmpty(ctx, processScopePollInterval, func() (bool, error) {
		return cgroupEmptyAt(s.file)
	}, sleepContext); err != nil {
		return fmt.Errorf("wait for cgroup scope to empty: %w", err)
	}
	return nil
}

func (s *cgroupScope) empty(launchRefs []LaunchRef) (bool, error) {
	if err := validateCgroupLaunchRefs(launchRefs); err != nil {
		return false, err
	}
	return cgroupEmptyAt(s.file)
}

func (s *cgroupScope) release() error {
	return s.file.Close()
}

func (s *cgroupScope) destroyEmpty() error {
	empty, err := s.empty(nil)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if !empty {
		return ErrNotEmpty
	}
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove empty cgroup scope: %w", err)
	}
	return nil
}

func validateCgroupLaunchRefs(refs []LaunchRef) error {
	for _, ref := range refs {
		if err := validateLaunchRefShape(ref); err != nil || ref.pgid != 0 {
			return fmt.Errorf("%w: cgroup launch reference is invalid", ErrInvalidLaunchRef)
		}
	}
	return nil
}

func openCgroupDirectory(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		closeErr := unix.Close(fd)
		return nil, errors.Join(errors.New("create cgroup file handle"), closeErr)
	}
	return file, nil
}

func cgroupIdentity(file *os.File) (uint64, uint64, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return 0, 0, err
	}
	return uint64(stat.Dev), uint64(stat.Ino), nil
}

func encodeCgroupIdentifier(path string, dev, ino uint64) string {
	encodedPath := base64.RawURLEncoding.EncodeToString([]byte(path))
	return "cgroup2:" + encodedPath + ":" + strconv.FormatUint(dev, 10) + ":" + strconv.FormatUint(ino, 10)
}

func decodeCgroupIdentifier(identifier string) (string, uint64, uint64, error) {
	parts := strings.Split(identifier, ":")
	if len(parts) != 4 || parts[0] != "cgroup2" || parts[1] == "" {
		return "", 0, 0, fmt.Errorf("%w: cgroup identifier is malformed", ErrInvalidRef)
	}
	pathBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", 0, 0, fmt.Errorf("%w: cgroup path encoding is malformed", ErrInvalidRef)
	}
	path := string(pathBytes)
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, '\x00') {
		return "", 0, 0, fmt.Errorf("%w: cgroup path is malformed", ErrInvalidRef)
	}
	dev, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil {
		return "", 0, 0, fmt.Errorf("%w: cgroup device identity is malformed", ErrInvalidRef)
	}
	ino, err := strconv.ParseUint(parts[3], 10, 64)
	if err != nil || ino == 0 {
		return "", 0, 0, fmt.Errorf("%w: cgroup inode identity is malformed", ErrInvalidRef)
	}
	return path, dev, ino, nil
}

func checkCgroupControls(path string) error {
	directory, err := openCgroupDirectory(path)
	if err != nil {
		return err
	}
	return errors.Join(checkCgroupControlsAt(directory), directory.Close())
}

func cgroupEmpty(path string) (bool, error) {
	directory, err := openCgroupDirectory(path)
	if err != nil {
		return false, err
	}
	empty, emptyErr := cgroupEmptyAt(directory)
	return empty, errors.Join(emptyErr, directory.Close())
}

func checkCgroupControlsAt(directory *os.File) error {
	events, err := readCgroupControlAt(directory, cgroupEventsName)
	if err != nil {
		return err
	}
	if _, parseErr := parseCgroupEvents(events); parseErr != nil {
		return parseErr
	}
	kill, err := openCgroupControlAt(directory, cgroupKillName, unix.O_WRONLY)
	if err != nil {
		return err
	}
	return kill.Close()
}

func writeCgroupKillAt(directory *os.File) error {
	kill, err := openCgroupControlAt(directory, cgroupKillName, unix.O_WRONLY)
	if err != nil {
		return err
	}
	_, writeErr := io.WriteString(kill, "1")
	return errors.Join(writeErr, kill.Close())
}

func cgroupEmptyAt(directory *os.File) (bool, error) {
	data, err := readCgroupControlAt(directory, cgroupEventsName)
	if err != nil {
		return false, err
	}
	return parseCgroupEvents(data)
}

func readCgroupControlAt(directory *os.File, name string) ([]byte, error) {
	control, err := openCgroupControlAt(directory, name, unix.O_RDONLY)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(control)
	return data, errors.Join(readErr, control.Close())
}

func openCgroupControlAt(directory *os.File, name string, flags int) (*os.File, error) {
	if directory == nil || filepath.Base(name) != name || name == "." || name == "" {
		return nil, ErrInvalidRef
	}
	fd, err := unix.Openat(int(directory.Fd()), name, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		return nil, errors.Join(errors.New("create cgroup control handle"), unix.Close(fd))
	}
	return file, nil
}

func parseCgroupEvents(data []byte) (bool, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	seen := false
	populated := false
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			return false, errors.New("cgroup.events line is malformed")
		}
		value, err := strconv.ParseUint(fields[1], 10, 8)
		if err != nil || value > 1 {
			return false, errors.New("cgroup.events value is malformed")
		}
		if fields[0] != "populated" {
			continue
		}
		if seen {
			return false, errors.New("cgroup.events has duplicate populated field")
		}
		seen = true
		populated = value == 1
	}
	if err := scanner.Err(); err != nil {
		return false, err
	}
	if !seen {
		return false, errors.New("cgroup.events has no populated field")
	}
	return !populated, nil
}

func processCgroupPath(pid int) (string, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
	if err != nil {
		return "", err
	}
	mountInfo, err := os.ReadFile(procSelfMountinfoPath)
	if err != nil {
		return "", err
	}
	location, err := resolveCgroupLocation(data, mountInfo)
	if err != nil {
		return "", err
	}
	return location.currentPath, nil
}

func discoverCgroupLocation(cgroupPath, mountInfoPath string) (cgroupLocation, error) {
	cgroupData, err := os.ReadFile(cgroupPath)
	if err != nil {
		return cgroupLocation{}, err
	}
	mountData, err := os.ReadFile(mountInfoPath)
	if err != nil {
		return cgroupLocation{}, err
	}
	return resolveCgroupLocation(cgroupData, mountData)
}

func resolveCgroupLocation(cgroupData, mountInfoData []byte) (cgroupLocation, error) {
	cgroupPath, err := parseCgroupV2Path(cgroupData)
	if err != nil {
		return cgroupLocation{}, err
	}
	mounts, err := parseMountInfo(mountInfoData)
	if err != nil {
		return cgroupLocation{}, err
	}
	var selected cgroupLocation
	bestRootLength := -1
	for _, mount := range mounts {
		relative, ok := relativeCgroupPath(mount.root, cgroupPath)
		if !ok || len(mount.root) <= bestRootLength {
			continue
		}
		current := filepath.Clean(filepath.Join(mount.mountPath, relative))
		if !pathWithin(mount.mountPath, current) {
			continue
		}
		selected = cgroupLocation{mountRoot: mount.root, mountPath: mount.mountPath, currentPath: current}
		bestRootLength = len(mount.root)
	}
	if bestRootLength < 0 {
		return cgroupLocation{}, errors.New("no cgroup v2 mount contains the current cgroup")
	}
	return selected, nil
}

func parseCgroupV2Path(data []byte) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	var path string
	for scanner.Scan() {
		parts := strings.SplitN(scanner.Text(), ":", 3)
		if len(parts) != 3 || parts[0] != "0" || parts[1] != "" {
			continue
		}
		if path != "" {
			return "", errors.New("/proc cgroup has duplicate v2 entries")
		}
		if parts[2] == "" || !strings.HasPrefix(parts[2], "/") || strings.ContainsRune(parts[2], '\x00') {
			return "", errors.New("/proc cgroup v2 path is malformed")
		}
		path = filepath.Clean(parts[2])
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if path == "" {
		return "", errors.New("/proc cgroup has no v2 entry")
	}
	return path, nil
}

func parseMountInfo(data []byte) ([]cgroupMount, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	scanner.Buffer(make([]byte, 1024), 1<<20)
	var mounts []cgroupMount
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		separator := -1
		for index, field := range fields {
			if field == "-" {
				separator = index
				break
			}
		}
		if separator < 6 || separator+1 >= len(fields) || fields[separator+1] != "cgroup2" {
			continue
		}
		root, err := unescapeMountInfoPath(fields[3])
		if err != nil {
			return nil, err
		}
		mountPath, err := unescapeMountInfoPath(fields[4])
		if err != nil {
			return nil, err
		}
		if !filepath.IsAbs(root) || !filepath.IsAbs(mountPath) {
			return nil, errors.New("mountinfo cgroup path is not absolute")
		}
		mounts = append(mounts, cgroupMount{root: filepath.Clean(root), mountPath: filepath.Clean(mountPath)})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(mounts) == 0 {
		return nil, errors.New("mountinfo has no cgroup v2 mount")
	}
	return mounts, nil
}

func unescapeMountInfoPath(value string) (string, error) {
	var builder strings.Builder
	for index := 0; index < len(value); index++ {
		if value[index] != '\\' {
			builder.WriteByte(value[index])
			continue
		}
		if index+3 >= len(value) {
			return "", errors.New("mountinfo path escape is truncated")
		}
		code := value[index+1 : index+4]
		var decoded byte
		switch code {
		case "040":
			decoded = ' '
		case "011":
			decoded = '\t'
		case "134":
			decoded = '\\'
		default:
			return "", fmt.Errorf("mountinfo path escape %q is unsupported", code)
		}
		builder.WriteByte(decoded)
		index += 3
	}
	return builder.String(), nil
}

func relativeCgroupPath(root, current string) (string, bool) {
	if root == "/" {
		return current, true
	}
	if current == root {
		return "/", true
	}
	prefix := root + "/"
	if strings.HasPrefix(current, prefix) {
		return strings.TrimPrefix(current, root), true
	}
	return "", false
}

func pathWithin(base, target string) bool {
	base = filepath.Clean(base)
	target = filepath.Clean(target)
	if base == "/" {
		return filepath.IsAbs(target)
	}
	return target == base || strings.HasPrefix(target, base+string(filepath.Separator))
}
