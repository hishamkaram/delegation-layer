package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// These are finite fake supervisor observations; actual supervisor transport is
// Phase 2. An empty or unavailable observation cannot erase a submission guard.
type fixtureAdmissionObservation struct {
	RootID     string `json:"root_id"`
	TaskID     string `json:"task_id"`
	SpecSHA256 string `json:"spec_sha256"`
	MetaSHA256 string `json:"meta_sha256"`
}

func loadObservations(path string) ([]fixtureAdmissionObservation, error) {
	data, err := readControlFile(path)
	if err != nil {
		return nil, err
	}
	var observations []fixtureAdmissionObservation
	if err = task.DecodeStrict(data, &observations); err != nil {
		return nil, err
	}
	return observations, nil
}

func reconciliation(td *taskdir.TaskDir, path string) int {
	req, meta, err := td.PreparedRecords()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	metaBytes, err := task.MarshalCanonical(meta)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	inspection, err := td.Inspect()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	expected := fixtureAdmissionObservation{RootID: req.RootID, TaskID: req.TaskID, SpecSHA256: meta.SpecSHA256, MetaSHA256: task.ComputeSHA256(metaBytes)}
	observed, readErr := loadObservations(path)
	matches := 0
	for _, observation := range observed {
		if observation == expected {
			matches++
		}
	}
	state := "unknown"
	code := 3
	if readErr == nil && inspection.SubmitExists && matches == 1 {
		state = "admitted"
		code = 0
	}
	result := map[string]any{"admission": state, "matching_count": matches, "expected_identity": expected}
	if readErr != nil {
		result["observation_error"] = readErr.Error()
	}
	if outputCode := printJSON(result); outputCode != 0 {
		return outputCode
	}
	return code
}

func runReconcile(args []string) int {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	root := fs.String("root", "", "store root")
	id := fs.String("task-id", "", "task ID")
	observations := fs.String("observations", "", "finite fake observation file")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *root == "" || *id == "" || *observations == "" {
		return 2
	}
	s, err := taskdir.OpenStore(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer closeStore(s)
	td, err := s.OpenTask(*id)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer closeTask(td)
	return reconciliation(td, *observations)
}
