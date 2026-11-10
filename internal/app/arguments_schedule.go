package app

import (
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
)

const defaultSchedulePageSize = 50

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
	if err := validateContentSources(a.Brief, a.BriefBytes, false); err != nil {
		return err
	}
	if a.ScheduleHook == nil {
		return validateTeamTargetInputs(*a)
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
