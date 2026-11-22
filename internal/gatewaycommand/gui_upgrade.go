package gatewaycommand

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"runtime"

	"github.com/hishamkaram/delegation-layer-gateway/deviceclient"
	"github.com/hishamkaram/delegation-layer/internal/gatewayservice"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

func guiUpgradeSource(local deviceclient.LocalContextInfo) gatewayservice.GUIUpgradeSource {
	return gatewayservice.GUIUpgradeSource{Executable: local.DelegateExecutable, Root: local.DelegateRoot, Context: local.DelegateRootContext}
}

func guiUpgradeUnknownOnly(err error) bool {
	return errors.Is(err, gatewayservice.ErrGUIUpgradeUnknown) && task.AllErrorCauses(err, func(cause error) bool {
		return errors.Is(cause, gatewayservice.ErrGUIUpgradeUnknown) || errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded)
	})
}

func (c invocation) requestGUIUpgrade(ctx context.Context, local deviceclient.LocalContextInfo, executable string, response Response) (bool, Response, error) {
	control := c.dependencies.GUIUpgrade
	if control == nil && runtime.GOOS != "darwin" {
		return false, response, nil
	}
	if c.dependencies.NativeMaintenance == nil {
		return true, response, errors.New("original Native upgrade invocation authority is unavailable")
	}
	if err := c.guardGUIRoot(ctx, local.DelegateRoot, local.DelegateRootContext); err != nil {
		return true, response, err
	}
	if control == nil {
		manager, err := c.nativeServiceManager(local.DelegateExecutable)
		if err != nil {
			return true, response, err
		}
		control = manager
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return true, response, gatewayservice.ErrConfiguration
	}
	request := gatewayservice.GUIUpgradeRequest{Profile: c.profile, Root: local.DelegateRoot, ExpectedRoot: local.DelegateRootContext, SavedSource: guiUpgradeSource(local), Executable: executable, Accepted: c.arguments.AcceptExecutionPolicyRemoval, Deadline: deadline}
	result, err := control.RunGUIUpgrade(ctx, request)
	if len(result.Response) == 0 {
		if err == nil {
			err = gatewayservice.ErrGUIUpgradeRecord
		}
		if errors.Is(err, gatewayservice.ErrGUIUpgradeUnknown) && response.Upgrade != nil {
			response.Upgrade.Phase = "gui_invocation_observation"
		}
		return true, response, err
	}
	return c.decodeGUIUpgradeResult(result, response, err)
}

func (c invocation) decodeGUIUpgradeResult(result gatewayservice.GUIUpgradeResult, response Response, err error) (bool, Response, error) {
	var reply Response
	if decodeErr := json.Unmarshal(result.Response, &reply); decodeErr != nil || reply.SchemaVersion != 1 || reply.Command != "gateway" || reply.Action != "upgrade" || reply.Profile != c.profile {
		return true, response, errors.Join(err, gatewayservice.ErrGUIUpgradeRecord, decodeErr)
	}
	if result.ExitCode < 0 || result.ExitCode > 255 {
		return true, reply, errors.Join(err, gatewayservice.ErrGUIUpgradeRecord)
	}
	if err != nil {
		return true, reply, err
	}
	reply.guiExitCode = &result.ExitCode
	return true, reply, nil
}

func (c invocation) upgradeGUIChild(parent context.Context, response Response) (Response, error) {
	if c.dependencies.NativeMaintenance == nil {
		return response, gatewayservice.ErrGUIUpgradeRecord
	}
	request, err := gatewayservice.ReadGUIUpgradeRequest(c.arguments.GUIUpgradeRequest, c.profile)
	if err != nil {
		return response, err
	}
	ctx, cancel := context.WithDeadline(parent, request.Deadline)
	defer cancel()
	if err = c.dependencies.NativeMaintenance.ValidateUpgrade(ctx); err != nil {
		return response, err
	}
	if err = c.validateGUIChildIdentity(request); err != nil {
		return response, err
	}
	manager, err := c.nativeServiceManager(request.SavedSource.Executable)
	if err != nil {
		return response, err
	}
	claim, err := manager.ClaimGUIUpgrade(ctx, request)
	if err != nil {
		return response, err
	}
	reply, result, operationErr := c.executeGUIUpgrade(ctx, request, response)
	closeErr := claim.Finish(result)
	if err = closeErr; err != nil {
		return reply, errors.Join(operationErr, err)
	}
	reply.guiExitCode = &result.ExitCode
	return reply, nil
}

func (c invocation) validateGUIChildRequest(ctx context.Context, request gatewayservice.GUIUpgradeRequest) error {
	if err := c.validateGUIChildIdentity(request); err != nil {
		return err
	}
	local, err := c.dependencies.Client.ProfileMigrationSource(ctx, c.profile)
	if err != nil || guiUpgradeSource(local) != request.SavedSource {
		return errors.Join(task.ErrRequestConflict, err)
	}
	return c.guardGUIRoot(ctx, request.Root, request.ExpectedRoot)
}

func (c invocation) guardGUIRoot(ctx context.Context, root, expected string) (resultErr error) {
	store, err := taskdir.OpenExistingStoreMetadataContext(ctx, root, c.dependencies.Predicates)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	observed, err := store.RootTokenContext(ctx)
	if err != nil || observed != expected {
		return errors.Join(task.ErrIdentityMismatch, err)
	}
	return nil
}

func (c invocation) validateGUIChildIdentity(request gatewayservice.GUIUpgradeRequest) error {
	if request.Root != c.root || request.Profile != c.profile || request.Accepted != c.arguments.AcceptExecutionPolicyRemoval || verifiedexec.EnvironmentValue(c.dependencies.Environment, "PATH") != request.RuntimePATH {
		return task.ErrRequestConflict
	}
	executable, err := c.currentExecutable()
	if err != nil || executable != request.Executable {
		return errors.Join(task.ErrIdentityMismatch, err)
	}
	if request.ExpectedOperation != "" {
		if _, err = pueue.ParseUpgradeOperationID(request.ExpectedOperation); err != nil {
			return err
		}
	}
	return nil
}

func (c invocation) executeGUIUpgrade(ctx context.Context, request gatewayservice.GUIUpgradeRequest, response Response) (Response, gatewayservice.GUIUpgradeResult, error) {
	c.arguments.GUIUpgradeRequest = ""
	c.guiChild, c.guiSource, c.expectedOperation = true, &request.SavedSource, request.ExpectedOperation
	c.noNewNativeOperation = request.NoNewNativeOperation
	c.diagnostic = io.Discard
	reply := response
	operationErr := c.validateGUIChildRequest(ctx, request)
	if operationErr == nil {
		reply, operationErr = c.upgrade(ctx, response)
	}
	if reply.Upgrade != nil {
		reply.Upgrade.RuntimePATHSource = request.PATHSource
	}
	code := 0
	if operationErr != nil {
		reply.Failure, reply.NextAction, code = publicError(operationErr)
	}
	payload, encodeErr := json.Marshal(reply)
	operationErr = errors.Join(operationErr, encodeErr)
	result := gatewayservice.GUIUpgradeResult{Version: 1, Invocation: request.Invocation, ExitCode: code, Response: payload}
	if reply.Upgrade != nil {
		transition := reply.Upgrade.Supervisor
		result.OperationID, result.Phase, result.Pending = transition.OperationID, transition.Phase, transition.Pending
	}
	return reply, result, operationErr
}

func guiUpgradePublicError(err error) (string, string, int, bool) {
	if guiUpgradeUnknownOnly(err) {
		return "gateway_upgrade_uncertain", "inspect_same_profile", 3, true
	}
	if errors.Is(err, gatewayservice.ErrGUIUpgradeRecord) {
		return "gateway_upgrade_record_invalid", "stop_and_report", 1, true
	}
	return "", "", 0, false
}
