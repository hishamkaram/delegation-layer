package workspaces

import (
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// Description is independent from provider outcome and publication.
// It reports resource availability and Git-content evidence only.
type Description struct {
	ID                    string                     `json:"id"`
	Isolation             string                     `json:"isolation"`
	State                 string                     `json:"state"`
	Path                  string                     `json:"path,omitempty"`
	CanonicalCwd          string                     `json:"canonical_cwd,omitempty"`
	SourceRoot            string                     `json:"source_root,omitempty"`
	SourceChangesExcluded bool                       `json:"source_changes_excluded"`
	BaseOID               string                     `json:"base_oid,omitempty"`
	BaseRef               string                     `json:"base_ref,omitempty"`
	OperationID           string                     `json:"operation_id,omitempty"`
	Operation             *workspace.OperationResult `json:"operation,omitempty"`
	Capture               *workspace.CaptureReceipt  `json:"capture,omitempty"`
	NextAction            string                     `json:"next_action,omitempty"`
	ReasonCode            string                     `json:"reason_code,omitempty"`
	EvidenceState         string                     `json:"evidence_state"`
	Artifacts             []ArtifactDescription      `json:"artifacts,omitempty"`
}

// ArtifactDescription derives a usable location from immutable evidence.
// Retained describes inventory; consumers still verify length and SHA-256 when
// reading bytes. Forgetting never rewrites the underlying capture receipt.
type ArtifactDescription struct {
	workspace.Artifact
	OperationID   string `json:"operation_id"`
	Path          string `json:"path,omitempty"`
	EvidenceState string `json:"evidence_state"`
}
