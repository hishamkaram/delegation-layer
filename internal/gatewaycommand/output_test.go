package gatewaycommand

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer-gateway/deviceclient"
	"github.com/hishamkaram/delegation-layer/internal/gatewayservice"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestGatewayFailureProjectionKeepsSDKKindAndRedactsCause(t *testing.T) {
	var output, diagnostic bytes.Buffer
	err := errors.Join(deviceclient.ErrDenied, errors.New("PRIVATE-FAILURE-CAUSE"))
	code := present(Response{Command: "gateway", Action: "connect"}, err, true, &output, &diagnostic)
	if code != 1 || !strings.Contains(output.String(), "gateway_connection_denied") || strings.Contains(output.String(), "PRIVATE-FAILURE-CAUSE") {
		t.Fatalf("failure projection: code%d output%s", code, output.String())
	}
}

func TestGatewayServiceFailureProjectionKeepsDistinctRecoveryActions(t *testing.T) {
	for _, tc := range []struct {
		kind error
		code string
		next string
	}{
		{gatewayservice.ErrServiceChanged, "gateway_service_changed", "retry_same_profile"},
		{gatewayservice.ErrForeignService, "gateway_service_changed", "stop_and_report"},
		{gatewayservice.ErrManagerCacheStale, "gateway_service_cache_stale", "retry_service_stop"},
	} {
		t.Run(tc.next, func(t *testing.T) {
			var output, diagnostic bytes.Buffer
			err := errors.Join(tc.kind, errors.New("PRIVATE-SERVICE-CAUSE"))
			code := present(Response{Command: "gateway", Action: "disconnect"}, err, true, &output, &diagnostic)
			if code != 1 || !strings.Contains(output.String(), `"failure":"`+tc.code+`"`) || !strings.Contains(output.String(), `"next_action":"`+tc.next+`"`) || strings.Contains(output.String(), "PRIVATE-SERVICE-CAUSE") {
				t.Fatalf("service change projection: code%d output%s", code, output.String())
			}
		})
	}
}

func TestGatewayOversizedOutputEmitsNoPartialAuthority(t *testing.T) {
	var output bytes.Buffer
	if err := writeJSON(&output, Response{Profile: strings.Repeat("x", task.MaxControlRecordSize)}); err == nil || output.Len() != 0 {
		t.Fatalf("oversized response emitted partial data: length%d err%v", output.Len(), err)
	}
}

type shortGatewayWriter struct{}

func (shortGatewayWriter) Write(data []byte) (int, error) { return len(data) - 1, nil }

func TestGatewayOutputReportsShortWrites(t *testing.T) {
	if err := writeJSON(shortGatewayWriter{}, Response{Command: "gateway"}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short output accepted: %v", err)
	}
}
