package app

import (
	"context"
	"errors"
	"flag"
	"io"

	"github.com/hishamkaram/delegation-layer/internal/gatewaycommand"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func gatewaySpec() commandSpec {
	return commandSpec{Name: "gateway", Summary: "Connect this Native installation to a trusted Gateway", Register: registerGatewayParserFlags, Children: []commandSpec{
		{Name: "connect", Summary: "Enroll this device using browser, headless Device Flow or an unattended token", Operands: "HTTPS_URL"},
		leaf("status", "Read local credentials without claiming online execution readiness", ""),
		leaf("doctor", "Check local Gateway profile and Native root statically", ""),
		leaf("upgrade", "Preview or explicitly apply a profile and installed executable handoff", ""),
		leaf("repair-context", "Prepare or explicitly confirm a history-preserving native root reconciliation", ""),
		leaf("serve", "Run the saved connector in this foreground process", ""),
		leaf("disconnect", "Stop and uninstall only this profile's connector service; retain credentials and tasks", ""),
		{Name: "service", Summary: "Manage only this profile's per-user connector service", Children: []commandSpec{
			leaf("install", "Install the exact saved profile's service definition", ""),
			leaf("start", "Start its installed service without replacing an active owner", ""),
			leaf("stop", "Request stop and confirm only the connector runtime is inactive", ""),
			leaf("status", "Observe the exact managed service without inferring task closure", ""),
			leaf("uninstall", "Stop and remove its service definition; retain profile and tasks", ""),
		}},
		{Name: "workspace", Summary: "Approve and revise explicit local Gateway workspace roots", Children: []commandSpec{
			leaf("list", "List local approvals and disabled history", ""),
			leaf("allow", "Approve one existing local workspace root", "DIRECTORY"),
			leaf("update", "Revise an alias without retargeting workspace identity", "WORKSPACE_ID"),
			leaf("remove", "Disable one approval and retain its history", "WORKSPACE_ID"),
		}},
	}}
}

func configureGatewayPath(a *gatewaycommand.Arguments, path []string) {
	a.Action = path[1]
	if len(path) < 3 {
		return
	}
	switch a.Action {
	case "workspace":
		a.WorkspaceAction = path[2]
	case "service":
		a.ServiceAction = path[2]
	}
}

func registerGatewayParserFlags(fs *flag.FlagSet, p *argumentParser) error {
	fs.StringVar(&p.arguments.Root, "root", "", "Advanced explicit Native root; omit for its configured default")
	fs.BoolVar(&p.arguments.JSON, "json", false, "emit versioned JSON")
	gatewaycommand.RegisterFlags(fs, &p.arguments.Gateway)
	return nil
}

func runGatewayCLI(ctx context.Context, a Arguments, deps Dependencies, stdout, stderr io.Writer) int {
	input := gatewaycommand.Dependencies{}
	if deps.Gateway != nil {
		input = *deps.Gateway
	}
	catalog := deps.Catalog
	if catalog.IsZero() {
		catalog = NativeCatalog()
	}
	input.Predicates = catalog.Registry()
	if input.Environment == nil {
		input.Environment = deps.Environment
	}
	if input.Input == nil {
		input.Input = deps.Input
	}
	if input.NativeMaintenance == nil {
		input.NativeMaintenance = gatewayNativeMaintenance{dependencies: deps.normalized()}
	}
	return gatewaycommand.Run(ctx, a.Gateway, a.Root, a.JSON, stdout, stderr, input)
}

type gatewayNativeMaintenance struct{ dependencies Dependencies }

func (m gatewayNativeMaintenance) Ensure(ctx context.Context, request gatewaycommand.NativeMaintenanceRequest) (gatewaycommand.NativeTransition, error) {
	if err := m.dependencies.invocationSource.ValidateUpgrade(ctx); err != nil {
		return gatewaycommand.NativeTransition{}, err
	}
	if request.Root == "" || request.ExpectedRoot == "" {
		return gatewaycommand.NativeTransition{}, task.ErrIdentityMismatch
	}
	arguments := Arguments{Root: request.Root}
	if request.OperationID != "" {
		if _, err := pueue.ParseUpgradeOperationID(request.OperationID); err != nil {
			return gatewaycommand.NativeTransition{}, err
		}
		arguments.SupervisorCompleteUpgrade = request.OperationID
	}
	result := supervisorEnsureOwnerContext(ctx, arguments, m.dependencies, request.ExpectedRoot)
	observed := gatewayMaintenanceObservation(result.response)
	if result.err != nil {
		return observed, result.err
	}
	if result.code == 3 {
		return observed, gatewaycommand.ErrNativeUpgradePending
	}
	if result.code != 0 {
		return observed, errors.Join(task.ErrEvidenceFault, errors.New("native maintenance returned an invalid control result"))
	}
	return observed, nil
}

func gatewayMaintenanceObservation(response Response) gatewaycommand.NativeTransition {
	control := response.SupervisorControl
	if control == nil || control.Upgrade == nil || control.Upgrade.OperationID == "" {
		return gatewaycommand.NativeTransition{}
	}
	observed := control.Upgrade
	return gatewaycommand.NativeTransition{OperationID: observed.OperationID, Phase: observed.Phase, Pending: !observed.Completed}
}
