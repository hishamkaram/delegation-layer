package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestInstallStateSupervisorPairPersistsCurrentExecutables(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	base := filepath.Join(root, ".supervisor")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	writeLegacySupervisorPairFixture(t, base)
	client := filepath.Join(supervisorfixture.CanonicalTempDir(t), "pueue")
	daemon := filepath.Join(supervisorfixture.CanonicalTempDir(t), "pueued")
	supervisorfixture.WriteExecutable(t, client, "client v1")
	supervisorfixture.WriteExecutable(t, daemon, "daemon v1")
	firstClient, firstDaemon := installAndResolveSupervisorPairFixture(t, root, base, client, daemon, "client v1", "daemon v1")
	resolvedClient, resolvedDaemon, err := resolveBundledExecutables(filepath.Join(base, "delegate-run"))
	if err != nil || resolvedClient != firstClient || resolvedDaemon != firstDaemon {
		t.Fatalf("state runner did not resolve the published pair: client=%q daemon=%q err=%v", resolvedClient, resolvedDaemon, err)
	}
	requireSupervisorPairRecordFixture(t, base)
	supervisorfixture.WriteExecutable(t, client, "client v2")
	supervisorfixture.WriteExecutable(t, daemon, "daemon v2")
	secondClient, secondDaemon := installAndResolveSupervisorPairFixture(t, root, base, client, daemon, "client v2", "daemon v2")
	requireSupervisorPairGenerationChanged(t, firstClient, firstDaemon, secondClient, secondDaemon)
	supervisorfixture.RequireExecutable(t, firstClient, "client v1")
	supervisorfixture.RequireExecutable(t, firstDaemon, "daemon v1")
	requireLegacySupervisorPairFixture(t, base)
}

func TestInstallStateSupervisorPairDoesNotWaitForTaskMaintenanceLease(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	lock, err := taskdir.OpenLockFile(filepath.Join(root, ".maintenance.lock"), taskdir.LockLevelMaintenance)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := lock.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = lock.LockSH(context.Background()); err != nil {
		t.Fatal(err)
	}

	client := filepath.Join(supervisorfixture.CanonicalTempDir(t), "pueue")
	daemon := filepath.Join(supervisorfixture.CanonicalTempDir(t), "pueued")
	supervisorfixture.WriteExecutable(t, client, "client while a task is active")
	supervisorfixture.WriteExecutable(t, daemon, "daemon while a task is active")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = installStateSupervisorPair(ctx, root, client, daemon); err != nil {
		t.Fatalf("supervisor pair installation waited on the shared task lease: %v", err)
	}
	if _, err = os.Stat(filepath.Join(root, ".supervisor", stateSupervisorPairRecordName)); err != nil {
		t.Fatalf("supervisor pair was not published while task lease remained held: %v", err)
	}
}

func writeLegacySupervisorPairFixture(t *testing.T, base string) {
	t.Helper()
	for _, legacy := range []struct {
		name string
		data string
	}{{name: "pueue", data: "legacy client"}, {name: "pueued", data: "legacy daemon"}} {
		if err := os.WriteFile(filepath.Join(base, legacy.name), []byte(legacy.data), 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

func installAndResolveSupervisorPairFixture(t *testing.T, root, base, client, daemon, wantClient, wantDaemon string) (string, string) {
	t.Helper()
	if err := installStateSupervisorPair(context.Background(), root, client, daemon); err != nil {
		t.Fatal(err)
	}
	return requireResolvedSupervisorPair(t, base, wantClient, wantDaemon)
}

func requireSupervisorPairRecordFixture(t *testing.T, base string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(base, stateSupervisorPairRecordName)); err != nil {
		t.Fatalf("state-root pair record was not published: %v", err)
	}
}

func requireSupervisorPairGenerationChanged(t *testing.T, firstClient, firstDaemon, secondClient, secondDaemon string) {
	t.Helper()
	if firstClient == secondClient || firstDaemon == secondDaemon {
		t.Fatal("supervisor pair upgrade reused the old generation")
	}
}

func requireLegacySupervisorPairFixture(t *testing.T, base string) {
	t.Helper()
	for _, legacy := range []struct {
		name string
		want string
	}{{name: "pueue", want: "legacy client"}, {name: "pueued", want: "legacy daemon"}} {
		got, err := os.ReadFile(filepath.Join(base, legacy.name))
		if err != nil || string(got) != legacy.want {
			t.Fatalf("legacy state-root %s pair was changed: %q err=%v", legacy.name, got, err)
		}
	}
}

func requireResolvedSupervisorPair(t *testing.T, base, wantClient, wantDaemon string) (string, string) {
	t.Helper()
	client, daemon, found, err := ResolveStateSupervisorPair(base)
	if err != nil || !found {
		t.Fatalf("state-root pair resolution found=%t err=%v", found, err)
	}
	for _, executable := range []struct {
		path string
		want string
	}{{path: client, want: wantClient}, {path: daemon, want: wantDaemon}} {
		got, readErr := os.ReadFile(executable.path)
		if readErr != nil || !strings.HasSuffix(string(got), executable.want) {
			t.Fatalf("resolved state-root executable %q = %q, err=%v", executable.path, got, readErr)
		}
	}
	return client, daemon
}

func TestInstallStateSupervisorPairPublishesCoherentGenerations(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	clientA, daemonA := filepath.Join(supervisorfixture.CanonicalTempDir(t), "pueue-a"), filepath.Join(supervisorfixture.CanonicalTempDir(t), "pueued-a")
	clientB, daemonB := filepath.Join(supervisorfixture.CanonicalTempDir(t), "pueue-b"), filepath.Join(supervisorfixture.CanonicalTempDir(t), "pueued-b")
	supervisorfixture.WriteExecutable(t, clientA, strings.Repeat("client-a", 16*1024))
	supervisorfixture.WriteExecutable(t, daemonA, strings.Repeat("daemon-a", 16*1024))
	supervisorfixture.WriteExecutable(t, clientB, strings.Repeat("client-b", 16*1024))
	supervisorfixture.WriteExecutable(t, daemonB, strings.Repeat("daemon-b", 16*1024))
	if err := installStateSupervisorPair(context.Background(), root, clientA, daemonA); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(root, ".supervisor")
	start := make(chan struct{})
	done := make(chan struct{})
	errorsFound := make(chan error, 9)
	var writers sync.WaitGroup
	for index := 0; index < 8; index++ {
		writers.Add(1)
		go writeSupervisorPairGeneration(start, &writers, root, index, clientA, daemonA, clientB, daemonB, errorsFound)
	}
	var reader sync.WaitGroup
	reader.Add(1)
	go readSupervisorPairGenerations(start, done, &reader, base, errorsFound)
	close(start)
	writers.Wait()
	close(done)
	reader.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Error(err)
	}
}

func writeSupervisorPairGeneration(start <-chan struct{}, writers *sync.WaitGroup, root string, index int, clientA, daemonA, clientB, daemonB string, errorsFound chan<- error) {
	defer writers.Done()
	<-start
	client, daemon := clientA, daemonA
	if index%2 != 0 {
		client, daemon = clientB, daemonB
	}
	for iteration := 0; iteration < 3; iteration++ {
		if err := installStateSupervisorPair(context.Background(), root, client, daemon); err != nil {
			errorsFound <- err
			return
		}
	}
}

func readSupervisorPairGenerations(start, done <-chan struct{}, reader *sync.WaitGroup, base string, errorsFound chan<- error) {
	defer reader.Done()
	<-start
	for {
		select {
		case <-done:
			return
		default:
		}
		if err := inspectSupervisorPairGeneration(base); err != nil {
			errorsFound <- err
			return
		}
	}
}

func inspectSupervisorPairGeneration(base string) error {
	client, daemon, found, err := ResolveStateSupervisorPair(base)
	if err != nil {
		return fmt.Errorf("resolve pair during concurrent install: %w", err)
	}
	if !found {
		return errors.New("state supervisor pair was not published during concurrent install")
	}
	clientData, clientErr := os.ReadFile(client)
	daemonData, daemonErr := os.ReadFile(daemon)
	clientAFound := strings.HasSuffix(string(clientData), strings.Repeat("client-a", 16*1024))
	daemonAFound := strings.HasSuffix(string(daemonData), strings.Repeat("daemon-a", 16*1024))
	clientBFound := strings.HasSuffix(string(clientData), strings.Repeat("client-b", 16*1024))
	daemonBFound := strings.HasSuffix(string(daemonData), strings.Repeat("daemon-b", 16*1024))
	completePair := (clientAFound && daemonAFound) || (clientBFound && daemonBFound)
	if clientErr != nil || daemonErr != nil || !completePair {
		return fmt.Errorf("observed mixed or incomplete supervisor pair: client=%t/%t daemon=%t/%t", clientAFound, clientBFound, daemonAFound, daemonBFound)
	}
	return nil
}

func TestInstallStateSupervisorPairForRecoveryUsesCurrentBundle(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	bundle := supervisorfixture.CanonicalTempDir(t)
	delegate := filepath.Join(bundle, "delegate")
	client := filepath.Join(bundle, "pueue")
	daemon := filepath.Join(bundle, "pueued")
	supervisorfixture.WriteExecutable(t, delegate, "delegate")
	supervisorfixture.WriteExecutable(t, client, "current client")
	supervisorfixture.WriteExecutable(t, daemon, "current daemon")

	if err := InstallStateSupervisorPairForRecovery(context.Background(), root, delegate, task.SupervisorRef{}); err != nil {
		t.Fatal(err)
	}
	requireResolvedSupervisorPair(t, filepath.Join(root, ".supervisor"), "current client", "current daemon")
}

func TestInstallStateSupervisorPairForRecoveryUsesOnlyVerifiedSavedFallback(t *testing.T) {
	t.Run("saved pair matches", func(t *testing.T) {
		root := supervisorfixture.CanonicalTempDir(t)
		client, daemon := writeSavedSupervisorPair(t)
		binding := savedSupervisorPairRef(t, client, daemon)
		missingDelegate := filepath.Join(supervisorfixture.CanonicalTempDir(t), "delegate")
		if err := InstallStateSupervisorPairForRecovery(context.Background(), root, missingDelegate, binding); err != nil {
			t.Fatal(err)
		}
		requireResolvedSupervisorPair(t, filepath.Join(root, ".supervisor"), "saved client", "saved daemon")
	})
	t.Run("saved pair digest differs", func(t *testing.T) {
		root := supervisorfixture.CanonicalTempDir(t)
		client, daemon := writeSavedSupervisorPair(t)
		binding := savedSupervisorPairRef(t, client, daemon)
		binding.DaemonSHA256 = strings.Repeat("a", 64)
		missingDelegate := filepath.Join(supervisorfixture.CanonicalTempDir(t), "delegate")
		if err := InstallStateSupervisorPairForRecovery(context.Background(), root, missingDelegate, binding); !errors.Is(err, pueue.ErrBinding) {
			t.Fatalf("mismatched saved pair error = %v, want binding error", err)
		}
		if _, err := os.Lstat(filepath.Join(root, ".supervisor", stateSupervisorPairRecordName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unverified saved pair was published: %v", err)
		}
	})
}

func TestPrivateRecoveryPrefersSavedHistoricalSupervisorPair(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	savedClientSource, savedDaemonSource := writeSavedSupervisorPair(t)
	if err := installStateSupervisorPair(context.Background(), root, savedClientSource, savedDaemonSource); err != nil {
		t.Fatal(err)
	}
	savedClient, savedDaemon, found, err := ResolveStateSupervisorPair(filepath.Join(root, ".supervisor"))
	if err != nil || !found {
		t.Fatalf("resolve saved state-root pair: found=%t err=%v", found, err)
	}
	saved := savedSupervisorPairRef(t, savedClient, savedDaemon)
	saved.ConfigPath = pueue.PrivateConfigPath(root)
	currentClientSource, currentDaemonSource := writeSavedSupervisorPair(t)
	if err = installStateSupervisorPair(context.Background(), root, currentClientSource, currentDaemonSource); err != nil {
		t.Fatal(err)
	}
	gotClient, gotDaemon, err := ResolveRecoveryPrivateSupervisorExecutables(root, saved, filepath.Join(root, "missing", "delegate"))
	if err != nil {
		t.Fatal(err)
	}
	if gotClient != savedClient || gotDaemon != savedDaemon {
		t.Fatalf("recovery selected current pair %q / %q, want saved pair %q / %q", gotClient, gotDaemon, savedClient, savedDaemon)
	}
}

func TestValidateSavedStateSupervisorPairKeepsExternalExecutablesUnchanged(t *testing.T) {
	client, daemon := writeSavedSupervisorPair(t)
	saved := savedSupervisorPairRef(t, client, daemon)
	if err := ValidateSavedStateSupervisorPair(supervisorfixture.CanonicalTempDir(t), saved); err != nil {
		t.Fatalf("external saved supervisor pair was subjected to state-root validation: %v", err)
	}
}

func TestValidateSavedStateSupervisorPairAcceptsHistoricalGeneration(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	client, daemon := writeSavedSupervisorPair(t)
	if err := installStateSupervisorPair(context.Background(), root, client, daemon); err != nil {
		t.Fatal(err)
	}
	historicalClient, historicalDaemon, found, err := ResolveStateSupervisorPair(filepath.Join(root, ".supervisor"))
	if err != nil || !found {
		t.Fatalf("resolve historical state-root pair: found=%t err=%v", found, err)
	}
	saved := savedSupervisorPairRef(t, historicalClient, historicalDaemon)

	currentClient := filepath.Join(supervisorfixture.CanonicalTempDir(t), "pueue")
	currentDaemon := filepath.Join(supervisorfixture.CanonicalTempDir(t), "pueued")
	supervisorfixture.WriteExecutable(t, currentClient, "current client")
	supervisorfixture.WriteExecutable(t, currentDaemon, "current daemon")
	if err = installStateSupervisorPair(context.Background(), root, currentClient, currentDaemon); err != nil {
		t.Fatal(err)
	}
	if err = ValidateSavedStateSupervisorPair(root, saved); err != nil {
		t.Fatalf("intact historical state-root pair failed validation: %v", err)
	}
}

func TestInstallStateSupervisorPairForRecoveryUsesVerifiedStatePair(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	client, daemon := writeSavedSupervisorPair(t)
	binding := savedSupervisorPairRef(t, client, daemon)
	if err := installStateSupervisorPair(context.Background(), root, client, daemon); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(root, ".supervisor")
	stateClient, stateDaemon, found, err := ResolveStateSupervisorPair(base)
	if err != nil || !found {
		t.Fatalf("resolve installed state-root pair: found=%t err=%v", found, err)
	}
	recordPath := filepath.Join(base, stateSupervisorPairRecordName)
	recordBefore, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(client); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(daemon); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", supervisorfixture.CanonicalTempDir(t))
	missingDelegate := filepath.Join(supervisorfixture.CanonicalTempDir(t), "missing", "delegate")
	if err = InstallStateSupervisorPairForRecovery(context.Background(), root, missingDelegate, binding); err != nil {
		t.Fatalf("recovery rejected the verified state-root pair: %v", err)
	}
	resolvedClient, resolvedDaemon, found, err := ResolveStateSupervisorPair(base)
	if err != nil || !found || resolvedClient != stateClient || resolvedDaemon != stateDaemon {
		t.Fatalf("recovery changed the state-root pair: client=%q daemon=%q found=%t err=%v", resolvedClient, resolvedDaemon, found, err)
	}
	recordAfter, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(recordAfter, recordBefore) {
		t.Fatal("recovery rewrote the verified state-root pair record")
	}
}

func writeSavedSupervisorPair(t *testing.T) (string, string) {
	t.Helper()
	client := filepath.Join(supervisorfixture.CanonicalTempDir(t), "pueue")
	daemon := filepath.Join(supervisorfixture.CanonicalTempDir(t), "pueued")
	supervisorfixture.WriteExecutable(t, client, "saved client")
	supervisorfixture.WriteExecutable(t, daemon, "saved daemon")
	return client, daemon
}

func savedSupervisorPairRef(t *testing.T, client, daemon string) task.SupervisorRef {
	t.Helper()
	clientDigest, err := stateExecutableSourceDigest(client)
	if err != nil {
		t.Fatal(err)
	}
	daemonDigest, err := stateExecutableSourceDigest(daemon)
	if err != nil {
		t.Fatal(err)
	}
	return task.SupervisorRef{
		ClientExecutable: client, ClientSHA256: clientDigest,
		DaemonExecutable: daemon, DaemonSHA256: daemonDigest,
	}
}
