package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestObserveAdmissionExecutionTargetKeepsExternalConfigUnknown(t *testing.T) {
	root := t.TempDir()
	external := filepath.Join(t.TempDir(), "pueue.yml")
	saved := task.SupervisorRef{ConfigPath: external}
	tests := []struct {
		name      string
		selection AdmissionSelection
		cause     error
	}{
		{name: "explicit config", selection: AdmissionSelection{ConfigPath: external}},
		{name: "launch scope config", selection: AdmissionSelection{EnvironmentConfigPath: external}},
		{name: "saved external supervisor", selection: AdmissionSelection{SavedSupervisor: &saved}},
		{name: "relative scope config", selection: AdmissionSelection{EnvironmentConfigPath: "relative/pueue.yml"}, cause: pueue.ErrConfiguration},
		{name: "explicit private collision", selection: AdmissionSelection{ConfigPath: pueue.PrivateConfigPath(root)}, cause: pueue.ErrConfiguration},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target, err := ObserveAdmissionExecutionTarget(context.Background(), test.selection, root, AdmissionOptions{})
			if !errors.Is(err, pueue.ErrExecutionTargetUnverified) {
				t.Fatalf("observation error = %v, want ErrExecutionTargetUnverified", err)
			}
			if test.cause != nil && !errors.Is(err, test.cause) {
				t.Fatalf("observation error = %v, want cause %v", err, test.cause)
			}
			if target.State() != pueue.ExecutionTargetUnknown || target.PID() != 0 {
				t.Fatalf("target = %+v, want unknown", target)
			}
		})
	}
	for _, path := range []string{external, filepath.Join(root, ".supervisor")} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("observation created %s: %v", path, err)
		}
	}
}

func TestObserveAdmissionExecutionTargetReportsColdPrivateAbsence(t *testing.T) {
	root := t.TempDir()
	target, err := ObserveAdmissionExecutionTarget(context.Background(), AdmissionSelection{}, root, AdmissionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if target.State() != pueue.ExecutionTargetAbsent {
		t.Fatalf("target = %+v, want absent", target)
	}
	if _, err = os.Lstat(filepath.Join(root, ".supervisor")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("observation created the private supervisor directory: %v", err)
	}
}
