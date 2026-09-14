package pueue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

// Client has no mutable observation state; concurrent reads synchronize only
// through their own command completion handles.
type Client struct {
	binding task.SupervisorRef
	options Options
}

// Bind validates an explicit config and executable before recording a binding.
func Bind(ctx context.Context, executable, configPath string, options Options) (*Client, error) {
	options, err := normalizeOptions(options)
	if err != nil {
		return nil, err
	}
	path, err := canonicalExecutable(executable)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(configPath) || filepath.Clean(configPath) != configPath {
		return nil, ErrConfiguration
	}
	c := &Client{options: options, binding: task.SupervisorRef{ClientExecutable: path, ConfigPath: configPath, ObservedVersion: SupportedVersion}}
	current, err := c.readBinding()
	if err != nil {
		return nil, err
	}
	c.binding = current
	if err := c.checkVersion(ctx); err != nil {
		return c, err
	}
	return c, nil
}

// NewClient constructs from saved identity without consulting external files.
// Each operation performs its own fresh validation before execution.
func NewClient(saved task.SupervisorRef, options Options) (*Client, error) {
	if err := task.ValidateFreshSupervisorRef(saved); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBinding, err)
	}
	if saved.ObservedVersion != SupportedVersion {
		return nil, ErrBinding
	}
	options, err := normalizeOptions(options)
	if err != nil {
		return nil, err
	}
	return &Client{binding: saved, options: options}, nil
}

func normalizeOptions(options Options) (Options, error) {
	if options.ObservationTimeout == 0 {
		options.ObservationTimeout = DefaultObservationTimeout
	}
	if options.ObservationTimeout < 0 {
		return Options{}, ErrConfiguration
	}
	if options.Resolution != nil {
		copy := *options.Resolution
		options.Resolution = &copy
	}
	if options.Environment != nil {
		options.Environment = append([]string{}, options.Environment...)
	}
	return options, nil
}

func (c *Client) Binding() task.SupervisorRef { return c.binding }

func canonicalExecutable(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%w: executable must be absolute", ErrConfiguration)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("%w: executable unavailable: %w", ErrConfiguration, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%w: executable must be an executable regular file", ErrConfiguration)
	}
	return resolved, nil
}

func (c *Client) resolution() (ResolutionContext, error) {
	current, err := resolutionContext(environmentForCommand(c.options.Environment))
	if err != nil {
		return ResolutionContext{}, err
	}
	if c.options.Resolution == nil {
		return current, nil
	}
	resolution := *c.options.Resolution
	if err := validateResolutionContext(resolution); err != nil {
		return ResolutionContext{}, err
	}
	if !sameEnvironmentResolution(resolution, current) {
		return ResolutionContext{}, fmt.Errorf("%w: resolution does not match command environment", ErrConfiguration)
	}
	return resolution, nil
}

func environmentForCommand(environment []string) []string {
	if environment != nil {
		return environment
	}
	return os.Environ()
}

func sameEnvironmentResolution(resolution, current ResolutionContext) bool {
	return resolution.OS == current.OS &&
		resolution.Home == current.Home &&
		resolution.DataLocalDirectory == current.DataLocalDirectory &&
		resolution.ConfigDirectory == current.ConfigDirectory &&
		resolution.RuntimeDirectory == current.RuntimeDirectory &&
		resolution.Username == current.Username
}

func (c *Client) readBinding() (task.SupervisorRef, error) {
	if err := rejectSymlinkComponents(c.binding.ConfigPath); err != nil {
		return task.SupervisorRef{}, err
	}
	data, err := readRegular(c.binding.ConfigPath, MaxControlBytes)
	if err != nil {
		return task.SupervisorRef{}, fmt.Errorf("%w: read explicit config: %w", ErrConfiguration, err)
	}
	config, err := ParseConfig(data)
	if err != nil {
		return task.SupervisorRef{}, err
	}
	resolution, err := c.resolution()
	if err != nil {
		return task.SupervisorRef{}, err
	}
	resolved, err := ResolveConfig(config, resolution)
	if err != nil {
		return task.SupervisorRef{}, err
	}
	fingerprint, err := resolved.Digest()
	if err != nil {
		return task.SupervisorRef{}, err
	}
	executable, err := canonicalExecutable(c.binding.ClientExecutable)
	if err != nil || executable != c.binding.ClientExecutable {
		return task.SupervisorRef{}, errors.Join(ErrBinding, err)
	}
	digest, err := hashExecutable(executable)
	if err != nil {
		return task.SupervisorRef{}, err
	}
	return task.SupervisorRef{ClientExecutable: executable, ClientSHA256: digest, ConfigPath: c.binding.ConfigPath, ConfigDigest: task.ComputeSHA256(data), Endpoint: resolved.Endpoint(), ResolvedConfigSHA256: fingerprint, ObservedVersion: SupportedVersion}, nil
}

func readRegular(path string, limit int64) (data []byte, err error) {
	f, err := openRegular(path)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	return task.ReadBounded(f, limit)
}

func hashExecutable(path string) (digest string, err error) {
	f, err := openRegular(path)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func openRegular(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: open regular file: %w", ErrConfiguration, err)
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		return nil, errors.Join(ErrConfiguration, unix.Close(fd))
	}
	info, err := f.Stat()
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	if !info.Mode().IsRegular() {
		return nil, errors.Join(ErrConfiguration, f.Close())
	}
	return f, nil
}

func (c *Client) verify(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := c.readBinding()
	if err != nil {
		return errors.Join(ErrBinding, err)
	}
	if current != c.binding {
		return ErrBinding
	}
	return c.checkVersion(ctx)
}

func (c *Client) checkVersion(ctx context.Context) error {
	result, err := c.command(ctx, nil, "--version")
	if err != nil {
		return errors.Join(ErrBinding, err)
	}
	if strings.TrimSpace(string(result.Stdout)) != "pueue "+SupportedVersion {
		return fmt.Errorf("%w: unsupported client version", ErrBinding)
	}
	return nil
}

func (c *Client) command(ctx context.Context, consume func() error, args ...string) (CommandResult, error) {
	if err := ctx.Err(); err != nil {
		return CommandResult{}, err
	}
	resolution, err := c.resolution()
	if err != nil {
		return CommandResult{}, err
	}
	cmd := exec.Command(c.binding.ClientExecutable, append([]string{"-c", c.binding.ConfigPath}, args...)...)
	cmd.Dir = resolution.Cwd
	cmd.Env = c.options.Environment
	commandID := ""
	if c.options.Observer != nil {
		commandID, err = task.NewRandomID()
		if err != nil {
			return CommandResult{}, err
		}
	}
	if consume != nil {
		if err := consume(); err != nil {
			return CommandResult{}, err
		}
	}
	return observeCommand(ctx, startOwnedObserved(cmd, commandID, c.options.Observer), c.options.ObservationTimeout)
}

func validateIdentity(i Identity) error {
	for _, err := range []error{task.ValidateRootID(i.RootID), task.ValidateTaskID(i.TaskID), task.ValidateSHA256(i.SpecSHA256), task.ValidateSHA256(i.MetaSHA256)} {
		if err != nil {
			return err
		}
	}
	if i.NumericTaskID != nil && *i.NumericTaskID < 0 {
		return ErrUnknown
	}
	return nil
}

// Reconcile never adds work. Empty, duplicate, malformed and mismatched rows
// remain unknown, including when a reachable queue is empty.
func (c *Client) Reconcile(ctx context.Context, identity Identity) (Observation, error) {
	identity = snapshotIdentity(identity)
	unknown := Observation{State: StateUnknown, Identity: identity, Binding: c.binding}
	if err := validateIdentity(identity); err != nil {
		return unknown, err
	}
	if err := c.verify(ctx); err != nil {
		unknown.Pending = pendingFrom(err)
		return unknown, err
	}
	result, err := c.command(ctx, nil, "status", "--json")
	if err != nil {
		unknown.Pending = pendingFrom(err)
		return unknown, errors.Join(ErrUnknown, err)
	}
	jobs, err := ParseStatus(result.Stdout, c.binding.ObservedVersion)
	if err != nil {
		return unknown, err
	}
	return matchingObservation(jobs, identity, c.binding)
}

func matchingObservation(jobs []Job, identity Identity, binding task.SupervisorRef) (Observation, error) {
	identity = snapshotIdentity(identity)
	result := Observation{State: StateUnknown, Identity: identity, Binding: binding}
	matches := 0
	for _, job := range jobs {
		if job.Label == nil || *job.Label != identity.Label() {
			continue
		}
		matches++
		result.NumericTaskID, result.State = job.ID, job.State
	}
	if matches != 1 || (identity.NumericTaskID != nil && *identity.NumericTaskID != result.NumericTaskID) {
		result.State = StateUnknown
		return result, ErrUnknown
	}
	result.Matched = true
	return result, nil
}

func snapshotIdentity(identity Identity) Identity {
	if identity.NumericTaskID != nil {
		id := *identity.NumericTaskID
		identity.NumericTaskID = &id
	}
	return identity
}
