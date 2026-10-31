package verifiedexec

// Owner-facing disposal operations of Pool: ReleaseOwner and ReleasePending. A
// Namespace names one lifetime owner's witness directory; Pool treats it as an
// opaque safe name and never reads a lifetime record. The caller derives it
// (for example from RootID, owner kind and owner ID) and verifies canonical
// closure before ReleaseOwner.
//
// Both operations require a pool bound to an identity (NewPoolWithIdentity)
// and prove it first, before they change anything: see
// Pool.requireIdentity. An unbound pool refuses them with errPoolUnbound.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const maxNamespaceBytes = 128

var (
	errNamespaceInvalid = errors.New("invalid custody namespace")
	// errNilContext rejects a nil context before any state is read or changed.
	// It is an invalid argument, not a canceled operation and not a request for
	// a default context.
	errNilContext = errors.New("custody pool operation requires a non-nil context")
)

// Namespace is a validated single path component naming one owner. It uses
// only lowercase ASCII letters, digits, '-', '_' and non-leading '.', and is at
// most 128 bytes. Uppercase is refused, never folded: case-insensitive volumes
// (APFS by default) map "A" and "a" to one directory, so two owners could share
// a witness and one's release would delete the other's views. Callers encode
// identifiers in lowercase (for example lowercase hex). The zero value is
// invalid.
type Namespace struct{ name string }

// NewNamespace validates name. It is a pure check.
func NewNamespace(name string) (Namespace, error) {
	if name == "" || len(name) > maxNamespaceBytes || name[0] == '.' || strings.Contains(name, "..") {
		return Namespace{}, fmt.Errorf("%w: %q", errNamespaceInvalid, name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return Namespace{}, fmt.Errorf("%w: %q", errNamespaceInvalid, name)
		}
	}
	return Namespace{name: name}, nil
}

// String returns the validated name.
func (n Namespace) String() string { return n.name }

func (p *Pool) ownerPath(ns Namespace) string { return filepath.Join(p.viewsPath(), ns.name) }

func requireUsableNamespace(ns Namespace) error {
	if ns.name == "" {
		return fmt.Errorf("%w: the zero namespace", errNamespaceInvalid)
	}
	return nil
}

// ReleaseOwner removes ns's views and then its witness directory. Its caller
// has just verified canonical closure; this method makes no such decision. It
// removes only entries it owns, never follows a symlink and never changes a
// permission outside the views. A missing namespace is success. If any view
// cannot be removed the witness stays (possibly empty) and the concrete cause
// is returned, so ReleasePending keeps reporting true.
//
// Idle-image reclaim follows as a separate best-effort step that takes the pool
// gate without waiting. Its failure or a busy gate is logged and never changes
// this result. A nil ctx is rejected with errNilContext before anything is
// read or changed.
//
// A missing root or marker is not a closed owner: the identity is proved first
// and a pool that cannot prove it returns the concrete cause, never success.
func (p *Pool) ReleaseOwner(ctx context.Context, ns Namespace) error {
	if ctx == nil {
		return errNilContext
	}
	if err := requireUsableNamespace(ns); err != nil {
		return err
	}
	if err := p.requireIdentity(); err != nil {
		return err
	}
	err := p.removeOwner(ctx, ns)
	p.maintain(ctx, true)
	return err
}

// removeOwner removes ns's views and witness. Every "already gone" answer is
// trusted only while the root still proves its identity: a root that vanished
// after the guard is a fault, not a closed owner.
func (p *Pool) removeOwner(ctx context.Context, ns Namespace) error {
	present, err := p.observeDirectory(poolViewsDir, ns.name)
	if err != nil {
		return err
	}
	if !present {
		return p.requireIdentity()
	}
	owner := p.ownerPath(ns)
	entries, err := os.ReadDir(owner)
	if errors.Is(err, fs.ErrNotExist) {
		return p.requireIdentity() // an already closed owner released by someone else meanwhile
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err = ctx.Err(); err != nil {
			return fmt.Errorf("release %s interrupted: %w", ns, err)
		}
		if !strings.HasPrefix(entry.Name(), poolViewPrefix) {
			continue
		}
		view := filepath.Join(owner, entry.Name())
		if err = errors.Join(unsealTemporaryDirectory(view), os.RemoveAll(view)); err != nil {
			return err
		}
		p.at(pointAfterViewRemoved, "")
	}
	p.at(pointBeforeWitnessRemove, "")
	if err = os.Remove(owner); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return p.requireIdentity() // released by someone else between the views and the witness
		}
		return err
	}
	return barrierPath(p.viewsPath())
}

// ReleasePending reports whether ns's witness directory exists, by a read-only
// check of the canonical private chain root, views, owner that creates nothing.
// A missing views or witness link of a root that proves its identity is false;
// any other fault, including a missing root or marker and an alias at any link,
// is returned with its concrete cause and the boolean is then meaningless.
func (p *Pool) ReleasePending(ns Namespace) (bool, error) {
	if err := requireUsableNamespace(ns); err != nil {
		return false, err
	}
	if err := p.requireIdentity(); err != nil {
		return false, err
	}
	present, err := p.observeDirectory(poolViewsDir, ns.name)
	if err != nil {
		return false, err
	}
	if !present {
		// Absence is an answer only from a root that still proves its identity.
		return false, p.requireIdentity()
	}
	return true, nil
}
