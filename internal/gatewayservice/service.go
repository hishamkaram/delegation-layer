// Package gatewayservice manages only the current user's Gateway connector
// service. Native task admission, processes and outcomes have separate owners.
package gatewayservice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
	"golang.org/x/sys/unix"
)

var (
	ErrConfiguration     = errors.New("invalid Gateway service configuration")
	ErrForeignService    = errors.New("gateway service identity or settings differ")
	ErrManager           = errors.New("gateway service manager operation failed")
	ErrPending           = errors.New("gateway connector stop remains unconfirmed")
	ErrGUIRequired       = errors.New("gateway LaunchAgent requires this user's Aqua session")
	ErrServiceChanged    = errors.New("gateway service definition changed during reconciliation; retry")
	ErrManagerCacheStale = errors.New("gateway service manager cache is stale; run service stop on this profile, then retry")
)

type Platform string

const (
	Linux Platform = "linux"
	MacOS Platform = "darwin"
)

// Result describes joined command completion. Nonzero ExitCode is distinct
// from command transport, cancellation, cleanup or output-limit errors.
type Result struct {
	Stdout   []byte
	ExitCode int
}

// Runner executes exactly the selected installed executable and argv, with a
// finite caller deadline, bounded output and joined process cleanup.
type Runner interface {
	Run(context.Context, string, []string) (Result, error)
}

// Config is selected by the thin Native caller. Directory is the canonical
// user service-manager directory. Only Path is persisted from the environment.
// RuntimeInactive borrows the profile owner's identity lease and releases it;
// it must never infer absence from a stale PID or inspect Native task records.
type Config struct {
	Platform          Platform
	ProfileDirectory  string
	Executable        string
	Directory         string
	ManagerExecutable string
	UID               int
	Path              string
	Runner            Runner
	RuntimeInactive   func(context.Context) (bool, error)
}

type State string

const (
	StateAbsent     State = "absent"
	StateInactive   State = "inactive"
	StateRunning    State = "running"
	StateRegistered State = "registered"
	StatePending    State = "pending"
	StateUnknown    State = "unknown"
)

// Status is a service-manager observation, never Native task closure.
type Status struct {
	Name          string `json:"name"`
	Installed     bool   `json:"installed"`
	State         State  `json:"state"`
	StopRequested bool   `json:"stop_requested"`
}

// Manager holds immutable installation choices. The profile directory inode
// serializes operations across processes; the service file is installation
// authority, and the OS manager owns connector process registration.
type Manager struct {
	config   Config
	profile  os.FileInfo
	name     string
	filename string
	rendered []byte
	// Per-manager durability boundary; production uses Native's platform barrier.
	directoryBarrier func(*os.File) error
}

func New(c Config) (*Manager, error) {
	if err := validateInstallation(c); err != nil {
		return nil, err
	}
	profile, err := os.Lstat(c.ProfileDirectory)
	if err != nil {
		return nil, errors.Join(ErrConfiguration, err)
	}
	if err = privateDirectory(profile, c.UID, true); err != nil {
		return nil, errors.Join(ErrConfiguration, err)
	}
	digest := sha256.Sum256([]byte(c.ProfileDirectory))
	name := "delegate-gateway-" + hex.EncodeToString(digest[:])
	suffix := ".service"
	if c.Platform == MacOS {
		name, suffix = "delegate.gateway."+hex.EncodeToString(digest[:]), ".plist"
	}
	m := &Manager{config: c, profile: profile, name: name, filename: name + suffix, directoryBarrier: taskdir.BarrierFile}
	if validPath(c.Path) {
		m.rendered, err = render(c, name)
		if len(m.rendered) > maxServiceDefinition {
			return nil, ErrConfiguration
		}
	}
	return m, err
}

func validateInstallation(c Config) error {
	if c.Runner == nil || c.RuntimeInactive == nil || c.UID != os.Getuid() || (c.Platform != Linux && c.Platform != MacOS) {
		return ErrConfiguration
	}
	// The saved executable is inert service-definition identity during cleanup.
	// Removal or replacement must not prevent stopping its exact managed unit.
	if !filepath.IsAbs(c.Executable) || filepath.Clean(c.Executable) != c.Executable || !validText(c.Executable) {
		return ErrConfiguration
	}
	for _, path := range []string{c.ProfileDirectory, c.Directory, c.ManagerExecutable} {
		canonical, err := config.CanonicalizePath(path)
		if err != nil || canonical != path || !validText(path) {
			return errors.Join(ErrConfiguration, err)
		}
	}
	if _, err := verifiedexec.LocatePath(c.ManagerExecutable); err != nil {
		return errors.Join(ErrConfiguration, err)
	}
	return nil
}

func (m *Manager) requireSourceExecutable() error {
	selected, err := verifiedexec.LocatePath(m.config.Executable)
	if err != nil || selected != m.config.Executable {
		return errors.Join(ErrConfiguration, err)
	}
	return nil
}

func validPath(value string) bool {
	if value == "" || len(value) > 16384 || !validText(value) {
		return false
	}
	for _, directory := range filepath.SplitList(value) {
		if !filepath.IsAbs(directory) {
			return false
		}
	}
	return true
}

func validText(value string) bool {
	return utf8.ValidString(value) && strings.IndexFunc(value, unicode.IsControl) < 0
}

func finite(ctx context.Context) error {
	if ctx == nil {
		return ErrConfiguration
	}
	if _, ok := ctx.Deadline(); !ok {
		return fmt.Errorf("%w: finite caller deadline required", ErrConfiguration)
	}
	return ctx.Err()
}

// WithProfileLock serializes a Native serve startup decision with service
// operations. The callback retains its own runtime identity lease before it
// returns; this coordination lock must not span the connector's lifetime.
func (m *Manager) WithProfileLock(ctx context.Context, action func() error) error {
	return withProfileLock(ctx, m.config.ProfileDirectory, m.profile, action)
}

// WithProfileLock coordinates foreground serve startup without requiring an
// installed OS service manager. The profile's runtime identity lease is a
// separate lock, acquired by action before this startup decision is released.
func WithProfileLock(ctx context.Context, profileDirectory string, action func() error) error {
	if err := finite(ctx); err != nil {
		return err
	}
	canonical, err := config.CanonicalizePath(profileDirectory)
	if err != nil || canonical != profileDirectory {
		return errors.Join(ErrConfiguration, err)
	}
	profile, err := os.Lstat(profileDirectory)
	if err != nil {
		return err
	}
	return withProfileLock(ctx, profileDirectory, profile, action)
}

func withProfileLock(ctx context.Context, profileDirectory string, profile os.FileInfo, action func() error) (resultErr error) {
	if action == nil {
		return ErrConfiguration
	}
	if err := finite(ctx); err != nil {
		return err
	}
	current, err := os.Lstat(profileDirectory)
	if err != nil || !os.SameFile(current, profile) {
		return errors.Join(ErrConfiguration, err)
	}
	if err = privateDirectory(current, os.Getuid(), true); err != nil {
		return err
	}
	lock, err := taskdir.OpenDirectoryLock(profileDirectory, taskdir.LockLevelNone)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()
	if err = lock.LockEX(ctx); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Unlock()) }()
	var held unix.Stat_t
	if err = unix.Fstat(lock.Fd(), &held); err != nil {
		return err
	}
	frozen, ok := profile.Sys().(*syscall.Stat_t)
	if !ok || held.Dev != frozen.Dev || held.Ino != frozen.Ino {
		return ErrConfiguration
	}
	current, err = os.Lstat(profileDirectory)
	if err != nil || !os.SameFile(current, profile) {
		return errors.Join(ErrConfiguration, err)
	}
	return action()
}
