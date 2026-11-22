package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer-gateway/deviceclient"
	"github.com/hishamkaram/delegation-layer/internal/gatewaycommand"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

func TestGatewayGUIUpgradeRejectsCapturedContainedCallerBeforeQuiesce(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	token, err := store.RootTokenContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(store.Root, "gateway")
	if err = os.Mkdir(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	executable, err := verifiedexec.LocatePath("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	client := &gatewayUpgradeNativeProfile{directory: profile, selected: executable, source: deviceclient.LocalContextInfo{DelegateExecutable: executable, DelegateRoot: store.Root, DelegateRootContext: token}}
	services := &gatewayUpgradeNativeServices{profile: profile}
	t.Setenv("DELEGATION_LAYER_PROCESS_SCOPE", "pg:"+strings.Repeat("d", 32))
	deps := Dependencies{Gateway: &gatewaycommand.Dependencies{Client: client, Executable: executable, Services: services, NativeInstallation: guiUpgradeReadyNative{}}}
	var output, diagnostic bytes.Buffer
	code := RunContext(ctx, []string{"gateway", "upgrade", "--root", store.Root, "--profile", profile, "--accept-execution-policy-removal", "--json"}, &output, &diagnostic, deps)
	if len(services.calls) != 0 || client.migrated || client.updated {
		t.Fatalf("contained original invocation quiesced or committed before validation: service_calls=%v migrated=%t updated=%t", services.calls, client.migrated, client.updated)
	}
	if code == 0 {
		t.Fatal("contained original invocation was accepted")
	}
}

type guiUpgradeReadyNative struct{}

func (guiUpgradeReadyNative) Inspect(context.Context, string, string) (gatewaycommand.NativeTransition, error) {
	return gatewaycommand.NativeTransition{}, nil
}

func (guiUpgradeReadyNative) Ensure(context.Context, string, string, gatewaycommand.NativeTransition) (gatewaycommand.NativeTransition, error) {
	return gatewaycommand.NativeTransition{}, nil
}

func (guiUpgradeReadyNative) Verify(context.Context, string, string) error { return nil }

func TestGatewayGUIUpgradePrivateEntryGrammar(t *testing.T) {
	locator := filepath.Join(t.TempDir(), "request.json")
	valid := []string{"gateway", "upgrade", "--accept-execution-policy-removal", "--gui-upgrade-request", locator}
	arguments, err := ParseArguments(valid)
	if err != nil || arguments.Gateway.GUIUpgradeRequest != locator {
		t.Fatalf("fixed private upgrade entry unavailable: %v", err)
	}
	for _, argv := range [][]string{
		{"gateway", "upgrade", "--preview", "--gui-upgrade-request", locator},
		{"gateway", "upgrade", "--accept-execution-policy-removal", "--gui-upgrade-request", "relative.json"},
		{"gateway", "serve", "--gui-upgrade-request", locator},
	} {
		if _, parseErr := ParseArguments(argv); parseErr == nil {
			t.Fatalf("private upgrade flag escaped its fixed accepted entry: %q", argv)
		}
	}
	help, err := HelpText([]string{"gateway", "upgrade"})
	if err != nil || strings.Contains(help, "gui-upgrade-request") {
		t.Fatalf("private request locator entered public help: %v", err)
	}
	for _, name := range []string{"--profile", "--preview", "--accept-execution-policy-removal"} {
		if !strings.Contains(help, name) {
			t.Fatalf("legitimate public upgrade flag hidden: %s", name)
		}
	}
}
