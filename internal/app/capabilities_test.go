package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

func TestCapabilitiesJSONIsBoundedDeterministicAndDoesNotInspectHost(t *testing.T) {
	t.Setenv("HOME", filepath.Join(t.TempDir(), "missing-home"))
	first, firstResponse := capabilitiesJSON(t)
	second, _ := capabilitiesJSON(t)
	if !bytes.Equal(first, second) {
		t.Fatalf("capability output changed between calls:\n%s\n%s", first, second)
	}
	if len(first) > MaxJSONResponseBytes {
		t.Fatalf("capability response exceeded bound: %d", len(first))
	}
	assertDeclaredCapability(t, firstResponse)
	assertNoTaskFields(t, first)
}

func capabilitiesJSON(t *testing.T) ([]byte, CapabilityResponse) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"capabilities", "--provider", "pi:json", "--json"}, &stdout, &stderr, Dependencies{}); code != 0 {
		t.Fatalf("capabilities failed: code=%d output=%q", code, stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("capabilities wrote stderr: %q", stderr.String())
	}
	var response CapabilityResponse
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return stdout.Bytes(), response
}

func assertDeclaredCapability(t *testing.T, response CapabilityResponse) {
	t.Helper()
	if response.SchemaVersion != OutputSchemaVersion || response.Command != "capabilities" {
		t.Fatalf("unexpected response envelope: %+v", response)
	}
	capability := response.Capability
	if capability.Provider != "pi:json" || capability.Status != capabilityStatusUnknown || capability.Verification != capabilityVerificationCatalog {
		t.Fatalf("unexpected declaration: %+v", capability)
	}
	if capability.Contract != commonprovider.RuntimeInspectionRevision || len(capability.RequiredFlags) == 0 {
		t.Fatalf("capability contract is incomplete: %+v", capability)
	}
	if capability.LiveAcceptance.Status != acceptanceStatusNotRun || capability.LiveAcceptance.Authentication != authenticationStatusUnknown {
		t.Fatalf("static command claimed live proof: %+v", capability.LiveAcceptance)
	}
}

func assertNoTaskFields(t *testing.T, data []byte) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"task_id", "admission", "liveness", "publication", "supervisor"} {
		if _, found := fields[forbidden]; found {
			t.Fatalf("capability response contains task field %q: %s", forbidden, data)
		}
	}
}

func TestCapabilitiesUnknownProviderReturnsMachineReadableRejection(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"capabilities", "--provider", "missing:provider", "--json"}, &stdout, &stderr, Dependencies{})
	if code != 2 {
		t.Fatalf("unknown provider exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var response CapabilityResponse
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Capability.Status != capabilityStatusUnsupported || response.Capability.ReasonCode != capabilityReasonUnavailable {
		t.Fatalf("unexpected unknown-provider capability: %+v", response.Capability)
	}
	if response.Capability.RequiredFlags == nil {
		t.Fatalf("unknown provider omitted empty required_flags array: %+v", response.Capability)
	}
	if response.Capability.LiveAcceptance.Status != acceptanceStatusNotRun {
		t.Fatalf("unknown provider claimed live acceptance: %+v", response.Capability.LiveAcceptance)
	}
	if response.Error == "" || stderr.Len() != 0 {
		t.Fatalf("unexpected unknown-provider streams: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestCapabilitiesRejectsHistoricalOnlyProvider(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runCapabilities(true, &stdout, &stderr, NativeCatalog(), config.ProviderFixture)
	if code != 2 {
		t.Fatalf("historical provider exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var response CapabilityResponse
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Capability.Status != capabilityStatusUnsupported || response.Error == "" || stderr.Len() != 0 {
		t.Fatalf("unexpected historical-provider response: stdout=%q stderr=%q response=%+v", stdout.String(), stderr.String(), response)
	}
	if response.Capability.RequiredFlags == nil {
		t.Fatalf("historical provider omitted empty required_flags array: %+v", response.Capability)
	}
}

func TestRuntimeCapabilityReportRequiresObservedBehaviorForReady(t *testing.T) {
	digest := strings.Repeat("a", 64)
	candidate := commonprovider.ProfileCandidate{
		Inspection: &commonprovider.InspectionDefinition{
			ExecutableSHA256: digest,
			Runtime: &commonprovider.RuntimeProbeDefinition{
				ExecutableSHA256: digest,
				HelpArgs:         []string{"exec"},
				RequiredFlags:    []string{"--json", "--sandbox"},
			},
		},
	}
	profile := commonprovider.PreparedProfile{
		Plan:            execution.Plan{Executable: "/absolute/provider"},
		ObservedVersion: "release-without-a-policy-pin",
	}
	report, err := runtimeCapability("example:run", candidate, profile)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != capabilityStatusReady || report.Verification != capabilityVerificationRuntime {
		t.Fatalf("runtime probe did not produce ready report: %+v", report)
	}
	if report.Version != profile.ObservedVersion || report.ExecutableSHA256 != digest || len(report.RequiredFlags) != 2 {
		t.Fatalf("runtime observations were not projected: %+v", report)
	}
	if report.LiveAcceptance.Status != acceptanceStatusNotRun {
		t.Fatalf("runtime probe claimed live acceptance: %+v", report.LiveAcceptance)
	}
}

func TestRuntimeCapabilityReportDoesNotClaimReadyWithoutProbe(t *testing.T) {
	report, err := runtimeCapability("example:run", commonprovider.ProfileCandidate{}, commonprovider.PreparedProfile{ObservedVersion: "arbitrary-release"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != capabilityStatusUnknown || report.Verification != capabilityVerificationCatalog || report.ReasonCode != capabilityReasonNotRun {
		t.Fatalf("static profile claimed runtime readiness: %+v", report)
	}
}

func TestRuntimeCapabilityOmitsOversizedVersionObservation(t *testing.T) {
	digest := strings.Repeat("a", 64)
	candidate := commonprovider.ProfileCandidate{Inspection: &commonprovider.InspectionDefinition{Runtime: &commonprovider.RuntimeProbeDefinition{ExecutableSHA256: digest}}}
	profile := commonprovider.PreparedProfile{ObservedVersion: strings.Repeat("v", maxCapabilityVersionBytes+1)}
	report, err := runtimeCapability("example:run", candidate, profile)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != capabilityStatusReady || report.Version != "" {
		t.Fatalf("oversized version was not omitted from the bounded projection: %+v", report)
	}
}

func TestTaskResponseEmbedsCapabilityProjection(t *testing.T) {
	response := newResponse("dispatch")
	report := CapabilityReport{
		Contract:      commonprovider.RuntimeInspectionRevision,
		Provider:      "example:run",
		Status:        capabilityStatusReady,
		Verification:  capabilityVerificationRuntime,
		Version:       "arbitrary-release",
		RequiredFlags: []string{"--json"},
		ReasonCode:    capabilityReasonReady,
		LiveAcceptance: LiveAcceptanceReport{
			Status:         acceptanceStatusNotRun,
			Authentication: authenticationStatusUnknown,
			ReasonCode:     capabilityReasonNotRun,
		},
	}
	response.Capability = &report
	var output bytes.Buffer
	if err := writeJSON(&output, response); err != nil {
		t.Fatal(err)
	}
	var decoded Response
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Capability == nil || decoded.Capability.Status != capabilityStatusReady || decoded.Capability.Version != report.Version {
		t.Fatalf("dispatch response omitted capability projection: %+v", decoded)
	}
}

func TestCapabilityJSONRejectsOversizedDiagnostics(t *testing.T) {
	response := CapabilityResponse{
		SchemaVersion: OutputSchemaVersion,
		Command:       "capabilities",
		Capability: CapabilityReport{
			Contract:      commonprovider.RuntimeInspectionRevision,
			Provider:      "example:run",
			Status:        capabilityStatusUnknown,
			Verification:  capabilityVerificationCatalog,
			RequiredFlags: []string{strings.Repeat("x", MaxJSONResponseBytes)},
			ReasonCode:    capabilityReasonNotRun,
			LiveAcceptance: LiveAcceptanceReport{
				Status:         acceptanceStatusNotRun,
				Authentication: authenticationStatusUnknown,
				ReasonCode:     capabilityReasonNotRun,
			},
		},
	}
	if err := writeCapabilityJSON(&bytes.Buffer{}, response); !errors.Is(err, ErrJSONResponseTooLarge) {
		t.Fatalf("oversized capability response error=%v, want %v", err, ErrJSONResponseTooLarge)
	}
}

func TestCapabilitiesDoesNotCreateState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if code := Run([]string{"capabilities", "--provider", "opencode:run", "--root", root, "--json"}, &bytes.Buffer{}, &bytes.Buffer{}, Dependencies{}); code != 0 {
		t.Fatalf("capabilities failed: %d", code)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("capabilities created state root: %v", err)
	}
}
