package remotecommands

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/sshconfig"
	"github.com/hishamkaram/delegation-layer/internal/sshtransport"
)

func probeFixtureResponse() remote.Response {
	return remote.Response{Version: remote.ProtocolVersion, Identity: remote.Identity{Host: "worker", User: "remote", HostKey: "SHA256:fixture", Port: 22, Root: "/state", RootID: strings.Repeat("a", 32)}}
}

func requireProbeStage(t *testing.T, stages []ProbeStage, name, state, code string) ProbeStage {
	t.Helper()
	for _, stage := range stages {
		if stage.Stage != name {
			continue
		}
		if stage.State != state || stage.Code != code {
			t.Fatalf("%s = %#v, want state=%s code=%s", name, stage, state, code)
		}
		return stage
	}
	t.Fatalf("missing stage %s", name)
	return ProbeStage{}
}

func TestProbeStagesDoNotInventSuccessWithoutResponse(t *testing.T) {
	for _, response := range []*remote.Response{nil, {}, {Version: remote.ProtocolVersion}, {Version: remote.ProtocolVersion, Identity: remote.Identity{Root: "/state", RootID: strings.Repeat("a", 32)}}} {
		stages := probeStages(response, nil, "claude:print", "/project")
		for _, stage := range stages {
			if stage.State != "unknown" {
				t.Fatalf("invented success without verified endpoint: %#v", stage)
			}
		}
	}
}

func TestProbeConfigurationSourceIsTypedAndRedacted(t *testing.T) {
	cause := errors.Join(&sshconfig.Error{Code: "unsupported_ssh_directive", File: "/home/user/.ssh/config", Line: 7, Directive: "proxyjump"}, errors.New("SECRET_USER_VALUE"))
	stages := probeStages(nil, cause, "claude:print", "/project")
	stage := requireProbeStage(t, stages, "config", "failed", "unsupported_ssh_directive")
	if stage.File != "/home/user/.ssh/config" || stage.Line != 7 || stage.Directive != "proxyjump" || stage.NextAction == "" {
		t.Fatalf("lost actionable source: %#v", stage)
	}
	requireProbeStage(t, stages, "connectivity", "skipped", "earlier_stage_failed")
	data, err := json.Marshal(stages)
	if err != nil || strings.Contains(string(data), "SECRET_USER_VALUE") {
		t.Fatalf("cause value exposed: %s %v", data, err)
	}
}

func TestProbeTransportStageFailures(t *testing.T) {
	for _, test := range []struct{ code, stage, connectivity, trust string }{
		{"unreachable", "connectivity", "failed", "skipped"},
		{"host_key_unknown", "trust", "succeeded", "failed"},
		{"host_key_changed", "trust", "succeeded", "failed"},
		{"agent_unavailable", "authentication", "unknown", "unknown"},
		{"auth_key_needs_agent", "authentication", "unknown", "unknown"},
		{"authentication_unavailable", "authentication", "unknown", "unknown"},
		{"authentication_failed", "authentication", "succeeded", "succeeded"},
		{"remote_delegate_missing", "endpoint", "succeeded", "succeeded"},
		{"remote_delegate_not_executable", "endpoint", "succeeded", "succeeded"},
		{"remote_endpoint_failed", "endpoint", "succeeded", "succeeded"},
	} {
		t.Run(test.code, func(t *testing.T) {
			stages := probeStages(nil, &sshtransport.Error{Code: test.code}, "claude:print", "/project")
			requireProbeStage(t, stages, test.stage, "failed", test.code)
			for _, stage := range stages {
				if stage.Stage == "connectivity" && stage.State != test.connectivity {
					t.Fatalf("connectivity overclaim: %#v", stage)
				}
				if stage.Stage == "trust" && stage.State != test.trust {
					t.Fatalf("trust overclaim: %#v", stage)
				}
			}
			requireProbeStage(t, stages, "provider_preparation", "skipped", "earlier_stage_failed")
		})
	}
}

func TestProbeAuthenticationExplainsOnlySelectedUnsupportedGSSAPI(t *testing.T) {
	for _, code := range []string{"authentication_unavailable", "authentication_failed", "agent_unavailable"} {
		for _, unavailable := range []bool{false, true} {
			cause := fmt.Errorf("SECRET_AUTH_DATA: %w", &sshtransport.Error{Code: code, GSSAPIUnavailable: unavailable})
			stages := probeStages(nil, cause, "claude:print", "/project")
			stage := requireProbeStage(t, stages, "authentication", "failed", code)
			if strings.Contains(stage.NextAction, "GSSAPI/Kerberos") != unavailable {
				t.Fatalf("incorrect authentication guidance: %+v", stage)
			}
			data, err := json.Marshal(stages)
			if err != nil || strings.Contains(string(data), "SECRET_AUTH_DATA") {
				t.Fatalf("authentication data exposed: %s %v", data, err)
			}
		}
	}
}

func TestProbeRemoteFailureKeepsVerifiedEarlierStages(t *testing.T) {
	for _, test := range []struct{ code, stage string }{{"root_unavailable", "root"}, {"root_identity_changed", "root"}, {"resident_not_ready", "resident"}} {
		t.Run(test.code, func(t *testing.T) {
			response := probeFixtureResponse()
			response.Failure = &remote.Failure{Code: test.code, Message: "SECRET_REMOTE_VALUE"}
			stages := probeStages(&response, &remote.OperationError{Code: test.code, Message: "SECRET_REMOTE_VALUE"}, "claude:print", "/project")
			requireProbeStage(t, stages, "endpoint", "succeeded", "")
			requireProbeStage(t, stages, test.stage, "failed", test.code)
			requireProbeStage(t, stages, "provider_preparation", "skipped", "earlier_stage_failed")
			data, err := json.Marshal(stages)
			if err != nil || strings.Contains(string(data), "SECRET_REMOTE_VALUE") {
				t.Fatalf("remote value exposed: %s %v", data, err)
			}
		})
	}
}

func TestProbeProviderPreparationNeedsDiscriminatedStaticEvidence(t *testing.T) {
	good := `{"schema_version":1,"command":"preflight","status":"ready","capability":{"provider":"claude:print","status":"ready","verification":"preflight","reason_code":"static_admission_ready"}}`
	for _, test := range []struct{ name, data, state, code string }{
		{"ready", good, "succeeded", "static_admission_ready"},
		{"malformed", `{"error":"SECRET`, "failed", "invalid_preflight_response"},
		{"wrong command", strings.Replace(good, `"preflight","status"`, `"status","status"`, 1), "failed", "invalid_preflight_response"},
		{"wrong provider", strings.Replace(good, "claude:print", "codex:exec", 1), "failed", "invalid_preflight_response"},
		{"catalog only", strings.Replace(good, `"verification":"preflight"`, `"verification":"catalog"`, 1), "failed", "invalid_preflight_response"},
		{"blocked", `{"schema_version":1,"command":"preflight","status":"blocked","error":"SECRET_REMOTE_VALUE"}`, "failed", "provider_preparation_failed"},
		{"empty", `{}`, "failed", "invalid_preflight_response"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := probeFixtureResponse()
			response.Envelope = json.RawMessage(test.data)
			stages := probeStages(&response, nil, "claude:print", "/project")
			requireProbeStage(t, stages, "resident", "succeeded", "")
			requireProbeStage(t, stages, "provider_preparation", test.state, test.code)
			requireProbeStage(t, stages, "provider_authentication", "unknown", "preflight_not_authentication")
			data, err := json.Marshal(stages)
			if err != nil || strings.Contains(string(data), "SECRET_REMOTE_VALUE") {
				t.Fatalf("provider text exposed: %s %v", data, err)
			}
		})
	}
}

func TestProbeProviderPreparationSkippedWithoutSelection(t *testing.T) {
	response := probeFixtureResponse()
	for _, test := range []struct{ provider, cwd, code string }{{"", "/project", "provider_not_selected"}, {"claude:print", "", "remote_cwd_not_selected"}, {"", "", "provider_not_selected"}} {
		stages := probeStages(&response, nil, test.provider, test.cwd)
		requireProbeStage(t, stages, "provider_preparation", "skipped", test.code)
		requireProbeStage(t, stages, "resident", "succeeded", "")
	}
}

func TestProbeStagesPreserveJoinedFaultsAndBoundUnknownCodes(t *testing.T) {
	stages := probeStages(nil, errors.Join(&sshtransport.Error{Code: "agent_unavailable"}, &remote.OperationError{Code: "root_unavailable"}), "claude:print", "/project")
	requireProbeStage(t, stages, "authentication", "failed", "agent_unavailable")
	requireProbeStage(t, stages, "root", "failed", "root_unavailable")
	stages = probeStages(nil, &sshtransport.Error{Code: "SECRET_CODE"}, "claude:print", "/project")
	requireProbeStage(t, stages, "endpoint", "failed", "ssh_transport_failed")
}
