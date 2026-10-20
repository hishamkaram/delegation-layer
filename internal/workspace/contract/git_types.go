package contract

import (
	"errors"
)

var (
	// ErrInvalidGitContext reports an inherited Git redirection or malformed
	// operation context that could select a repository other than the one the
	// caller validated.
	ErrInvalidGitContext = errors.New("invalid git context")
	// ErrIdentityMismatch reports a changed or unverifiable source, checkout,
	// registration, or admin backlink identity.
	ErrIdentityMismatch = errors.New("git identity mismatch")
	// ErrUnsupportedObjectFormat reports an object format other than SHA-1 or
	// SHA-256, the formats whose OID widths this package validates.
	ErrUnsupportedObjectFormat = errors.New("unsupported git object format")
	// ErrUnownedRef reports a ref outside the Delegate-owned namespace.
	ErrUnownedRef = errors.New("git ref is outside the owned namespace")
	// ErrRefConflict reports an owned ref that already points at a different
	// object than the create-only publication expected.
	ErrRefConflict = errors.New("git ref conflicts with the expected object")
	// ErrRefUnreadable reports that an owned ref could not be classified safely.
	ErrRefUnreadable = errors.New("git ref is unreadable")
)

// Source is the validated committed source context used by a managed
// checkout. Dirty is an observation only; no dirty source bytes enter a
// checkout or capture. Root and GitDir are immutable provenance needed only
// to provision; afterwards the shared CommonDir is the stable repository
// identity. CommonDirDevice and CommonDirInode pin that directory when
// recorded; sources recorded before this pin omit both.
type Source struct {
	Root            string
	CommonDir       string
	GitDir          string
	RelativeCwd     string
	BaseOID         string
	ObjectFormat    string
	Dirty           bool
	CommonDirDevice uint64 `json:"CommonDirDevice,omitempty"`
	CommonDirInode  uint64 `json:"CommonDirInode,omitempty"`
}

// CommonDirIdentity returns the recorded common-directory identity, if any.
func (s Source) CommonDirIdentity() (DirectoryIdentity, bool) {
	return DirectoryIdentity{Device: s.CommonDirDevice, Inode: s.CommonDirInode}, s.CommonDirInode != 0
}

// WithCommonDirIdentity returns a copy that pins the common directory.
func (s Source) WithCommonDirIdentity(identity DirectoryIdentity) Source {
	s.CommonDirDevice, s.CommonDirInode = identity.Device, identity.Inode
	return s
}

// Checkout identifies one Git-linked managed worktree and its admin
// registration. CheckoutPath is the canonical worktree root; Workdir is the
// source-relative directory where the provider runs. Both are retained
// because Git identity and provider execution have different path owners.
// AdminBacklink is the canonical admin path read from the checkout's .git
// file.
type Checkout struct {
	Source        Source
	CheckoutPath  string
	Workdir       string
	AdminDir      string
	AdminBacklink string
}

// CaptureArtifact describes one optional exported capture artifact. An empty
// Path with OmittedReason set means no truncated artifact was retained.
type CaptureArtifact struct {
	Path          string
	Length        int64
	SHA256        string
	OmittedReason string
}

// Omitted reports whether the artifact was intentionally not materialized.
func (a CaptureArtifact) Omitted() bool { return a.OmittedReason != "" }

// CaptureExport contains the optional patch and NUL-delimited name-status
// artifacts produced from one validated candidate. Patch and NameStatus are
// cumulative from the workspace base. Turn is set only for an observed turn
// and applies the same export policy from that turn's prelaunch baseline tree
// to the same final tree.
type CaptureExport struct {
	Patch      CaptureArtifact
	NameStatus CaptureArtifact
	Turn       *CaptureDiff
}

// CaptureDiff is one exported patch and name-status pair.
type CaptureDiff struct {
	Patch      CaptureArtifact
	NameStatus CaptureArtifact
}
