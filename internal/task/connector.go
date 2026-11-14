package task

import (
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"unicode/utf8"
)

// NamespaceAdmissionFeature requires every writer to use root-scoped ID exclusion.
const NamespaceAdmissionFeature = "namespace-admission-v1"

// NamespaceMutationFeature excludes membership mutation independently of live tasks.
const NamespaceMutationFeature = "namespace-mutation-lease-v1"

// WorkspaceConstrainedConnectorFeature prevents older writers from ignoring
// the approved workspace identity frozen in a guarded request.
const WorkspaceConstrainedConnectorFeature = "workspace-constrained-connector-v1"

// PathProjectConnectorFeature fences older writers from path-bound task records.
const PathProjectConnectorFeature = "connector-path-project-v1"

// WorkspaceConstraint identifies a locally approved directory and a selection
// beneath it. Token uses Native's opaque project-token representation.
type WorkspaceConstraint struct {
	Token           string `json:"token"`
	RelativeProject string `json:"relative_project"`
}

func (w *WorkspaceConstraint) Validate() error {
	if w == nil {
		return nil
	}
	if w.Token == "" || len(w.Token) > MaxControlRecordSize || len(w.RelativeProject) > 4096 || !utf8.ValidString(w.RelativeProject) || !fs.ValidPath(w.RelativeProject) || strings.ContainsAny(w.RelativeProject, "\\\x00\r\n") {
		return ErrIdentityMismatch
	}
	return nil
}

func (w *WorkspaceConstraint) Clone() *WorkspaceConstraint {
	if w == nil {
		return nil
	}
	copy := *w
	return &copy
}

// GatewayOrigin is immutable request correlation, never task lifecycle authority.
type GatewayOrigin struct {
	GatewayID            string `json:"gateway_id"`
	AdmissionGeneration  string `json:"admission_generation"`
	OperationID          string `json:"operation_id"`
	PrincipalRef         string `json:"principal_ref"`
	GatewayRequestSHA256 string `json:"gateway_request_sha256"`
}

// DirectoryIdentity records either a legacy physical binding or a canonical
// path binding. Physical observations used during launch retain the empty kind.
type DirectoryIdentity struct {
	Path   string               `json:"path"`
	Device uint64               `json:"device"`
	Inode  uint64               `json:"inode"`
	Kind   DirectoryBindingKind `json:"kind,omitempty"`
}

type DirectoryBindingKind string

const PathDirectoryBinding DirectoryBindingKind = "path-v1"

// IsPath reports an explicit path binding, never a sentinel inode value.
func (d DirectoryIdentity) IsPath() bool { return d.Kind == PathDirectoryBinding }

func (d DirectoryIdentity) Validate() error {
	if !IsCanonicalCwd(d.Path) {
		return ErrIdentityMismatch
	}
	switch d.Kind {
	case "":
		if d.Inode == 0 {
			return ErrIdentityMismatch
		}
	case PathDirectoryBinding:
		// Path tokens use JSON; invalid UTF-8 would change the approved path.
		if !utf8.ValidString(d.Path) || d.Device != 0 || d.Inode != 0 {
			return ErrIdentityMismatch
		}
	default:
		return ErrIdentityMismatch
	}
	return nil
}

// MatchesObservation compares saved authority with a current physical observation.
// It must not replace exact equality of immutable task requests or receipts.
func (d DirectoryIdentity) MatchesObservation(observed DirectoryIdentity) bool {
	if d.Validate() != nil || observed.Validate() != nil || observed.Kind != "" {
		return false
	}
	if d.IsPath() {
		return d.Path == observed.Path
	}
	return d == observed
}

// ConnectorContext is Native's immutable execution constraint. Gateway-origin
// correlation is separate; local path-bound successors need no Gateway origin.
type ConnectorContext struct {
	RootToken           string               `json:"root_token"`
	Project             DirectoryIdentity    `json:"project"`
	WorkspaceConstraint *WorkspaceConstraint `json:"workspace_constraint,omitempty"`
}

// ProjectLaunchContext carries saved authority separately from a transient
// physical observation. It is borrowed for one command start, never persisted.
type ProjectLaunchContext struct {
	Binding  ConnectorContext
	Observed DirectoryIdentity
}

var opaqueConnectorID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

func ValidateConnectorID(id string) error {
	if !opaqueConnectorID.MatchString(id) {
		return errors.New("connector ID must contain 1..128 ASCII letters, digits, dot, underscore, colon or hyphen")
	}
	return nil
}

func ValidateGatewayOrigin(origin GatewayOrigin) error {
	for _, id := range []string{origin.GatewayID, origin.AdmissionGeneration, origin.OperationID, origin.PrincipalRef} {
		if err := ValidateConnectorID(id); err != nil {
			return err
		}
	}
	return ValidateSHA256(origin.GatewayRequestSHA256)
}

func ValidateConnectorBinding(record *TaskRecord) error {
	if record.Origin == nil && record.ConnectorContext == nil {
		return nil
	}
	if record.ConnectorContext == nil {
		return errors.New("origin requires connector context")
	}
	if record.Origin != nil {
		if err := ValidateGatewayOrigin(*record.Origin); err != nil {
			return err
		}
	}
	binding := record.ConnectorContext
	if err := binding.Project.Validate(); err != nil {
		return err
	}
	if record.Origin == nil && !binding.Project.IsPath() {
		return errors.New("legacy physical connector context requires origin")
	}
	if err := binding.WorkspaceConstraint.Validate(); err != nil {
		return err
	}
	if err := ValidateSHA256(binding.RootToken); err != nil {
		return fmt.Errorf("root token: %w", err)
	}
	if binding.Project.Path != record.CanonicalCwd {
		return ErrIdentityMismatch
	}
	return nil
}

func (origin *GatewayOrigin) Clone() *GatewayOrigin {
	if origin == nil {
		return nil
	}
	copy := *origin
	return &copy
}

func (binding *ConnectorContext) Clone() *ConnectorContext {
	if binding == nil {
		return nil
	}
	copy := *binding
	copy.WorkspaceConstraint = binding.WorkspaceConstraint.Clone()
	return &copy
}
