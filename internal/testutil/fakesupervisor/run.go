package fakesupervisor

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Main runs the finite fake client with the process's literal arguments.
func Main() int {
	executable, err := os.Executable()
	if err != nil {
		return 64
	}
	return RunWithExecutable(executable, "", os.Args[1:])
}

// Run loads the fixed sibling configuration for the current executable.
// It is useful to a small command wrapper and does not consult the environment.
func Run(configPath string, argv []string) int {
	executable, err := os.Executable()
	if err != nil {
		return 64
	}
	return RunWithExecutable(executable, configPath, argv)
}

// RunWithExecutable is the testable entry point for a copied canonical fake
// executable. configPath may be empty, but a nonempty value must equal the
// executable's fixed fake-supervisor.json sibling. The command's -c value
// is checked against Config.ExpectedConfigPath.
func RunWithExecutable(executable, configPath string, argv []string) int {
	canonical, err := canonicalExecutable(executable)
	if err != nil {
		return 64
	}
	fixedConfig := filepath.Join(filepath.Dir(canonical), ConfigName)
	if configPath != "" && configPath != fixedConfig {
		return 64
	}
	cfg, err := LoadConfig(fixedConfig)
	if err != nil {
		return 64
	}
	if err = ensureReceiptDir(cfg.ArtifactDir); err != nil {
		return 64
	}
	return runConfigured(canonical, fixedConfig, cfg, argv)
}

func runConfigured(executable, fixedConfig string, cfg Config, argv []string) int {
	invocationID, err := newInvocationID()
	if err != nil {
		return 64
	}
	verb := invocationVerb(argv)
	entry := EntryReceipt{
		SchemaVersion:     SchemaVersion,
		InvocationID:      invocationID,
		PID:               os.Getpid(),
		Executable:        executable,
		FixtureConfigPath: fixedConfig,
		ConfigPath:        invocationConfigPath(argv, fixedConfig),
		Argv:              append([]string(nil), argv...),
		Verb:              verb,
		StartedAt:         receiptTime(),
	}
	if err = writeEntryReceipt(cfg.ArtifactDir, entry); err != nil {
		return 1
	}

	stdout, stderr, exitCode, commandErr := executeCommand(cfg, argv)
	if outputErr := writeProcessOutput(stdout, stderr); outputErr != nil {
		commandErr = errors.Join(commandErr, outputErr)
		exitCode = 1
	}
	stdoutBytes, stdoutSHA := outputReceipt(stdout)
	stderrBytes, stderrSHA := outputReceipt(stderr)
	completion := CompletionReceipt{
		SchemaVersion:     SchemaVersion,
		InvocationID:      invocationID,
		PID:               os.Getpid(),
		Executable:        executable,
		FixtureConfigPath: fixedConfig,
		ConfigPath:        entry.ConfigPath,
		Argv:              append([]string(nil), argv...),
		Verb:              verb,
		CompletedAt:       receiptTime(),
		ExitCode:          exitCode,
		StdoutBytes:       stdoutBytes,
		StdoutSHA256:      stdoutSHA,
		StderrBytes:       stderrBytes,
		StderrSHA256:      stderrSHA,
	}
	if commandErr != nil {
		completion.Error = commandErr.Error()
	}
	if err = writeCompletionReceipt(cfg.ArtifactDir, completion); err != nil {
		return 1
	}
	return exitCode
}

func executeCommand(cfg Config, argv []string) ([]byte, []byte, int, error) {
	command, err := parseCommand(cfg.ExpectedConfigPath, argv)
	if err != nil {
		return nil, outputError(err), 64, err
	}
	command.config = configForVerb(cfg, command.verb)
	if err = waitForInput(command.config.ReleasePath, command.config.Delay, MaxRunDuration); err != nil {
		return nil, outputError(err), 1, err
	}
	var stdout []byte
	if command.config.StdoutPath != "" {
		stdout, err = readConfiguredOutput(command.config.StdoutPath)
	} else {
		stdout, err = defaultOutput(cfg, command.verb)
	}
	if err != nil {
		return nil, outputError(err), 1, err
	}
	stderr, err := readConfiguredOutput(command.config.StderrPath)
	if err != nil {
		return stdout, outputError(err), 1, err
	}
	return stdout, stderr, command.config.ExitCode, nil
}

func configForVerb(cfg Config, verb string) VerbConfig {
	switch verb {
	case "status":
		return cfg.Status
	case "add":
		return cfg.Add
	case "kill":
		return cfg.Kill
	case "remove":
		return cfg.Remove
	default:
		return VerbConfig{}
	}
}

func defaultOutput(cfg Config, verb string) ([]byte, error) {
	switch verb {
	case "--version":
		return []byte("pueue " + cfg.Version + "\n"), nil
	case "status":
		return readConfiguredOutput(cfg.StatusPath)
	case "add":
		return []byte(strconv.FormatInt(cfg.AddID, 10) + "\n"), nil
	default:
		return nil, nil
	}
}

type parsedCommand struct {
	verb   string
	config VerbConfig
}

func parseCommand(fixedConfig string, argv []string) (parsedCommand, error) {
	if len(argv) < 3 || argv[0] != "-c" || argv[1] != fixedConfig {
		return parsedCommand{}, ErrInvalidArguments
	}
	switch argv[2] {
	case "--version":
		if len(argv) != 3 {
			return parsedCommand{}, ErrInvalidArguments
		}
		return parsedCommand{verb: "--version"}, nil
	case "status":
		if len(argv) != 4 || argv[3] != "--json" {
			return parsedCommand{}, ErrInvalidArguments
		}
		return parsedCommand{verb: "status"}, nil
	case "add":
		return parseAdd(argv)
	case "kill", "remove":
		if len(argv) != 4 || !validNumericID(argv[3]) {
			return parsedCommand{}, ErrInvalidArguments
		}
		return parsedCommand{verb: argv[2]}, nil
	default:
		return parsedCommand{}, ErrInvalidArguments
	}
}

func parseAdd(argv []string) (parsedCommand, error) {
	if len(argv) != 12 || argv[3] != "--escape" || argv[4] != "--label" || argv[5] == "" || argv[6] != "--print-task-id" || argv[7] != "--" || argv[8] == "" || argv[9] != "--root" || argv[10] == "" {
		return parsedCommand{}, ErrInvalidArguments
	}
	if !cleanAbsoluteArgument(argv[8]) || !cleanAbsoluteArgument(argv[10]) {
		return parsedCommand{}, ErrInvalidArguments
	}
	if err := task.ValidateTaskID(argv[11]); err != nil {
		return parsedCommand{}, ErrInvalidArguments
	}
	return parsedCommand{verb: "add"}, nil
}

func cleanAbsoluteArgument(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path
}

func validNumericID(value string) bool {
	id, err := strconv.ParseInt(value, 10, 64)
	return err == nil && id >= 0 && strconv.FormatInt(id, 10) == value
}

func invocationVerb(argv []string) string {
	if len(argv) >= 3 {
		return argv[2]
	}
	return "invalid"
}

func invocationConfigPath(argv []string, fixedConfig string) string {
	if len(argv) >= 2 && argv[0] == "-c" {
		return argv[1]
	}
	return fixedConfig
}

func writeProcessOutput(stdout, stderr []byte) error {
	var result error
	if len(stdout) > 0 {
		if err := writeFull(os.Stdout, stdout); err != nil {
			result = errors.Join(result, err)
		}
	}
	if len(stderr) > 0 {
		if err := writeFull(os.Stderr, stderr); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}
