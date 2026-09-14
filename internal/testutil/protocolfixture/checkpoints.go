package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// Checkpoints belong only to this finite acceptance executable. Every injected
// exit identifies its reached boundary before exiting without deferred cleanup.
type checkpointFaultInjector struct {
	taskdir.BaseFaultInjector
	checkpoint  string
	rendezvous  string
	events      string
	release     string
	timeout     time.Duration
	stages      map[string]stageTrace
	skipCleanup map[string]int
	cleanupDest string
}

type stageTrace struct {
	dest          string
	linkAttempted bool
}

func checkpointFlags(fs *flag.FlagSet) *checkpointFaultInjector {
	c := &checkpointFaultInjector{timeout: 10 * time.Second}
	fs.StringVar(&c.checkpoint, "checkpoint", "", "exact crash/fault/hold boundary")
	fs.StringVar(&c.rendezvous, "rendezvous", "", "checkpoint receipt path")
	fs.StringVar(&c.events, "events", "", "ordered operation trace path")
	fs.StringVar(&c.release, "release-file", "", "cooperative release; otherwise a checkpoint self-exits")
	fs.DurationVar(&c.timeout, "wait-budget", 10*time.Second, "finite handshake budget")
	return c
}

func recordKind(path string) string {
	name := filepath.Base(path)
	names := map[string]string{"brief.md": "brief", "task.json": "task", "meta.json": "meta", "submit.json": "submit", "provider.start": "start", "provider.started.json": "started", "provider.exit": "seal", "result.txt": "payload", "publish.reject": "payload", "outcome.json": "outcome"}
	if kind, ok := names[name]; ok {
		return kind
	}
	for suffix, kind := range map[string]string{".claim.json": "session-claim-record", ".release.json": "session-release-record"} {
		if id, ok := strings.CutSuffix(name, suffix); ok && task.ValidateTaskID(id) == nil {
			return kind
		}
	}
	return name
}

func (c *checkpointFaultInjector) event(operation, path string) error {
	return recordFakeSinkEvent(c.events, operation, filepath.Base(path))
}

func (c *checkpointFaultInjector) point(name string) error {
	if err := c.event(name, ""); err != nil {
		return err
	}
	if c.checkpoint != name {
		return nil
	}
	if err := signalRendezvous(c.rendezvous, name); err != nil {
		return err
	}
	if c.release != "" {
		return waitForFile(c.release, c.timeout)
	}
	os.Exit(3)
	return nil
}

func (c *checkpointFaultInjector) fault(operation, path string) error {
	if err := c.event(operation, path); err != nil {
		return err
	}
	name := "fail-" + recordKind(path) + "-" + operation
	if c.checkpoint != name {
		return nil
	}
	return errors.Join(fmt.Errorf("injected %s", name), signalRendezvous(c.rendezvous, name))
}

func (c *checkpointFaultInjector) OnStageWrite(path string, data []byte) (int, error) {
	if c.checkpoint == "fail-"+recordKind(path)+"-short-write" {
		if err := signalRendezvous(c.rendezvous, c.checkpoint); err != nil {
			return 0, err
		}
		return len(data) / 2, nil
	}
	if err := c.fault("write", path); err != nil {
		return 0, err
	}
	return len(data), nil
}

func (c *checkpointFaultInjector) OnStageBarrier(path string) error {
	return c.fault("file-barrier", path)
}
func (c *checkpointFaultInjector) OnStageClose(path string) error { return c.fault("close", path) }

func (c *checkpointFaultInjector) OnBeforeStageCreate(stage, path string) error {
	if c.stages == nil {
		c.stages = make(map[string]stageTrace)
	}
	c.stages[stage] = stageTrace{dest: path}
	return c.event("stage-create", path)
}

func (c *checkpointFaultInjector) OnLink(stage, path string) error {
	trace, ok := c.stages[stage]
	if !ok {
		return fmt.Errorf("link stage has no creation trace: %s", stage)
	}
	if trace.dest != path {
		return fmt.Errorf("link stage destination mismatch: stage=%s got=%s want=%s", stage, path, trace.dest)
	}
	trace.linkAttempted = true
	c.stages[stage] = trace
	if err := c.fault("link", path); err != nil {
		return err
	}
	if recordKind(path) == "seal" {
		if err := c.point("before-seal"); err != nil {
			return err
		}
	}
	return c.point("before-" + recordKind(path) + "-link")
}

func (c *checkpointFaultInjector) OnPostLinkDirBarrier(path string) error {
	if err := c.point("after-" + recordKind(path) + "-link-before-barrier"); err != nil {
		return err
	}
	return c.fault("barrier", path)
}

func (c *checkpointFaultInjector) OnAfterLinkDirBarrier(path string) error {
	if err := c.event("barrier-complete", path); err != nil {
		return err
	}
	return c.point("after-" + recordKind(path) + "-barrier")
}

func (c *checkpointFaultInjector) OnCleanupUnlink(stage string) error {
	trace, ok := c.stages[stage]
	if !ok {
		return fmt.Errorf("cleanup stage has no destination trace: %s", stage)
	}
	if !trace.linkAttempted {
		if c.skipCleanup == nil {
			c.skipCleanup = make(map[string]int)
		}
		c.skipCleanup[filepath.Dir(stage)]++
		return nil
	}
	c.cleanupDest = trace.dest
	if err := c.point("before-" + recordKind(trace.dest) + "-cleanup"); err != nil {
		return err
	}
	return c.fault("cleanup", trace.dest)
}

func (c *checkpointFaultInjector) OnCleanupDirBarrier(path string) error {
	if n := c.skipCleanup[path]; n > 0 {
		if n == 1 {
			delete(c.skipCleanup, path)
		} else {
			c.skipCleanup[path] = n - 1
		}
		return nil
	}
	return c.fault("cleanup-barrier", c.cleanupDest)
}

func (c *checkpointFaultInjector) OnDirBarrier(path string) error {
	return c.fault("directory-barrier", path)
}

func (c *checkpointFaultInjector) OnParentDirBarrier(path string) error {
	return c.fault("parent-barrier", path)
}

func (c *checkpointFaultInjector) OnRawWrite(path string) error { return c.fault("raw-write", path) }

func (c *checkpointFaultInjector) OnRawBarrier(path string) error {
	return c.fault("raw-barrier", path)
}

func (c *checkpointFaultInjector) OnRawClose(path string) error { return c.fault("raw-close", path) }

func (c *checkpointFaultInjector) OnRawDirBarrier(path string) error {
	return c.fault("raw-directory-barrier", path)
}
