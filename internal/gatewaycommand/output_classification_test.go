package gatewaycommand

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer-gateway/deviceclient"
	"github.com/hishamkaram/delegation-layer/internal/gatewayservice"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestGatewayFailureProjectionClassifiesWholeError(t *testing.T) {
	privateFault := errors.New("PRIVATE-FAILURE-CAUSE")
	for _, tc := range []struct {
		name    string
		err     error
		exit    int
		failure string
		next    string
	}{
		{"wrapped busy", fmt.Errorf("profile lease: %w", deviceclient.ErrBusy), 3, "gateway_profile_in_use", "inspect_same_profile"},
		{"wrapped pending", fmt.Errorf("stop observation: %w", gatewayservice.ErrPending), 3, "gateway_stop_pending", "inspect_same_profile"},
		{"pending deadline", errors.Join(gatewayservice.ErrPending, context.DeadlineExceeded), 3, "gateway_stop_pending", "inspect_same_profile"},
		{"pending cancellation", errors.Join(gatewayservice.ErrPending, context.Canceled), 3, "gateway_stop_pending", "inspect_same_profile"},
		{"pending foreign", errors.Join(gatewayservice.ErrPending, gatewayservice.ErrForeignService), 1, "gateway_service_changed", "stop_and_report"},
		{"pending manager", errors.Join(gatewayservice.ErrPending, gatewayservice.ErrManager), 1, "gateway_service_manager_failed", "inspect_user_service_manager"},
		{"pending observation fault", errors.Join(gatewayservice.ErrPending, privateFault), 1, "gateway_operation_failed", "inspect_same_profile"},
		{"pending identity fault", errors.Join(gatewayservice.ErrPending, task.ErrIdentityMismatch), 1, "native_context_changed", "stop_and_report"},
		{"pending service configuration", errors.Join(gatewayservice.ErrPending, gatewayservice.ErrConfiguration), 1, "gateway_operation_failed", "inspect_same_profile"},
		{"pending foreign deadline", errors.Join(gatewayservice.ErrPending, gatewayservice.ErrForeignService, context.DeadlineExceeded), 1, "gateway_service_changed", "stop_and_report"},
		{"busy cleanup fault", errors.Join(deviceclient.ErrBusy, privateFault), 1, "gateway_operation_failed", "inspect_same_profile"},
		{"busy configuration fault", errors.Join(deviceclient.ErrBusy, deviceclient.ErrConfiguration), 1, "gateway_connection_invalid_configuration", "inspect_same_profile"},
		{"busy persistence fault", errors.Join(deviceclient.ErrBusy, deviceclient.ErrPersistence), 1, "gateway_profile_persistence_failed", "inspect_same_profile"},
		{"busy foreign", errors.Join(deviceclient.ErrBusy, gatewayservice.ErrForeignService), 1, "gateway_service_changed", "stop_and_report"},
		{"deadline", fmt.Errorf("bounded operation: %w", context.DeadlineExceeded), 1, "gateway_operation_interrupted", "inspect_same_profile"},
		{"deadline concrete fault", errors.Join(context.DeadlineExceeded, privateFault), 1, "gateway_operation_failed", "inspect_same_profile"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, jsonOutput := range []bool{true, false} {
				t.Run(map[bool]string{true: "JSON", false: "human"}[jsonOutput], func(t *testing.T) {
					requireGatewayFailureProjection(t, tc.err, jsonOutput, tc.exit, tc.failure, tc.next)
				})
			}
		})
	}
}

func requireGatewayFailureProjection(t *testing.T, operationErr error, jsonOutput bool, expectedExit int, expectedFailure, expectedNext string) {
	t.Helper()
	var output, diagnostic bytes.Buffer
	exit := present(Response{Command: "gateway", Action: "service"}, operationErr, jsonOutput, &output, &diagnostic)
	if exit != expectedExit {
		t.Errorf("exit=%d, want %d; output=%s diagnostic=%s", exit, expectedExit, output.String(), diagnostic.String())
	}
	if strings.Contains(output.String()+diagnostic.String(), "PRIVATE-FAILURE-CAUSE") {
		t.Fatal("public failure exposed a private cause")
	}
	if !jsonOutput {
		expected := expectedFailure + "; next_action=" + expectedNext + "\n"
		if diagnostic.String() != expected {
			t.Fatalf("human failure=%q, want %q", diagnostic.String(), expected)
		}
		return
	}
	var response Response
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Failure != expectedFailure || response.NextAction != expectedNext || diagnostic.Len() != 0 {
		t.Fatalf("JSON failure=%q next=%q diagnostic=%q, want %q/%q", response.Failure, response.NextAction, diagnostic.String(), expectedFailure, expectedNext)
	}
}
