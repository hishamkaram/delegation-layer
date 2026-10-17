package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

func (c *Client) readBinding() (task.SupervisorRef, error) {
	runtimeBinding := c.runtimeBinding
	if err := rejectSymlinkComponents(runtimeBinding.ConfigPath); err != nil {
		return task.SupervisorRef{}, err
	}
	data, err := readRegular(runtimeBinding.ConfigPath, MaxControlBytes)
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
	executable, err := canonicalExecutable(runtimeBinding.ClientExecutable)
	if err != nil || executable != runtimeBinding.ClientExecutable {
		return task.SupervisorRef{}, errors.Join(ErrBinding, err)
	}
	digest, err := hashExecutable(executable)
	if err != nil {
		return task.SupervisorRef{}, err
	}
	binding := task.SupervisorRef{ClientExecutable: executable, ClientSHA256: digest, DaemonExecutable: runtimeBinding.DaemonExecutable, DaemonSHA256: runtimeBinding.DaemonSHA256, ResolutionCwd: runtimeBinding.ResolutionCwd, ConfigPath: runtimeBinding.ConfigPath, ConfigDigest: task.ComputeSHA256(data), Endpoint: resolved.Endpoint(), ResolvedConfigSHA256: fingerprint, ObservedVersion: runtimeBinding.ObservedVersion}
	if runtimeBinding.Endpoint == "" || runtimeBinding.ResolutionOS != "" {
		setBindingResolution(&binding, resolution)
	}
	return binding, nil
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
	if err := c.verifyBinding(); err != nil {
		return err
	}
	return ctx.Err()
}

func (c *Client) verifyBinding() error {
	current, err := c.readBinding()
	if err != nil {
		return errors.Join(ErrBinding, err)
	}
	current.ObservedVersion = c.runtimeBinding.ObservedVersion
	if current != c.runtimeBinding {
		return ErrBinding
	}
	return nil
}

func (c *Client) checkVersion(ctx context.Context) (string, error) {
	result, err := c.command(ctx, nil, "--version")
	if err != nil {
		return "", bindingProbeError(err)
	}
	return validateVersionResult(result)
}

// An unfinished probe has no result that can establish binding drift. Retain
// its command owner and any independently established failure unchanged.
func bindingProbeError(err error) error {
	if err == nil || pendingFrom(err) != nil {
		return err
	}
	return errors.Join(ErrBinding, err)
}

func validateVersionResult(result CommandResult) (string, error) {
	if result.Err != nil {
		return "", errors.Join(ErrBinding, result.Err)
	}
	if len(result.Stdout) == 0 || len(result.Stdout) > MaxControlBytes || !utf8.Valid(result.Stdout) {
		return "", fmt.Errorf("%w: supervisor did not report a usable version", ErrBinding)
	}
	version := strings.TrimSpace(string(result.Stdout))
	if version == "" {
		return "", fmt.Errorf("%w: supervisor did not report a usable version", ErrBinding)
	}
	for _, character := range version {
		if unicode.IsControl(character) {
			return "", fmt.Errorf("%w: supervisor did not report a usable version", ErrBinding)
		}
	}
	return version, nil
}
