// Package app composes public commands with durable task and supervisor APIs.
package app

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Arguments contains requested values only. Parsing grants no external authority.
type Arguments struct {
	Command     string
	Root        string
	PueueConfig string
	Runner      string
	TaskID      string
	Provider    string
	Brief       string
	Cwd         string
	Config      task.TaskConfig
	ResumeTask  string
	JSON        bool
	Watch       time.Duration
}

func ParseArguments(args []string) (Arguments, error) {
	command, rest, err := separateCommand(args)
	if err != nil {
		return Arguments{}, err
	}
	a := Arguments{Command: command}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&a.Root, "root", "", "absolute state root")
	fs.StringVar(&a.PueueConfig, "pueue-config", "", "absolute initial supervisor configuration")
	fs.StringVar(&a.Runner, "runner", "", "absolute runner executable")
	watch := "0s"
	registerCommandFlags(fs, &a, &watch)
	ordered, err := orderFlags(fs, rest)
	if err != nil {
		return Arguments{}, err
	}
	if err := fs.Parse(ordered); err != nil {
		return Arguments{}, err
	}
	if err := a.validate(fs.Args(), watch); err != nil {
		return Arguments{}, err
	}
	return a, nil
}

func separateCommand(args []string) (string, []string, error) {
	for i := 0; i < len(args); {
		name, _, assigned := strings.Cut(strings.TrimLeft(args[i], "-"), "=")
		if strings.HasPrefix(args[i], "-") && isGlobal(name) {
			if !assigned {
				i++
				if i == len(args) {
					return "", nil, fmt.Errorf("missing value for --%s", name)
				}
			}
			i++
			continue
		}
		command := args[i]
		switch command {
		case "-h", "--help":
			command = "help"
		case "--version":
			command = "version"
		case "help", "version", "dispatch", "status", "collect", "cancel", "logs":
		default:
			return "", nil, fmt.Errorf("unknown command or flag %q", args[i])
		}
		rest := append([]string(nil), args[:i]...)
		return command, append(rest, args[i+1:]...), nil
	}
	return "help", append([]string(nil), args...), nil
}

func isGlobal(name string) bool {
	return name == "root" || name == "pueue-config" || name == "runner"
}

func registerCommandFlags(fs *flag.FlagSet, a *Arguments, watch *string) {
	if a.Command == "help" || a.Command == "version" {
		return
	}
	fs.BoolVar(&a.JSON, "json", false, "emit versioned JSON")
	if a.Command == "collect" {
		fs.StringVar(watch, "watch", "0s", "finite observation bound")
	}
	if a.Command != "dispatch" {
		return
	}
	fs.StringVar(&a.TaskID, "id", "", "task ID")
	fs.StringVar(&a.Provider, "provider", "", "provider profile")
	fs.StringVar(&a.Brief, "brief", "", "brief file")
	fs.StringVar(&a.Cwd, "cwd", "", "absolute working directory")
	fs.StringVar(&a.Config.Permission, "permission", config.DefaultMode, "permission intent")
	fs.StringVar(&a.Config.Budget, "budget", "", "finite execution budget")
	fs.StringVar(&a.Config.Model, "model", "", "requested model")
	fs.StringVar(&a.Config.Effort, "effort", "", "requested effort")
	fs.StringVar(&a.ResumeTask, "resume-task", "", "exact predecessor task ID")
}

// orderFlags permits documented ID --json placement without silently accepting
// unknown options as positional values. A flag's following value remains literal.
func orderFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var options, positional []string
	seen := make(map[string]bool)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		name, _, assigned := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		f := fs.Lookup(name)
		if f == nil {
			return nil, fmt.Errorf("unknown flag %q", arg)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate flag --%s", name)
		}
		seen[name] = true
		options = append(options, arg)
		boolean, ok := f.Value.(interface{ IsBoolFlag() bool })
		if assigned || (ok && boolean.IsBoolFlag()) {
			continue
		}
		i++
		if i == len(args) {
			return nil, fmt.Errorf("missing value for --%s", name)
		}
		options = append(options, args[i])
	}
	options = append(options, "--")
	return append(options, positional...), nil
}

func (a *Arguments) validate(positional []string, watch string) error {
	for _, p := range []string{a.Root, a.PueueConfig, a.Runner} {
		if p != "" && !filepath.IsAbs(p) {
			return errors.New("root, pueue-config and runner must be absolute paths")
		}
	}
	switch a.Command {
	case "help", "version":
		if len(positional) != 0 {
			return fmt.Errorf("unexpected extra argument %q for %s", positional[0], a.Command)
		}
	case "dispatch":
		return a.validateDispatch(positional)
	case "status", "collect", "cancel", "logs":
		if len(positional) != 1 {
			return errors.New("exactly one task ID is required")
		}
		a.TaskID = positional[0]
		if err := task.ValidateTaskID(a.TaskID); err != nil {
			return err
		}
		parsed, err := time.ParseDuration(watch)
		if err != nil || parsed < 0 {
			return errors.New("watch must be a finite nonnegative duration")
		}
		a.Watch = parsed
	}
	return nil
}

func (a *Arguments) validateDispatch(positional []string) error {
	if len(positional) != 0 {
		return errors.New("dispatch does not accept positional arguments or raw argv")
	}
	if err := config.ValidateProvider(a.Provider); err != nil {
		return err
	}
	if a.Brief == "" || !filepath.IsAbs(a.Cwd) {
		return errors.New("dispatch requires --brief FILE and --cwd ABS")
	}
	if err := config.ValidateMode(a.Config.Permission); err != nil {
		return err
	}
	if _, err := config.ParseBudget(a.Config.Budget); err != nil {
		return err
	}
	for _, id := range []string{a.TaskID, a.ResumeTask} {
		if id != "" {
			if err := task.ValidateTaskID(id); err != nil {
				return err
			}
		}
	}
	if a.TaskID != "" && a.TaskID == a.ResumeTask {
		return errors.New("continuation requires a different task ID")
	}
	return nil
}
