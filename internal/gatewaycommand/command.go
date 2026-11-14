package gatewaycommand

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/hishamkaram/delegation-layer-gateway/deviceclient"
	"github.com/hishamkaram/delegation-layer/internal/gatewaycommand/browser"
	"github.com/hishamkaram/delegation-layer/internal/gatewayservice"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

// Dependencies are selected command-edge resources, never global hooks.
// The caller owns the input streams and immutable Native catalog snapshot.
type Dependencies struct {
	Client       Client
	Executable   string
	Descriptions []provider.Description
	Predicates   predicate.Registry
	Environment  []string
	Input        io.Reader
	OpenBrowser  func(context.Context, string) error
	Services     ServiceFactory
}

type Response struct {
	SchemaVersion      int                            `json:"schema_version"`
	Command            string                         `json:"command"`
	Action             string                         `json:"action"`
	Profile            string                         `json:"profile,omitempty"`
	Credential         *deviceclient.ConnectionStatus `json:"credential,omitempty"`
	Online             string                         `json:"online"`
	ExecutionReadiness string                         `json:"execution_readiness"`
	Workspaces         *[]deviceclient.Workspace      `json:"workspaces,omitempty"`
	Workspace          *deviceclient.Workspace        `json:"workspace,omitempty"`
	Doctor             *DoctorObservation             `json:"doctor,omitempty"`
	Service            *gatewayservice.Status         `json:"service,omitempty"`
	Revoked            bool                           `json:"revoked,omitempty"`
	Failure            string                         `json:"failure,omitempty"`
	NextAction         string                         `json:"next_action,omitempty"`
}

type DoctorObservation struct {
	NativeRoot             string    `json:"native_root"`
	RootContextMatches     bool      `json:"root_context_matches"`
	WorkspaceFeature       bool      `json:"workspace_feature"`
	ProviderAuthentication string    `json:"provider_authentication"`
	CheckedAt              time.Time `json:"checked_at"`
}

type invocation struct {
	arguments    Arguments
	root         string
	profile      string
	dependencies Dependencies
	diagnostic   io.Writer
}

func Run(parent context.Context, a Arguments, rawRoot string, jsonOutput bool, stdout, stderr io.Writer, deps Dependencies) int {
	if parent == nil {
		return 1
	}
	ctx := parent
	if a.Action != "serve" && (a.Action != "connect" || !a.Foreground) {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(parent, 10*time.Minute)
		defer cancel()
	}
	if deps.Client == nil {
		deps.Client = sdkClient{}
	}
	response := Response{SchemaVersion: 1, Command: "gateway", Action: a.Action, Online: "unknown", ExecutionReadiness: "unknown"}
	root, err := tasklifecycle.ResolveRoot(rawRoot)
	if err != nil {
		return present(response, err, jsonOutput, stdout, stderr)
	}
	profile, err := profileDirectory(root, a.Profile)
	response.Profile = profile
	if err != nil {
		return present(response, err, jsonOutput, stdout, stderr)
	}
	call := invocation{arguments: a, root: root, profile: profile, dependencies: deps, diagnostic: stderr}
	if rawRoot != "" && a.Action != "connect" {
		if err = call.requireSelectedRoot(ctx); err != nil {
			return present(response, err, jsonOutput, stdout, stderr)
		}
	}
	response, err = call.execute(ctx, response)
	return present(response, err, jsonOutput, stdout, stderr)
}

func (c invocation) requireSelectedRoot(ctx context.Context) error {
	local, err := c.dependencies.Client.LocalContext(ctx, c.profile)
	if err != nil {
		return err
	}
	if local.DelegateRoot != c.root {
		return task.ErrRequestConflict
	}
	return nil
}

func (c invocation) execute(ctx context.Context, response Response) (Response, error) {
	switch c.arguments.Action {
	case "connect":
		return c.connect(ctx, response)
	case "status":
		status, err := c.dependencies.Client.Status(ctx, c.profile)
		response.Credential = &status
		return response, err
	case "doctor":
		return c.doctor(ctx, response)
	case "workspace":
		return c.workspace(ctx, response)
	case "disconnect":
		return c.disconnect(ctx, response)
	case "serve":
		return c.serve(ctx, response)
	case "service":
		return c.service(ctx, response)
	default:
		return response, errors.New("unsupported_gateway_action")
	}
}

func (c invocation) connect(ctx context.Context, response Response) (Response, error) {
	auth, cancel := context.WithTimeout(ctx, 10*time.Minute)
	response, err := c.enroll(auth, response)
	cancel()
	if err != nil {
		return response, err
	}
	if c.arguments.Foreground {
		return c.serve(ctx, response)
	}
	return c.installAndStart(ctx, response)
}

func (c invocation) enroll(ctx context.Context, response Response) (Response, error) {
	policies, err := providerPolicies(c.arguments, c.dependencies.Descriptions)
	if err != nil {
		return response, err
	}
	root, observed, err := rootContext(ctx, c.root, c.dependencies.Predicates, true)
	if err != nil {
		return response, err
	}
	executable, err := c.currentExecutable()
	if err != nil {
		return response, err
	}
	name := c.arguments.DeviceName
	if name == "" {
		name, err = os.Hostname()
		if err != nil {
			return response, err
		}
	}
	options := deviceclient.ConnectOptions{GatewayURL: c.arguments.GatewayURL, ProfileDirectory: c.profile, DelegateExecutable: executable, DelegateRoot: root, DelegateRootContext: observed.RootToken, DeviceName: name, Providers: policies, ReplaceExisting: c.arguments.Replace, PresentChallenge: c.challenge(executable)}
	if c.arguments.PrivateCA != "" {
		options.PrivateCAFile, err = filepath.Abs(c.arguments.PrivateCA)
		if err != nil {
			return response, err
		}
	}
	if c.arguments.TokenFile == "-" {
		options.EnrollmentTokenReader = c.dependencies.Input
	} else if c.arguments.TokenFile != "" {
		options.EnrollmentTokenFile, err = filepath.Abs(c.arguments.TokenFile)
		if err != nil {
			return response, err
		}
	}
	status, err := c.dependencies.Client.Connect(ctx, options)
	response.Credential = &status
	return response, err
}

func (c invocation) currentExecutable() (string, error) {
	executable := c.dependencies.Executable
	if executable == "" {
		var err error
		executable, err = os.Executable()
		if err != nil {
			return "", err
		}
	}
	return verifiedexec.LocatePath(executable)
}

func (c invocation) selectedContext(ctx context.Context) (deviceclient.LocalContextInfo, error) {
	local, err := c.dependencies.Client.LocalContext(ctx, c.profile)
	if err != nil {
		return local, err
	}
	root, observation, err := rootContext(ctx, local.DelegateRoot, c.dependencies.Predicates, false)
	if err != nil {
		return local, err
	}
	if root != local.DelegateRoot || observation.RootToken != local.DelegateRootContext {
		return local, task.ErrIdentityMismatch
	}
	return local, nil
}

func (c invocation) doctor(ctx context.Context, response Response) (Response, error) {
	status, err := c.dependencies.Client.Status(ctx, c.profile)
	response.Credential = &status
	if err != nil {
		return response, err
	}
	local, err := c.selectedContext(ctx)
	if err != nil {
		return response, err
	}
	response.Doctor = &DoctorObservation{NativeRoot: local.DelegateRoot, RootContextMatches: true, WorkspaceFeature: true, ProviderAuthentication: "unknown", CheckedAt: time.Now().UTC()}
	workspaces, err := c.dependencies.Client.ListWorkspaces(ctx, c.profile)
	response.Workspaces = &workspaces
	return response, err
}

func publicError(err error) (failure, next string, exitCode int) {
	switch {
	case errors.Is(err, ErrOfflineCutoverRequired):
		return "root_cutover_required", "run_explicit_offline_cutover", 1
	case errors.Is(err, ErrRootExists):
		return "root_exists", "inspect_existing_root", 1
	case deviceclient.IsBusy(err):
		return "gateway_profile_in_use", "inspect_same_profile", 3
	case errors.Is(err, deviceclient.ErrTrustChanged):
		return "gateway_trust_changed", "stop_and_report", 1
	case errors.Is(err, deviceclient.ErrConflict):
		return "gateway_connection_conflict", "inspect_same_profile", 1
	case publicInterruption(err):
		return "gateway_operation_interrupted", "inspect_same_profile", 1
	case errors.Is(err, task.ErrIdentityMismatch):
		return "native_context_changed", "stop_and_report", 1
	case errors.Is(err, task.ErrRequestConflict):
		return "gateway_source_conflict", "inspect_same_profile", 1
	case gatewayservice.IsPending(err):
		return "gateway_stop_pending", "inspect_same_profile", 3
	case errors.Is(err, gatewayservice.ErrGUIRequired):
		return "gateway_gui_session_required", "run_from_user_gui_terminal", 1
	case errors.Is(err, gatewayservice.ErrServiceChanged):
		return "gateway_service_changed", "retry_same_profile", 1
	case errors.Is(err, gatewayservice.ErrManagerCacheStale):
		return "gateway_service_cache_stale", "retry_service_stop", 1
	case errors.Is(err, gatewayservice.ErrForeignService):
		return "gateway_service_changed", "stop_and_report", 1
	case errors.Is(err, gatewayservice.ErrManager):
		return "gateway_service_manager_failed", "inspect_user_service_manager", 1
	default:
		failure, next = publicSDKError(err)
		return failure, next, 1
	}
}

func publicInterruption(err error) bool {
	return task.AllErrorCauses(err, func(cause error) bool {
		return errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded)
	})
}

func publicSDKError(err error) (code, next string) {
	if errors.Is(err, deviceclient.ErrReplacementRequired) {
		return deviceclient.ErrReplacementRequired.Error(), "authorize_same_profile_replacement"
	}
	for _, kind := range []error{deviceclient.ErrConfiguration, deviceclient.ErrProfile, deviceclient.ErrCapacity, deviceclient.ErrPersistence, deviceclient.ErrDiscovery, deviceclient.ErrUnsupported, deviceclient.ErrTransport, deviceclient.ErrDenied, deviceclient.ErrEnrollment, deviceclient.ErrRevocation} {
		if errors.Is(err, kind) {
			return kind.Error(), "inspect_same_profile"
		}
	}
	return "gateway_operation_failed", "inspect_same_profile"
}

func (c invocation) challenge(executable string) func(context.Context, deviceclient.Challenge) error {
	return func(ctx context.Context, challenge deviceclient.Challenge) error {
		if _, err := fmt.Fprintf(c.diagnostic, "Authorize this device at %s\nUser code: %s\n", challenge.VerificationURI, challenge.UserCode); err != nil {
			return err
		}
		if c.arguments.Headless {
			return nil
		}
		opener := c.dependencies.OpenBrowser
		if opener == nil {
			opener = func(ctx context.Context, uri string) error {
				return browser.Open(ctx, executable, c.dependencies.Environment, uri)
			}
		}
		openCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := opener(openCtx, challenge.VerificationURI); err != nil {
			_, writeErr := io.WriteString(c.diagnostic, "Browser activation was not confirmed; open the URL above in a browser and enter the same code.\n")
			return writeErr
		}
		return nil
	}
}
