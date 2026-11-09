package app

import (
	"errors"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/config"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

func preflightCommandResult(response Response, observed admissioncore.PreflightResult, err error) commandResult {
	if observed.Description == nil {
		return failed(response, err, 2)
	}
	report := declaredCapability(*observed.Description)
	report.ProviderExecutable = observed.ProviderExecutable
	report.Verification = "preflight"
	report.ReasonCode = "preflight_not_launched"
	response.Capability = &report
	if err != nil {
		report.Status = capabilityStatusUnsupported
		reason := staticPreflightRejection(observed, err)
		if reason == "state_placement_rejected" {
			response.Status = preflightStatusUnsupported
			report.ReasonCode = "state_placement_rejected"
		} else {
			report.ReasonCode = "static_admission_rejected"
			if reason == "authentication_unavailable" {
				response.Status = preflightStatusBlocked
				report.LiveAcceptance.Status = acceptanceStatusBlocked
				report.LiveAcceptance.Authentication = authenticationStatusBlocked
				report.LiveAcceptance.ReasonCode = "authentication_unavailable"
			} else {
				response.Status = preflightStatusUnsupported
			}
		}
		return failed(response, err, classifyCode(err, 2))
	}
	response.Status = preflightStatusReady
	report.Status = capabilityStatusReady
	report.ReasonCode = "static_admission_ready"
	report.LiveAcceptance.Authentication = authenticationStatusUnknown
	response.Capability = &report
	return commandResult{response: response, code: 0}
}

// staticPreflightRejection classifies only observed static facts. It makes no
// claim about live authentication or whether a provider turn can execute.
func staticPreflightRejection(observed admissioncore.PreflightResult, err error) string {
	if observed.Stage == admissioncore.PreflightPlacement || errors.Is(err, config.ErrRootWorkspaceOverlap) {
		return "state_placement_rejected"
	}
	if errors.Is(err, commonprovider.ErrAuthenticationBlocked) {
		return "authentication_unavailable"
	}
	return "static_admission_rejected"
}
