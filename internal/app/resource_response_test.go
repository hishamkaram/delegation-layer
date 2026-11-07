package app

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func unknownResourceResponse(message string) *ResourceAdmissionResponse {
	return &ResourceAdmissionResponse{
		Status: ResourceAdmissionUnknown,
		Warnings: []ResourceAdmissionWarning{{
			Code:       ResourceObservationUnavailable,
			Metric:     "host_cpu",
			ReasonCode: "deadline",
			Message:    message,
		}},
	}
}

func TestResourceAdmissionJSONContract(t *testing.T) {
	response := newResponse("dispatch")
	response.ResourceAdmission = unknownResourceResponse("host CPU was not observed")
	response.ResourceAdmission.Warnings = append(response.ResourceAdmission.Warnings, ResourceAdmissionWarning{
		Code: ResourceObservationUnavailable, ReasonCode: "unsupported", Message: "cgroup scope is unavailable",
	})
	response.Failure = &DispatchFailureResponse{Code: "resource_pressure", Stage: "check-resources", NextAction: "retry_later", RetryAfterSeconds: 30}
	data, err := encodeJSONResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err = json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	wantResource := `{"status":"unknown","warnings":[{"code":"resource_observation_unavailable","metric":"host_cpu","reason_code":"deadline","message":"host CPU was not observed"},{"code":"resource_observation_unavailable","reason_code":"unsupported","message":"cgroup scope is unavailable"}]}`
	if got := string(wire["resource_admission"]); got != wantResource {
		t.Fatalf("resource_admission = %s, want %s", got, wantResource)
	}
	if !strings.Contains(string(wire["failure"]), `"retry_after_seconds":30`) {
		t.Fatalf("failure = %s, want retry_after_seconds", wire["failure"])
	}
	var decoded Response
	if err = json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.ResourceAdmission, response.ResourceAdmission) || decoded.Failure.RetryAfterSeconds != 30 {
		t.Fatalf("round trip = %+v %+v", decoded.ResourceAdmission, decoded.Failure)
	}

	clear := newResponse("dispatch")
	clear.ResourceAdmission = &ResourceAdmissionResponse{Status: ResourceAdmissionClear}
	clear.Failure = &DispatchFailureResponse{Code: "supervisor_setup_failed", Stage: "supervisor", NextAction: "stop_and_report"}
	data, err = encodeJSONResponse(clear)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"resource_admission":{"status":"clear"}`)) || bytes.Contains(data, []byte("retry_after_seconds")) {
		t.Fatalf("optional resource fields were not omitted: %s", data)
	}
}

func TestResourceAdmissionNilKeepsLegacyRendering(t *testing.T) {
	response := newResponse("dispatch")
	response.TaskID = "task-1"
	response.Failure = &DispatchFailureResponse{Code: "supervisor_setup_failed", Stage: "supervisor", NextAction: "stop_and_report"}
	data, err := encodeJSONResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("resource_admission")) || bytes.Contains(data, []byte("retry_after_seconds")) {
		t.Fatalf("legacy JSON gained resource fields: %s", data)
	}
	var output bytes.Buffer
	if err = writeHumanResponse(&output, response); err != nil {
		t.Fatal(err)
	}
	const want = "command=dispatch root_id= task_id=task-1 admission=unknown liveness=undetermined publication=unknown\n"
	if output.String() != want {
		t.Fatalf("legacy human output = %q, want %q", output.String(), want)
	}
}

func TestResourceAdmissionUnknownWarningIsAdvisory(t *testing.T) {
	response := newResponse("dispatch")
	response.TaskID = "task-1"
	response.Admission = "accepted"
	response.ResourceAdmission = unknownResourceResponse("host CPU was not observed")
	before := response
	beforeResource := *response.ResourceAdmission
	beforeResource.Warnings = append([]ResourceAdmissionWarning(nil), response.ResourceAdmission.Warnings...)

	var human bytes.Buffer
	if err := writeHumanResponse(&human, response); err != nil {
		t.Fatal(err)
	}
	wantLines := "resource_admission=unknown\nresource_warning code=resource_observation_unavailable reason_code=deadline message=host CPU was not observed metric=host_cpu\n"
	if !strings.Contains(human.String(), wantLines) {
		t.Fatalf("human output = %q, want %q", human.String(), wantLines)
	}
	var stdout bytes.Buffer
	if err := writeJSONResponse(&stdout, "dispatch", &response, nil); err != nil {
		t.Fatal(err)
	}
	if response.Error != "" || response.Failure != nil || response.Admission != before.Admission || response.Outcome != nil {
		t.Fatalf("warning changed the response: %+v", response)
	}
	if !reflect.DeepEqual(*response.ResourceAdmission, beforeResource) {
		t.Fatalf("rendering mutated resource metadata: %+v", response.ResourceAdmission)
	}
}

func TestResourceAdmissionWarningUsesSharedTruncation(t *testing.T) {
	response := newResponse("dispatch")
	large := strings.Repeat("é", MaxJSONResponseBytes)
	response.ResourceAdmission = unknownResourceResponse(large)
	var stdout bytes.Buffer
	if err := writeJSON(&stdout, response); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() > MaxJSONResponseBytes {
		t.Fatalf("response size = %d, limit %d", stdout.Len(), MaxJSONResponseBytes)
	}
	var decoded Response
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ResourceAdmission == nil || len(decoded.ResourceAdmission.Warnings) != 1 {
		t.Fatalf("decoded resource = %+v", decoded.ResourceAdmission)
	}
	warning := decoded.ResourceAdmission.Warnings[0]
	if decoded.ResourceAdmission.Status != ResourceAdmissionUnknown || warning.Code != ResourceObservationUnavailable || warning.Metric != "host_cpu" || warning.ReasonCode != "deadline" {
		t.Fatalf("truncation changed structured fields: %+v", decoded.ResourceAdmission)
	}
	if !strings.HasSuffix(warning.Message, "[truncated]") || len(warning.Message) >= len(large) || strings.ToValidUTF8(warning.Message, "�") != warning.Message {
		t.Fatalf("message was not truncated on a UTF-8 boundary: %d bytes", len(warning.Message))
	}
	if response.ResourceAdmission.Warnings[0].Message != large {
		t.Fatal("truncation mutated the original warning")
	}
}
