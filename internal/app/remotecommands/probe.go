package remotecommands

import (
	"encoding/json"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/sshconfig"
	"github.com/hishamkaram/delegation-layer/internal/sshtransport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ProbeStage is an observation, never permission to initialize or repair a host.
// The command owns these derived values; source owners retain all authority.
type ProbeStage struct {
	Stage      string `json:"stage"`
	State      string `json:"state"`
	Code       string `json:"code,omitempty"`
	NextAction string `json:"next_action,omitempty"`
	File       string `json:"file,omitempty"`
	Line       int    `json:"line,omitempty"`
	Directive  string `json:"directive,omitempty"`
}

func probeStages(response *remote.Response, cause error, provider, cwd string) []ProbeStage {
	stages := []ProbeStage{
		{Stage: "config", State: "unknown"},
		{Stage: "connectivity", State: "unknown"},
		{Stage: "trust", State: "unknown"},
		{Stage: "authentication", State: "unknown"},
		{Stage: "endpoint", State: "unknown"},
		{Stage: "root", State: "unknown"},
		{Stage: "resident", State: "unknown"},
		{Stage: "provider_preparation", State: "unknown"},
		{Stage: "provider_authentication", State: "unknown", Code: "preflight_not_authentication"},
	}
	verified := probeEndpointVerified(response)
	if verified {
		probeSucceededThrough(stages, "endpoint")
		if response.Identity.Validate() == nil {
			probeSet(stages, "root", "succeeded", "", "")
			if response.Failure == nil {
				probeSet(stages, "resident", "succeeded", "", "")
			}
		}
	}
	if response != nil && response.Failure != nil {
		probeRemoteFailure(stages, response.Failure.Code)
	}
	probeCause(stages, cause)
	if provider == "" || cwd == "" {
		code := "provider_not_selected"
		if provider != "" {
			code = "remote_cwd_not_selected"
		}
		probeSet(stages, "provider_preparation", "skipped", code, "Specify --provider and --remote-cwd to check static provider preparation.")
	} else if verified && response.Failure == nil && len(response.Envelope) > 0 {
		probeProvider(stages, response.Envelope, provider)
	}
	return stages
}

func probeEndpointVerified(response *remote.Response) bool {
	return response != nil && response.Version == remote.ProtocolVersion && response.Identity.Host != "" && response.Identity.HostKey != "" && response.Identity.User != "" && response.Identity.Port > 0 && response.Identity.Port <= 65535
}

func probeSet(stages []ProbeStage, name, state, code, action string) {
	for i := range stages {
		if stages[i].Stage == name {
			stages[i].State, stages[i].Code, stages[i].NextAction = state, code, action
			return
		}
	}
}

func probeSucceededThrough(stages []ProbeStage, name string) {
	for i := range stages {
		if stages[i].State == "unknown" {
			stages[i].State = "succeeded"
		}
		if stages[i].Stage == name {
			return
		}
	}
}

func probeFailed(stages []ProbeStage, name, code, action string) {
	found := false
	for i := range stages {
		if stages[i].Stage == name {
			stages[i].State, stages[i].Code, stages[i].NextAction = "failed", code, action
			found = true
			continue
		}
		if found && stages[i].State == "unknown" && stages[i].Stage != "provider_authentication" {
			stages[i].State = "skipped"
			stages[i].Code = "earlier_stage_failed"
		}
	}
}

// Visit the whole error tree so one classified fault cannot hide a joined one.
// Native error strings are deliberately never copied into stage diagnostics.
func probeCause(stages []ProbeStage, cause error) {
	task.AnyErrorNode(cause, func(node error) bool {
		probeCauseNode(stages, node)
		return false
	})
}

func probeCauseNode(stages []ProbeStage, cause error) {
	var configuration *sshconfig.Error
	var transport *sshtransport.Error
	var operation *remote.OperationError
	switch {
	case errors.As(cause, &configuration):
		code := configuration.Code
		switch code {
		case "unsupported_ssh_directive", "invalid_ssh_setting", "invalid_ssh_expansion", "ssh_home_unavailable", "invalid_ssh_config_path", "ssh_config_include_depth", "ssh_config_include_cycle", "ssh_config_read_failed", "ssh_config_limit", "ssh_config_parse_failed", "invalid_ssh_alias", "ssh_user_unconfigured":
		default:
			code = "ssh_config_invalid"
		}
		probeFailed(stages, "config", code, "Correct the indicated SSH configuration or selected alias.")
		stages[0].File, stages[0].Line, stages[0].Directive = configuration.File, configuration.Line, configuration.Directive
		return
	case errors.As(cause, &transport):
		probeTransportFailure(stages, transport)
		return
	case errors.As(cause, &operation):
		probeRemoteFailure(stages, operation.Code)
		return
	}
	if errors.Is(cause, remote.ErrProtocol) {
		probeFailed(stages, "endpoint", "protocol_invalid", "Check that the destination runs a compatible Delegate installation.")
	}
	if errors.Is(cause, remote.ErrIdentity) {
		probeFailed(stages, "root", "identity_mismatch", "Inspect the selected destination and root identity before retrying.")
	}
}

func probeTransportFailure(stages []ProbeStage, failure *sshtransport.Error) {
	code := failure.Code
	switch code {
	case "unreachable":
		probeSucceededThrough(stages, "config")
		probeFailed(stages, "connectivity", code, "Check the selected host, SSH port and network connection.")
	case "host_key_unknown", "host_key_changed", "host_key_revoked", "host_key_verification_failed", "host_key_identity_mismatch", "host_key_fingerprint_mismatch", "host_key_enrollment_failed":
		probeSucceededThrough(stages, "connectivity")
		probeFailed(stages, "trust", code, "Verify the host fingerprint independently; use devices trust only for a verified unknown key.")
	case "authentication_unavailable", "agent_unavailable", "identity_unreadable", "identity_invalid", "auth_key_needs_agent":
		probeSucceededThrough(stages, "config")
		probeFailed(stages, "authentication", code, probeAuthenticationAction("Check the selected authentication policy, SSH key and local agent.", failure))
	case "authentication_failed":
		probeSucceededThrough(stages, "trust")
		probeFailed(stages, "authentication", code, probeAuthenticationAction("Check that the selected SSH identity is authorized for the remote user.", failure))
	case "ssh_handshake_failed":
		probeSucceededThrough(stages, "connectivity")
		probeFailed(stages, "endpoint", code, "Check SSH protocol compatibility and the selected destination.")
	case "remote_delegate_missing", "remote_delegate_not_executable", "remote_endpoint_failed":
		probeSucceededThrough(stages, "authentication")
		probeFailed(stages, "endpoint", code, "Check the installed remote Delegate and --remote-executable selection.")
	case "ssh_environment_limit", "ssh_environment_invalid":
		probeFailed(stages, "config", code, "Correct or reduce the variables selected by SendEnv.")
	default:
		probeFailed(stages, "endpoint", "ssh_transport_failed", "Inspect the SSH connection failure before retrying.")
	}
}

func probeAuthenticationAction(action string, failure *sshtransport.Error) string {
	if failure.GSSAPIUnavailable {
		return action + " Embedded SSH does not support GSSAPI/Kerberos; select an SSH key or local agent."
	}
	return action
}

func probeRemoteFailure(stages []ProbeStage, code string) {
	switch code {
	case "root_unavailable", "root_identity_changed", "root_pin_changed", "identity_mismatch", "identity_required":
		probeFailed(stages, "root", code, "Check the existing remote state root and its recorded identity.")
	case "resident_not_ready":
		probeFailed(stages, "resident", code, "Run delegate supervisor ensure-owner --json from a persistent user context on the destination.")
	default:
		probeFailed(stages, "endpoint", "remote_probe_failed", "Check the destination Delegate response and compatibility.")
	}
}

// This consumer reads only the discriminated static-preflight facts it needs.
// Other public envelope fields remain owned and validated by the frontend.
type probePreflight struct {
	SchemaVersion int    `json:"schema_version"`
	Command       string `json:"command"`
	Status        string `json:"status"`
	Error         string `json:"error"`
	Capability    *struct {
		Provider     string `json:"provider"`
		Status       string `json:"status"`
		Verification string `json:"verification"`
		ReasonCode   string `json:"reason_code"`
	} `json:"capability"`
}

func probeProvider(stages []ProbeStage, data json.RawMessage, provider string) {
	var facts probePreflight
	if err := json.Unmarshal(data, &facts); err != nil || facts.SchemaVersion != 1 || facts.Command != "preflight" {
		probeFailed(stages, "provider_preparation", "invalid_preflight_response", "Inspect the destination's static preflight response.")
		return
	}
	if facts.Error != "" || facts.Status == "blocked" || facts.Status == "unsupported" {
		probeFailed(stages, "provider_preparation", "provider_preparation_failed", "Inspect provider installation and static configuration on the destination.")
		return
	}
	if facts.Status == "ready" && facts.Capability != nil && facts.Capability.Provider == provider && facts.Capability.Status == "ready" && facts.Capability.Verification == "preflight" && facts.Capability.ReasonCode == "static_admission_ready" {
		probeSet(stages, "provider_preparation", "succeeded", "static_admission_ready", "")
		return
	}
	probeFailed(stages, "provider_preparation", "invalid_preflight_response", "Inspect the destination's static preflight response.")
}
