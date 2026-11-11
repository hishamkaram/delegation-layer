package task

import (
	"errors"
	"fmt"
	"regexp"
)

// NamespaceAdmissionFeature requires every writer to use root-scoped ID exclusion.
const NamespaceAdmissionFeature = "namespace-admission-v1"

// GatewayOrigin is immutable request correlation, never task lifecycle authority.
type GatewayOrigin struct {
	GatewayID            string `json:"gateway_id"`
	AdmissionGeneration  string `json:"admission_generation"`
	OperationID          string `json:"operation_id"`
	PrincipalRef         string `json:"principal_ref"`
	GatewayRequestSHA256 string `json:"gateway_request_sha256"`
}

// DirectoryIdentity binds a canonical directory, independently of its contents.
type DirectoryIdentity struct {
	Path   string `json:"path"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

// ConnectorContext is the immutable physical root/project binding for execution.
type ConnectorContext struct {
	RootToken string            `json:"root_token"`
	Project   DirectoryIdentity `json:"project"`
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
	if record.Origin == nil || record.ConnectorContext == nil {
		return errors.New("origin and connector context must be supplied together")
	}
	if err := ValidateGatewayOrigin(*record.Origin); err != nil {
		return err
	}
	binding := record.ConnectorContext
	if err := ValidateSHA256(binding.RootToken); err != nil {
		return fmt.Errorf("root token: %w", err)
	}
	if binding.Project.Path != record.CanonicalCwd || !IsCanonicalCwd(binding.Project.Path) || binding.Project.Inode == 0 {
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
	return &copy
}
