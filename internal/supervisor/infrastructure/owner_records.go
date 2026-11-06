package infrastructure

import (
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// StartupReference identifies one immutable attempt, including its original
// deadline, boot and target pins. Its root namespace belongs to the caller's
// journal/connection, not an alternate mutable routing head.
type StartupReference struct {
	Sequence     uint64 `json:"sequence"`
	Epoch        string `json:"epoch"`
	IntentSHA256 string `json:"intent_sha256"`
}

func ReferenceStartup(intent StartupIntent) (StartupReference, error) {
	digest, err := intent.Digest()
	if err != nil {
		return StartupReference{}, err
	}
	return StartupReference{Sequence: intent.Sequence, Epoch: intent.Epoch, IntentSHA256: digest}, nil
}

func (r StartupReference) Validate() error {
	if r.Sequence == 0 || task.ValidateTaskID(r.Epoch) != nil || task.ValidateSHA256(r.IntentSHA256) != nil {
		return ErrIdentity
	}
	return nil
}

func (r StartupReference) validateIntent(intent StartupIntent) error {
	if err := r.Validate(); err != nil {
		return err
	}
	expected, err := ReferenceStartup(intent)
	if err != nil {
		return err
	}
	if r != expected {
		return fmt.Errorf("%w: startup reference changed", ErrIdentity)
	}
	return nil
}

// ProcessIdentity is record data only. Observing a live incarnation must first
// establish the intent's BootSame relation, then use lifetime's native observer.
type ProcessIdentity struct {
	PID        int    `json:"pid"`
	StartToken string `json:"start_token"`
}

func (p ProcessIdentity) Reference() (lifetime.ProcessRef, error) {
	return lifetime.NewProcessRef(p.PID, p.StartToken)
}

// StartupAssumption is the sole process/UID selection for this attempt. Only
// the fixed child entry may create it, before publication or daemon authority.
// A matching existing record alone does not authorize a second assumption.
type StartupAssumption struct {
	Startup   StartupReference `json:"startup"`
	Process   ProcessIdentity  `json:"process"`
	UID       int              `json:"uid"`
	AssumedAt string           `json:"assumed_at"`
}

func (a StartupAssumption) Validate(intent StartupIntent) error {
	if err := a.Startup.validateIntent(intent); err != nil {
		return err
	}
	if _, err := a.Process.Reference(); err != nil {
		return errors.Join(ErrIdentity, err)
	}
	if a.UID < 0 {
		return ErrIdentity
	}
	assumed, err := canonicalStartupTime(a.AssumedAt)
	if err != nil {
		return err
	}
	deadline, err := canonicalStartupTime(intent.Deadline)
	if err != nil {
		return err
	}
	if !assumed.Before(deadline) {
		return fmt.Errorf("%w: expired startup cannot be assumed", ErrIdentity)
	}
	return nil
}

func (a StartupAssumption) Digest(intent StartupIntent) (string, error) {
	if err := a.Validate(intent); err != nil {
		return "", err
	}
	return digestStartupRecord(a)
}

// EndpointIdentity describes the owner's private listener, never Pueue's
// endpoint. Socket I/O validates these observations and native path limits.
type EndpointIdentity struct {
	Path           string `json:"path"`
	DirectoryDev   uint64 `json:"directory_device"`
	DirectoryInode uint64 `json:"directory_inode"`
	SocketDev      uint64 `json:"socket_device"`
	SocketInode    uint64 `json:"socket_inode"`
}

func (e EndpointIdentity) validate(intent StartupIntent) error {
	target := intent.Inputs
	if e.Path != ownerEndpointPath(target.Root, intent.Sequence) ||
		e.DirectoryDev != target.RootDevice || e.DirectoryInode != target.RootInode || e.SocketInode == 0 {
		return fmt.Errorf("%w: invalid owner endpoint identity", ErrIdentity)
	}
	return nil
}

// OwnerPublication binds endpoint and runtime observations to the child-owned
// assumption. Its process/UID are derived from that verified immutable record;
// no second copy can drift. Publication is not private-supervisor readiness.
type OwnerPublication struct {
	Protocol         int              `json:"protocol"`
	Startup          StartupReference `json:"startup"`
	AssumptionSHA256 string           `json:"assumption_sha256"`
	Endpoint         EndpointIdentity `json:"endpoint"`
}

func (p OwnerPublication) Validate(intent StartupIntent, assumption StartupAssumption) error {
	if p.Protocol != RecordProtocolVersion {
		return ErrIdentity
	}
	if err := p.Startup.validateIntent(intent); err != nil {
		return err
	}
	digest, err := assumption.Digest(intent)
	if err != nil {
		return err
	}
	if digest != p.AssumptionSHA256 {
		return ErrIdentity
	}
	return p.Endpoint.validate(intent)
}

func (p OwnerPublication) Digest(intent StartupIntent, assumption StartupAssumption) (string, error) {
	if err := p.Validate(intent, assumption); err != nil {
		return "", err
	}
	return digestStartupRecord(p)
}
