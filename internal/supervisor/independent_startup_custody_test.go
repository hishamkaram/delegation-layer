//go:build linux || darwin

package supervisor

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
)

func TestIndependentStartupCustodyChild(t *testing.T) {
	output := os.Getenv("DELEGATE_INDEPENDENT_CUSTODY_OUTPUT")
	if output == "" {
		t.Skip("fixed copied-image custody component fixture")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	wanted, err := filepath.EvalSymlinks(os.Getenv("DELEGATE_INDEPENDENT_CUSTODY_IMAGE"))
	if err != nil || executable != wanted {
		t.Fatalf("executed different custody path: %v", err)
	}
	digest, err := protectedStateExecutableDigest(executable)
	if err != nil || digest != os.Getenv("DELEGATE_INDEPENDENT_CUSTODY_DIGEST") {
		t.Fatalf("executed custody image mismatch: digest=%s err=%v", digest, err)
	}
	file, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.WriteString(digest + "\n")
	if err = errors.Join(writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
}

func TestIndependentStartupCustodyNativeProof(t *testing.T) {
	if os.Getenv("DELEGATE_INDEPENDENT_EXPECTED_BACKEND") == "" {
		t.Skip("explicit native custody component proof")
	}
	origin := supervisorfixture.CanonicalTempDir(t)
	target := supervisorfixture.CanonicalTempDir(t)
	image, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image, err = filepath.EvalSymlinks(image)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := stateExecutableSourceDigest(image)
	if err != nil {
		t.Fatal(err)
	}
	installed := independentMutableInstallation(t, image)
	owned, err := InstallStateRunner(origin, installed)
	if err != nil {
		t.Fatal(err)
	}
	originDigest, err := protectedStateExecutableDigest(owned)
	if err != nil || originDigest != expected {
		t.Fatalf("origin custody changed image: %s %v", originDigest, err)
	}
	replaceIndependentInstallation(t, installed, expected)
	staged, err := InstallStateRunner(target, owned)
	if err != nil {
		t.Fatal(err)
	}
	targetDigest, err := protectedStateExecutableDigest(staged)
	if err != nil || targetDigest != expected {
		t.Fatalf("target custody changed image: %s %v", targetDigest, err)
	}
	output := filepath.Join(target, "executed-copy")
	command := exec.Command(staged, "-test.run=^TestIndependentStartupCustodyChild$", "-test.timeout=30s")
	command.Dir = target
	command.Env = append(os.Environ(), "DELEGATE_INDEPENDENT_CUSTODY_OUTPUT="+output, "DELEGATE_INDEPENDENT_CUSTODY_DIGEST="+expected, "DELEGATE_INDEPENDENT_CUSTODY_IMAGE="+staged)
	// The copied fixture does no spawning. Run owns its one Start and exact Wait
	// before the fixture roots can be removed, including a failing child result.
	if data, runErr := command.CombinedOutput(); runErr != nil {
		t.Fatalf("copied native fixture execution: %v: %s", runErr, data)
	}
	data, err := os.ReadFile(output)
	if err != nil || string(data) != expected+"\n" {
		t.Fatalf("copied image execution evidence: %q %v", data, err)
	}
	t.Log("native component A->B executes the exact established custody image after replacing the installation fixture; production resident authority remains unverified")
}

func TestIndependentStartupOwnStatePairProof(t *testing.T) {
	if os.Getenv("DELEGATE_INDEPENDENT_EXPECTED_BACKEND") == "" {
		t.Skip("explicit native own-state pair component proof")
	}
	sourceClient, sourceDaemon := os.Getenv("DELEGATE_TEST_PUEUE"), os.Getenv("DELEGATE_TEST_PUEUED")
	if sourceClient == "" || sourceDaemon == "" {
		t.Fatal("explicit verified test supervisor paths required; no ambient executable fallback")
	}
	origin := supervisorfixture.CanonicalTempDir(t)
	target := supervisorfixture.CanonicalTempDir(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	client, daemon, err := InstallStateSupervisorPairPaths(ctx, origin, sourceClient, sourceDaemon)
	if err != nil {
		t.Fatal(err)
	}
	resolvedClient, resolvedDaemon, found, err := ResolveStateSupervisorPair(filepath.Join(origin, ".supervisor"))
	if err != nil || !found || resolvedClient != client || resolvedDaemon != daemon {
		t.Fatalf("origin own state pair: found=%t err=%v", found, err)
	}
	stagedClient, stagedDaemon, err := InstallStateSupervisorPairPaths(ctx, target, resolvedClient, resolvedDaemon)
	if err != nil {
		t.Fatal(err)
	}
	pairs := [][2]string{{client, stagedClient}, {daemon, stagedDaemon}}
	for _, pair := range pairs {
		expected, sourceErr := protectedStateExecutableDigest(pair[0])
		actual, targetErr := protectedStateExecutableDigest(pair[1])
		if sourceErr != nil || targetErr != nil || expected != actual {
			t.Fatalf("state pair custody changed bytes: %v %v", sourceErr, targetErr)
		}
	}
	requireIndependentTargetPair(t, target, stagedClient, stagedDaemon)
	t.Log("native component staged a verified fixture pair through A state into B without ExecutableLookup; production installation provenance and resident composition remain unverified")
}

func requireIndependentTargetPair(t *testing.T, target, stagedClient, stagedDaemon string) {
	t.Helper()
	clientAgain, daemonAgain, found, err := ResolveStateSupervisorPair(filepath.Join(target, ".supervisor"))
	if err != nil || !found || clientAgain != stagedClient || daemonAgain != stagedDaemon {
		t.Fatalf("target own state pair: found=%t err=%v", found, err)
	}
}

func independentMutableInstallation(t *testing.T, image string) string {
	t.Helper()
	installed := filepath.Join(supervisorfixture.CanonicalTempDir(t), "delegate-run")
	source, err := os.Open(image)
	if err != nil {
		t.Fatal(err)
	}
	destination, err := os.OpenFile(installed, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		t.Fatal(errors.Join(err, source.Close()))
	}
	_, copyErr := io.Copy(destination, source)
	if err = errors.Join(copyErr, destination.Close(), source.Close()); err != nil {
		t.Fatal(err)
	}
	return installed
}

func replaceIndependentInstallation(t *testing.T, installed, priorDigest string) {
	t.Helper()
	replacement := installed + ".next"
	if err := os.WriteFile(replacement, []byte("#!/bin/sh\nexit 99\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, installed); err != nil {
		t.Fatal(err)
	}
	digest, err := stateExecutableSourceDigest(installed)
	if err != nil || digest == priorDigest {
		t.Fatalf("installation replacement counterfactual did not change bytes: %v", err)
	}
}
