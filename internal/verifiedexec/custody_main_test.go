package verifiedexec

import (
	"bufio"
	"fmt"
	"os"
	"testing"
	"time"
)

const proofWorkerEnv = "DELEGATE_CUSTODY_PROOF_WORKER"

// The exact test executable runs finite legacy cleanup workers in place.
func TestMain(m *testing.M) {
	if mode := os.Getenv(proofWorkerEnv); mode != "" {
		os.Exit(runProofWorker(mode, os.Args[1:]))
	}
	os.Exit(m.Run())
}

const handshakeTimeout = 20 * time.Second

// helperHandshake coordinates finite cleanup workers; it observes no program bytes.
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
