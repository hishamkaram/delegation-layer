package inspection

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type modelCommandResult struct {
	stdout, stderr []byte
	success        bool
}

func inspectModels(scope execution.PreflightScope, definition provider.InspectionDefinition) (json.RawMessage, error) {
	probe := provider.RuntimeProbeDefinition{Executable: definition.Executable, ExecutableSHA256: definition.ExecutableSHA256}
	if definition.Models == nil {
		return nil, errNativeInspection
	}
	probe.Directory = definition.Directory
	probe.Environment = append([]string(nil), definition.Environment...)
	probe.HelpArgs = append([]string(nil), definition.Models.Capability.HelpArgs...)
	probe.RequiredFlags = append([]string(nil), definition.Models.Capability.RequiredFlags...)
	if err := verifyRuntimeExecutable(probe); err != nil {
		return nil, err
	}
	_, help, err := inspectRuntimeWithHelp(scope, probe, definition.OutputLimit)
	if err != nil {
		return nil, err
	}
	defer clear(help)
	exchange, err := newModelExchange(definition.Models.NewExchange)
	if err != nil {
		return nil, err
	}
	if err = verifyRuntimeExecutable(probe); err != nil {
		return nil, err
	}
	output, err := runModelCommand(scope, definition, definition.Models.Arguments, exchange, nativeHooks{})
	if err != nil {
		return nil, err
	}
	defer clear(output.stdout)
	defer clear(output.stderr)
	if scope.Authorize() != nil || verifyRuntimeExecutable(probe) != nil {
		return nil, errNativeInspection
	}
	return projectModels(definition.Models, output, help)
}

func projectModels(definition *provider.ModelDiscoveryDefinition, output modelCommandResult, help []byte) (facts json.RawMessage, err error) {
	defer func() {
		if recover() != nil {
			facts = nil
			err = errNativeInspection
		}
	}()
	catalog, projectErr := definition.Project(output.stdout, output.stderr, help, output.success)
	if projectErr != nil || provider.ValidateModelCatalog(catalog) != nil {
		catalog = provider.ModelCatalog{Status: "failed", ReasonCode: "invalid_native_output", Source: definition.Source, Models: []provider.ModelInfo{}}
	}
	data, marshalErr := task.MarshalCanonical(catalog)
	if marshalErr != nil || len(data) > provider.MaxModelFactsBytes {
		return nil, errNativeInspection
	}
	return data, nil
}

// Discovery uses the same supervisor lifetime as native inspection. Closing
// stdin ends a metadata exchange; deadline stopping belongs to Pueue.
func runModelCommand(scope execution.PreflightScope, definition provider.InspectionDefinition, args []string, exchange provider.ModelExchange, hooks nativeHooks) (modelCommandResult, error) {
	ctx := scope.Context()
	if ctx == nil || scope.Authorize() != nil {
		return modelCommandResult{}, errNativeInspection
	}
	stdout, err := newNativeStream(definition.OutputLimit, true)
	if err != nil {
		return modelCommandResult{}, err
	}
	stderr, err := newNativeStream(definition.OutputLimit, true)
	if err != nil {
		stdout.discard()
		return modelCommandResult{}, err
	}
	cmd, verified, err := newVerifiedCommand(definition.Executable, definition.ExecutableSHA256, definition.Directory, definition.Environment, args...)
	if err != nil {
		stdout.discard()
		stderr.discard()
		return modelCommandResult{}, errNativeInspection
	}
	cmd.Dir, cmd.Env = definition.Directory, definition.Environment
	cmd.Stdout, cmd.Stderr = stdout.writer, stderr.writer
	input, err := startModelCapture(stdout, stderr, exchange)
	if err != nil {
		stdout.discard()
		stderr.discard()
		return modelCommandResult{}, errors.Join(err, verified.Close())
	}
	if input != nil {
		cmd.Stdin = input
	}
	startErr := scope.Start(func() error { return hooks.startCommand(cmd) })
	closeErr := errors.Join(stdout.writer.Close(), stderr.writer.Close())
	if input != nil {
		closeErr = errors.Join(closeErr, input.Close())
	}
	if startErr == nil {
		closeErr = errors.Join(closeErr, verified.ReleaseDescriptors())
	}
	var waitErr error
	if startErr == nil {
		waitErr = hooks.waitCommand(cmd)
	}
	stdoutErr, stderrErr := <-stdout.done, <-stderr.done
	closeErr = errors.Join(closeErr, verified.Close())
	if errors.Join(startErr, closeErr, stdoutErr, stderrErr, ctx.Err()) != nil {
		clear(stdout.buffer.bytes)
		clear(stderr.buffer.bytes)
		return modelCommandResult{}, errNativeInspection
	}
	return modelCommandResult{stdout: stdout.buffer.bytes, stderr: stderr.buffer.bytes, success: waitErr == nil}, nil
}

func startModelCapture(stdout, stderr *nativeStream, exchange provider.ModelExchange) (*os.File, error) {
	if exchange == nil {
		go stdout.drain()
		go stderr.drain()
		return nil, nil
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, errNativeInspection
	}
	go stdout.exchange(exchange, writer)
	go stderr.drain()
	return reader, nil
}

func (s *nativeStream) exchange(exchange provider.ModelExchange, input *os.File) {
	protocolErr := runModelExchange(exchange, io.TeeReader(io.LimitReader(s.reader, provider.MaxInspectionOutput+1), &s.buffer), input)
	closeInputErr := input.Close()
	// Drain even after a malformed reply so an output-heavy helper cannot
	// deadlock while the supervisor owns its lifetime.
	_, drainErr := io.Copy(&s.buffer, s.reader)
	closeErr := s.reader.Close()
	if s.buffer.overflow || errors.Join(protocolErr, closeInputErr, drainErr, closeErr) != nil {
		s.done <- errNativeInspection
		return
	}
	s.done <- nil
}

func runModelExchange(exchange provider.ModelExchange, output io.Reader, input io.Writer) (resultErr error) {
	defer func() {
		if recover() != nil {
			resultErr = errNativeInspection
		}
	}()
	messages, err := exchange.Start()
	if err != nil {
		return errNativeInspection
	}
	if err = writeModelMessages(input, messages); err != nil {
		return err
	}
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 4096), int(provider.MaxInspectionOutput))
	for scanner.Scan() {
		replies, done, acceptErr := exchange.Accept(scanner.Bytes())
		if acceptErr != nil {
			return errNativeInspection
		}
		if err = writeModelMessages(input, replies); err != nil {
			return err
		}
		if done {
			return nil
		}
	}
	return scanner.Err()
}

func writeModelMessages(writer io.Writer, messages [][]byte) error {
	for _, message := range messages {
		if len(message) > int(provider.MaxInspectionOutput) {
			return errNativeInspection
		}
		line := append(append([]byte(nil), message...), '\n')
		if n, err := writer.Write(line); err != nil || n != len(line) {
			return errNativeInspection
		}
	}
	return nil
}

func (o *Operation) factsLimit() int {
	if o.request.Binding.DefinitionRevision == provider.ModelsRevision {
		return provider.MaxModelFactsBytes
	}
	return maxProjectedFacts
}

func inspectionFactsLimit(limits []int) int {
	if len(limits) > 0 {
		return limits[0]
	}
	return maxProjectedFacts
}

func newModelExchange(factory func() provider.ModelExchange) (exchange provider.ModelExchange, err error) {
	defer func() {
		if recover() != nil {
			exchange = nil
			err = errNativeInspection
		}
	}()
	if factory == nil {
		return nil, nil
	}
	exchange = factory()
	if exchange == nil {
		return nil, errNativeInspection
	}
	return exchange, nil
}

// Model metadata initialization loads the harness's configured integrations;
// measured cold starts can exceed the ordinary capability probe's deadline.
// The revision keeps historical admission deadlines unchanged.
func inspectionTimeout(binding Binding) time.Duration {
	if binding.DefinitionRevision == provider.ModelsRevision {
		return time.Minute
	}
	return AdmissionTimeout
}

// TimeoutForRevision is the finite inspection budget selected by a persisted
// definition revision. Admission probes keep the shorter historical budget;
// model discovery gets its explicitly documented cold-start allowance.
func TimeoutForRevision(revision string) time.Duration {
	return inspectionTimeout(Binding{DefinitionRevision: revision})
}
