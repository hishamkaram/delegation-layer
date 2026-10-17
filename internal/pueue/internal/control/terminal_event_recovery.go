package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func readPrivateTerminalEventInbox(base string) (map[int64]struct{}, error) {
	ids := make(map[int64]struct{})
	directory, present, err := privateTerminalEventExistingDirectory(base)
	if err != nil || !present {
		return ids, err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("%w: read private terminal event inbox: %w", ErrConfiguration, err)
	}
	for _, entry := range entries {
		id, entryErr := terminalEventIDFromEntry(entry.Name())
		if entryErr != nil {
			return nil, entryErr
		}
		_, found, readErr := readTerminalEvent(base, id)
		if readErr != nil {
			return nil, readErr
		}
		if !found {
			return nil, fmt.Errorf("%w: private terminal event disappeared during locked read", ErrBinding)
		}
		ids[id] = struct{}{}
	}
	return ids, nil
}

// recoverPrivateTerminalEventStages removes only publisher-owned temporary
// records after validating their reserved random name and file shape. Stages
// are never authority: committed ticket and event records retain their strict
// name and payload validation. The caller holds the terminal-event mutation
// lock, so a stage cannot still belong to an active publisher.
func recoverPrivateTerminalEventStages(base string) error {
	for _, name := range []string{privateTerminalEventTicketDir, privateTerminalEventRecordDir} {
		if err := recoverPrivateTerminalEventDirectoryStages(base, name); err != nil {
			return err
		}
	}
	return nil
}

func recoverPrivateTerminalEventDirectoryStages(base, name string) error {
	directory, present, err := privateTerminalEventExistingNamedDirectory(base, name)
	if err != nil || !present {
		return err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("%w: read private terminal event directory: %w", ErrConfiguration, err)
	}
	stages := make([]string, 0)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), privateRecordStagePrefix) {
			continue
		}
		if err = validatePrivateTerminalEventStage(name, directory, entry.Name()); err != nil {
			return err
		}
		stages = append(stages, filepath.Join(directory, entry.Name()))
	}
	for _, stage := range stages {
		if err = os.Remove(stage); err != nil {
			return fmt.Errorf("%w: remove private terminal event stage: %w", ErrConfiguration, err)
		}
	}
	if len(stages) == 0 {
		return nil
	}
	return syncPrivateDirectory(directory)
}

func validatePrivateTerminalEventStage(directory, parent, name string) error {
	if directory != privateTerminalEventTicketDir && directory != privateTerminalEventRecordDir {
		return ErrConfiguration
	}
	if task.ValidateTaskID(strings.TrimPrefix(name, privateRecordStagePrefix)) != nil {
		return fmt.Errorf("%w: invalid private terminal event stage %q", ErrBinding, name)
	}
	stagePath := filepath.Join(parent, name)
	info, err := os.Lstat(stagePath)
	if err != nil {
		return fmt.Errorf("%w: inspect private terminal event stage: %w", ErrBinding, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o177 != 0 {
		return fmt.Errorf("%w: invalid private terminal event stage %q", ErrBinding, name)
	}
	return nil
}

func privateTerminalEventExistingDirectory(base string) (string, bool, error) {
	return privateTerminalEventExistingNamedDirectory(base, privateTerminalEventRecordDir)
}

func privateTerminalEventExistingNamedDirectory(base, name string) (string, bool, error) {
	if name != privateTerminalEventTicketDir && name != privateTerminalEventRecordDir {
		return "", false, ErrConfiguration
	}
	directory := filepath.Join(base, name)
	if err := rejectSymlinkComponents(directory); err != nil {
		return "", false, err
	}
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return directory, false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("%w: inspect private terminal event inbox: %w", ErrConfiguration, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return "", false, ErrConfiguration
	}
	return directory, true, nil
}

func terminalEventIDFromEntry(name string) (int64, error) {
	const suffix = ".json"
	if !strings.HasSuffix(name, suffix) {
		return 0, fmt.Errorf("%w: invalid private terminal event entry %q", ErrBinding, name)
	}
	id, err := strconv.ParseInt(strings.TrimSuffix(name, suffix), 10, 64)
	if err != nil || id < 0 || strconv.FormatInt(id, 10)+suffix != name {
		return 0, fmt.Errorf("%w: invalid private terminal event entry %q", ErrBinding, name)
	}
	return id, nil
}

// privateTerminalEventJobs selects candidate labels by exact event-backed
// numeric ID during inbox processing, then preserves every row sharing those
// labels so the application can reject ambiguous identities. Full recovery
// exposes the entire current snapshot: a lost candidate hint must not hide
// durable terminal evidence. The application owns every removal decision.
func (c *Client) privateTerminalEventJobs(ctx context.Context, stateRoot string, jobs []Job, eventOnly bool) ([]Job, error) {
	enabled, err := c.PrivateTerminalEventsEnabled(ctx)
	if err != nil {
		return nil, err
	}
	if !enabled || !eventOnly {
		return jobs, nil
	}
	candidateIDs, err := privateTerminalEventInboxIDs(ctx, stateRoot)
	if err != nil {
		return nil, err
	}
	return filterPrivateTerminalEventJobsByID(jobs, candidateIDs), nil
}

func filterPrivateTerminalEventJobsByID(jobs []Job, candidateIDs map[int64]struct{}) []Job {
	candidateLabels, candidateWithoutLabel := privateTerminalEventLabelsByID(jobs, candidateIDs)
	filtered := make([]Job, 0, len(jobs))
	for _, job := range jobs {
		if job.Label == nil {
			if candidateWithoutLabel {
				filtered = append(filtered, job)
			}
			continue
		}
		if _, candidate := candidateLabels[*job.Label]; candidate {
			filtered = append(filtered, job)
		}
	}
	return filtered
}

func privateTerminalEventLabelsByID(jobs []Job, candidateIDs map[int64]struct{}) (map[string]struct{}, bool) {
	candidateLabels := make(map[string]struct{}, len(candidateIDs))
	candidateWithoutLabel := false
	for _, job := range jobs {
		if _, candidate := candidateIDs[job.ID]; !candidate {
			continue
		}
		if job.Label == nil {
			candidateWithoutLabel = true
			continue
		}
		candidateLabels[*job.Label] = struct{}{}
	}
	return candidateLabels, candidateWithoutLabel
}
