package inspection

import (
	"errors"
	"os/exec"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/provider"
)

// errRuntimeProbe is the fixed failure crossing the inspection-worker
// boundary. Provider output and process diagnostics never become control data.
var errRuntimeProbe = errors.New("provider runtime capability unavailable")

type capabilityOutput struct {
	stdout []byte
	stderr []byte
}

// InspectRuntime runs the common version/help capability check inside the
// already-supervised inspection lifetime. It performs no local cancellation:
// every started command and both capture streams are joined before returning.
func InspectRuntime(scope execution.PreflightScope, definition provider.RuntimeProbeDefinition, outputLimit int64) (provider.RuntimeFacts, error) {
	if scope.Context() == nil || scope.Authorize() != nil || outputLimit <= 0 || outputLimit > provider.MaxInspectionOutput {
		return provider.RuntimeFacts{}, errRuntimeProbe
	}
	if err := verifyRuntimeExecutable(definition); err != nil {
		return provider.RuntimeFacts{}, errRuntimeProbe
	}
	version, err := runCapabilityCommand(scope, definition, outputLimit, "--version")
	if err != nil {
		return provider.RuntimeFacts{}, errRuntimeProbe
	}
	reportedVersion, err := provider.ParseCLIVersion(version.stdout)
	if err != nil {
		return provider.RuntimeFacts{}, errRuntimeProbe
	}
	if err = verifyRuntimeExecutable(definition); err != nil {
		return provider.RuntimeFacts{}, errRuntimeProbe
	}
	helpArgs := append(append([]string(nil), definition.HelpArgs...), "--help")
	help, err := runCapabilityCommand(scope, definition, outputLimit, helpArgs...)
	if err != nil {
		return provider.RuntimeFacts{}, errRuntimeProbe
	}
	for _, flag := range definition.RequiredFlags {
		if !provider.ContainsCLIFlag(help.stdout, flag) && !provider.ContainsCLIFlag(help.stderr, flag) {
			return provider.RuntimeFacts{}, errRuntimeProbe
		}
	}
	if err = verifyRuntimeExecutable(definition); err != nil {
		return provider.RuntimeFacts{}, errRuntimeProbe
	}
	facts := provider.RuntimeFacts{Executable: definition.Executable, Version: reportedVersion, SHA256: definition.ExecutableSHA256}
	if err = provider.ValidateRuntimeFacts(facts); err != nil {
		return provider.RuntimeFacts{}, errRuntimeProbe
	}
	return facts, nil
}

func verifyRuntimeExecutable(definition provider.RuntimeProbeDefinition) error {
	digest, err := provider.FingerprintExecutable(definition.Executable)
	if err != nil || digest != definition.ExecutableSHA256 {
		return errRuntimeProbe
	}
	return nil
}

func runCapabilityCommand(scope execution.PreflightScope, definition provider.RuntimeProbeDefinition, outputLimit int64, args ...string) (capabilityOutput, error) {
	ctx := scope.Context()
	if ctx == nil || ctx.Err() != nil || scope.Authorize() != nil {
		return capabilityOutput{}, errRuntimeProbe
	}
	stdout, err := newNativeStream(outputLimit, true)
	if err != nil {
		return capabilityOutput{}, errRuntimeProbe
	}
	stderr, err := newNativeStream(outputLimit, true)
	if err != nil {
		stdout.discard()
		return capabilityOutput{}, errRuntimeProbe
	}
	cmd := exec.Command(definition.Executable, args...)
	cmd.Dir, cmd.Env = definition.Directory, definition.Environment
	cmd.Stdout, cmd.Stderr = stdout.writer, stderr.writer
	go stdout.drain()
	go stderr.drain()
	startErr := scope.Authorize()
	if startErr == nil && ctx.Err() == nil {
		startErr = cmd.Start()
	} else {
		startErr = errRuntimeProbe
	}
	closeErr := errors.Join(stdout.writer.Close(), stderr.writer.Close())
	var waitErr error
	if startErr == nil {
		waitErr = cmd.Wait()
		if cmd.ProcessState == nil || !cmd.ProcessState.Success() {
			waitErr = errors.Join(waitErr, errRuntimeProbe)
		}
	}
	stdoutErr, stderrErr := <-stdout.done, <-stderr.done
	if joined := errors.Join(startErr, closeErr, waitErr, stdoutErr, stderrErr, ctx.Err()); joined != nil {
		clear(stdout.buffer.bytes)
		clear(stderr.buffer.bytes)
		return capabilityOutput{}, errRuntimeProbe
	}
	return capabilityOutput{stdout: stdout.buffer.bytes, stderr: stderr.buffer.bytes}, nil
}
