package infrastructure

import (
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// LaunchContext is an immutable snapshot selected by trusted supervisor
// composition. The target journal owns its create-once, content-addressed
// record; the intent binds that exact digest. This is consequential launch
// data, never executable selection, birth-chain or native Start authority.
// Capture and consumers clone the environments; values never belong in errors.
type LaunchContext struct {
	Protocol               int      `json:"protocol"`
	Root                   string   `json:"root"`
	RootID                 string   `json:"root_id"`
	OS                     string   `json:"os"`
	ControlEnvironment     []string `json:"control_environment"`
	MaintenanceEnvironment []string `json:"maintenance_environment"`
}

func (c LaunchContext) Clone() LaunchContext {
	c.ControlEnvironment = slices.Clone(c.ControlEnvironment)
	c.MaintenanceEnvironment = slices.Clone(c.MaintenanceEnvironment)
	return c
}

func (c LaunchContext) Validate() error {
	if c.Protocol != ProtocolVersion || (c.OS != "linux" && c.OS != "darwin") {
		return ErrIdentity
	}
	if _, err := NewOrigin(c.Root, c.RootID); err != nil {
		return err
	}
	for _, environment := range [][]string{c.ControlEnvironment, c.MaintenanceEnvironment} {
		// Nil would let a later process silently substitute its ambient scope.
		if environment == nil || task.ValidateEnvironment(environment) != nil {
			return fmt.Errorf("%w: invalid explicit infrastructure environment", ErrIdentity)
		}
		for _, entry := range environment {
			if !utf8.ValidString(entry) {
				return fmt.Errorf("%w: infrastructure environment is not UTF-8", ErrIdentity)
			}
		}
	}
	return nil
}

func (c LaunchContext) Digest() (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	return digestStartupRecord(c)
}

// LaunchContextReference uses the content digest as its sole identity. Storage
// derives a bounded context-<digest prefix>.json name and checks the full digest;
// callers cannot supply another path. A prefix collision fails byte verification.
type LaunchContextReference struct {
	SHA256 string `json:"sha256"`
}

func (r LaunchContextReference) Validate() error {
	if task.ValidateSHA256(r.SHA256) != nil {
		return ErrIdentity
	}
	return nil
}

func (r LaunchContextReference) ValidateContext(c LaunchContext, root, rootID string) error {
	if err := r.Validate(); err != nil {
		return err
	}
	digest, err := c.Digest()
	if err != nil {
		return err
	}
	if digest != r.SHA256 || c.Root != root || c.RootID != rootID {
		return fmt.Errorf("%w: infrastructure launch context changed", ErrIdentity)
	}
	return nil
}
