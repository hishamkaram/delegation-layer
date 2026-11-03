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

const ProtocolVersion = 1

// These comparable record values contain no shared mutable collections. The
// target journal owns their immutable publication. Validation establishes shape
// and consistency, never executable provenance, root pins or native authority.
type StartupKind string

const (
	StartupRootOwner StartupKind = "root-owner"
	StartupUpgrade   StartupKind = "owner-upgrade"
)

// Artifacts identifies copies already selected and staged by the trusted
// supervisor producer. RPC-supplied paths must never construct this authority.
type Artifacts struct {
	Runner       string `json:"runner"`
	RunnerSHA256 string `json:"runner_sha256"`
	Client       string `json:"client"`
	ClientSHA256 string `json:"client_sha256"`
	Daemon       string `json:"daemon"`
	DaemonSHA256 string `json:"daemon_sha256"`
}

func (a Artifacts) validate() error {
	for _, member := range []struct{ path, digest string }{
		{a.Runner, a.RunnerSHA256}, {a.Client, a.ClientSHA256}, {a.Daemon, a.DaemonSHA256},
	} {
		if !cleanAbsolutePath(member.path) || task.ValidateSHA256(member.digest) != nil {
			return fmt.Errorf("%w: invalid startup artifact reference", ErrIdentity)
		}
	}
	return nil
}

// StartupInputs is the operation identity used to join equivalent callers.
// Caller/process provenance and observation deadlines are deliberately outside
// this digest. An upgrade references its sole lifecycle artifact authority;
// its startup attempt makes no independent artifact or resolution selection.
type StartupInputs struct {
	Kind          StartupKind            `json:"kind"`
	Root          string                 `json:"root"`
	RootID        string                 `json:"root_id"`
	BootID        string                 `json:"boot_id"`
	RootDevice    uint64                 `json:"root_device"`
	RootInode     uint64                 `json:"root_inode"`
	Artifacts     Artifacts              `json:"artifacts"`
	Context       LaunchContextReference `json:"launch_context"`
	UpgradeID     string                 `json:"upgrade_id,omitempty"`
	UpgradeSHA256 string                 `json:"upgrade_sha256,omitempty"`
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
		if i.UpgradeID != "" || i.UpgradeSHA256 != "" {
			return ErrIdentity
		}
		return errors.Join(i.Artifacts.validate(), i.Context.Validate())
	case StartupUpgrade:
		if task.ValidateTaskID(i.UpgradeID) != nil || task.ValidateSHA256(i.UpgradeSHA256) != nil ||
			i.Artifacts != (Artifacts{}) || i.Context != (LaunchContextReference{}) {
			return fmt.Errorf("%w: owner upgrade must derive its artifacts from the lifecycle intent", ErrIdentity)
		}
		return nil
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

type StarterKind string

const (
	StarterOutside StarterKind = "outside"
	StarterOwner   StarterKind = "owner"
)

// StartupProvenance records the trusted starter selected by composition. A PID
// is diagnostic identity; it does not authorize a successor or require that the
// requester stay alive after acceptance. Native authentication happens before
// publication and again at the fixed child entry.
type StartupProvenance struct {
	Kind       StarterKind `json:"kind"`
	Root       string      `json:"root"`
	RootID     string      `json:"root_id"`
	Epoch      string      `json:"epoch,omitempty"`
	PID        int         `json:"pid"`
	StartToken string      `json:"start_token"`
}

func (p StartupProvenance) validate(target StartupInputs) error {
	if _, err := NewOrigin(p.Root, p.RootID); err != nil {
		return err
	}
	if _, err := lifetime.NewProcessRef(p.PID, p.StartToken); err != nil {
		return errors.Join(ErrIdentity, err)
	}
	if target.Kind == StartupUpgrade && p.Kind != StarterOutside {
		return fmt.Errorf("%w: owner upgrade requires a trusted outside invocation", ErrIdentity)
	}
	switch p.Kind {
	case StarterOutside:
		if p.Epoch != "" || p.Root != target.Root || p.RootID != target.RootID {
			return ErrIdentity
		}
	case StarterOwner:
		if p.Root == target.Root || p.RootID == target.RootID {
			return fmt.Errorf("%w: an owner cannot create its own successor", ErrIdentity)
		}
		if err := task.ValidateTaskID(p.Epoch); err != nil {
			return errors.Join(ErrIdentity, err)
		}
	default:
		return ErrIdentity
	}
	return nil
}

// StartupIntent fixes one attempt before native Start. Epoch is also its unique
// startup ID; Sequence orders historical attempts without a mutable head. The
// original deadline is never changed by observation, retries or assumption.
type StartupIntent struct {
	Protocol     int               `json:"protocol"`
	Sequence     uint64            `json:"sequence"`
	Epoch        string            `json:"epoch"`
	Inputs       StartupInputs     `json:"inputs"`
	InputsSHA256 string            `json:"inputs_sha256"`
	Starter      StartupProvenance `json:"starter"`
	AcceptedAt   string            `json:"accepted_at"`
	Deadline     string            `json:"deadline"`
}

func (i StartupIntent) Validate() error {
	if i.Protocol != ProtocolVersion || i.Sequence == 0 || task.ValidateTaskID(i.Epoch) != nil {
		return ErrIdentity
	}
	digest, err := i.Inputs.Digest()
	if err != nil {
		return err
	}
	if digest != i.InputsSHA256 {
		return fmt.Errorf("%w: startup inputs changed", ErrIdentity)
	}
	if err = i.Starter.validate(i.Inputs); err != nil {
		return err
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
