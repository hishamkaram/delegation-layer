package gatewaycommand

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer-gateway/deviceclient"
	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

type commandClient struct {
	sdkClient
	connectCalls   int
	options        deviceclient.ConnectOptions
	challenge      bool
	local          deviceclient.LocalContextInfo
	approval       deviceclient.WorkspaceApproval
	allowCalls     int
	workspaces     []deviceclient.Workspace
	update         deviceclient.WorkspaceUpdate
	updateRevision uint64
}

func TestGatewayUpgradeReviewDispositionPresentation(t *testing.T) {
	operation := "upgrade-v1:" + strings.Repeat("a", 32)
	response := Response{SchemaVersion: 1, Command: "gateway", Action: "upgrade", Upgrade: &UpgradeObservation{Supervisor: NativeTransition{OperationID: operation, Phase: "migrating", Pending: true}}}
	for _, test := range []struct {
		name, failure, next string
		cause               error
		code                int
	}{
		{"review", "gateway_upgrade_review_required", "stop_and_report", ErrNativeUpgradeReviewRequired, 1},
		{"pending", "gateway_upgrade_pending", "retry_gateway_upgrade_same_profile", ErrNativeUpgradePending, 3},
		{"review with concrete fault", "gateway_operation_failed", "inspect_same_profile", errors.Join(ErrNativeUpgradeReviewRequired, os.ErrPermission), 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output, diagnostic bytes.Buffer
			code := present(response, test.cause, true, &output, &diagnostic)
			var got Response
			if err := json.Unmarshal(output.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if code != test.code || got.Failure != test.failure || got.NextAction != test.next || got.Upgrade == nil || got.Upgrade.Supervisor != response.Upgrade.Supervisor {
				t.Fatalf("upgrade disposition or retained operation changed: code=%d response=%+v", code, got)
			}
			output.Reset()
			code = present(response, test.cause, false, &output, &diagnostic)
			if code != test.code || !strings.Contains(diagnostic.String(), test.failure+"; next_action="+test.next) {
				t.Fatalf("human output lost disposition: code=%d diagnostic=%s", code, &diagnostic)
			}
		})
	}
}

func (c *commandClient) Connect(ctx context.Context, o deviceclient.ConnectOptions) (deviceclient.ConnectionStatus, error) {
	c.connectCalls++
	c.options = o
	c.local = deviceclient.LocalContextInfo{DelegateRoot: o.DelegateRoot, DelegateRootContext: o.DelegateRootContext, DelegateExecutable: o.DelegateExecutable}
	if c.challenge {
		if err := o.PresentChallenge(ctx, deviceclient.Challenge{VerificationURI: "https://issuer.example/activate", VerificationURIComplete: "https://issuer.example/activate?user_code=PRIVATE-COMPLETE", UserCode: "USER-CODE", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
			return deviceclient.ConnectionStatus{}, err
		}
	}
	return deviceclient.ConnectionStatus{CredentialState: "valid"}, nil
}

func (c *commandClient) LocalContext(context.Context, string) (deviceclient.LocalContextInfo, error) {
	local := c.local
	if local.DelegateExecutable == "" {
		var err error
		local.DelegateExecutable, err = verifiedexec.LocatePath("/usr/bin/true")
		return local, err
	}
	return local, nil
}

func (c *commandClient) ProfileMigrationSource(ctx context.Context, directory string) (deviceclient.LocalContextInfo, error) {
	return c.LocalContext(ctx, directory)
}

func (c *commandClient) AllowWorkspace(_ context.Context, _ string, a deviceclient.WorkspaceApproval) (deviceclient.Workspace, error) {
	c.approval = a
	c.allowCalls++
	return deviceclient.Workspace{ID: strings.Repeat("a", 32), Alias: a.Alias, Directory: a.Directory, Revision: 1, Enabled: true}, nil
}

func (c *commandClient) ListWorkspaces(context.Context, string) ([]deviceclient.Workspace, error) {
	return c.workspaces, nil
}

func (c *commandClient) UpdateWorkspace(_ context.Context, _ string, id string, revision uint64, update deviceclient.WorkspaceUpdate) (deviceclient.Workspace, error) {
	c.update, c.updateRevision = update, revision
	return deviceclient.Workspace{ID: id, Revision: revision + 1, Enabled: true}, nil
}

func commandDependencies(t *testing.T, client Client) Dependencies {
	t.Helper()
	executable, err := verifiedexec.LocatePath("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	return Dependencies{Client: client, Executable: executable, Predicates: predicate.Default(), Environment: []string{}, Services: commandServices{service: &commandService{}}}
}

func TestGatewayWorkspaceAliasUpdatePreservesSDKRevision(t *testing.T) {
	id := strings.Repeat("a", 32)
	client := &commandClient{}
	a := Arguments{Action: "workspace", WorkspaceAction: "update"}
	fs := flag.NewFlagSet("workspace", flag.ContinueOnError)
	RegisterFlags(fs, &a)
	if err := fs.Parse([]string{"--expected-revision", "7", "--alias", "renamed"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Validate([]string{id}); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "unused-state")
	canonical, err := config.CanonicalizePath(root)
	if err != nil {
		t.Fatal(err)
	}
	client.local.DelegateRoot = canonical
	if code := Run(boundedTestContext(t), a, root, true, io.Discard, io.Discard, commandDependencies(t, client)); code != 0 {
		t.Fatalf("update exit%d", code)
	}
	if client.updateRevision != 7 || client.update.Alias != "renamed" {
		t.Fatalf("lost exact alias or revision: %+v", client.update)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local SDK alias update initialized Native state: %v", err)
	}
}

func TestGatewayExplicitRootCannotOverrideSavedProfileSource(t *testing.T) {
	client := &commandClient{local: deviceclient.LocalContextInfo{DelegateRoot: filepath.Join(t.TempDir(), "original-state")}}
	a := Arguments{Action: "workspace", WorkspaceAction: "list", Profile: filepath.Join(t.TempDir(), "profile")}
	var output, diagnostic bytes.Buffer
	code := Run(boundedTestContext(t), a, filepath.Join(t.TempDir(), "other-state"), true, &output, &diagnostic, commandDependencies(t, client))
	if code == 0 || !strings.Contains(output.String(), "gateway_source_conflict") {
		t.Fatalf("explicit root ignored: exit%d output%s", code, output.String())
	}
}

func TestGatewayConnectPinsNativeSourceWithoutApprovingWorkspaces(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	client := &commandClient{}
	a := Arguments{Action: "connect", GatewayURL: "https://gateway.example", Headless: true, DeviceName: "test-device"}
	var output, diagnostic bytes.Buffer
	if code := Run(boundedTestContext(t), a, root, true, &output, &diagnostic, commandDependencies(t, client)); code != 0 {
		t.Fatalf("connect exit=%d output=%s diagnostic=%s", code, output.String(), diagnostic.String())
	}
	if client.connectCalls != 1 || client.options.DelegateRootContext == "" || client.options.DelegateRoot == "" || client.options.ProfileDirectory != filepath.Join(client.options.DelegateRoot, "gateway") || client.options.Workspaces != nil {
		t.Fatalf("invalid pinned connect source: calls=%d options=%s", client.connectCalls, client.options.String())
	}
	var response Response
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Online != "unknown" || response.ExecutionReadiness != "unknown" {
		t.Fatalf("credential was treated as live readiness: %+v", response)
	}
	if bytes.Contains(output.Bytes(), []byte(client.options.DelegateRootContext)) {
		t.Fatal("raw Native root token exposed in CLI response")
	}
}

func TestGatewayDeviceFlowBrowserFailureAndHeadlessKeepOneChallenge(t *testing.T) {
	for _, headless := range []bool{false, true} {
		t.Run(map[bool]string{false: "browser-unavailable", true: "headless"}[headless], func(t *testing.T) { testGatewayDeviceFlowChallenge(t, headless) })
	}
}

func testGatewayDeviceFlowChallenge(t *testing.T, headless bool) {
	t.Helper()
	client := &commandClient{challenge: true}
	deps := commandDependencies(t, client)
	opened := 0
	deps.OpenBrowser = func(ctx context.Context, uri string) error {
		opened++
		if uri != "https://issuer.example/activate" {
			t.Fatalf("secret or unverified URI passed to opener: %q", uri)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("browser activation is unbounded")
		}
		return errors.New("activation unavailable")
	}
	a := Arguments{Action: "connect", GatewayURL: "https://gateway.example", Headless: headless, DeviceName: "test-device"}
	var output, diagnostic bytes.Buffer
	if code := Run(boundedTestContext(t), a, filepath.Join(t.TempDir(), "state"), true, &output, &diagnostic, deps); code != 0 {
		t.Fatalf("flow exit=%d output=%s", code, output.String())
	}
	if client.connectCalls != 1 || strings.Count(diagnostic.String(), "User code: USER-CODE") != 1 {
		t.Fatalf("flow or challenge repeated: calls%d diagnostic%s", client.connectCalls, diagnostic.String())
	}
	if (headless && opened != 0) || (!headless && opened != 1) {
		t.Fatalf("headless=%t opener calls%d", headless, opened)
	}
	if bytes.Contains(output.Bytes(), []byte("USER-CODE")) || strings.Contains(diagnostic.String(), "PRIVATE-COMPLETE") {
		t.Fatal("auth secrets escaped intentional challenge display")
	}
}

func TestGatewayUnattendedStdinUsesSameSDKSource(t *testing.T) {
	client := &commandClient{}
	deps := commandDependencies(t, client)
	input := bytes.NewBufferString("owned test token source")
	deps.Input = input
	a := Arguments{Action: "connect", GatewayURL: "https://gateway.example", TokenFile: "-", Headless: true, DeviceName: "test-device"}
	if code := Run(boundedTestContext(t), a, filepath.Join(t.TempDir(), "state"), true, io.Discard, io.Discard, deps); code != 0 {
		t.Fatalf("unattended exit%d", code)
	}
	if client.options.EnrollmentTokenFile != "" || client.options.EnrollmentTokenReader != input || input.Len() == 0 {
		t.Fatal("Native wrapper consumed/replaced the SDK token reader")
	}
}

func TestGatewayAllowInspectsExplicitExistingDirectoryAndHidesTokens(t *testing.T) {
	ctx := boundedTestContext(t)
	root, observation, err := rootContext(ctx, filepath.Join(t.TempDir(), "state"), predicate.Default(), true)
	if err != nil {
		t.Fatal(err)
	}
	client := &commandClient{local: deviceclient.LocalContextInfo{DelegateRoot: root, DelegateRootContext: observation.RootToken}}
	a := Arguments{Action: "workspace", WorkspaceAction: "allow", Directory: t.TempDir(), Alias: "approved"}
	var output, diagnostic bytes.Buffer
	if code := Run(ctx, a, root, true, &output, &diagnostic, commandDependencies(t, client)); code != 0 {
		t.Fatalf("allow exit=%d output=%s", code, output.String())
	}
	if client.allowCalls != 1 || client.approval.NativeContext == "" || client.approval.Alias != "approved" {
		t.Fatal("workspace approval lost inspected context or copied a second policy authority")
	}
	if bytes.Contains(output.Bytes(), []byte(client.approval.NativeContext)) || bytes.Contains(output.Bytes(), []byte(observation.RootToken)) {
		t.Fatal("local identity token leaked")
	}
	missing := filepath.Join(t.TempDir(), "absent")
	a.Directory = missing
	if code := Run(ctx, a, root, true, io.Discard, io.Discard, commandDependencies(t, client)); code == 0 || client.allowCalls != 1 {
		t.Fatal("missing workspace was approved")
	}
	if _, err = os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace was created: %v", err)
	}
}
