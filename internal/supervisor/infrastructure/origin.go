package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// OriginEnvironment carries a locator, never an owner epoch or executable.
// Composition extracts it before filtering control environments, then rebases
// it explicitly when launching a managed provider in another root.
const OriginEnvironment = "DELEGATE_INFRASTRUCTURE_ORIGIN"

var ErrIdentity = errors.New("infrastructure identity mismatch")

// Origin is an immutable root locator. Its shape is checked at construction;
// opening the root and authenticating its current owner are separate checks.
// A task never retains the epoch of the owner which happened to launch it.
type Origin struct {
	root   string
	rootID string
}

type originRecord struct {
	Root   string `json:"root"`
	RootID string `json:"root_id"`
}

func NewOrigin(root, rootID string) (Origin, error) {
	if !utf8.ValidString(root) || strings.ContainsRune(root, 0) || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return Origin{}, fmt.Errorf("%w: origin root must be a clean absolute path", ErrIdentity)
	}
	if err := task.ValidateRootID(rootID); err != nil {
		return Origin{}, errors.Join(ErrIdentity, err)
	}
	return Origin{root: root, rootID: rootID}, nil
}

func (o Origin) Root() string   { return o.root }
func (o Origin) RootID() string { return o.rootID }

// ValidateRoot observes existing canonical root metadata without initialization,
// filesystem probes or repairs. Success is not RPC or daemon-start authority.
func (o Origin) ValidateRoot(ctx context.Context) (resultErr error) {
	if _, err := NewOrigin(o.root, o.rootID); err != nil {
		return err
	}
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, o.root, predicate.Default())
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, reader.Close()) }()
	if reader.Root() != o.root || reader.RootID() != o.rootID {
		return ErrIdentity
	}
	return nil
}

// ReadOrigin distinguishes missing routing context from malformed context.
// Repeated entries are refused rather than selecting an ambiguous root.
func ReadOrigin(environment []string) (Origin, bool, error) {
	var result Origin
	found := false
	for _, entry := range environment {
		key, raw, present := strings.Cut(entry, "=")
		if key != OriginEnvironment {
			continue
		}
		if !present || found {
			return Origin{}, true, fmt.Errorf("%w: repeated infrastructure origin", ErrIdentity)
		}
		found = true
		if !utf8.ValidString(raw) {
			return Origin{}, true, ErrIdentity
		}
		var record originRecord
		if err := task.DecodeStrict([]byte(raw), &record); err != nil {
			return Origin{}, true, errors.Join(ErrIdentity, err)
		}
		var err error
		result, err = NewOrigin(record.Root, record.RootID)
		if err != nil {
			return Origin{}, true, err
		}
	}
	return result, found, nil
}

// WithOrigin returns an owned launch snapshot. Rebasing removes every previous
// locator; it leaves all other selected environment entries unchanged.
func WithOrigin(environment []string, origin Origin) ([]string, error) {
	if _, err := NewOrigin(origin.root, origin.rootID); err != nil {
		return nil, err
	}
	encoded, err := task.MarshalCanonical(originRecord{Root: origin.root, RootID: origin.rootID})
	if err != nil {
		return nil, err
	}
	if len(encoded) > task.MaxControlRecordSize {
		return nil, errors.Join(ErrIdentity, task.ErrControlRecordTooBig)
	}
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		if key != OriginEnvironment {
			result = append(result, entry)
		}
	}
	return append(result, OriginEnvironment+"="+strings.TrimSuffix(string(encoded), "\n")), nil
}
