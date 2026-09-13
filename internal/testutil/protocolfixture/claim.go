package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type claimOptions struct {
	root, id, kind, budget, sink          string
	endpoint, configPath, digest, version string
	pid                                   int
	consume                               bool
	checkpoint                            *checkpointFaultInjector
}

func claimError(err error, already error, code int) int {
	fmt.Fprintln(os.Stderr, err)
	if errors.Is(err, already) || errors.Is(err, task.ErrLockBusy) {
		return code
	}
	if errors.Is(err, task.ErrUncertainDurability) {
		return 13
	}
	if errors.Is(err, task.ErrPermitAlreadyUsed) {
		return 12
	}
	return 1
}

func runAdmissionClaim(td *taskdir.TaskDir, o *claimOptions) int {
	_, meta, err := td.PreparedRecords()
	if err != nil {
		return claimError(err, task.ErrAlreadySubmitted, 10)
	}
	sup := meta.SupervisorConfig
	if o.endpoint != "" {
		sup.Endpoint = o.endpoint
	}
	if o.configPath != "" {
		sup.ConfigPath = o.configPath
	}
	if o.digest != "" {
		sup.ConfigDigest = o.digest
	}
	if o.version != "" {
		sup.ObservedVersion = o.version
	}
	permit, err := td.PrepareSubmission(sup)
	if err != nil {
		return claimError(err, task.ErrAlreadySubmitted, 10)
	}
	defer func() { reportCleanup(permit.Release()) }()
	if err = o.checkpoint.event("submit-permit-issued", o.id); err != nil {
		return claimError(err, task.ErrAlreadySubmitted, 10)
	}
	if err = o.checkpoint.point("before-submit-consume"); err != nil {
		return claimError(err, task.ErrAlreadySubmitted, 10)
	}
	if o.consume {
		if err = permit.Consume(); err != nil {
			return claimError(err, task.ErrAlreadySubmitted, 10)
		}
		if err = o.checkpoint.point("after-submit-consume"); err != nil {
			return claimError(err, task.ErrAlreadySubmitted, 10)
		}
		if err = recordFakeSinkEvent(o.sink, "admission_sink", o.id); err != nil {
			return claimError(err, task.ErrAlreadySubmitted, 10)
		}
		if err = o.checkpoint.point("after-submit-sink"); err != nil {
			return claimError(err, task.ErrAlreadySubmitted, 10)
		}
	}
	return printJSON(map[string]any{"status": "submitted", "task_id": o.id, "consumed": permit.IsConsumed()})
}

func consumeRunnerStart(td *taskdir.TaskDir, permit *taskdir.StartPermit, o *claimOptions) int {
	if err := permit.Consume(); err != nil {
		return claimError(err, task.ErrAlreadyStarted, 11)
	}
	if err := o.checkpoint.point("after-start-consume"); err != nil {
		return claimError(err, task.ErrAlreadyStarted, 11)
	}
	if err := recordFakeSinkEvent(o.sink, "runner_sink", o.id); err != nil {
		return claimError(err, task.ErrAlreadyStarted, 11)
	}
	if err := o.checkpoint.point("after-start-sink"); err != nil {
		return claimError(err, task.ErrAlreadyStarted, 11)
	}
	pid := o.pid
	if pid == 0 {
		pid = os.Getpid()
	}
	if err := td.RecordStarted(int64(pid)); err != nil {
		return claimError(err, task.ErrAlreadyStarted, 11)
	}
	return 0
}

func prepareRunner(td *taskdir.TaskDir, o *claimOptions) (*taskdir.StartPermit, int) {
	var nanos int64
	if o.budget != "" {
		duration, err := config.ParseBudget(o.budget)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return nil, 2
		}
		nanos = duration.Nanoseconds()
	}
	permit, err := td.PrepareStart(nanos)
	if err != nil {
		return nil, claimError(err, task.ErrAlreadyStarted, 11)
	}
	if err = o.checkpoint.event("start-permit-issued", o.id); err != nil {
		reportCleanup(permit.Release())
		return nil, 1
	}
	if err = o.checkpoint.point("before-start-consume"); err != nil {
		reportCleanup(permit.Release())
		return nil, 1
	}
	return permit, 0
}

func runRunnerClaim(td *taskdir.TaskDir, o *claimOptions) int {
	permit, code := prepareRunner(td, o)
	if code != 0 {
		return code
	}
	defer func() { reportCleanup(permit.Release()) }()
	if o.consume {
		if code = consumeRunnerStart(td, permit, o); code != 0 {
			return code
		}
	}
	return printJSON(map[string]any{"status": "started", "task_id": o.id, "consumed": permit.IsConsumed()})
}

func parseClaim(args []string) (*claimOptions, int) {
	fs := flag.NewFlagSet("claim", flag.ContinueOnError)
	o := &claimOptions{}
	fs.StringVar(&o.root, "root", "", "store root")
	fs.StringVar(&o.id, "task-id", "", "task ID")
	fs.StringVar(&o.kind, "kind", "admission", "admission or runner")
	fs.StringVar(&o.budget, "budget", "", "optional exact immutable budget")
	fs.StringVar(&o.sink, "fake-sink-event", "", "actual fake side-effect entry trace")
	fs.StringVar(&o.endpoint, "sup-endpoint", "", "supervisor endpoint override")
	fs.StringVar(&o.configPath, "sup-config", "", "supervisor config override")
	fs.StringVar(&o.digest, "sup-digest", "", "supervisor digest override")
	fs.StringVar(&o.version, "sup-version", "", "supervisor version override")
	fs.IntVar(&o.pid, "pid", 0, "finite fake provider PID")
	fs.BoolVar(&o.consume, "consume-permit", true, "consume issued authority")
	o.checkpoint = checkpointFlags(fs)
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || o.root == "" || o.id == "" {
		return nil, 2
	}
	if o.kind != "admission" && o.kind != "runner" {
		return nil, 2
	}
	return o, 0
}

func runClaim(args []string) int {
	o, code := parseClaim(args)
	if code != 0 {
		return code
	}
	s, err := taskdir.OpenStore(o.root)
	if err != nil {
		if o.kind == "runner" {
			return claimError(err, task.ErrAlreadyStarted, 11)
		}
		return claimError(err, task.ErrAlreadySubmitted, 10)
	}
	defer closeStore(s)
	s.SetFaultInjector(o.checkpoint)
	td, err := s.OpenTask(o.id)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer closeTask(td)
	if o.kind == "admission" {
		return runAdmissionClaim(td, o)
	}
	return runRunnerClaim(td, o)
}
