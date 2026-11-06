package control

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestInstalledBindingReadsMetadataWithoutOpeningExecutable(t *testing.T) {
	base := canonicalTemp(t)
	executable := filepath.Join(base, "pueue")
	writeExecutable(t, executable, "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(executable, 0o111); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(base, "pueue.yml")
	writeFakeConfig(t, configPath, base)
	resolution, err := resolutionContext(os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{runtimeBinding: task.SupervisorRef{ExecutableIdentity: task.ExecutableIdentityInstalledPathV1, ClientExecutable: executable, ConfigPath: configPath}, options: Options{Resolution: &resolution}}
	binding, err := client.readBinding()
	if err != nil {
		t.Fatalf("metadata-only executable binding failed: %v", err)
	}
	if binding.ExecutableIdentity != task.ExecutableIdentityInstalledPathV1 || binding.ClientSHA256 != "" || binding.DaemonSHA256 != "" || binding.ClientExecutable != executable {
		t.Fatalf("fresh binding has program fingerprint: %+v", binding)
	}
	if binding.ConfigDigest == "" || binding.ResolvedConfigSHA256 == "" || binding.Endpoint == "" {
		t.Fatalf("config identity was lost: %+v", binding)
	}
	if err = os.Chmod(executable, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = client.readBinding(); !errors.Is(err, ErrBinding) {
		t.Fatalf("non-executable role accepted: %v", err)
	}
}
