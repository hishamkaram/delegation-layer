package app

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/config"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

var errNoReadyProvider = errors.New("no provider is ready for the requested delegation")

const (
	preflightStatusReady       = "ready"
	preflightStatusBlocked     = "blocked"
	preflightStatusUnsupported = "unsupported"
)

// selectAutoProvider performs provider-owned static admission before a task is
// created. The selected provider is dispatched once by the normal path, which
// performs the stronger supervised runtime probe. A runtime failure after
// selection is returned to the caller; this function never silently retries a
// dispatched task with another provider.
func selectAutoProvider(deps Dependencies, root, rootID, taskID, cwd string, requested task.TaskConfig, brief []byte) (string, error) {
	if requested.Permission == "" {
		requested.Permission = config.DefaultMode
	}
	providers := deps.normalized().Catalog.Descriptions()
	var reasons []string
	considered := 0
	authenticationBlocked := true
	for _, description := range providers {
		if !slices.Contains(description.SupportedModes, requested.Permission) {
			continue
		}
		considered++
		request, err := buildRequest(rootID, taskID, description.ID, cwd, requested, brief, nil)
		if err != nil {
			return "", err
		}
		err = staticAutoProviderError(deps, root, request)
		if err != nil {
			if !errors.Is(err, commonprovider.ErrAuthenticationBlocked) {
				authenticationBlocked = false
			}
			reasons = append(reasons, boundedSelectionReason(description.ID, err))
			continue
		}
		return description.ID, nil
	}
	if len(reasons) == 0 {
		return "", fmt.Errorf("%w: no provider supports permission %s", errNoReadyProvider, requested.Permission)
	}
	if considered > 0 && authenticationBlocked {
		return "", fmt.Errorf("%w: %w: %s", commonprovider.ErrAuthenticationBlocked, errNoReadyProvider, strings.Join(reasons, "; "))
	}
	return "", fmt.Errorf("%w: %s", errNoReadyProvider, strings.Join(reasons, "; "))
}

func staticAutoProviderError(deps Dependencies, root string, request task.TaskRecord) error {
	normalized := deps.normalized()
	if err := normalized.Catalog.ValidateRequest(request); err != nil {
		return err
	}
	candidate, err := prepareCandidate(normalized, root, request)
	if err != nil {
		return err
	}
	if err = candidate.ValidateStatePlacement(root); err != nil {
		return err
	}
	return finalizeStaticCandidate(candidate, request)
}

func boundedSelectionReason(provider string, err error) string {
	if err == nil {
		return provider + ": unavailable"
	}
	message := err.Error()
	if len(message) > 240 {
		message = message[:240] + "..."
	}
	return provider + ": " + message
}

func preflight(a Arguments, deps Dependencies) commandResult {
	response := newResponse("preflight")
	root, err := resolveRoot(a.Root)
	if err != nil {
		return failed(response, err, 1)
	}
	_, cwd, err := validateRootAndCwd(root, a.Cwd)
	if err != nil {
		return failed(response, err, 2)
	}
	providerID := a.Provider
	request, err := buildRequest(strings.Repeat("0", 32), strings.Repeat("1", 32), providerID, cwd, a.Config, []byte("preflight"), nil)
	if err != nil {
		return failed(response, err, 2)
	}
	normalized := deps.normalized()
	registration, lookupErr := normalized.Catalog.Lookup(providerID)
	if lookupErr != nil {
		return failed(response, lookupErr, 2)
	}
	report := declaredCapability(registration.Description)
	report.Verification = "preflight"
	report.ReasonCode = "preflight_not_launched"
	response.Capability = &report
	candidate, candidateErr := prepareCandidate(normalized, root, request)
	if candidateErr != nil {
		report.Status = capabilityStatusUnsupported
		report.ReasonCode = "static_admission_rejected"
		if errors.Is(candidateErr, commonprovider.ErrAuthenticationBlocked) {
			response.Status = preflightStatusBlocked
			report.LiveAcceptance.Status = acceptanceStatusBlocked
			report.LiveAcceptance.Authentication = authenticationStatusBlocked
			report.LiveAcceptance.ReasonCode = "authentication_unavailable"
		} else {
			response.Status = preflightStatusUnsupported
		}
		return failed(response, candidateErr, classifyCode(candidateErr, 2))
	}
	if candidateErr = candidate.ValidateStatePlacement(root); candidateErr != nil {
		response.Status = preflightStatusUnsupported
		report.Status = capabilityStatusUnsupported
		report.ReasonCode = "state_placement_rejected"
		return failed(response, candidateErr, classifyCode(candidateErr, 2))
	}
	if candidateErr = finalizeStaticCandidate(candidate, request); candidateErr != nil {
		report.Status = capabilityStatusUnsupported
		report.ReasonCode = "static_admission_rejected"
		if errors.Is(candidateErr, commonprovider.ErrAuthenticationBlocked) {
			response.Status = preflightStatusBlocked
			report.LiveAcceptance.Status = acceptanceStatusBlocked
			report.LiveAcceptance.Authentication = authenticationStatusBlocked
			report.LiveAcceptance.ReasonCode = "authentication_unavailable"
		} else {
			response.Status = preflightStatusUnsupported
		}
		return failed(response, candidateErr, classifyCode(candidateErr, 2))
	}
	response.Status = preflightStatusReady
	report.Status = capabilityStatusReady
	report.ReasonCode = "static_admission_ready"
	report.LiveAcceptance.Authentication = authenticationStatusUnknown
	response.Capability = &report
	return commandResult{response: response, code: 0}
}

func finalizeStaticCandidate(candidate commonprovider.ProfileCandidate, request task.TaskRecord) error {
	if candidate.Inspection != nil {
		return nil
	}
	_, err := finalizeCandidate(candidate, request, nil)
	return err
}
