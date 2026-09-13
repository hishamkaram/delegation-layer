package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const fixtureConfig = "{\"kind\":\"finite-protocolfixture\",\"containment\":\"fixture-local\",\"approval\":\"never\"}\n"

type prepareOptions struct {
	root, taskID, cwd, briefFile, provider, mode string
	priorTaskID, priorConversation               string
	budgetDuration                               time.Duration
	checkpoints                                  *checkpointFaultInjector
}

func parsePrepareArgs(args []string) (*prepareOptions, int) {
	fs := flag.NewFlagSet("prepare", flag.ContinueOnError)
	o := &prepareOptions{}
	fs.StringVar(&o.root, "root", "", "store root")
	fs.StringVar(&o.taskID, "task-id", "", "task ID")
	fs.StringVar(&o.cwd, "canonical-cwd", "", "workspace")
	fs.StringVar(&o.briefFile, "brief-file", "", "finite UTF-8 brief")
	fs.StringVar(&o.provider, "provider", config.ProviderFixture, "explicit fake provider")
	fs.StringVar(&o.mode, "mode", config.ModeReadOnly, "requested permission")
	fs.StringVar(&o.priorTaskID, "prior-task-id", "", "exact predecessor task ID")
	fs.StringVar(&o.priorConversation, "prior-conversation", "", "exact prior fake conversation")
	budget := fs.String("budget", "30m", "finite task budget")
	o.checkpoints = checkpointFlags(fs)
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return nil, 2
	}
	if o.root == "" || o.taskID == "" || o.cwd == "" || o.briefFile == "" {
		return nil, 2
	}
	if (o.priorTaskID == "") != (o.priorConversation == "") {
		return nil, 2
	}
	var err error
	o.budgetDuration, err = config.ParseBudget(*budget)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return nil, 2
	}
	o.root, o.cwd, err = config.ValidateDirectories(o.root, o.cwd)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return nil, 2
	}
	if _, err = config.ValidateBriefFile(o.briefFile); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return nil, 2
	}
	return o, 0
}

func readBriefFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, readErr := task.ReadBounded(f, config.MaxBriefBytes)
	return data, errors.Join(readErr, f.Close())
}

func fixtureSupervisor(root, rootID string) task.SupervisorRef {
	return task.SupervisorRef{Endpoint: "fixture://" + rootID, ConfigPath: filepath.Join(root, "fixture-supervisor.json"), ConfigDigest: task.ComputeSHA256([]byte(fixtureConfig)), ObservedVersion: "fixture-v1"}
}

func ensureFixtureConfig(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		data, readErr := readBriefFile(path)
		if readErr != nil {
			return readErr
		}
		if string(data) != fixtureConfig {
			return errors.New("fixture supervisor configuration changed")
		}
		return nil
	}
	if err != nil {
		return err
	}
	_, writeErr := io.WriteString(f, fixtureConfig)
	return errors.Join(writeErr, f.Close())
}

func fixtureMetadata(s *taskdir.Store, req *task.TaskRecord) (*task.MetaRecord, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, err
	}
	build, err := fixtureBuildIdentity(executable)
	if err != nil {
		return nil, err
	}
	spec, err := task.MarshalCanonical(req)
	if err != nil {
		return nil, err
	}
	sup := fixtureSupervisor(s.Root, s.RootID)
	if err = ensureFixtureConfig(sup.ConfigPath); err != nil {
		return nil, err
	}
	return &task.MetaRecord{
		SchemaVersion: task.SchemaVersion, RootID: s.RootID, TaskID: req.TaskID, SpecSHA256: task.ComputeSHA256(spec),
		RequestedConfig: req.RequestedConfig, EffectiveConfig: task.EffectiveConfig{Containment: "fixture-local", Approval: "never", Digest: task.ComputeSHA256([]byte(fixtureConfig))},
		Containment: "fixture-local", Approval: "never", ProviderExecutable: executable, ProviderVersion: "protocolfixture-v1", PublisherBuild: build, PublisherVersion: "1",
		Predicate: task.FixturePredicateRef(), SupervisorConfig: sup, CreatedAt: "2026-09-13T00:00:00Z",
	}, nil
}

func fixtureBuildIdentity(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	_, readErr := io.Copy(digest, f)
	if err = errors.Join(readErr, f.Close()); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func runPrepare(args []string) int {
	opts, code := parsePrepareArgs(args)
	if code != 0 {
		return code
	}
	brief, err := readBriefFile(opts.briefFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	s, err := taskdir.InitStore(opts.root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer closeStore(s)
	req := &task.TaskRecord{SchemaVersion: task.SchemaVersion, RootID: s.RootID, TaskID: opts.taskID, Provider: opts.provider, Mode: opts.mode, CanonicalCwd: opts.cwd, BudgetNanos: opts.budgetDuration.Nanoseconds(), BriefSHA256: task.ComputeSHA256(brief), BriefLength: int64(len(brief))}
	if opts.priorTaskID != "" {
		req.PriorSession = &task.PriorSession{Provider: opts.provider, ConversationID: opts.priorConversation, PredecessorTaskID: opts.priorTaskID}
	}
	if err = taskdir.NormalizeTaskRecord(s.Root, req); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	meta, err := fixtureMetadata(s, req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	s.SetFaultInjector(opts.checkpoints)
	td, err := s.CreateTask(opts.taskID, req, brief, meta)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer closeTask(td)
	if err = signalRendezvous(opts.checkpoints.rendezvous, "prepared"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return printJSON(map[string]any{"status": "prepared", "task_id": opts.taskID, "root_id": s.RootID, "brief_sha": req.BriefSHA256, "spec_sha": meta.SpecSHA256})
}
