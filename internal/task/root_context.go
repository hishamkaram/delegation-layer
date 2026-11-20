package task

import (
	"fmt"
	"unicode/utf8"
)

// RootContextBindingFeature fences writers that cannot preserve a reconciled token.
const RootContextBindingFeature = "root-context-binding-v1"

// RootContextCoordinates are the exact native inputs to the v1 root token.
type RootContextCoordinates struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

// RootContextBinding preserves the original token preimage while explicitly
// accepting one current physical acquisition. RootID belongs to RootRecord.
type RootContextBinding struct {
	Version     int                    `json:"version"`
	Root        string                 `json:"root"`
	Origin      RootContextCoordinates `json:"origin"`
	Accepted    RootContextCoordinates `json:"accepted"`
	Challenge   string                 `json:"challenge"`
	ConfirmedAt string                 `json:"confirmed_at"`
}

func (b RootContextBinding) Validate() error {
	if b.Version != 1 {
		return fmt.Errorf("%w: root context binding version %d", ErrUnsupportedSchema, b.Version)
	}
	if !IsCanonicalCwd(b.Root) || !utf8.ValidString(b.Root) || b.Origin.Inode == 0 || b.Origin.Inode != b.Accepted.Inode {
		return fmt.Errorf("%w: invalid physical root context binding", ErrIdentityMismatch)
	}
	if err := ValidateSHA256(b.Challenge); err != nil {
		return err
	}
	return validateTimestamp(b.ConfirmedAt)
}

// ConnectorRootToken preserves the exact canonical struct and trailing newline
// used by the original v1 connector root token.
func ConnectorRootToken(rootID string, coordinates RootContextCoordinates) (string, error) {
	if err := ValidateRootID(rootID); err != nil {
		return "", err
	}
	if coordinates.Inode == 0 {
		return "", ErrIdentityMismatch
	}
	data, err := MarshalCanonical(struct {
		Version int    `json:"version"`
		RootID  string `json:"root_id"`
		Device  uint64 `json:"device"`
		Inode   uint64 `json:"inode"`
	}{1, rootID, coordinates.Device, coordinates.Inode})
	if err != nil {
		return "", err
	}
	return ComputeSHA256(data), nil
}
