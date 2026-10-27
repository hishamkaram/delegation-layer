package app

import (
	"fmt"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/resourceadmission"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// Resource admission statuses and warning codes on the public wire contract.
const (
	ResourceAdmissionClear    = "clear"
	ResourceAdmissionPressure = "pressure"
	ResourceAdmissionUnknown  = "unknown"

	ResourceObservationUnavailable = "resource_observation_unavailable"
	ResourceAdvisory               = "resource_advisory"
)

// resourceRetryAfterSeconds is the advisory delay for retry_later guidance.
const resourceRetryAfterSeconds = 30

// ResourceAdmissionResponse is the invocation-local resource check summary
// for a fresh admission. It is not durable and never changes the command's
// exit code, admission, liveness, publication, failure, or outcome by itself.
type ResourceAdmissionResponse struct {
	Status   string                     `json:"status"`
	Warnings []ResourceAdmissionWarning `json:"warnings,omitempty"`
}

// ResourceAdmissionWarning is one bounded, sanitized resource observation
// warning. It carries no timestamps, process IDs, paths, or raw errors.
type ResourceAdmissionWarning struct {
	Code       string `json:"code"`
	Metric     string `json:"metric,omitempty"`
	ReasonCode string `json:"reason_code"`
	Message    string `json:"message"`
}

func writeHumanResourceAdmission(output *strings.Builder, resource *ResourceAdmissionResponse) {
	if resource == nil {
		return
	}
	fmt.Fprintf(output, "resource_admission=%s\n", resource.Status)
	for _, warning := range resource.Warnings {
		fmt.Fprintf(output, "resource_warning code=%s reason_code=%s message=%s", warning.Code, warning.ReasonCode, warning.Message)
		if warning.Metric != "" {
			fmt.Fprintf(output, " metric=%s", warning.Metric)
		}
		output.WriteByte('\n')
	}
}

// truncateResourceAdmission applies the shared diagnostic truncation to each
// warning message of a copy, leaving the original response unchanged.
func truncateResourceAdmission(resource *ResourceAdmissionResponse, budget int) *ResourceAdmissionResponse {
	if resource == nil {
		return nil
	}
	candidate := *resource
	if resource.Warnings != nil {
		candidate.Warnings = append([]ResourceAdmissionWarning(nil), resource.Warnings...)
		for i := range candidate.Warnings {
			candidate.Warnings[i].Message, _ = truncateDiagnostic(resource.Warnings[i].Message, budget)
		}
	}
	return &candidate
}

// resourceAdmissionResponse projects one invocation-local assessment. A nil
// assessment, for an existing task or an unchecked admission, adds nothing.
func resourceAdmissionResponse(assessment *resourceadmission.Assessment) *ResourceAdmissionResponse {
	if assessment == nil {
		return nil
	}
	response := &ResourceAdmissionResponse{Status: string(assessment.Status())}
	for _, unavailable := range assessment.Unavailable() {
		response.Warnings = append(response.Warnings, ResourceAdmissionWarning{
			Code: ResourceObservationUnavailable, Metric: string(unavailable.Metric), ReasonCode: string(unavailable.Reason),
			Message: "This resource metric could not be observed, so it did not limit admission.",
		})
	}
	for _, advisory := range assessment.Advisories() {
		response.Warnings = append(response.Warnings, ResourceAdmissionWarning{
			Code: ResourceAdvisory, Metric: string(resourceadmission.MetricCgroupMemory), ReasonCode: string(advisory),
			Message: "The execution cgroup is near its memory.high throttle point; this does not limit admission.",
		})
	}
	return response
}

// resourcePressureReason names the refused metric and how it was confirmed.
func resourcePressureReason(pressure resourceadmission.Pressure) string {
	return string(pressure.Metric) + "_" + string(pressure.Basis)
}

// resourcePublicError presents a resource refusal with its metric and basis
// while keeping the complete command error as its cause.
type resourcePublicError struct {
	message string
	cause   error
}

func (e *resourcePublicError) Error() string { return e.message }

func (e *resourcePublicError) Unwrap() error { return e.cause }

func publicResourceError(message string, err error) error {
	if pressure, ok := tasklifecycle.ResourcePressureRefusal(err); ok {
		message += fmt.Sprintf(" Pressure: %s (%s).", pressure.Metric, pressure.Basis)
	}
	return &resourcePublicError{message: message, cause: err}
}
