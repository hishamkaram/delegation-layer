package app

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func registerRemoteFlags(fs *flag.FlagSet, a *Arguments) {
	if a.Command == "dispatch" {
		fs.StringVar(&a.Device, "device", "", "configured SSH destination")
		fs.StringVar(&a.SSHConfig, "ssh-config", "", "explicit SSH configuration file")
		fs.StringVar(&a.RemoteRoot, "remote-root", "", "explicit remote state root")
		fs.StringVar(&a.RemoteCwd, "remote-cwd", "", "prepared remote workspace")
		fs.StringVar(&a.InputManifest, "input-manifest", "", "selected snapshot manifest")
		fs.StringVar(&a.OutputManifest, "output-manifest", "", "selected output paths")
		fs.StringVar(&a.RemoteExecutable, "remote-executable", "", "installed remote Delegate path")
	}
	if a.Command == "logs" {
		fs.StringVar(&a.LogStream, "stream", "", "explicit remote stdout or stderr range")
		fs.StringVar(&a.LogCursor, "cursor", "", "remote log observation cursor")
		fs.Int64Var(&a.LogOffset, "offset", 0, "remote log byte offset")
		fs.Int64Var(&a.LogLimit, "limit", 64<<10, "remote log range limit")
	}
	if a.Command == "collect" {
		fs.StringVar(&a.OutputDirectory, "out", "", "explicit remote download directory")
	}
}

func validateRoutedTaskID(id string) error {
	return task.ValidateTaskID(strings.TrimPrefix(id, "remote:"))
}

func parseRemoteArguments(a Arguments, rest []string) (Arguments, error) {
	action, remaining, err := parseSupervisorAction(rest)
	if err != nil {
		return a, err
	}
	a.RemoteAction = action
	fs := flag.NewFlagSet(a.Command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&a.Root, "root", "", "origin state root")
	fs.StringVar(&a.RemoteAfter, "after", "", "remote reference cursor")
	fs.StringVar(&a.Device, "device", "", "SSH alias")
	fs.StringVar(&a.SSHConfig, "ssh-config", "", "explicit SSH configuration file")
	fs.StringVar(&a.Provider, "provider", "", "remote provider")
	fs.StringVar(&a.RemoteRoot, "remote-root", "", "remote state root")
	fs.StringVar(&a.RemoteCwd, "remote-cwd", "", "remote workspace")
	fs.StringVar(&a.RemoteExecutable, "remote-executable", "", "installed Delegate")
	fs.StringVar(&a.Fingerprint, "fingerprint", "", "expected host key fingerprint")
	fs.BoolVar(&a.Stdio, "stdio", false, "framed control protocol")
	fs.BoolVar(&a.JSON, "json", false, "machine readable output")
	fs.StringVar(&a.RemoteScope, "scope", "both", "origin, destination or both")
	fs.BoolVar(&a.RemoteApply, "apply", false, "retire selected owned bytes")
	fs.BoolVar(&a.RemoteDryRun, "dry-run", false, "preview retirement")
	ordered, err := orderFlags(fs, remaining)
	if err != nil {
		return a, err
	}
	if err = fs.Parse(ordered); err != nil {
		return a, err
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "scope" {
			a.remoteScopeSet = true
		}
	})
	return validateRemoteAction(a, fs.Args())
}

func validateRemoteAction(a Arguments, positional []string) (Arguments, error) {
	if err := validateRemoteStorageOptions(a); err != nil {
		return a, err
	}
	switch a.Command {
	case "remote":
		return validateRemoteSubcommand(a, positional)
	case "devices":
		return validateDeviceAction(a, positional)
	}
	return a, nil
}

func validateRemoteStorageOptions(a Arguments) error {
	if a.RemoteAction != "retire" && a.RemoteAction != "usage" && (a.RemoteApply || a.RemoteDryRun || a.remoteScopeSet) {
		return errors.New("storage options require remote usage or retire")
	}
	if a.RemoteApply && a.RemoteDryRun {
		return errors.New("apply conflicts with dry-run")
	}
	if a.RemoteScope != "both" && a.RemoteScope != "origin" && a.RemoteScope != "destination" {
		return errors.New("scope must be origin, destination or both")
	}
	if a.RemoteAction == "usage" && (a.RemoteApply || a.RemoteDryRun) {
		return errors.New("usage is read-only")
	}
	return nil
}

func validateRemoteSubcommand(a Arguments, positional []string) (Arguments, error) {
	switch a.RemoteAction {
	case "serve":
		if !a.Stdio || len(positional) != 0 {
			return a, errors.New("remote serve requires --stdio")
		}
	case "reconcile", "usage", "retire":
		if len(positional) != 1 || !strings.HasPrefix(positional[0], "remote:") {
			return a, errors.New("reconcile requires remote:ID")
		}
		a.TaskID = positional[0]
		return a, validateRoutedTaskID(a.TaskID)
	case "list":
		if len(positional) != 0 {
			return a, errors.New("remote list takes no positional arguments")
		}
	default:
		return a, fmt.Errorf("unsupported remote action %q", a.RemoteAction)
	}
	return a, nil
}

func (a *Arguments) validateRemoteDispatch(positional []string) error {
	if len(positional) != 0 || remoteDispatchLocalOptions(*a) {
		return errors.New("remote dispatch conflicts with local workspace, auto, Team, continuation or supervisor options")
	}
	if a.Brief == "" || (a.RemoteCwd == "") == (a.InputManifest == "") {
		return errors.New("remote dispatch requires --brief and exactly one of --remote-cwd or --input-manifest")
	}
	if a.RemoteCwd != "" && !filepath.IsAbs(a.RemoteCwd) {
		return errors.New("remote cwd must be absolute on destination")
	}
	if a.RemoteRoot != "" && !filepath.IsAbs(a.RemoteRoot) {
		return errors.New("remote root must be absolute on destination")
	}
	if err := config.ValidateProvider(a.Provider); err != nil {
		return err
	}
	if err := config.ValidateMode(a.Config.Permission); err != nil {
		return err
	}
	if _, err := config.ParseBudget(a.Config.Budget); err != nil {
		return err
	}
	if err := a.applyReviewTarget(); err != nil {
		return err
	}
	return a.validateDispatchIDs()
}

func isRemoteCommand(a Arguments) bool {
	return a.Command == "devices" || a.Command == "remote" || a.Device != "" || strings.HasPrefix(a.TaskID, "remote:")
}

func remoteDispatchLocalOptions(a Arguments) bool {
	return a.Cwd != "" || a.WorkspaceID != "" || a.Auto || requestedTeamTarget(a) != nil || a.ResumeTask != "" || a.Isolation != "" || a.PueueConfig != "" || a.Runner != ""
}

func validateDeviceAction(a Arguments, positional []string) (Arguments, error) {
	if len(positional) != 0 {
		return a, errors.New("unexpected device arguments")
	}
	switch a.RemoteAction {
	case "list":
	case "probe", "trust":
		if a.Device == "" {
			return a, errors.New("device required")
		}
	default:
		return a, fmt.Errorf("unknown devices action %q", a.RemoteAction)
	}
	return a, nil
}

func (a *Arguments) validateLogArguments() error {
	if a.Command != "logs" {
		return nil
	}
	if !a.logOptionsSet {
		return nil
	}
	if !strings.HasPrefix(a.TaskID, "remote:") {
		return errors.New("log range options require remote:ID")
	}
	if a.LogStream != "stdout" && a.LogStream != "stderr" {
		return errors.New("log ranges require --stream stdout or stderr")
	}
	if a.LogOffset < 0 || a.LogLimit < 1 || a.LogLimit > 64<<10 {
		return errors.New("log range requires nonnegative offset and limit 1..65536")
	}
	return nil
}
