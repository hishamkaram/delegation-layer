package infrastructure

import (
	"context"
	"errors"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestWarmOwnerOtherRootUnknownContactNeverFallsBack(t *testing.T) {
	b := newOwnerContactFixture(t)
	selected := warmOwnerContactRequestContext(t, b, runtime.GOOS)
	a := newOwnerServerFixture(t, &ownerServerTestHost{})
	work := warmOtherWork(t, a, b.auth.intent, selected, b.auth.intent.Inputs.Artifacts)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	before := producerBudgetTree(t, filepath.Join(b.journal.pin.Root(), "infrastructure"))
	var received atomic.Int32
	join := b.serve(t, func(_ *net.UnixConn, request ownerRequest) error {
		received.Add(1)
		if request.Action != ownerEnsureOwn || request.Context != work.request.Context {
			return ErrIdentity
		}
		return nil // Lost reply after receipt is an unknown outcome.
	})
	established, err := a.server.prepareOtherRoot(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := RequestBoundPreparation(ctx, b.reader, established.Owner, PreparationRequest{Context: work.request.Context})
	if err == nil || established.Owner != b.auth.request.Owner {
		t.Fatalf("unknown owner contact became success/election: ref=%v disposition=%v err=%v", established.Owner, outcome.Disposition, err)
	}
	requireOwnerContactError(t, err)
	if joinErr := join(); joinErr != nil {
		t.Fatal(joinErr)
	}
	if received.Load() != 1 || !maps.Equal(before, producerBudgetTree(t, filepath.Join(b.journal.pin.Root(), "infrastructure"))) {
		t.Fatal("unknown contact replayed or changed durable startup evidence")
	}
	requireNoOwnerContactConnection(t, b.auth.listener)
}

// A request selects native context independently of the immutable historical
// birth fixture. A foreign birth reference must never become a native request.
func warmOwnerContactRequestContext(t *testing.T, b ownerContactFixture, selectedOS string) LaunchContextReference {
	t.Helper()
	selected := contextForJournal(t, b.journal)
	selected.OS = selectedOS
	reference, _, err := b.journal.stageContext(t.Context(), selected)
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := readLaunchContext(t.Context(), b.reader, reference)
	if err != nil {
		t.Fatal(err)
	}
	if recorded.OS != selectedOS {
		t.Fatalf("request context OS=%q want=%q", recorded.OS, selectedOS)
	}
	if err = reference.ValidateContext(recorded, b.journal.pin.Root(), b.journal.pin.RootID()); err != nil {
		t.Fatal(err)
	}
	return reference
}

func TestWarmOwnerOtherRootForeignSelectedContextNeverContacts(t *testing.T) {
	b := newOwnerContactFixture(t)
	foreignOS := "darwin"
	if runtime.GOOS == foreignOS {
		foreignOS = "linux"
	}
	if foreignOS == runtime.GOOS {
		t.Fatal("foreign request fixture selected the native OS")
	}
	selected := warmOwnerContactRequestContext(t, b, foreignOS)
	a := newOwnerServerFixture(t, &ownerServerTestHost{})
	work := warmOtherWork(t, a, b.auth.intent, selected, b.auth.intent.Inputs.Artifacts)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	before := producerBudgetTree(t, filepath.Join(b.journal.pin.Root(), "infrastructure"))
	established, err := a.server.prepareOtherRoot(ctx, work)
	if !errors.Is(err, ErrIdentity) || established != (EstablishedOwner{}) {
		t.Fatalf("foreign selected context reached warm contact/election: owner=%v err=%v", established, err)
	}
	requireNoOwnerContactConnection(t, b.auth.listener)
	if !maps.Equal(before, producerBudgetTree(t, filepath.Join(b.journal.pin.Root(), "infrastructure"))) {
		t.Fatal("foreign selected context changed B startup evidence")
	}
}

func warmOtherWork(t *testing.T, a *ownerServerFixture, intent StartupIntent, selected LaunchContextReference, artifacts Artifacts) acceptedOwnerWork {
	t.Helper()
	request := a.request(t, ownerEnsureOther)
	request.Target = originRecord{Root: intent.Inputs.Root, RootID: intent.Inputs.RootID}
	request.Context = selected
	return acceptedOwnerWork{request: request, operation: a.entry.operation}
}

func TestWarmOwnerOtherRootNativeAuthenticationFailureNeverFallsBack(t *testing.T) {
	b := warmOwnerWrongUIDFixture(t)
	a := newOwnerServerFixture(t, &ownerServerTestHost{})
	work := warmOtherWork(t, a, b.auth.intent, b.auth.intent.Inputs.Context, b.auth.intent.Inputs.Artifacts)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	before := producerBudgetTree(t, filepath.Join(b.journal.pin.Root(), "infrastructure"))
	join := warmOwnerNoRequestBytes(t, b.auth.listener)
	established, err := a.server.prepareOtherRoot(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := RequestBoundPreparation(ctx, b.reader, established.Owner, PreparationRequest{Context: work.request.Context})
	if !errors.Is(err, ErrIdentity) || established.Owner != b.auth.request.Owner {
		t.Fatalf("native authentication failure became success/election: ref=%v disposition=%v err=%v", established.Owner, outcome.Disposition, err)
	}
	requireOwnerContactError(t, err)
	join()
	if !maps.Equal(before, producerBudgetTree(t, filepath.Join(b.journal.pin.Root(), "infrastructure"))) {
		t.Fatal("native authentication failure changed startup evidence")
	}
	requireNoOwnerContactConnection(t, b.auth.listener)
}

func warmOwnerWrongUIDFixture(t *testing.T) ownerContactFixture {
	t.Helper()
	auth := newOwnerAuthFixture(t)
	if err := errors.Join(auth.client.Close(), auth.server.Close()); err != nil {
		t.Fatal(err)
	}
	journal, err := openIntentJournal(t.Context(), auth.store, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := journal.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	lease, acquired, err := journal.tryAcquire(t.Context())
	if lease != nil {
		t.Cleanup(func() {
			if closeErr := lease.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
	if err != nil || !acquired {
		t.Fatalf("authentication fixture lease: %t %v", acquired, err)
	}
	selected := contextForJournal(t, journal)
	selected.OS = runtime.GOOS
	reference, _, err := journal.stageContext(t.Context(), selected)
	if err != nil {
		t.Fatal(err)
	}
	auth.intent.Inputs.Context = reference
	auth.assumption.UID++
	auth.rebind(t)
	requireJournalPut(t, journal, lease, auth.intent)
	// Shape-valid historical evidence must never defeat native peer UID checks.
	writeOwnerEvidenceFixture(t, journal, assumptionName(auth.intent.Sequence), auth.assumption)
	if _, err = journal.putPublication(t.Context(), lease, auth.publication); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(auth.publication.Endpoint.Path, 0o600); err != nil {
		t.Fatal(err)
	}
	return ownerContactFixture{auth: auth, journal: journal, lease: lease, reader: contextPassiveReader(t, journal)}
}

func warmOwnerNoRequestBytes(t *testing.T, listener *net.UnixListener) func() {
	t.Helper()
	done := make(chan error, 1)
	joined := false
	join := func() {
		if !joined {
			if err := <-done; err != nil {
				t.Error(err)
			}
			joined = true
		}
	}
	t.Cleanup(join)
	go func() {
		connection, err := listener.AcceptUnix()
		if err != nil {
			done <- err
			return
		}
		if err = connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			done <- errors.Join(err, connection.Close())
			return
		}
		var data [1]byte
		n, readErr := connection.Read(data[:])
		if n != 0 {
			readErr = errors.New("unauthenticated peer received request bytes")
		} else if errors.Is(readErr, io.EOF) {
			readErr = nil
		}
		done <- errors.Join(readErr, connection.Close())
	}()
	return join
}
