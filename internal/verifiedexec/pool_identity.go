package verifiedexec

// The pool identity marker: data and pure decisions. Pool owns one immutable
// marker per root; its operations on the filesystem live in pool_identity_fs.go
// and the protocol that publishes it in pool_prepare.go.
//
// A marker is the canonical JSON {"version":1,"pool_id":"<64 lowercase hex>"}
// in a regular file named poolIdentityName, mode 0600. The id is a random
// nonce, never derived from the root path, the boot or the clock, and it never
// changes once published. The marker carries no timestamp, source or boot field.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
)

const (
	poolIdentityName        = ".identity"
	poolIdentityStagePrefix = ".identity-stage-"
	poolIdentityVersion     = 1
	// maxPoolIdentityBytes bounds a read; the canonical record is under 100 bytes.
	maxPoolIdentityBytes = 256
	// poolIdentityMode is the exact mode of the marker and of its stage.
	poolIdentityMode = 0o600
	// identityStageNonceLen is the length of a stage name's lowercase-hex nonce.
	identityStageNonceLen = 32
)

var (
	errPoolIdentityInvalid = errors.New("custody pool identity is not a 64-character lowercase hex id")
	errPoolUnbound         = errors.New("custody pool is not bound to an identity: Prepare it and use NewPoolWithIdentity")
	errPoolPrepareGuarded  = errors.New("custody pool is bound to an identity and is never prepared again")
	// errPoolIdentityUnverified wraps the concrete cause when a guarded operation
	// cannot prove that the root holds the expected marker. The operation then
	// changes nothing and the pool stays unresolved: it never reports a release,
	// reselects an identity or repairs the root.
	errPoolIdentityUnverified = errors.New("custody pool identity could not be verified")
	errPoolIdentityMismatch   = errors.New("custody pool holds a different identity than expected")
	errPoolIdentityRecord     = errors.New("custody pool identity record is not the canonical record")
	errPoolIdentityStage      = errors.New("custody pool identity stage is not a regular file owned by this user")
)

// poolIdentityRecord is the marker's content.
type poolIdentityRecord struct {
	Version int    `json:"version"`
	PoolID  string `json:"pool_id"`
}

// newPoolID returns a fresh random 64-lowercase-hex identity from the
// repository's random-id source.
func newPoolID() (string, error) {
	head, err := randomid.NewRandomID()
	if err != nil {
		return "", err
	}
	tail, err := randomid.NewRandomID()
	if err != nil {
		return "", err
	}
	return head + tail, nil
}

// encodePoolIdentity is the canonical marker bytes for id.
func encodePoolIdentity(id string) ([]byte, error) {
	if err := task.ValidateSHA256(id); err != nil {
		return nil, fmt.Errorf("%w: %w", errPoolIdentityInvalid, err)
	}
	return json.Marshal(poolIdentityRecord{Version: poolIdentityVersion, PoolID: id})
}

// decodePoolIdentity accepts only the exact canonical bytes of a marker and
// returns its id: unknown fields, other versions, a malformed id, whitespace,
// key reordering and trailing bytes are all refused.
func decodePoolIdentity(raw []byte) (string, error) {
	var record poolIdentityRecord
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return "", fmt.Errorf("%w: %w", errPoolIdentityRecord, err)
	}
	if record.Version != poolIdentityVersion {
		return "", fmt.Errorf("%w: version %d", errPoolIdentityRecord, record.Version)
	}
	canonical, err := encodePoolIdentity(record.PoolID)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errPoolIdentityRecord, err)
	}
	if !bytes.Equal(canonical, raw) {
		return "", fmt.Errorf("%w: not the canonical bytes", errPoolIdentityRecord)
	}
	return record.PoolID, nil
}

// newIdentityStageName is a fresh recognized marker-stage name.
func newIdentityStageName() (string, error) {
	nonce, err := randomid.NewRandomID()
	if err != nil {
		return "", err
	}
	return poolIdentityStagePrefix + nonce, nil
}

// isIdentityStageName reports whether name has exactly the grammar of a marker
// stage. Anything else in the root is never treated as an abandoned stage.
func isIdentityStageName(name string) bool {
	nonce, found := strings.CutPrefix(name, poolIdentityStagePrefix)
	return found && isLowerHex(nonce, identityStageNonceLen)
}
