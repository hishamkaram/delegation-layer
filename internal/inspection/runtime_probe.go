package inspection

import (
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/provider"
)

// errRuntimeProbe is the fixed failure crossing the inspection-worker
// boundary. Provider output and process diagnostics never become control data.
var errRuntimeProbe = errors.New("provider runtime capability unavailable")

func runtimeProbeError(reason string) error {
	if reason == "" {
		return errRuntimeProbe
	}
	return fmt.Errorf("%w: %s", errRuntimeProbe, reason)
}

type capabilityOutput struct {
	stdout []byte
	stderr []byte
}

// InspectRuntime runs the common version/help capability check inside the
// already-supervised inspection lifetime. It performs no local cancellation:
// every started command and both capture streams are joined before returning.
func InspectRuntime(scope execution.PreflightScope, definition provider.RuntimeProbeDefinition, outputLimit int64) (provider.RuntimeFacts, error) {
	facts, help, err := inspectRuntimeWithHelp(scope, definition, outputLimit)
	clear(help)
	return facts, err
}

// inspectRuntimeWithHelp performs the common capability check and returns the
// bounded help stream to a caller that needs to project metadata from it. The
// caller owns the returned buffer and must clear it after projection.
func inspectRuntimeWithHelp(scope execution.PreflightScope, definition provider.RuntimeProbeDefinition, outputLimit int64) (provider.RuntimeFacts, []byte, error) {
	if scope.Context() == nil || scope.Authorize() != nil || outputLimit <= 0 || outputLimit > provider.MaxInspectionOutput {
		return provider.RuntimeFacts{}, nil, runtimeProbeError("inspection-scope-unavailable")
	}
	if err := verifyRuntimeExecutable(definition); err != nil {
		return provider.RuntimeFacts{}, nil, err
	}
	version, err := runCapabilityCommand(scope, definition, outputLimit, "--version")
	if err != nil {
		return provider.RuntimeFacts{}, nil, fmt.Errorf("%w: version-command", errRuntimeProbe)
	}
	defer clear(version.stdout)
	defer clear(version.stderr)
	reportedVersion, err := provider.ParseCLIVersion(version.stdout)
	if err != nil {
		return provider.RuntimeFacts{}, nil, runtimeProbeError("invalid-version-output")
	}
	if err = verifyRuntimeExecutable(definition); err != nil {
		return provider.RuntimeFacts{}, nil, err
	}
	helpArgs := append(append([]string(nil), definition.HelpArgs...), "--help")
	help, err := runCapabilityCommand(scope, definition, outputLimit, helpArgs...)
	if err != nil {
		return provider.RuntimeFacts{}, nil, fmt.Errorf("%w: help-command", errRuntimeProbe)
	}
	defer clear(help.stdout)
	defer clear(help.stderr)
	for _, flag := range definition.RequiredFlags {
		if !provider.ContainsCLIFlag(help.stdout, flag) && !provider.ContainsCLIFlag(help.stderr, flag) {
			return provider.RuntimeFacts{}, nil, runtimeProbeError("missing-required-flag-" + flag)
		}
	}
	if err = verifyRuntimeExecutable(definition); err != nil {
		return provider.RuntimeFacts{}, nil, err
	}
	facts := provider.RuntimeFacts{Executable: definition.Executable, Version: reportedVersion, SHA256: definition.ExecutableSHA256}
	if err = provider.ValidateRuntimeFacts(facts); err != nil {
		return provider.RuntimeFacts{}, nil, runtimeProbeError("invalid-runtime-facts")
	}
	helpBytes := make([]byte, 0, len(help.stdout)+len(help.stderr))
	helpBytes = append(helpBytes, help.stdout...)
	helpBytes = append(helpBytes, help.stderr...)
	return facts, helpBytes, nil
}

func verifyRuntimeExecutable(definition provider.RuntimeProbeDefinition) error {
	digest, err := provider.FingerprintExecutable(definition.Executable)
	if err != nil || digest != definition.ExecutableSHA256 {
		return runtimeProbeError("executable-identity-drift")
	}
	return nil
}

func runCapabilityCommand(scope execution.PreflightScope, definition provider.RuntimeProbeDefinition, outputLimit int64, args ...string) (capabilityOutput, error) {
	ctx := scope.Context()
	if ctx == nil || ctx.Err() != nil || scope.Authorize() != nil {
		return capabilityOutput{}, runtimeProbeError("inspection-scope-expired")
	}
	stdout, err := newNativeStream(outputLimit, true)
	if err != nil {
		return capabilityOutput{}, runtimeProbeError("stdout-capture-unavailable")
	}
	stderr, err := newNativeStream(outputLimit, true)
	if err != nil {
		stdout.discard()
		return capabilityOutput{}, runtimeProbeError("stderr-capture-unavailable")
	}
	cmd, verified, err := newVerifiedCommand(definition.Executable, definition.ExecutableSHA256, definition.Directory, definition.Environment, args...)
	if err != nil {
		stdout.discard()
		stderr.discard()
		return capabilityOutput{}, runtimeProbeError("executable-identity-drift")
	}
	cmd.Dir, cmd.Env = definition.Directory, definition.Environment
	cmd.Stdout, cmd.Stderr = stdout.writer, stderr.writer
	go stdout.drain()
	go stderr.drain()
	startErr := scope.Start(cmd.Start)
	closeErr := errors.Join(stdout.writer.Close(), stderr.writer.Close())
	if startErr == nil {
		closeErr = errors.Join(closeErr, verified.ReleaseDescriptors())
	}
	var waitErr error
	if startErr == nil {
		waitErr = cmd.Wait()
		if cmd.ProcessState == nil || !cmd.ProcessState.Success() {
			waitErr = errors.Join(waitErr, errRuntimeProbe)
		}
	}
	stdoutErr, stderrErr := <-stdout.done, <-stderr.done
	closeErr = errors.Join(closeErr, verified.Close())
	if joined := errors.Join(startErr, closeErr, waitErr, stdoutErr, stderrErr, ctx.Err()); joined != nil {
		clear(stdout.buffer.bytes)
		clear(stderr.buffer.bytes)
		return capabilityOutput{}, runtimeProbeError("capability-command-failed")
	}
	return capabilityOutput{stdout: stdout.buffer.bytes, stderr: stderr.buffer.bytes}, nil
}
