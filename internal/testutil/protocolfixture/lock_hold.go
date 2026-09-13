package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func resolveLockPath(root, taskID, level string) string {
	switch level {
	case "maintenance":
		return filepath.Join(root, ".maintenance.lock")
	case "admission":
		return filepath.Join(root, "tasks", taskID, ".admission.lock")
	case "run":
		return filepath.Join(root, "tasks", taskID, ".run.lock")
	case "session":
		return filepath.Join(root, "sessions", "test-session", ".session.lock")
	default:
		return filepath.Join(root, ".test.lock")
	}
}

func waitForFile(filePath string, timeout time.Duration) error {
	if filePath == "" {
		return nil
	}
	start := time.Now()
	for {
		if _, err := os.Stat(filePath); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if time.Since(start) > timeout {
			return fmt.Errorf("timeout waiting for file: %s", filePath)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func runWait(args []string) int {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	waitFile := fs.String("wait-file", "", "file to wait for")
	timeoutMs := fs.Int("timeout-ms", 5000, "timeout in milliseconds")
	rendezvous := fs.String("rendezvous", "", "rendezvous file path")
	exitReceipt := fs.String("exit-receipt", "", "last successful child exit receipt")

	if pErr := fs.Parse(args); pErr != nil {
		return 2
	}
	if *waitFile == "" || *timeoutMs <= 0 {
		return 2
	}
	if err := signalRendezvous(*rendezvous, "waiting"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if wErr := waitForFile(*waitFile, time.Duration(*timeoutMs)*time.Millisecond); wErr != nil {
		fmt.Fprintf(os.Stderr, "wait error: %v\n", wErr)
		return 1
	}
	if err := signalRendezvous(*exitReceipt, "wait-exited-0"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func acquireHeldLock(l *taskdir.LockFile, mode string, budget time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	if mode == "sh" {
		return l.LockSH(ctx)
	}
	return l.LockEX(ctx)
}

func handleExecChild(execChild bool, childWaitFile, childReadyFile, childExitFile string) error {
	if !execChild || childWaitFile == "" || childReadyFile == "" {
		return nil
	}
	if childExitFile == "" {
		return errors.New("exec-child requires an exit receipt")
	}
	cmd := exec.Command(os.Args[0], "wait", "-wait-file", childWaitFile, "-rendezvous", childReadyFile, "-exit-receipt", childExitFile, "-timeout-ms", "10000")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = nil
	if sErr := cmd.Start(); sErr != nil {
		return fmt.Errorf("starting child: %w", sErr)
	}
	if _, wErr := waitForRendezvous(childReadyFile, "waiting", cmd.Process.Pid, 5*time.Second); wErr != nil {
		return fmt.Errorf("waiting for child ready: %w", wErr)
	}
	return nil
}

type lockHoldOpts struct {
	root           string
	taskID         string
	level          string
	mode           string
	rendezvous     string
	waitFile       string
	childWaitFile  string
	childReadyFile string
	childExitFile  string
	timeoutMs      int
	exitCode       int
	noUnlock       bool
	execChild      bool
}

func parseLockHoldFlags(args []string) (*lockHoldOpts, int) {
	fs := flag.NewFlagSet("lock-hold", flag.ContinueOnError)
	opts := &lockHoldOpts{}
	fs.StringVar(&opts.root, "root", "", "store root directory")
	fs.StringVar(&opts.taskID, "task-id", "", "task ID")
	fs.StringVar(&opts.level, "level", "maintenance", "level: maintenance, admission, run, session")
	fs.StringVar(&opts.mode, "mode", "ex", "mode: ex or sh")
	fs.StringVar(&opts.rendezvous, "rendezvous", "", "rendezvous file path")
	fs.StringVar(&opts.waitFile, "wait-file", "", "wait file before exit")
	fs.StringVar(&opts.childWaitFile, "child-wait-file", "", "child wait file for exec-child")
	fs.StringVar(&opts.childReadyFile, "child-ready-file", "", "child ready file for exec-child")
	fs.StringVar(&opts.childExitFile, "child-exit-file", "", "child success exit receipt for exec-child")
	fs.IntVar(&opts.timeoutMs, "timeout-ms", 5000, "timeout in milliseconds")
	fs.IntVar(&opts.exitCode, "exit-code", 0, "exit code to exit with")
	fs.BoolVar(&opts.noUnlock, "no-unlock", true, "exit directly without unlocking")
	fs.BoolVar(&opts.execChild, "exec-child", false, "exec child to test close-on-exec")

	if pErr := fs.Parse(args); pErr != nil {
		return nil, 2
	}
	if opts.root == "" || opts.timeoutMs <= 0 || (opts.mode != "sh" && opts.mode != "ex") {
		return nil, 2
	}
	return opts, 0
}

func runLockHold(args []string) int {
	opts, code := parseLockHoldFlags(args)
	if code != 0 {
		return code
	}

	lockPath := resolveLockPath(opts.root, opts.taskID, opts.level)
	if mErr := os.MkdirAll(filepath.Dir(lockPath), 0o700); mErr != nil {
		fmt.Fprintf(os.Stderr, "error creating lock parent: %v\n", mErr)
		return 1
	}

	l, lErr := taskdir.OpenLockFile(lockPath, taskdir.LockLevelMaintenance)
	if lErr != nil {
		fmt.Fprintf(os.Stderr, "error opening lock: %v\n", lErr)
		return 1
	}

	if err := acquireHeldLock(l, opts.mode, time.Duration(opts.timeoutMs)*time.Millisecond); err != nil {
		closeLock(l)
		fmt.Fprintf(os.Stderr, "error acquiring lock: %v\n", err)
		return 1
	}

	if err := signalRendezvous(opts.rendezvous, "locked"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		closeLock(l)
		return 1
	}

	if err := handleExecChild(opts.execChild, opts.childWaitFile, opts.childReadyFile, opts.childExitFile); err != nil {
		fmt.Fprintf(os.Stderr, "exec-child error: %v\n", err)
		return 1
	}

	if opts.waitFile != "" {
		if wErr := waitForFile(opts.waitFile, time.Duration(opts.timeoutMs)*time.Millisecond); wErr != nil {
			fmt.Fprintf(os.Stderr, "error waiting for wait-file: %v\n", wErr)
			return 1
		}
	}

	if opts.noUnlock {
		os.Exit(opts.exitCode)
	}

	if uErr := l.Unlock(); uErr != nil {
		fmt.Fprintf(os.Stderr, "error unlocking: %v\n", uErr)
		return 1
	}
	closeLock(l)
	return 0
}
