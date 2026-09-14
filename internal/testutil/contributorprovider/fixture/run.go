// Package fixture implements a finite, controlled provider executable. It does
// not import adapter registration or certification, so its measured binary is
// independent of the certificate later embedded in the acceptance CLI.
package fixture

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/testutil/contributorprovider/protocol"
)

const RuntimeVersion = "contributor-provider 1"

var sessionPattern = regexp.MustCompile(`^session-[0-9a-f]{32}$`)

type options struct {
	runtimeConfig string
	toolsConfig   string
	output        string
	taskID        string
	sessionID     string
	resume        bool
}

// Run always consumes finite input and completes synchronously. The caller
// owns standard streams; no child, goroutine, retry, or shell is created.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "--version" {
		return report(stderr, writeText(stdout, RuntimeVersion+"\n"))
	}
	if len(args) == 1 && args[0] == "--self-test" {
		return report(stderr, selfTest(stdout))
	}
	opts, err := parseOptions(args, stderr)
	if err != nil {
		return report(stderr, err)
	}
	code, err := execute(opts, stdin, stdout)
	if err != nil {
		return report(stderr, err)
	}
	return code
}

func report(stderr io.Writer, err error) int {
	if err == nil {
		return 0
	}
	if _, writeErr := fmt.Fprintln(stderr, err); writeErr != nil {
		return 2
	}
	return 2
}

func writeText(writer io.Writer, value string) error {
	_, err := io.WriteString(writer, value)
	return err
}

func parseOptions(args []string, stderr io.Writer) (options, error) {
	var opts options
	flags := flag.NewFlagSet("contributor-provider", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&opts.runtimeConfig, "runtime-config", "", "core-owned runtime configuration")
	flags.StringVar(&opts.toolsConfig, "tools-config", "", "core-owned tool configuration")
	flags.StringVar(&opts.output, "output", "", "core-owned optional output staging path")
	flags.StringVar(&opts.taskID, "task-id", "", "exact layer task identity")
	flags.StringVar(&opts.sessionID, "session-id", "", "exact synthetic session")
	flags.BoolVar(&opts.resume, "resume", false, "resume the explicit session")
	if err := flags.Parse(args); err != nil {
		return opts, err
	}
	if flags.NArg() != 0 {
		return opts, errors.New("unexpected positional argument")
	}
	if err := task.ValidateTaskID(opts.taskID); err != nil {
		return opts, err
	}
	if !sessionPattern.MatchString(opts.sessionID) {
		return opts, errors.New("invalid synthetic session ID")
	}
	for _, path := range []string{opts.runtimeConfig, opts.toolsConfig, opts.output} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return opts, errors.New("fixture paths must be absolute and clean")
		}
	}
	return opts, nil
}

func execute(opts options, stdin io.Reader, stdout io.Writer) (int, error) {
	runtimeDir, err := loadConfiguration(opts)
	if err != nil {
		return 0, err
	}
	var brief protocol.Brief
	if err = readJSON(stdin, &brief); err != nil {
		return 0, err
	}
	if opts.resume != (brief.Case == "resume") {
		return 0, errors.New("resume request and launch flags disagree")
	}
	if err = writeReceipt(runtimeDir, "launches", opts, "entered"); err != nil {
		return 0, err
	}
	answer, err := sessionAnswer(runtimeDir, opts, brief)
	if err != nil {
		return 0, err
	}
	envelope := protocol.Envelope{
		Protocol: "contributor-proof/v1", TaskID: opts.taskID,
		SessionID: opts.sessionID, Status: "complete", Answer: answer,
	}
	output, present, code, err := applyCase(brief.Case, &envelope)
	if err != nil {
		return 0, err
	}
	envelopeData, err := marshalEnvelope(brief.Case, envelope)
	if err != nil {
		return 0, err
	}
	if present {
		if err := writeOnce(opts.output, []byte(output)); err != nil {
			return 0, err
		}
	}
	if err := emitEnvelope(stdout, envelopeData); err != nil {
		return 0, err
	}
	if err := writeReceipt(runtimeDir, "completions", opts, "completed"); err != nil {
		return 0, err
	}
	return code, nil
}

func applyCase(name string, envelope *protocol.Envelope) (string, bool, int, error) {
	output, present, code := envelope.Answer, true, 0
	switch name {
	case "present", "resume":
	case "absent", "invalid-utf8":
		present = false
	case "empty":
		output = ""
	case "conflict":
		output = envelope.Answer + " conflicting artifact"
	case "rejected":
		envelope.Status = "rejected"
	case "malformed":
	case "wrong-task":
		envelope.TaskID = "00000000000000000000000000000000"
	case "wrong-session":
		envelope.SessionID = "session-00000000000000000000000000000000"
	case "nonzero":
		code = 7
	case "oversized-envelope":
		envelope.Answer = strings.Repeat("x", protocol.MaxEnvelopeBytes)
	case "oversized-output":
		output = strings.Repeat("x", protocol.MaxEnvelopeBytes+1)
	default:
		return "", false, 0, errors.New("unsupported fixture case")
	}
	return output, present, code, nil
}

func marshalEnvelope(name string, envelope protocol.Envelope) ([]byte, error) {
	if name == "malformed" {
		return []byte("{\"protocol\":"), nil
	}
	data, err := task.MarshalCanonical(envelope)
	if err != nil {
		return nil, err
	}
	if name == "invalid-utf8" {
		marker := []byte(`"answer":"`)
		start := bytes.Index(data, marker)
		if start < 0 {
			return nil, errors.New("fixture answer marker missing")
		}
		data[start+len(marker)] = 0xff
	}
	// The oversized-envelope case is an intentional semantic fault fixture.
	// Every normal case is checked after JSON marshaling, so escaping overhead
	// cannot turn an admitted brief into an over-limit successful envelope.
	if name != "oversized-envelope" && len(data) > protocol.MaxEnvelopeBytes {
		return nil, task.ErrControlRecordTooBig
	}
	return data, nil
}

func emitEnvelope(stdout io.Writer, data []byte) error {
	n, err := stdout.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

func loadConfiguration(opts options) (string, error) {
	var runtime protocol.RuntimeConfig
	if err := readJSONFile(opts.runtimeConfig, &runtime); err != nil {
		return "", err
	}
	var tools protocol.ToolsConfig
	if err := readJSONFile(opts.toolsConfig, &tools); err != nil {
		return "", err
	}
	if runtime.SchemaVersion != 1 || tools.SchemaVersion != 1 || tools.WorkspaceWrite || len(tools.AllowedTools) != 0 {
		return "", errors.New("unsupported fixture configuration")
	}
	canonical, err := filepath.EvalSymlinks(runtime.RuntimeDir)
	if err != nil || !filepath.IsAbs(runtime.RuntimeDir) || canonical != runtime.RuntimeDir {
		return "", errors.New("runtime directory must exist at its canonical absolute path")
	}
	for _, directory := range []string{runtime.RuntimeDir, filepath.Join(runtime.RuntimeDir, "sessions"), filepath.Join(runtime.RuntimeDir, "launches"), filepath.Join(runtime.RuntimeDir, "completions")} {
		if err := ensurePrivateDirectory(directory); err != nil {
			return "", err
		}
	}
	return runtime.RuntimeDir, nil
}

func readJSON(reader io.Reader, value any) error {
	return readBoundedJSON(reader, value, protocol.MaxBriefBytes)
}

func readBoundedJSON(reader io.Reader, value any, maximum int64) error {
	data, err := task.ReadBounded(reader, maximum)
	if err != nil {
		return err
	}
	return task.DecodeStrict(data, value)
}

func readJSONFile(path string, value any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("configuration must be a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(readBoundedJSON(file, value, protocol.MaxEnvelopeBytes), file.Close())
}
