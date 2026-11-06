package infrastructure

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Infrastructure record formats are unreleased. A future change must retain
// passive readers needed to settle accepted work; unreadable never means absent.
const RecordProtocolVersion = 3

// These comparable record values contain no shared mutable collections. The
// target journal owns their immutable publication. Validation establishes shape
// and consistency, never executable provenance, root pins or native authority.
type StartupKind string

const (
	StartupRootOwner StartupKind = "root-owner"
)

// Artifacts is the fixed installed-role selection accepted for this birth.
// The trusted producer selects these paths; RPC callers cannot choose a runner
// or arbitrary argv. File metadata and required behavior are checked at I/O edges.
type Artifacts struct {
	Runner string `json:"runner"`
}

func (a Artifacts) validate() error {
	if !cleanAbsolutePath(a.Runner) {
		return fmt.Errorf("%w: invalid installed startup runner", ErrIdentity)
	}
	return nil
}

// StartupInputs records the accepted operation identity. Pending callers join
// by matching the launch context and initial operation, not this entire digest.
// The intent owns the observation deadline and artifact and context selection.
type StartupInputs struct {
	Kind       StartupKind            `json:"kind"`
	Initial    InitialOperation       `json:"initial_operation,omitzero"`
	Root       string                 `json:"root"`
	RootID     string                 `json:"root_id"`
	BootID     string                 `json:"boot_id"`
	RootDevice uint64                 `json:"root_device"`
	RootInode  uint64                 `json:"root_inode"`
	Artifacts  Artifacts              `json:"artifacts"`
	Context    LaunchContextReference `json:"launch_context"`
}

func (i StartupInputs) Validate() error {
	if _, err := NewOrigin(i.Root, i.RootID); err != nil {
		return err
	}
	if i.RootInode == 0 || lifetime.CompareBootIDs(i.BootID, i.BootID) != lifetime.BootSame {
		return fmt.Errorf("%w: startup requires stable boot and directory identities", ErrIdentity)
	}
	switch i.Kind {
	case StartupRootOwner:
		return errors.Join(i.Artifacts.validate(), i.Context.Validate(), i.Initial.Validate(i.RootID))
	default:
		return fmt.Errorf("%w: unknown startup operation", ErrIdentity)
	}
}

func (i StartupInputs) Digest() (string, error) {
	if err := i.Validate(); err != nil {
		return "", err
	}
	return digestStartupRecord(i)
}

// StartupIntent fixes one attempt before native Start. Epoch is also its unique
// startup ID; Sequence orders historical attempts without a mutable head. The
// original deadline is never changed by observation, retries or assumption.
type StartupIntent struct {
	Protocol     int           `json:"protocol"`
	Sequence     uint64        `json:"sequence"`
	Epoch        string        `json:"epoch"`
	Inputs       StartupInputs `json:"inputs"`
	InputsSHA256 string        `json:"inputs_sha256"`
	AcceptedAt   string        `json:"accepted_at"`
	Deadline     string        `json:"deadline"`
}

func (i StartupIntent) Validate() error {
	if i.Sequence == 0 || task.ValidateTaskID(i.Epoch) != nil {
		return ErrIdentity
	}
	if i.Protocol != RecordProtocolVersion {
		return fmt.Errorf("%w: unsupported startup record protocol %d", ErrIdentity, i.Protocol)
	}
	digest, err := i.Inputs.Digest()
	if err != nil {
		return err
	}
	if digest != i.InputsSHA256 {
		return fmt.Errorf("%w: startup inputs changed", ErrIdentity)
	}
	accepted, acceptedErr := canonicalStartupTime(i.AcceptedAt)
	deadline, deadlineErr := canonicalStartupTime(i.Deadline)
	if acceptedErr != nil || deadlineErr != nil || !deadline.After(accepted) {
		return errors.Join(ErrIdentity, acceptedErr, deadlineErr)
	}
	return nil
}

func (i StartupIntent) Expired(now time.Time) (bool, error) {
	if err := i.Validate(); err != nil {
		return false, err
	}
	deadline, err := canonicalStartupTime(i.Deadline)
	return !now.Before(deadline), err
}

func (i StartupIntent) Digest() (string, error) {
	if err := i.Validate(); err != nil {
		return "", err
	}
	return digestStartupRecord(i)
}

func digestStartupRecord[T StartupInputs | StartupIntent | StartupAssumption | OwnerPublication | contextManifest | contextChunk](record T) (string, error) {
	data, err := task.MarshalCanonical(record)
	if err != nil {
		return "", err
	}
	if len(data) > task.MaxControlRecordSize {
		return "", errors.Join(ErrIdentity, task.ErrControlRecordTooBig)
	}
	return task.ComputeSHA256(data), nil
}

func canonicalStartupTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.UTC().Format(time.RFC3339Nano) != value {
		return time.Time{}, errors.Join(ErrIdentity, err)
	}
	return parsed, nil
}

func cleanAbsolutePath(path string) bool {
	return utf8.ValidString(path) && filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsRune(path, 0)
}
