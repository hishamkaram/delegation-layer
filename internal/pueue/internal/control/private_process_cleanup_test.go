package control

import (
	"errors"
	"os"
	"testing"
)

// TestStartBlockingProcessFinishJoinsChildAfterCloseError proves the blocking
// process fixture always joins its child. A caller that already closed the
// returned Stdin makes the read-end close fail; the finisher still closes the
// write end, waits for cat to exit on EOF, and returns that close error. A
// repeated finish is a no-op.
func TestStartBlockingProcessFinishJoinsChildAfterCloseError(t *testing.T) {
	process, _, finish := startBlockingProcess(t)
	stdin, ok := process.Stdin.(*os.File)
	if !ok {
		t.Fatalf("blocking process stdin type = %T, want *os.File", process.Stdin)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	err := finish()
	if process.ProcessState == nil {
		// The finisher left its child unjoined. Join it once here so this
		// failure leaves no zombie; cat exits on EOF from the closed write end.
		t.Errorf("finish returned %v without joining the child", err)
		if waitErr := process.Wait(); waitErr != nil {
			t.Errorf("join the unjoined child: %v", waitErr)
		}
		return
	}
	if !errors.Is(err, os.ErrClosed) {
		t.Errorf("finish error = %v, want the retained stdin close error", err)
	}
	if !process.ProcessState.Exited() || process.ProcessState.ExitCode() != 0 {
		t.Errorf("child state = %v, want a clean EOF exit", process.ProcessState)
	}
	if err = finish(); err != nil {
		t.Errorf("repeated finish = %v, want nil", err)
	}
}
