package pueue

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type RunnerCommandReplacement struct {
	NumericID              int64
	RootID                 string
	TaskID                 string
	Label                  string
	RootPath               string
	OldRunner              string
	PreviousManagedRunners []string
	NewRunner              string
	Inspection             bool
}

const privateManagedRunnerPrefix = "delegate-run-"

// ReplaceQueuedRunnerCommands updates matching queued private tasks. Pueue's
// editor locks each task while the exact command comparison and replacement
// run, and running tasks are never stopped or rewritten.
func (c *Client) ReplaceQueuedRunnerCommands(ctx context.Context, replacements []RunnerCommandReplacement) (resultErr error) {
	if len(replacements) == 0 {
		return nil
	}
	if c == nil {
		return ErrConfiguration
	}
	if ctx == nil {
		return context.Canceled
	}
	ctx, cancel := context.WithTimeout(ctx, RunnerCommandUpgradeTimeout)
	defer cancel()
	byID, err := indexRunnerCommandReplacements(c, replacements)
	if err != nil {
		return err
	}
	lock, err := acquireBootstrapLock(ctx, filepath.Join(replacements[0].RootPath, ".supervisor", "bootstrap.lock"))
	if err != nil {
		return err
	}
	defer func() { finishPrivateBootstrap(&lock, &resultErr) }()
	snapshot, err := c.Snapshot(ctx)
	if err != nil {
		return err
	}
	plans, err := planQueuedRunnerCommands(snapshot, byID)
	if err != nil {
		return err
	}
	if len(plans) == 0 {
		return nil
	}
	if err = applyQueuedRunnerCommandPlans(ctx, c, snapshot, plans); err != nil {
		return err
	}
	snapshot, err = c.Snapshot(ctx)
	if err != nil {
		return err
	}
	return verifyQueuedRunnerCommandPlans(snapshot, plans)
}

func indexRunnerCommandReplacements(c *Client, replacements []RunnerCommandReplacement) (map[int64]RunnerCommandReplacement, error) {
	byID := make(map[int64]RunnerCommandReplacement, len(replacements))
	for _, replacement := range replacements {
		if err := validateRunnerCommandReplacement(c, replacement); err != nil {
			return nil, err
		}
		if _, duplicate := byID[replacement.NumericID]; duplicate {
			return nil, ErrConfiguration
		}
		byID[replacement.NumericID] = replacement
	}
	return byID, nil
}

func validateRunnerCommandReplacement(c *Client, replacement RunnerCommandReplacement) error {
	if replacement.NumericID < 0 || task.ValidateRootID(replacement.RootID) != nil || task.ValidateTaskID(replacement.TaskID) != nil {
		return ErrConfiguration
	}
	expectedLabel := "delegate:" + replacement.RootID + ":" + replacement.TaskID
	if replacement.Inspection {
		expectedLabel = (InspectionIdentity{RootID: replacement.RootID, TaskID: replacement.TaskID}).Label()
	}
	if !IsPrivateConfig(replacement.RootPath, c.binding.ConfigPath) || replacement.Label != expectedLabel {
		return ErrBinding
	}
	if !filepath.IsAbs(replacement.OldRunner) || filepath.Clean(replacement.OldRunner) != replacement.OldRunner || strings.IndexByte(replacement.OldRunner, 0) >= 0 {
		return ErrConfiguration
	}
	for _, runner := range replacement.PreviousManagedRunners {
		if !isPrivateManagedRunnerPath(replacement.RootPath, runner) {
			return ErrConfiguration
		}
	}
	return validateLaunch(Launch{RunnerExecutable: replacement.NewRunner, RootPath: replacement.RootPath}, replacement.RootID)
}

func isPrivateManagedRunnerPath(root, runner string) bool {
	if !filepath.IsAbs(runner) || filepath.Clean(runner) != runner || filepath.Dir(runner) != filepath.Join(root, ".supervisor") {
		return false
	}
	digest := strings.TrimPrefix(filepath.Base(runner), privateManagedRunnerPrefix)
	return digest != filepath.Base(runner) && task.ValidateSHA256(digest) == nil
}

func runnerCommandGroupMatches(job Job, replacement RunnerCommandReplacement) bool {
	expectedGroup := "default"
	if replacement.Inspection {
		expectedGroup = (InspectionIdentity{RootID: replacement.RootID}).Group()
	}
	return job.Group == expectedGroup
}

func planQueuedRunnerCommands(snapshot QueueSnapshot, replacements map[int64]RunnerCommandReplacement) (map[int64]runnerCommandPlan, error) {
	plans := make(map[int64]runnerCommandPlan, len(replacements))
	for numericID, replacement := range replacements {
		job, found := queueJob(snapshot, numericID)
		if !found || job.State != StateQueued {
			continue
		}
		if job.Label == nil || *job.Label != replacement.Label || !runnerCommandGroupMatches(job, replacement) {
			return nil, ErrBinding
		}
		plan := runnerCommandPlan{
			RunnerCommandReplacement: replacement,
			group:                    job.Group,
			expected:                 runnerCommand(replacement.OldRunner, replacement),
			replacement:              runnerCommand(replacement.NewRunner, replacement),
		}
		if job.originalCommand == plan.replacement {
			continue
		}
		if job.originalCommand != plan.expected {
			for _, runner := range replacement.PreviousManagedRunners {
				candidate := runnerCommand(runner, replacement)
				if job.originalCommand == candidate {
					plan.expected = candidate
					break
				}
			}
		}
		if job.originalCommand != plan.expected {
			return nil, fmt.Errorf("%w: queued task command differs from its saved runner identity", ErrBinding)
		}
		plans[numericID] = plan
	}
	return plans, nil
}

func runnerCommand(runner string, replacement RunnerCommandReplacement) string {
	if replacement.Inspection {
		return escapedCommand(runner, "--inspection", "--root", replacement.RootPath, replacement.TaskID)
	}
	return escapedCommand(runner, "--root", replacement.RootPath, replacement.TaskID)
}

func applyQueuedRunnerCommandPlans(ctx context.Context, c *Client, snapshot QueueSnapshot, plans map[int64]runnerCommandPlan) error {
	ids := sortedRunnerCommandIDs(plans)
	for _, numericID := range ids {
		plan := plans[numericID]
		if err := applyQueuedRunnerCommandPlan(ctx, c, snapshot, numericID, plan); err != nil {
			var inFlight *InFlightError
			if errors.As(err, &inFlight) {
				return err
			}
			current, snapshotErr := c.Snapshot(ctx)
			if snapshotErr != nil {
				return errors.Join(err, snapshotErr)
			}
			complete, reconcileErr := runnerCommandPlanComplete(current, plan)
			if reconcileErr != nil {
				return errors.Join(err, reconcileErr)
			}
			if !complete {
				return err
			}
		}
	}
	return nil
}

func runnerCommandPlanComplete(snapshot QueueSnapshot, plan runnerCommandPlan) (bool, error) {
	job, found := queueJob(snapshot, plan.NumericID)
	if !found || job.State != StateQueued {
		return true, nil
	}
	if job.Label == nil || *job.Label != plan.Label || job.Group != plan.group {
		return false, ErrBinding
	}
	return job.originalCommand == plan.replacement, nil
}

func sortedRunnerCommandIDs(plans map[int64]runnerCommandPlan) []int64 {
	ids := make([]int64, 0, len(plans))
	for numericID := range plans {
		ids = append(ids, numericID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func applyQueuedRunnerCommandPlan(ctx context.Context, c *Client, snapshot QueueSnapshot, numericID int64, plan runnerCommandPlan) error {
	job, found := queueJob(snapshot, numericID)
	if !found || job.State != StateQueued {
		return nil
	}
	if job.Label == nil || *job.Label != plan.Label || job.Group != plan.group {
		return ErrBinding
	}
	if job.originalCommand == plan.replacement {
		return nil
	}
	if job.originalCommand != plan.expected {
		return fmt.Errorf("%w: queued task command changed during runner migration", ErrBinding)
	}
	return c.replaceQueuedRunnerCommand(ctx, numericID, plan.expected, plan.replacement)
}

func verifyQueuedRunnerCommandPlans(snapshot QueueSnapshot, plans map[int64]runnerCommandPlan) error {
	for numericID, plan := range plans {
		job, found := queueJob(snapshot, numericID)
		if found && job.State == StateQueued {
			if job.Label == nil || *job.Label != plan.Label || job.Group != plan.group || job.originalCommand != plan.replacement {
				return fmt.Errorf("%w: queued runner command replacement was not observed", ErrBinding)
			}
		}
	}
	return nil
}

type runnerCommandPlan struct {
	RunnerCommandReplacement
	group       string
	expected    string
	replacement string
}

func (c *Client) replaceQueuedRunnerCommand(ctx context.Context, numericID int64, expected, replacement string) error {
	if err := c.verify(ctx); err != nil {
		return err
	}
	configPath, err := privateEditConfig(filepath.Dir(c.runtimeBinding.ConfigPath), c.runtimeBinding.ConfigPath, c.runtimeBinding.ConfigDigest)
	if err != nil {
		return err
	}
	editClient := *c
	editClient.runtimeBinding.ConfigPath = configPath
	editClient.options = c.options
	editClient.options.Environment = runnerEditEnvironment(
		c.options.Environment, strconv.FormatInt(numericID, 10), expected, replacement,
		"/bin/sh -c "+quoteShell(editorScript),
	)
	if _, err = editClient.command(ctx, nil, "edit", strconv.FormatInt(numericID, 10)); err != nil {
		return fmt.Errorf("%w: edit queued runner command: %w", ErrBinding, err)
	}
	return nil
}

const editorScript = `set -eu
task_id=${DELEGATE_PUEUE_EDIT_TASK_ID:?}
edit_path=$0
command_file="$edit_path/$task_id/command"
printf '%s' "$DELEGATE_PUEUE_EDIT_EXPECTED_COMMAND" | /usr/bin/cmp -s - "$command_file"
printf '%s' "$DELEGATE_PUEUE_EDIT_REPLACEMENT_COMMAND" > "$command_file"
`

func privateEditConfig(base, sourceConfigPath, expectedDigest string) (string, error) {
	data, err := readRegular(sourceConfigPath, MaxControlBytes)
	if err != nil {
		return "", err
	}
	if task.ComputeSHA256(data) != expectedDigest {
		return "", ErrBinding
	}
	config, err := configWithFileEditMode(data)
	if err != nil {
		return "", err
	}
	path := filepath.Join(base, "pueue-edit.yml")
	if err = publishPrivateRecord(base, path, config); err != nil {
		return "", err
	}
	return path, nil
}

func queueJob(snapshot QueueSnapshot, id int64) (Job, bool) {
	for _, job := range snapshot.Jobs {
		if job.ID == id {
			return job, true
		}
	}
	return Job{}, false
}

func escapedCommand(args ...string) string {
	escaped := make([]string, len(args))
	for i, arg := range args {
		escaped[i] = escapePueueArgument(arg)
	}
	return strings.Join(escaped, " ")
}

func escapePueueArgument(arg string) string {
	if arg == "" {
		return "''"
	}
	needsQuotes := false
	for _, r := range arg {
		if isSafeUnquotedPueueRune(r) {
			continue
		}
		needsQuotes = true
		break
	}
	if !needsQuotes {
		return arg
	}
	var quoted strings.Builder
	quoted.WriteByte('\'')
	for _, r := range arg {
		switch r {
		case '\'':
			quoted.WriteString("'\\''")
		case '!':
			quoted.WriteString("'\\!'")
		default:
			quoted.WriteRune(r)
		}
	}
	quoted.WriteByte('\'')
	return quoted.String()
}

func isSafeUnquotedPueueRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_/,.+=", r)
}

func quoteShell(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func runnerEditEnvironment(environment []string, numericID, expected, replacement, editor string) []string {
	values := environmentForCommand(environment)
	values = setEnvironment(values, "EDITOR", editor)
	values = setEnvironment(values, "DELEGATE_PUEUE_EDIT_TASK_ID", numericID)
	values = setEnvironment(values, "DELEGATE_PUEUE_EDIT_EXPECTED_COMMAND", expected)
	values = setEnvironment(values, "DELEGATE_PUEUE_EDIT_REPLACEMENT_COMMAND", replacement)
	return values
}

func setEnvironment(environment []string, name, value string) []string {
	values := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		key, _, found := strings.Cut(entry, "=")
		if !found || key != name {
			values = append(values, entry)
		}
	}
	return append(values, name+"="+value)
}
