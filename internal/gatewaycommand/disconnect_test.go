package gatewaycommand

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer-gateway/deviceclient"
	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/gatewayservice"
)

type disconnectClient struct {
	commandClient
	service       *commandService
	inactive      bool
	revokeCalls   int
	inactiveCalls int
}

func (c *disconnectClient) RuntimeInactive(context.Context, string) (bool, error) {
	c.inactiveCalls++
	return c.inactive, nil
}

func (c *disconnectClient) RevokeDevice(ctx context.Context, _ string, present func(context.Context, deviceclient.Challenge) error) error {
	c.revokeCalls++
	if !reflect.DeepEqual(c.service.calls, []string{"uninstall"}) || c.inactiveCalls != 1 {
		return errors.New("fresh authorization preceded confirmed local stop")
	}
	return present(ctx, deviceclient.Challenge{VerificationURI: "https://issuer.example/activate", UserCode: "REVOKE-CODE", ExpiresAt: time.Now().Add(time.Minute)})
}

func TestGatewayDisconnectRequiresConfirmedLocalStopBeforeFreshRevocation(t *testing.T) {
	for _, tc := range []struct {
		name         string
		revoke       bool
		inactive     bool
		stopErr      error
		wantCode     int
		wantRevoke   int
		wantInactive int
	}{
		{name: "local-only", inactive: true},
		{name: "fresh-revocation", revoke: true, inactive: true, wantRevoke: 1, wantInactive: 1},
		{name: "stop-pending", revoke: true, inactive: true, stopErr: gatewayservice.ErrPending, wantCode: 3},
		{name: "foreground-still-owned", revoke: true, wantCode: 3, wantInactive: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testGatewayDisconnect(t, tc.revoke, tc.inactive, tc.stopErr, tc.wantCode, tc.wantRevoke, tc.wantInactive)
		})
	}
}

func testGatewayDisconnect(t *testing.T, revoke, inactive bool, stopErr error, wantCode, wantRevoke, wantInactive int) {
	t.Helper()
	service := &commandService{stopErr: stopErr}
	client := &disconnectClient{service: service, inactive: inactive}
	deps := commandDependencies(t, client)
	deps.Services = commandServices{service: service}
	deps.OpenBrowser = func(context.Context, string) error { t.Fatal("headless revocation opened a browser"); return nil }
	a := Arguments{Action: "disconnect", Revoke: revoke, Headless: revoke}
	root := filepath.Join(t.TempDir(), "unused-root")
	canonical, err := config.CanonicalizePath(root)
	if err != nil {
		t.Fatal(err)
	}
	client.local.DelegateRoot = canonical
	var output, diagnostic bytes.Buffer
	code := Run(boundedTestContext(t), a, root, true, &output, &diagnostic, deps)
	if code != wantCode || client.revokeCalls != wantRevoke || client.inactiveCalls != wantInactive {
		t.Fatalf("unexpected local/revoke boundary: exit%d revoke%d inactive%d output%s", code, client.revokeCalls, client.inactiveCalls, output.Bytes())
	}
	if wantRevoke == 1 && (!bytes.Contains(output.Bytes(), []byte(`"revoked":true`)) || bytes.Count(diagnostic.Bytes(), []byte("REVOKE-CODE")) != 1) {
		t.Fatalf("successful revocation/challenge missing: output%s diagnostic%s", output.Bytes(), diagnostic.Bytes())
	}
}
