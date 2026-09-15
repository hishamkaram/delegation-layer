package fakeprovider

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

const (
	maxProviderLifetime = 60 * time.Second
	maxGeneratedBytes   = 64 << 20
	maxArgvEntries      = 32
	maxArgvStringBytes  = 4096
)

// ProviderConfig is the strict, acceptance-only configuration for cmd/provider.
// Argv is recorded as inert data and is never passed to another executable.
type ProviderConfig struct {
	Scenario        string   `json:"scenario"`
	ArtifactDir     string   `json:"artifact_dir"`
	LifetimeMS      int64    `json:"lifetime_ms"`
	TaskID          string   `json:"task_id"`
	SessionID       string   `json:"session_id"`
	Argv            []string `json:"argv"`
	Answer          string   `json:"answer,omitempty"`
	StdoutBytes     int64    `json:"stdout_bytes,omitempty"`
	StderrBytes     int64    `json:"stderr_bytes,omitempty"`
	RendezvousPath  string   `json:"rendezvous_path,omitempty"`
	ChildLifetimeMS int64    `json:"child_lifetime_ms,omitempty"`
}

type providerReceipt struct {
	SchemaVersion int      `json:"schema_version"`
	InvocationID  string   `json:"invocation_id"`
	InvocationDir string   `json:"invocation_dir"`
	Scenario      string   `json:"scenario"`
	ArtifactDir   string   `json:"artifact_dir"`
	TaskID        string   `json:"task_id"`
	SessionID     string   `json:"session_id"`
	Argv          []string `json:"argv"`
	DeclaredArgv  []string `json:"declared_argv"`
	ProcessArgv   []string `json:"process_argv"`
	CWD           string   `json:"cwd"`
	PID           int      `json:"pid"`
	Entry         string   `json:"entry"`
	Completed     string   `json:"completed,omitempty"`
	ExitCode      int      `json:"exit_code"`
	Natural       bool     `json:"natural"`
	ChildPID      int      `json:"child_pid,omitempty"`
}

// ExitError carries a deliberate finite fixture exit status to cmd/provider.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("fixture provider exit %d", e.Code) }

// RunProvider executes one finite configured fixture using the supplied streams.
// It is the unit-test convenience form and uses the declared config literals.
func RunProvider(configPath string, input io.Reader, stdout, stderr io.Writer) error {
	return runProvider(configPath, nil, input, stdout, stderr, false)
}

// RunProviderArgs runs the provider after verifying explicit inert literals
// against ProviderConfig.Argv. The literals are recorded but never executed.
func RunProviderArgs(configPath string, literalArgs []string, input io.Reader, stdout, stderr io.Writer) error {
	return runProvider(configPath, literalArgs, input, stdout, stderr, false)
}

// RunChild is used only by the provider's fixed self-helper invocation.
func RunChild(configPath string, _ io.Reader, stdout, stderr io.Writer) error {
	return runProvider(configPath, nil, strings.NewReader(""), stdout, stderr, true)
}

// LoadProviderConfig strictly reads one public fixture configuration. The
// acceptance composition uses it to validate a prepared profile without
// duplicating the provider schema or allowing child-only scenarios.
func LoadProviderConfig(configPath string) (ProviderConfig, error) {
	return loadProviderConfig(configPath, false)
}

// ParseProviderConfig strictly parses one public fixture configuration from
// already-read bytes. Callers that authenticate a file by digest can parse
// the exact bytes they authenticated without reopening the path.
func ParseProviderConfig(data []byte) (ProviderConfig, error) {
	if int64(len(data)) > task.MaxControlRecordSize {
		return ProviderConfig{}, errors.New("provider config exceeds maximum size")
	}
	return parseProviderConfig(data, false)
}

func runProvider(configPath string, literalArgs []string, input io.Reader, stdout, stderr io.Writer, child bool) error {
	if input == nil || stdout == nil || stderr == nil {
		return errors.New("provider streams must be nonnil")
	}
	cfg, err := loadProviderConfig(configPath, child)
	if err != nil {
		return err
	}
	if child {
		cfg.Scenario = "child"
	}
	if literalArgs == nil {
		literalArgs = append([]string(nil), cfg.Argv...)
	}
	brief, entry, err := prepareProvider(cfg, literalArgs, input, child)
	if err != nil {
		return err
	}
	if !equalStrings(literalArgs, cfg.Argv) {
		return fmt.Errorf("provider literal argv does not match config")
	}
	if err = writeExpectedArtifacts(cfg, brief, child); err != nil {
		return err
	}
	childPID, err := runProviderScenario(cfg, brief, stdout, stderr, child, entry)
	if err != nil {
		return err
	}
	exitCode := 0
	if cfg.Scenario == "nonzero" {
		exitCode = 7
	}
	completion := entry
	completion.Completed = time.Now().UTC().Format(time.RFC3339Nano)
	completion.ExitCode, completion.Natural, completion.ChildPID = exitCode, true, childPID
	if err = writeReceiptExclusive(filepath.Join(entry.InvocationDir, "provider.complete.json"), completion); err != nil {
		return err
	}
	if err = writeReceiptIfAbsent(filepath.Join(cfg.ArtifactDir, artifactName(child, "provider.complete.json")), completion); err != nil {
		return err
	}
	if exitCode != 0 {
		return &ExitError{Code: exitCode}
	}
	return nil
}

func prepareProvider(cfg ProviderConfig, literalArgs []string, input io.Reader, child bool) ([]byte, providerReceipt, error) {
	if err := os.MkdirAll(cfg.ArtifactDir, 0o700); err != nil {
		return nil, providerReceipt{}, fmt.Errorf("creating provider artifact directory: %w", err)
	}
	invocationID, err := task.NewRandomID()
	if err != nil {
		return nil, providerReceipt{}, err
	}
	invocationDir := filepath.Join(cfg.ArtifactDir, "invocations", invocationID)
	if err = os.MkdirAll(invocationDir, 0o700); err != nil {
		return nil, providerReceipt{}, fmt.Errorf("creating provider invocation directory: %w", err)
	}
	brief, err := task.ReadBounded(input, task.MaxBriefSize)
	if err != nil {
		return nil, providerReceipt{}, err
	}
	if err = writeArtifact(filepath.Join(cfg.ArtifactDir, artifactName(child, "brief.input")), brief); err != nil {
		return nil, providerReceipt{}, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, providerReceipt{}, fmt.Errorf("getting provider cwd: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	entry := providerReceipt{SchemaVersion: 1, InvocationID: invocationID, InvocationDir: invocationDir, Scenario: cfg.Scenario, ArtifactDir: cfg.ArtifactDir, TaskID: cfg.TaskID, SessionID: cfg.SessionID, Argv: append([]string(nil), literalArgs...), DeclaredArgv: append([]string(nil), cfg.Argv...), ProcessArgv: append([]string(nil), os.Args...), CWD: cwd, PID: os.Getpid(), Entry: now}
	if err = writeReceiptExclusive(filepath.Join(invocationDir, "provider.entry.json"), entry); err != nil {
		return nil, providerReceipt{}, err
	}
	if err = writeReceiptIfAbsent(filepath.Join(cfg.ArtifactDir, artifactName(child, "provider.entry.json")), entry); err != nil {
		return nil, providerReceipt{}, err
	}
	return brief, entry, nil
}

func writeExpectedArtifacts(cfg ProviderConfig, brief []byte, child bool) error {
	expectedStdout := filepath.Join(cfg.ArtifactDir, artifactName(child, "expected.stdout"))
	expectedStderr := filepath.Join(cfg.ArtifactDir, artifactName(child, "expected.stderr"))
	stdoutFile, err := os.OpenFile(expectedStdout, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("creating expected stdout: %w", err)
	}
	stderrFile, err := os.OpenFile(expectedStderr, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return errors.Join(err, stdoutFile.Close())
	}
	writeErr := writeScenario(cfg, brief, stdoutFile, stderrFile)
	writeErr = errors.Join(writeErr, stdoutFile.Sync(), stderrFile.Sync(), stdoutFile.Close(), stderrFile.Close())
	return writeErr
}

func runProviderScenario(cfg ProviderConfig, brief []byte, stdout, stderr io.Writer, child bool, entry providerReceipt) (int, error) {
	childPID := 0
	var err error
	if cfg.Scenario == "parent_exit" && !child {
		childPID, err = startChild(cfg, stdout, stderr, entry)
		if err != nil {
			return 0, err
		}
	} else if cfg.Scenario == "child" {
		err = writeChildScenario(cfg, stdout)
		if err != nil {
			return 0, err
		}
	} else if cfg.Scenario == "late_session" {
		err = writeLateSessionScenario(cfg, stdout)
		if err != nil {
			return 0, err
		}
	} else {
		if err = writeScenario(cfg, brief, stdout, stderr); err != nil {
			return 0, err
		}
		if cfg.Scenario == "hold" {
			if err = finiteHold(cfg); err != nil {
				return 0, err
			}
		}
	}
	return childPID, nil
}

func writeLateSessionScenario(cfg ProviderConfig, stdout io.Writer) error {
	answer := cfg.Answer
	if answer == "" {
		answer = "late session answer"
	}
	if err := writeAnswer(stdout, cfg.TaskID, answer); err != nil {
		return err
	}
	if err := finiteHold(cfg); err != nil {
		return err
	}
	if err := writeSession(stdout, cfg.TaskID, cfg.SessionID); err != nil {
		return err
	}
	return writeResult(stdout, cfg.TaskID, cfg.SessionID, true, true)
}

func writeChildScenario(cfg ProviderConfig, stdout io.Writer) error {
	if err := writeSession(stdout, cfg.TaskID, cfg.SessionID); err != nil {
		return err
	}
	if err := finiteHold(cfg); err != nil {
		return err
	}
	answer := cfg.Answer
	if answer == "" {
		answer = "child fixture answer"
	}
	if err := writeAnswer(stdout, cfg.TaskID, answer); err != nil {
		return err
	}
	return writeResult(stdout, cfg.TaskID, cfg.SessionID, true, true)
}

func loadProviderConfig(path string, allowChild bool) (ProviderConfig, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ProviderConfig{}, errors.New("provider config path must be clean and absolute")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return ProviderConfig{}, fmt.Errorf("opening provider config: %w", err)
	}
	info, statErr := f.Stat()
	if statErr != nil {
		return ProviderConfig{}, errors.Join(statErr, f.Close())
	}
	if !info.Mode().IsRegular() {
		return ProviderConfig{}, errors.Join(errors.New("provider config must be a regular file"), f.Close())
	}
	data, readErr := task.ReadBounded(f, task.MaxControlRecordSize)
	closeErr := f.Close()
	if err = errors.Join(readErr, closeErr); err != nil {
		return ProviderConfig{}, err
	}
	return parseProviderConfig(data, allowChild)
}

func parseProviderConfig(data []byte, allowChild bool) (ProviderConfig, error) {
	var cfg ProviderConfig
	if err := task.DecodeStrict(data, &cfg); err != nil {
		return ProviderConfig{}, fmt.Errorf("decoding provider config: %w", err)
	}
	if err := validateProviderConfig(cfg, allowChild); err != nil {
		return ProviderConfig{}, err
	}
	return cfg, nil
}

func validateProviderConfig(cfg ProviderConfig, allowChild bool) error {
	if err := validateScenario(cfg.Scenario, allowChild); err != nil {
		return err
	}
	if err := validateProviderIdentity(cfg); err != nil {
		return err
	}
	if err := validateProviderArgv(cfg.Argv); err != nil {
		return err
	}
	return validateProviderBounds(cfg)
}

func validateScenario(scenario string, allowChild bool) error {
	switch scenario {
	case "success", "large", "empty", "diagnostic", "malformed", "truncated", "nonzero", "late_session", "hold", "parent_exit":
		return nil
	case "child":
		if allowChild {
			return nil
		}
	}
	return fmt.Errorf("unsupported provider scenario %q", scenario)
}

func validateProviderIdentity(cfg ProviderConfig) error {
	if !filepath.IsAbs(cfg.ArtifactDir) || filepath.Clean(cfg.ArtifactDir) != cfg.ArtifactDir {
		return errors.New("provider artifact_dir must be clean and absolute")
	}
	if cfg.LifetimeMS <= 0 || cfg.LifetimeMS > maxProviderLifetime.Milliseconds() {
		return errors.New("provider lifetime_ms must be positive and at most 60000")
	}
	if err := task.ValidateTaskID(cfg.TaskID); err != nil {
		return err
	}
	if !validSessionID(cfg.SessionID) {
		return errors.New("provider session_id is invalid")
	}
	return nil
}

func validateProviderArgv(argv []string) error {
	if argv == nil || len(argv) > maxArgvEntries {
		return errors.New("provider argv must be a bounded literal array")
	}
	for _, arg := range argv {
		if len(arg) > maxArgvStringBytes || strings.ContainsRune(arg, '\x00') {
			return errors.New("provider argv contains an invalid literal")
		}
	}
	return nil
}

func validateProviderBounds(cfg ProviderConfig) error {
	if cfg.StdoutBytes < 0 || cfg.StdoutBytes > maxGeneratedBytes || cfg.StderrBytes < 0 || cfg.StderrBytes > maxGeneratedBytes {
		return errors.New("provider generated output exceeds the finite bound")
	}
	if cfg.RendezvousPath != "" && (!filepath.IsAbs(cfg.RendezvousPath) || filepath.Clean(cfg.RendezvousPath) != cfg.RendezvousPath) {
		return errors.New("provider rendezvous_path must be clean and absolute")
	}
	if cfg.ChildLifetimeMS < 0 || cfg.ChildLifetimeMS > maxProviderLifetime.Milliseconds() {
		return errors.New("provider child_lifetime_ms exceeds the finite bound")
	}
	return nil
}

func writeScenario(cfg ProviderConfig, brief []byte, stdout, stderr io.Writer) error {
	switch cfg.Scenario {
	case "success", "hold", "nonzero", "diagnostic":
		return writeStandardScenario(cfg, brief, stdout, stderr)
	case "large":
		return writeLarge(cfg, stdout, stderr)
	case "empty":
		return writeEmptyScenario(cfg, stdout)
	case "malformed":
		return writeMalformedScenario(cfg, stdout)
	case "truncated":
		return writeTruncatedScenario(cfg, stdout)
	case "late_session":
		return writeLateSessionExpected(cfg, stdout)
	case "parent_exit":
		return nil
	case "child":
		return writeChildExpected(cfg, stdout)
	default:
		return fmt.Errorf("unsupported provider scenario %q", cfg.Scenario)
	}
}

func writeStandardScenario(cfg ProviderConfig, brief []byte, stdout, stderr io.Writer) error {
	answer := cfg.Answer
	if answer == "" {
		answer = string(brief)
		if answer == "" {
			answer = "fixture answer"
		}
	}
	if err := writeSession(stdout, cfg.TaskID, cfg.SessionID); err != nil {
		return err
	}
	if err := writeAnswer(stdout, cfg.TaskID, answer); err != nil {
		return err
	}
	if err := writeResult(stdout, cfg.TaskID, cfg.SessionID, true, true); err != nil {
		return err
	}
	if cfg.Scenario == "diagnostic" {
		return writeAll(stderr, []byte(stderrMarker))
	}
	return nil
}

func writeEmptyScenario(cfg ProviderConfig, stdout io.Writer) error {
	if err := writeSession(stdout, cfg.TaskID, cfg.SessionID); err != nil {
		return err
	}
	if err := writeAnswer(stdout, cfg.TaskID, " \t\n"); err != nil {
		return err
	}
	return writeResult(stdout, cfg.TaskID, cfg.SessionID, true, true)
}

func writeMalformedScenario(cfg ProviderConfig, stdout io.Writer) error {
	if err := writeSession(stdout, cfg.TaskID, cfg.SessionID); err != nil {
		return err
	}
	return writeAll(stdout, []byte(`{"type":"unknown","task_id":"`+cfg.TaskID+`"}`+"\n"))
}

func writeTruncatedScenario(cfg ProviderConfig, stdout io.Writer) error {
	if err := writeSession(stdout, cfg.TaskID, cfg.SessionID); err != nil {
		return err
	}
	if err := writeAnswer(stdout, cfg.TaskID, "truncated"); err != nil {
		return err
	}
	return writeResult(stdout, cfg.TaskID, cfg.SessionID, true, false)
}

func writeLateSessionExpected(cfg ProviderConfig, stdout io.Writer) error {
	answer := cfg.Answer
	if answer == "" {
		answer = "late session answer"
	}
	if err := writeAnswer(stdout, cfg.TaskID, answer); err != nil {
		return err
	}
	if err := writeSession(stdout, cfg.TaskID, cfg.SessionID); err != nil {
		return err
	}
	return writeResult(stdout, cfg.TaskID, cfg.SessionID, true, true)
}

func writeChildExpected(cfg ProviderConfig, stdout io.Writer) error {
	answer := cfg.Answer
	if answer == "" {
		answer = "child fixture answer"
	}
	if err := writeSession(stdout, cfg.TaskID, cfg.SessionID); err != nil {
		return err
	}
	if err := writeAnswer(stdout, cfg.TaskID, answer); err != nil {
		return err
	}
	return writeResult(stdout, cfg.TaskID, cfg.SessionID, true, true)
}

func writeLarge(cfg ProviderConfig, stdout, stderr io.Writer) error {
	stdoutBytes, stderrBytes := cfg.StdoutBytes, cfg.StderrBytes
	if stdoutBytes == 0 {
		stdoutBytes = 2 << 20
	}
	if stderrBytes == 0 {
		stderrBytes = 2 << 20
	}
	if err := writeSession(stdout, cfg.TaskID, cfg.SessionID); err != nil {
		return err
	}
	const chunkBytes = 32 * 1024
	chunk := strings.Repeat("S", chunkBytes)
	for remaining := stdoutBytes; remaining > 0; {
		n := int64(len(chunk))
		if n > remaining {
			n = remaining
		}
		if err := writeAnswer(stdout, cfg.TaskID, chunk[:n]); err != nil {
			return err
		}
		remaining -= n
	}
	if err := writeResult(stdout, cfg.TaskID, cfg.SessionID, true, true); err != nil {
		return err
	}
	return writeRepeated(stderr, 'E', stderrBytes)
}

func writeSession(w io.Writer, taskID, sessionID string) error {
	return writeJSONLine(w, struct {
		Type      string `json:"type"`
		TaskID    string `json:"task_id"`
		SessionID string `json:"session_id"`
	}{"session", taskID, sessionID}, true)
}

func writeAnswer(w io.Writer, taskID, answer string) error {
	if answer == "" {
		return writeAnswerFrame(w, taskID, answer)
	}
	// A JSON string can expand a control byte to a six-byte escape. Keeping the
	// source chunk below 8 KiB leaves ample room under the 64 KiB frame limit.
	const maxAnswerChunkBytes = 8 * 1024
	for len(answer) > 0 {
		n := len(answer)
		if n > maxAnswerChunkBytes {
			n = maxAnswerChunkBytes
			for n > 0 && !utf8.ValidString(answer[:n]) {
				n--
			}
		}
		if n == 0 {
			return errors.New("provider answer contains an invalid UTF-8 boundary")
		}
		if err := writeAnswerFrame(w, taskID, answer[:n]); err != nil {
			return err
		}
		answer = answer[n:]
	}
	return nil
}

func writeAnswerFrame(w io.Writer, taskID, answer string) error {
	return writeJSONLine(w, struct {
		Type   string `json:"type"`
		TaskID string `json:"task_id"`
		Text   string `json:"text"`
	}{"answer_chunk", taskID, answer}, true)
}

func writeResult(w io.Writer, taskID, sessionID string, success, newline bool) error {
	return writeJSONLine(w, struct {
		Type      string `json:"type"`
		TaskID    string `json:"task_id"`
		SessionID string `json:"session_id"`
		Success   bool   `json:"success"`
	}{"result", taskID, sessionID, success}, newline)
}

func writeJSONLine(w io.Writer, value any, newline bool) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if newline {
		data = append(data, '\n')
	}
	return writeAll(w, data)
}

func writeRepeated(w io.Writer, value byte, count int64) error {
	const blockSize = 32 * 1024
	block := strings.Repeat(string(value), blockSize)
	for count > 0 {
		n := int64(len(block))
		if n > count {
			n = count
		}
		if err := writeAll(w, []byte(block[:n])); err != nil {
			return err
		}
		count -= n
	}
	return nil
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if n < 0 || n > len(data) {
			return fmt.Errorf("provider writer returned invalid byte count %d", n)
		}
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func finiteHold(cfg ProviderConfig) error {
	if cfg.RendezvousPath == "" {
		timer := time.NewTimer(time.Duration(cfg.LifetimeMS) * time.Millisecond)
		defer timer.Stop()
		<-timer.C
		return nil
	}
	waiting := cfg.RendezvousPath + ".waiting"
	receipt := []byte(fmt.Sprintf("%d:waiting\n", os.Getpid()))
	if err := writeAtomic(waiting, receipt); err != nil {
		return err
	}
	deadline := time.NewTimer(time.Duration(cfg.LifetimeMS) * time.Millisecond)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(cfg.RendezvousPath + ".release"); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-deadline.C:
			return nil
		case <-ticker.C:
		}
	}
}

func startChild(cfg ProviderConfig, stdout, stderr io.Writer, entry providerReceipt) (int, error) {
	executable, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("resolving provider executable: %w", err)
	}
	childCfg := cfg
	childCfg.Scenario = "child"
	childCfg.LifetimeMS = cfg.ChildLifetimeMS
	if childCfg.LifetimeMS == 0 {
		childCfg.LifetimeMS = cfg.LifetimeMS
	}
	childCfg.Answer = "child fixture answer"
	childPath := filepath.Join(cfg.ArtifactDir, "child.config.json")
	if err = writeJSONArtifact(childPath, childCfg); err != nil {
		return 0, err
	}
	cmd := exec.Command(executable, "--child", childPath)
	cmd.Stdin = nil
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err = cmd.Start(); err != nil {
		return 0, fmt.Errorf("starting finite child: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return cmd.Process.Pid, fmt.Errorf("getting provider cwd: %w", err)
	}
	launch := entry
	launch.Entry = time.Now().UTC().Format(time.RFC3339Nano)
	launch.ProcessArgv = append([]string(nil), os.Args...)
	launch.CWD = cwd
	launch.ChildPID = cmd.Process.Pid
	data, err := task.MarshalCanonical(launch)
	if err != nil {
		return cmd.Process.Pid, err
	}
	data = append(data, '\n')
	if err = writeExclusiveArtifact(filepath.Join(entry.InvocationDir, "child.launch.json"), data); err != nil {
		return cmd.Process.Pid, err
	}
	return cmd.Process.Pid, writeReceiptIfAbsentBytes(filepath.Join(cfg.ArtifactDir, "child.launch.json"), data)
}

func artifactName(child bool, name string) string {
	if child {
		return "child." + name
	}
	return name
}

func writeReceiptExclusive(path string, receipt providerReceipt) error {
	data, err := task.MarshalCanonical(receipt)
	if err != nil {
		return err
	}
	return writeExclusiveArtifact(path, data)
}

func writeReceiptIfAbsent(path string, receipt providerReceipt) error {
	data, err := task.MarshalCanonical(receipt)
	if err != nil {
		return err
	}
	return writeReceiptIfAbsentBytes(path, data)
}

func writeReceiptIfAbsentBytes(path string, data []byte) error {
	err := writeExclusiveArtifact(path, data)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	return err
}

func writeJSONArtifact(path string, value any) error {
	data, err := task.MarshalCanonical(value)
	if err != nil {
		return err
	}
	return writeArtifact(path, data)
}

func writeArtifact(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	writeErr := writeAll(f, data)
	return errors.Join(writeErr, f.Sync(), f.Close())
}

func writeExclusiveArtifact(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	writeErr := writeAll(f, data)
	return errors.Join(writeErr, f.Sync(), f.Close())
}

func writeAtomic(path string, data []byte) error {
	tmp := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if err := writeArtifact(tmp, data); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	return nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
