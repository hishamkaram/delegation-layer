package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type sealOptions struct {
	claim                                                 claimOptions
	stdout, stderr, stdoutFile, stderrFile, reason, state string
	exit                                                  int
	publish                                               bool
}

func parseSeal(args []string) (*sealOptions, int) {
	fs := flag.NewFlagSet("seal", flag.ContinueOnError)
	o := &sealOptions{}
	fs.StringVar(&o.claim.root, "root", "", "store root")
	fs.StringVar(&o.claim.id, "task-id", "", "task ID")
	fs.StringVar(&o.claim.budget, "budget", "", "optional exact immutable budget")
	fs.StringVar(&o.claim.sink, "fake-sink-event", "", "actual fake Start entry recorder")
	fs.StringVar(&o.stdout, "raw-stdout", "fixture answer\n", "finite fake stdout")
	fs.StringVar(&o.stderr, "raw-stderr", "", "finite fake stderr")
	fs.StringVar(&o.stdoutFile, "raw-stdout-file", "", "stream stdout from regular fixture input")
	fs.StringVar(&o.stderrFile, "raw-stderr-file", "", "stream stderr from regular fixture input")
	fs.StringVar(&o.reason, "reason", "", "definite invocation failure reason")
	fs.StringVar(&o.state, "inv-state", task.InvocationStarted, "started or start_failed")
	fs.IntVar(&o.exit, "exit-code", 0, "observed exit code")
	fs.BoolVar(&o.publish, "publish", false, "finalize under the same continuous runner lease")
	o.claim.checkpoint = checkpointFlags(fs)
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || o.claim.root == "" || o.claim.id == "" {
		return nil, 2
	}
	if o.state != task.InvocationStarted && o.state != task.InvocationStartFailed {
		return nil, 2
	}
	return o, 0
}

func fixtureRawInput(text, path string) (io.ReadCloser, error) {
	if path == "" {
		return io.NopCloser(strings.NewReader(text)), nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("raw input must be a regular fixture file")
	}
	return os.Open(path)
}

func writeFixtureRaw(td *taskdir.TaskDir, name, text, path string, c *checkpointFaultInjector) error {
	input, err := fixtureRawInput(text, path)
	if err != nil {
		return err
	}
	writer, err := td.OpenRawWriter(name)
	if err != nil {
		return errors.Join(err, input.Close())
	}
	_, writeErr := io.Copy(writer, input)
	inputErr := input.Close()
	if writeErr == nil && inputErr == nil && name == "stdout" {
		if err = c.point("raw-writer-open"); err != nil {
			return errors.Join(err, writer.Close())
		}
	}
	return errors.Join(writeErr, inputErr, writer.Close())
}

func sealWithLease(td *taskdir.TaskDir, o *sealOptions) int {
	permit, code := prepareRunner(td, &o.claim)
	if code != 0 {
		return code
	}
	defer func() { reportCleanup(permit.Release()) }()
	if o.state == task.InvocationStarted {
		if code = consumeRunnerStart(td, permit, &o.claim); code != 0 {
			return code
		}
	} else {
		if err := permit.Consume(); err != nil {
			return claimError(err, task.ErrAlreadyStarted, 11)
		}
		if err := o.claim.checkpoint.point("after-start-consume"); err != nil {
			return claimError(err, task.ErrAlreadyStarted, 11)
		}
	}
	if err := writeFixtureRaw(td, "stdout", o.stdout, o.stdoutFile, o.claim.checkpoint); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := writeFixtureRaw(td, "stderr", o.stderr, o.stderrFile, o.claim.checkpoint); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	_, meta, err := td.PreparedRecords()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	seal, err := td.Seal(o.state, o.exit, o.reason, meta.Predicate)
	if err != nil {
		return handleCollectError(err)
	}
	if o.publish {
		outcome, cleanupErr, pubErr := td.Finalize(meta.Predicate)
		return collectionResult(td, outcome, cleanupErr, pubErr, false)
	}
	return printJSON(map[string]any{"status": "sealed", "task_id": o.claim.id, "manifest_sha": seal.ManifestSHA256, "seal": seal})
}

func runSeal(args []string) int {
	o, code := parseSeal(args)
	if code != 0 {
		return code
	}
	s, err := taskdir.OpenStore(o.claim.root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer closeStore(s)
	s.SetFaultInjector(o.claim.checkpoint)
	td, err := s.OpenTask(o.claim.id)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer closeTask(td)
	return sealWithLease(td, o)
}
