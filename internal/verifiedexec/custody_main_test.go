package verifiedexec

// TestMain turns this test binary into the finite owned processes the custody
// tests need. A worker (DELEGATE_CUSTODY_PROOF_WORKER) exercises the production
// Pool in a separate process and may os.Exit at a named point; the helper
// (first argument custody-helper) is the tiny native program that views run. No
// signal is ever sent: a crash is os.Exit, and the parent's Wait on that exact
// process is the oracle that it is gone.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

const (
	proofWorkerEnv   = "DELEGATE_CUSTODY_PROOF_WORKER"
	helperArg        = "custody-helper"
	handshakeTimeout = 20 * time.Second
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(proofWorkerEnv); mode != "" {
		os.Exit(runProofWorker(mode, os.Args[1:]))
	}
	if len(os.Args) > 1 && os.Args[1] == helperArg {
		os.Exit(runHelper(os.Args[2:]))
	}
	os.Exit(m.Run())
}

// runHelper prints one JSON report whose marker is the SHA-256 of the bytes of
// the executable that is actually running, so a view that runs anything other
// than the admitted bytes is visible in the report. With "handshake" it first
// announces READY and waits, within a fixed deadline, for the parent's GO line.
func runHelper(args []string) int {
	report := proofReport{Argv0: os.Args[0]}
	if executable, err := os.Executable(); err == nil {
		report.Executable = executable
		if content, readErr := os.ReadFile(executable); readErr == nil {
			report.Marker = digestOf(content)
		}
	}
	if len(args) > 0 && args[0] == "handshake" {
		report.Handshake = helperHandshake()
	}
	if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
		return 2
	}
	return 0
}

func helperHandshake() string {
	if _, err := fmt.Fprintln(os.Stdout, "READY"); err != nil {
		return "ready-write-failed"
	}
	answered := make(chan string, 1)
	go func() {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil || line != "GO\n" {
			answered <- "no-go"
			return
		}
		answered <- "go"
	}()
	select {
	case result := <-answered:
		return result
	case <-time.After(handshakeTimeout):
		return "deadline"
	}
}
