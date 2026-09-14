package fixture

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/testutil/contributorprovider/protocol"
)

// selfTest exercises real configuration reads, finite input, optional output,
// exact continuation and create-once receipts without an AI or subprocess.
func selfTest(stdout io.Writer) (resultErr error) {
	directory, err := os.MkdirTemp("", "contributor-fixture-")
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(directory)) }()
	directory, err = filepath.EvalSymlinks(directory)
	if err != nil {
		return err
	}
	settings := filepath.Join(directory, "runtime.json")
	tools := filepath.Join(directory, "tools.json")
	if err := writeJSONOnce(settings, protocol.RuntimeConfig{SchemaVersion: 1, RuntimeDir: directory}); err != nil {
		return err
	}
	if err := writeJSONOnce(tools, protocol.ToolsConfig{SchemaVersion: 1, AllowedTools: []string{}, WorkspaceWrite: false}); err != nil {
		return err
	}
	first := options{
		runtimeConfig: settings, toolsConfig: tools, output: filepath.Join(directory, "first.txt"),
		taskID: strings.Repeat("a", 32), sessionID: "session-" + strings.Repeat("a", 32),
	}
	nonce := "self-test-private-nonce"
	if err := checkExecution(first, protocol.Brief{Case: "present", Answer: nonce, Nonce: nonce}, nonce); err != nil {
		return err
	}
	second := first
	second.taskID, second.output, second.resume = strings.Repeat("b", 32), filepath.Join(directory, "second.txt"), true
	if err := checkExecution(second, protocol.Brief{Case: "resume"}, nonce); err != nil {
		return err
	}
	return writeText(stdout, "PASS contributor fixture configuration, output, continuation and receipts\n")
}

func checkExecution(opts options, brief protocol.Brief, answer string) error {
	data, err := task.MarshalCanonical(brief)
	if err != nil {
		return err
	}
	var output bytes.Buffer
	code, err := execute(opts, bytes.NewReader(data), &output)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("self-test execution code %d", code)
	}
	var envelope protocol.Envelope
	if err = task.DecodeStrict(output.Bytes(), &envelope); err != nil {
		return err
	}
	if envelope.Answer != answer || envelope.SessionID != opts.sessionID || envelope.TaskID != opts.taskID || envelope.Status != "complete" {
		return errors.New("self-test envelope mismatch")
	}
	artifact, err := os.ReadFile(opts.output)
	if err != nil {
		return err
	}
	if string(artifact) != answer {
		return errors.New("self-test artifact mismatch")
	}
	return nil
}
