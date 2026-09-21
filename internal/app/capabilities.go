package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

const (
	capabilityStatusUnknown       = "unknown"
	capabilityStatusReady         = "ready"
	capabilityStatusUnsupported   = "unsupported"
	capabilityVerificationCatalog = "catalog"
	capabilityVerificationRuntime = "runtime"
	capabilityReasonNotRun        = "runtime_probe_not_run"
	capabilityReasonReady         = "required_behavior_observed"
	capabilityReasonUnavailable   = "provider_unavailable"
	acceptanceStatusNotRun        = "not_run"
	acceptanceStatusBlocked       = "blocked"
	authenticationStatusUnknown   = "unknown"
	authenticationStatusBlocked   = "blocked"
	// maxCapabilityVersionBytes bounds the serialized observation carried in a
	// dispatch response. It is an output bound, not a provider release policy.
	maxCapabilityVersionBytes = 4 * 1024
)

// CapabilityResponse is the side-effect-free provider contract response. It
// describes the compiled adapter contract and explicitly distinguishes that
// declaration from a runtime probe or live provider acceptance.
type CapabilityResponse struct {
	SchemaVersion int              `json:"schema_version"`
	Command       string           `json:"command"`
	Capability    CapabilityReport `json:"capability"`
	Error         string           `json:"error,omitempty"`
}

// CapabilityReport is the provider-neutral readiness projection consumed by
// agents. Runtime versions and executable digests are observations only; they
// never authorize a release or replace behavioral checks.
type CapabilityReport struct {
	Contract         string               `json:"contract"`
	Provider         string               `json:"provider"`
	Status           string               `json:"status"`
	Verification     string               `json:"verification"`
	Continuation     string               `json:"continuation"`
	Version          string               `json:"version,omitempty"`
	ExecutableSHA256 string               `json:"executable_sha256,omitempty"`
	HelpArgs         []string             `json:"help_args,omitempty"`
	RequiredFlags    []string             `json:"required_flags"`
	ReasonCode       string               `json:"reason_code"`
	LiveAcceptance   LiveAcceptanceReport `json:"live_acceptance"`
}

// LiveAcceptanceReport keeps authentication and real-provider proof separate
// from local capability compatibility. A capability result cannot claim live
// acceptance when this value is not_run or blocked.
type LiveAcceptanceReport struct {
	Status         string `json:"status"`
	Authentication string `json:"authentication"`
	ReasonCode     string `json:"reason_code"`
}

func runCapabilities(jsonOutput bool, stdout, stderr io.Writer, catalog commonprovider.Catalog, providerID string) int {
	response := CapabilityResponse{
		SchemaVersion: OutputSchemaVersion,
		Command:       "capabilities",
		Capability: CapabilityReport{
			Contract:      commonprovider.RuntimeInspectionRevision,
			Provider:      providerID,
			Status:        capabilityStatusUnsupported,
			Verification:  capabilityVerificationCatalog,
			Continuation:  string(commonprovider.ContinuationUnsupported),
			RequiredFlags: []string{},
			ReasonCode:    capabilityReasonUnavailable,
			LiveAcceptance: LiveAcceptanceReport{
				Status:         acceptanceStatusNotRun,
				Authentication: authenticationStatusUnknown,
				ReasonCode:     capabilityReasonNotRun,
			},
		},
	}
	registration, err := catalog.Lookup(providerID)
	if err == nil && (!registration.Description.Discoverable || registration.Prepare == nil) {
		err = fmt.Errorf("%w: %s has no launch profile", commonprovider.ErrProfileUnavailable, providerID)
	}
	if err != nil {
		response.Error = err.Error()
	} else {
		response.Capability = declaredCapability(registration.Description)
	}

	if jsonOutput {
		if err := writeCapabilityJSON(stdout, response); err != nil {
			return writeProviderError(stderr, err)
		}
		if response.Error != "" {
			return 2
		}
		return 0
	}
	if err := writeCapabilityHuman(stdout, response); err != nil {
		return 1
	}
	if response.Error != "" {
		return writeCLIError(stderr, fmt.Errorf("provider capability unavailable"), 2)
	}
	return 0
}

func declaredCapability(description commonprovider.Description) CapabilityReport {
	return CapabilityReport{
		Contract:      commonprovider.RuntimeInspectionRevision,
		Provider:      description.ID,
		Status:        capabilityStatusUnknown,
		Verification:  capabilityVerificationCatalog,
		Continuation:  capabilityContinuation(description),
		HelpArgs:      cloneCapabilityStrings(description.Runtime.HelpArgs),
		RequiredFlags: cloneCapabilityStrings(description.Runtime.RequiredFlags),
		ReasonCode:    capabilityReasonNotRun,
		LiveAcceptance: LiveAcceptanceReport{
			Status:         acceptanceStatusNotRun,
			Authentication: authenticationStatusUnknown,
			ReasonCode:     capabilityReasonNotRun,
		},
	}
}

func capabilityContinuation(description commonprovider.Description) string {
	if description.Continuation == "" {
		return string(commonprovider.ContinuationUnsupported)
	}
	return string(description.Continuation)
}

func cloneCapabilityStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return slices.Clone(values)
}

func runtimeCapability(providerID string, candidate commonprovider.ProfileCandidate, profile commonprovider.PreparedProfile, descriptions ...commonprovider.Description) (CapabilityReport, error) {
	description := commonprovider.Description{ID: providerID}
	if len(descriptions) > 0 {
		description = descriptions[0]
	}
	if candidate.Inspection == nil || candidate.Inspection.Runtime == nil {
		return declaredCapability(description), nil
	}
	runtime := candidate.Inspection.Runtime
	report := CapabilityReport{
		Contract:      commonprovider.RuntimeInspectionRevision,
		Provider:      providerID,
		Status:        capabilityStatusReady,
		Verification:  capabilityVerificationRuntime,
		Continuation:  capabilityContinuation(description),
		ReasonCode:    capabilityReasonReady,
		RequiredFlags: cloneCapabilityStrings(runtime.RequiredFlags),
		LiveAcceptance: LiveAcceptanceReport{
			Status:         acceptanceStatusNotRun,
			Authentication: authenticationStatusUnknown,
			ReasonCode:     capabilityReasonNotRun,
		},
	}
	if len(profile.ObservedVersion) <= maxCapabilityVersionBytes {
		report.Version = profile.ObservedVersion
	}
	report.ExecutableSHA256 = runtime.ExecutableSHA256
	report.HelpArgs = cloneCapabilityStrings(runtime.HelpArgs)
	return report, nil
}

func writeCapabilityJSON(w io.Writer, response CapabilityResponse) error {
	data, err := encodeCapabilityResponse(response)
	if err != nil {
		return err
	}
	if len(data) > MaxJSONResponseBytes {
		return ErrJSONResponseTooLarge
	}
	return writeAll(w, data)
}

func encodeCapabilityResponse(response CapabilityResponse) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(response); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func writeCapabilityHuman(w io.Writer, response CapabilityResponse) error {
	line := fmt.Sprintf("command=%s provider=%s status=%s verification=%s live_acceptance=%s\n", response.Command, response.Capability.Provider, response.Capability.Status, response.Capability.Verification, response.Capability.LiveAcceptance.Status)
	return writeAll(w, []byte(line))
}
