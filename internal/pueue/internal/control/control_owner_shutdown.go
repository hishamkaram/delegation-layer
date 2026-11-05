package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/hishamkaram/delegation-layer/internal/task"

	"golang.org/x/sys/unix"
)

// Only withPrivateLockContext constructs this borrowed native capability after
// acquiring the actual root bootstrap EX. The context is not a persisted grant.
type privateBootstrapEX struct {
	admission sync.Mutex
	active    bool
	root      string
	lock      *bootstrapLock
	identity  controlLeaseIdentity
}

type (
	privateBootstrapEXKey struct{}
	privateShutdownKey    struct{}
)

type privateShutdownScope struct {
	bootstrap  *privateBootstrapEX
	configPath string
}

type controlLeaseIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

// This transport record classifies an already-authorized exact shutdown. It
// selects no command, executable or endpoint and confers no lifecycle authority.
type controlOwnerShutdown struct {
	Root       string               `json:"root"`
	ConfigPath string               `json:"config_path"`
	Bootstrap  controlLeaseIdentity `json:"bootstrap"`
	LeaseIndex int                  `json:"lease_index"`
}

func withAcquiredPrivateBootstrapEX(ctx context.Context, root, name string, mode int, lock *bootstrapLock) (context.Context, error) {
	if name != "bootstrap.lock" || mode&unix.LOCK_EX == 0 {
		return ctx, nil
	}
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.closed || lock.file == nil {
		return nil, os.ErrClosed
	}
	identity, err := checkedBootstrapIdentity(root, lock.file)
	if err != nil {
		return nil, err
	}
	return context.WithValue(ctx, privateBootstrapEXKey{}, &privateBootstrapEX{active: true, root: root, lock: lock, identity: identity}), nil
}

// consume linearizes new shutdown admission with callback revocation. admit
// may only inspect/update private admission state: no I/O, canonical calls or
// reentrant scope locking. Native prechecks must finish before this call.
func (bootstrap *privateBootstrapEX) consume(admit func() error) error {
	if bootstrap == nil {
		return fmt.Errorf("%w: private shutdown has no bootstrap scope", ErrBinding)
	}
	bootstrap.admission.Lock()
	defer bootstrap.admission.Unlock()
	if !bootstrap.active {
		return fmt.Errorf("%w: private shutdown bootstrap scope has ended", ErrBinding)
	}
	if admit != nil {
		return admit()
	}
	return nil
}

func revokeAcquiredPrivateBootstrapEX(ctx context.Context, lock *bootstrapLock) {
	bootstrap, tagged := ctx.Value(privateBootstrapEXKey{}).(*privateBootstrapEX)
	if !tagged || bootstrap == nil || bootstrap.lock != lock {
		return // A nested SH/other lock never revokes its inherited outer EX.
	}
	bootstrap.admission.Lock()
	bootstrap.active = false
	bootstrap.admission.Unlock()
}

// Nil is reserved for the existing unscoped legacy paths. Every qualified
// managed shutdown gets a final consume at the canonical owner.Start boundary.
// A future strict capture must establish its scope before using this helper.
func privateShutdownConsume(ctx context.Context, admit func() error) func() error {
	scope, scoped := ctx.Value(privateShutdownKey{}).(*privateShutdownScope)
	if !scoped || scope == nil {
		return nil
	}
	return func() error { return scope.bootstrap.consume(admit) }
}

func checkedBootstrapIdentity(root string, file *os.File) (controlLeaseIdentity, error) {
	var stat, named unix.Stat_t
	path := filepath.Join(root, privateSupervisorDirectory, "bootstrap.lock")
	if err := rejectSymlinkComponents(path); err != nil {
		return controlLeaseIdentity{}, err
	}
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return controlLeaseIdentity{}, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o077 != 0 {
		return controlLeaseIdentity{}, fmt.Errorf("%w: bootstrap descriptor is not a protected regular file", ErrBinding)
	}
	if err := unix.Lstat(path, &named); err != nil {
		return controlLeaseIdentity{}, err
	}
	identity := controlLeaseIdentity{Device: uint64(stat.Dev), Inode: stat.Ino}
	if named.Mode&unix.S_IFMT != unix.S_IFREG || identity != (controlLeaseIdentity{Device: uint64(named.Dev), Inode: named.Ino}) {
		return controlLeaseIdentity{}, fmt.Errorf("%w: bootstrap descriptor no longer names the root lock", ErrBinding)
	}
	return identity, nil
}

// The canonical constructor owns this immutable fact. Dispatch never infers
// it from a runtime pathname, prefix, logical binding or containment check.
type privateClientConstruction struct {
	root          string
	runtimeConfig task.SupervisorRef
}

func recordPrivateClientConstruction(client *Client, root string) error {
	if client == nil || client.privateConstruction != nil {
		return ErrConfiguration
	}
	if client.privateConstructionErr != nil {
		return client.privateConstructionErr
	}
	resolution, err := client.resolution()
	if err != nil {
		return err
	}
	if _, err = readPrivateConfig(client.runtimeBinding.ConfigPath, filepath.Join(root, privateSupervisorDirectory), resolution); err != nil {
		return err
	}
	client.privateConstruction = &privateClientConstruction{root: root, runtimeConfig: client.runtimeBinding}
	return nil
}

func newPrivateMigrationShutdownClient(root string, binding task.SupervisorRef, identity privateDaemonIdentity, options Options) (*Client, error) {
	client := privateMigrationPIDObserver(binding, identity, options)
	resolution, err := client.resolution()
	if err != nil {
		return nil, err
	}
	resolved, err := readPrivateConfig(client.runtimeBinding.ConfigPath, filepath.Join(root, privateSupervisorDirectory), resolution)
	if err != nil {
		return nil, err
	}
	if err = validateResolvedPrivateMigrationConfig(resolved, filepath.Join(root, privateSupervisorDirectory), client.runtimeBinding.Endpoint); err != nil {
		return nil, err
	}
	if err = recordPrivateClientConstruction(client, root); err != nil {
		return nil, err
	}
	return client, nil
}

func (c *Client) privateShutdownContext(ctx context.Context) (context.Context, error) {
	// Legacy embedders retain their existing behavior, without a C3 claim. The
	// future Stop/retry boundary requires the managed external owner explicitly.
	if c.options.ControlOwnerExecutable == "" {
		return ctx, nil
	}
	if c.privateConstructionErr != nil {
		return nil, errors.Join(fmt.Errorf("%w: private shutdown construction qualification failed", ErrBinding), c.privateConstructionErr)
	}
	bootstrap, tagged := ctx.Value(privateBootstrapEXKey{}).(*privateBootstrapEX)
	construction := c.privateConstruction
	if construction == nil {
		if tagged {
			return nil, fmt.Errorf("%w: private shutdown lacks canonical construction provenance", ErrBinding)
		}
		return ctx, nil
	}
	if !tagged || bootstrap == nil || bootstrap.root != construction.root {
		return nil, fmt.Errorf("%w: managed private shutdown requires the actual root bootstrap EX", ErrBinding)
	}
	if err := bootstrap.consume(nil); err != nil {
		return nil, err
	}
	if !task.SameSupervisorIdentity(c.runtimeBinding, construction.runtimeConfig) {
		return nil, fmt.Errorf("%w: private shutdown runtime configuration differs from its construction", ErrBinding)
	}
	return context.WithValue(ctx, privateShutdownKey{}, &privateShutdownScope{bootstrap: bootstrap, configPath: c.runtimeBinding.ConfigPath}), nil
}

func privateShutdownForwarding(ctx context.Context, files []*os.File) (*controlOwnerShutdown, error) {
	scope, ok := ctx.Value(privateShutdownKey{}).(*privateShutdownScope)
	if !ok || scope == nil {
		return nil, nil
	}
	if err := scope.bootstrap.consume(nil); err != nil {
		return nil, err
	}
	selected := -1
	for index, lock := range controlLeaseContextFrom(ctx).locks {
		if lock == scope.bootstrap.lock {
			selected = index
			break
		}
	}
	if selected < 0 || selected >= len(files) {
		return nil, fmt.Errorf("%w: shutdown bootstrap lease is not retained", ErrBinding)
	}
	identity, err := checkedBootstrapIdentity(scope.bootstrap.root, files[selected])
	if err != nil {
		return nil, err
	}
	if identity != scope.bootstrap.identity {
		return nil, fmt.Errorf("%w: shutdown bootstrap identity changed", ErrBinding)
	}
	return &controlOwnerShutdown{Root: scope.bootstrap.root, ConfigPath: scope.configPath, Bootstrap: identity, LeaseIndex: selected}, nil
}

func validateControlOwnerShutdown(request controlOwnerRequest) error {
	shutdown := request.Shutdown
	if shutdown == nil {
		return nil
	}
	if !filepath.IsAbs(shutdown.Root) || filepath.Clean(shutdown.Root) != shutdown.Root || !filepath.IsAbs(shutdown.ConfigPath) || filepath.Clean(shutdown.ConfigPath) != shutdown.ConfigPath {
		return ErrConfiguration
	}
	if err := pathWithin(filepath.Join(shutdown.Root, privateSupervisorDirectory), shutdown.ConfigPath); err != nil {
		return err
	}
	if shutdown.LeaseIndex < 0 || shutdown.Bootstrap.Inode == 0 || len(request.Arguments) != 4 || request.Arguments[1] != "-c" || request.Arguments[2] != shutdown.ConfigPath || request.Arguments[3] != "shutdown" || request.StatusVersion != "" {
		return ErrConfiguration
	}
	return nil
}

func controlOwnerShutdownFiles(request controlOwnerRequest, leases []*os.File) ([]*os.File, error) {
	if request.Shutdown == nil {
		return nil, nil
	}
	shutdown := request.Shutdown
	if shutdown.LeaseIndex < 0 || shutdown.LeaseIndex >= len(leases) {
		return nil, ErrConfiguration
	}
	identity, err := checkedBootstrapIdentity(shutdown.Root, leases[shutdown.LeaseIndex])
	if err != nil {
		return nil, err
	}
	if identity != shutdown.Bootstrap {
		return nil, fmt.Errorf("%w: inherited shutdown bootstrap identity changed", ErrBinding)
	}
	return []*os.File{leases[shutdown.LeaseIndex]}, nil
}

func markControlLeaseCloseOnExec(file *os.File) error {
	flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFD, 0)
	if err != nil {
		return err
	}
	if _, err = unix.FcntlInt(file.Fd(), unix.F_SETFD, flags|unix.FD_CLOEXEC); err != nil {
		return err
	}
	observed, err := unix.FcntlInt(file.Fd(), unix.F_GETFD, 0)
	if err != nil {
		return err
	}
	if observed&unix.FD_CLOEXEC == 0 {
		return fmt.Errorf("%w: inherited control lease CLOEXEC not established", ErrBinding)
	}
	return nil
}
