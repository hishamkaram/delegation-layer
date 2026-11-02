//go:build linux || darwin

package supervisor

import (
	"context"
	"errors"
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
	expected, err := stateExecutableSourceDigest(image)
	if err != nil {
		t.Fatal(err)
	}
	owned, err := InstallStateRunner(origin, image)
	if err != nil {
		t.Fatal(err)
	}
	originDigest, err := protectedStateExecutableDigest(owned)
	if err != nil || originDigest != expected {
		t.Fatalf("origin custody changed image: %s %v", originDigest, err)
	}
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
	command.Env = append(os.Environ(), "DELEGATE_INDEPENDENT_CUSTODY_OUTPUT="+output, "DELEGATE_INDEPENDENT_CUSTODY_DIGEST="+expected)
	// The copied fixture does no spawning. Run owns its one Start and exact Wait
	// before the fixture roots can be removed, including a failing child result.
	if data, runErr := command.CombinedOutput(); runErr != nil {
		t.Fatalf("copied native fixture execution: %v: %s", runErr, data)
	}
	data, err := os.ReadFile(output)
	if err != nil || string(data) != expected+"\n" {
		t.Fatalf("copied image execution evidence: %q %v", data, err)
	}
	t.Log("native component custody A->B copied and executed the actual test image; production CLI running-image authenticity remains unverified")
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
