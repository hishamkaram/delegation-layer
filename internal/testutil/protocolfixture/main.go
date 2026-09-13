package main

import (
	"fmt"
	"os"
)

const usageText = `protocolfixture - test helper exercising internal/taskdir durable protocol

Usage: protocolfixture <subcommand> [flags]

Available subcommands:
  prepare    - Prepare task directory, brief.md, task.json, and meta.json
  claim      - Prepare submission or start authority with one-use permits
  seal       - Own a finite runner through raw capture and provider.exit sealing
  collect    - Validate sealed evidence and publish payload and outcome
  candidate  - Stage and commit candidate files at store seam
  inspect    - Observational inspection of task directory state
  scavenge   - Run store scavenger to clean abandoned stage files
  lock-hold  - Hold locks, test close-on-exec, or test kernel release on self-exit
  session    - Claim or release durable continuation sessions
  reconcile  - Read finite fake supervisor observations without new authority
  wait       - Deterministic synchronization helper polling for a wait-file
`

func printUsage() int {
	if _, err := fmt.Fprint(os.Stdout, usageText); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func executeFixture(args []string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		return printUsage()
	}
	commands := map[string]func([]string) int{
		"prepare": runPrepare, "claim": runClaim, "seal": runSeal, "collect": runCollect,
		"candidate": runCandidate, "inspect": runInspect, "scavenge": runScavenge,
		"lock-hold": runLockHold, "session": runSession, "reconcile": runReconcile, "wait": runWait,
	}
	command, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", args[0])
		return 2
	}
	return command(args[1:])
}

func main() {
	code := executeFixture(os.Args[1:])
	if cleanupFailed.Load() && code == 0 {
		code = 1
	}
	os.Exit(code)
}
