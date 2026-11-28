package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"path/filepath"
	"slices"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspacefiles"
)

// FilesystemArguments are local public CLI inputs. Remote selectors are mapped
// to a saved local approval by the connector, never interpreted as raw argv.
type FilesystemArguments struct {
	Operation   string
	RequestFile string
}

func filesystemSpec() commandSpec {
	return commandSpec{Name: "filesystem", Summary: "Read a selected workspace or automatic system temporary scope", Children: []commandSpec{
		leaf("list", "List one bounded live directory page", ""),
		leaf("read", "Read a bounded regular-file range", ""),
		leaf("inspect", "Inspect one selected entry", ""),
		leaf("git", "Inspect rooted Git structural evidence", ""),
		leaf("scopes", "Observe automatic native temporary scopes", ""),
	}}
}

func registerFilesystemFlags(fs *flag.FlagSet, a *Arguments) {
	fs.StringVar(&a.Filesystem.RequestFile, "request", "", "strict filesystem selection JSON file or - for stdin")
	fs.DurationVar(&a.Connector.ControlTimeout, "control-timeout", defaultConnectorControlTimeout, "finite filesystem observation deadline")
}

func validateFilesystemArguments(a Arguments, positional []string) error {
	if err := validateNoPositional("filesystem", positional); err != nil {
		return err
	}
	if a.Filesystem.RequestFile == "" || (a.Filesystem.RequestFile != "-" && !filepath.IsAbs(a.Filesystem.RequestFile)) {
		return errors.New("filesystem requires --request ABS or -")
	}
	if a.Connector.ControlTimeout <= 0 || a.Connector.ControlTimeout > defaultConnectorControlTimeout {
		return errors.New("filesystem control timeout must be positive and at most 30s")
	}
	if a.PueueConfig != "" || a.Runner != "" {
		return errors.New("filesystem does not accept supervisor or runner overrides")
	}
	return nil
}

type filesystemEnvelope struct {
	SchemaVersion int                    `json:"schema_version"`
	Command       string                 `json:"command"`
	Filesystem    *workspacefiles.Result `json:"filesystem,omitempty"`
	Failure       *workspacefiles.Error  `json:"failure,omitempty"`
}

func runFilesystem(ctx context.Context, a Arguments, deps Dependencies, stdout, stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(ctx, a.Connector.ControlTimeout)
	defer cancel()
	var request workspacefiles.Request
	if err := readConnectorJSON(ctx, a.Filesystem.RequestFile, deps.Input, &request); err != nil {
		failure := &workspacefiles.Error{Code: workspacefiles.InvalidRequest, Stage: "request"}
		code := 2
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			failure.Code, failure.Retryable, code = workspacefiles.Timeout, true, 1
		}
		return writeFilesystemEnvelope(stdout, filesystemEnvelope{SchemaVersion: 1, Command: "filesystem", Failure: failure}, code)
	}
	root := a.Root
	if root == "" && !pureFilesystemScopeDiscovery(a, request) {
		resolved, err := config.ResolveDefaultRoot()
		if err != nil {
			return writeFilesystemEnvelope(stdout, filesystemEnvelope{SchemaVersion: 1, Command: "filesystem", Failure: &workspacefiles.Error{Code: workspacefiles.InvalidRequest, Stage: "root"}}, 1)
		}
		root = resolved
	}
	result, err := workspacefiles.Execute(ctx, root, workspacefiles.Operation(a.Filesystem.Operation), request)
	if err != nil {
		var failure *workspacefiles.Error
		if !errors.As(err, &failure) {
			failure = &workspacefiles.Error{Code: workspacefiles.ResourceLimit, Stage: "observe", Retryable: true}
		}
		return writeFilesystemEnvelope(stdout, filesystemEnvelope{SchemaVersion: 1, Command: "filesystem", Failure: failure}, 1)
	}
	return writeFilesystemEnvelope(stdout, filesystemEnvelope{SchemaVersion: 1, Command: "filesystem", Filesystem: &result}, 0)
}

func pureFilesystemScopeDiscovery(a Arguments, request workspacefiles.Request) bool {
	return a.Filesystem.Operation == string(workspacefiles.Scopes) && request.ExpectedRoot == "" && request.Scope == (workspacefiles.Scope{})
}

// Executable capabilities are a public projection, not persisted root format
// requirements. Only successful public context inspection calls this helper.
func projectExecutableContextFeatures(response *Response) {
	if response.Connector == nil {
		return
	}
	projected := *response.Connector
	projected.Features = slices.Clone(projected.Features)
	for _, feature := range []string{task.WorkspaceFilesystemFeature, task.NativeExecutionOptionsFeature, task.ProjectTokenSHA256Feature, task.GuardedContinuationFeature, task.ScopedModelDiscoveryFeature, task.OriginBindingFeature, task.ExecutionBudgetFeature} {
		if !slices.Contains(projected.Features, feature) {
			projected.Features = append(projected.Features, feature)
		}
	}
	response.Connector = &projected
}

func writeFilesystemEnvelope(output io.Writer, value filesystemEnvelope, code int) int {
	if err := json.NewEncoder(output).Encode(value); err != nil {
		return 1
	}
	return code
}
