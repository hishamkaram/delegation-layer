package gate

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAbsentGateAndProviderProbesNeverWait(t *testing.T) {
	root := privateTestRuntime(t)
	if err := Wait(root, Admission, "no-admission"); err != nil {
		t.Fatal(err)
	}
	for _, probe := range []string{"--help", "--version", "--self-test"} {
		if err := BeforeProvider([]string{probe}); err != nil {
			t.Fatalf("probe %s entered a provider gate: %v", probe, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "admission-entered")); !os.IsNotExist(err) {
		t.Fatalf("absent gate created a receipt: %v", err)
	}
}

func TestHeldGateRecordsIdentityAndRequiresOwnerRelease(t *testing.T) {
	root := privateTestRuntime(t)
	marker := filepath.Join(root, "hold-admission")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		finished <- wait(root, Admission, "exact-task-id", time.Second)
	}()
	t.Cleanup(func() {
		if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
			t.Error(err)
		}
		<-done
	})
	deadline := time.Now().Add(time.Second)
	for {
		data, err := os.ReadFile(filepath.Join(root, "admission-entered"))
		if err == nil && string(data) == "exact-task-id\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture did not publish entered identity")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-finished:
		t.Fatalf("held preparation completed before release: %v", err)
	default:
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}

func privateTestRuntime(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}
