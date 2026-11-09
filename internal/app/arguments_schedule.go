package app

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
)

const defaultSchedulePageSize = 50

func parseScheduleArguments(a Arguments, rest []string) (Arguments, error) {
	var err error
	a.ScheduleAction, rest, err = scheduleAction(rest)
	if err != nil {
		return Arguments{}, err
	}
	if a.ScheduleAction == "policy" {
		a.SchedulePolicyOperation, rest, err = scheduleAction(rest)
		if err != nil || (a.SchedulePolicyOperation != "get" && a.SchedulePolicyOperation != "set") {
			return Arguments{}, errors.New("schedule policy requires get or set")
		}
	}

	fs := flag.NewFlagSet("schedule", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	registerScheduleFlags(fs, &a)
	ordered, err := orderFlags(fs, rest)
	if err != nil {
		return Arguments{}, err
	}
	if err = fs.Parse(ordered); err != nil {
		return Arguments{}, err
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "timezone" {
			a.ScheduleTimezoneSet = true
		}
	})
	if a.Root != "" && !filepath.IsAbs(a.Root) {
		return Arguments{}, errors.New("root must be an absolute path")
	}
	if err = validateSchedulePositionals(&a, fs.Args()); err != nil {
		return Arguments{}, err
	}
	if err = validateScheduleCreateOptions(&a); err != nil {
		return Arguments{}, err
	}

	return a, nil
}

func scheduleAction(args []string) (string, []string, error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--json" || arg == "-json" || strings.HasPrefix(arg, "--json=") || strings.HasPrefix(arg, "-json=") {
			continue
		}
		if arg == "--root" || arg == "-root" {
			i++
			if i == len(args) {
				return "", nil, errors.New("missing value for --root")
			}
			continue
		}
		if strings.HasPrefix(arg, "--root=") || strings.HasPrefix(arg, "-root=") {
			continue
		}
		remaining := append(append([]string(nil), args[:i]...), args[i+1:]...)
		return arg, remaining, nil
	}
	return "", nil, errors.New("schedule requires create, list, show, history, execution, pause, resume, or delete")
}

func registerScheduleFlags(fs *flag.FlagSet, a *Arguments) {
	fs.StringVar(&a.Root, "root", "", "absolute state root")
	fs.BoolVar(&a.JSON, "json", false, "emit versioned JSON")
	switch a.ScheduleAction {
	case "create":
		a.ScheduleHook = &contract.FailureHook{Threshold: 1, Timeout: 10 * time.Second}
		fs.StringVar(&a.ScheduleHook.Executable, "failure-hook", "", "absolute failure-hook executable")
		fs.Int64Var(&a.ScheduleHook.Threshold, "failure-threshold", 1, "consecutive failures before notification")
		fs.DurationVar(&a.ScheduleHook.Timeout, "failure-hook-timeout", 10*time.Second, "bounded hook timeout")
		registerTeamTargetFlags(fs, a)
		fs.StringVar(&a.ScheduleID, "id", "", "schedule ID")
		fs.StringVar(&a.Brief, "brief", "", "brief file")
		fs.StringVar(&a.Cwd, "cwd", "", "working directory")
		fs.StringVar(&a.Provider, "provider", "", "requested provider")
		fs.BoolVar(&a.Auto, "auto", false, "request automatic provider selection")
		fs.StringVar(&a.ScheduleCron, "cron", "", "five-field cron rule")
		fs.StringVar(&a.ScheduleTimezone, "timezone", "", "named timezone")
		fs.StringVar(&a.Config.Model, "model", "", "requested model")
		fs.StringVar(&a.Config.Effort, "effort", "", "requested effort")
		fs.StringVar(&a.Config.Permission, "permission", "", "requested permission")
		fs.StringVar(&a.Config.Budget, "budget", "", "requested budget")
		fs.StringVar(&a.Config.OutputSilenceTimeout, "output-silence-timeout", "", "opt-in provider output silence timeout; 0 disables")
		fs.StringVar(&a.Config.NativeTimeout, "native-timeout", "", "requested native timeout")
		fs.StringVar(&a.ReviewTarget, "review-target", "", "requested review target")
	case "policy":
		if a.SchedulePolicyOperation == "set" {
			fs.StringVar(&a.ScheduleQuotaHold, "quota-hold", "", "on or off")
			fs.StringVar(&a.ScheduleQuotaMaxHold, "quota-max-hold", "", "maximum quota hold duration")
		}
	case "list":
		fs.StringVar(&a.ScheduleAfter, "after", "", "schedule cursor")
		fs.IntVar(&a.ScheduleLimit, "limit", defaultSchedulePageSize, "schedules per page")
	case "history":
		fs.StringVar(&a.ScheduleBefore, "before", "", "exclusive RFC3339Nano history cursor")
		fs.IntVar(&a.ScheduleLimit, "limit", defaultSchedulePageSize, "history entries per page")
	}
}

func validateSchedulePositionals(a *Arguments, positional []string) error {
	switch a.ScheduleAction {
	case "create", "list":
		return validateNoPositional("schedule "+a.ScheduleAction, positional)
	case "show", "health", "policy", "history", "pause", "resume", "delete":
		if len(positional) != 1 {
			return fmt.Errorf("schedule %s requires exactly one schedule ID", a.ScheduleAction)
		}
		a.ScheduleID = positional[0]
		return nil
	case "execution":
		if len(positional) != 1 {
			return errors.New("schedule execution requires exactly one task ID")
		}
		a.TaskID = positional[0]
		return nil
	default:
		return fmt.Errorf("unknown schedule action %q", a.ScheduleAction)
	}
}

func validateScheduleCreateOptions(a *Arguments) error {
	if a.ScheduleAction != "create" {
		return nil
	}
	if a.ScheduleHook.Executable == "" {
		if a.ScheduleHook.Threshold != 1 || a.ScheduleHook.Timeout != 10*time.Second {
			return errors.New("failure threshold/timeout require --failure-hook")
		}
		a.ScheduleHook = nil
	} else if err := contract.ValidateFailureHook(a.ScheduleHook); err != nil {
		return err
	}
	return validateTeamTargetInputs(*a)
}
