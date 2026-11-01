package teams

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestConcurrentCatalogInitializationAndSeparateTeams(t *testing.T) {
	catalog := testCatalog(t)
	const writers = 8
	results := make(chan error, writers)
	ids := make(chan string, writers)
	for i := range writers {
		go func() {
			ref := userRef(fmt.Sprintf("team-%d", i))
			_, id, err := New(catalog.path, nil).Register(t.Context(), ref, testTeam(ref.TeamID))
			ids <- id
			results <- err
		}()
	}
	for range writers {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	var identity string
	for range writers {
		id := <-ids
		if identity == "" {
			identity = id
		}
		if id != identity {
			t.Errorf("initializers returned different identities: %q %q", identity, id)
		}
	}
	listing, err := catalog.List(t.Context(), nil)
	if err != nil || len(listing.Records) != writers || listing.CatalogID != identity {
		t.Fatalf("lost independent writes: %+v %v", listing, err)
	}
}

func TestConcurrentCompareAndSwapHasOneWinner(t *testing.T) {
	catalog := testCatalog(t)
	ref := userRef("review")
	registerTeam(t, catalog, ref)
	const writers = 8
	results := make(chan error, writers)
	for i := range writers {
		go func() {
			definition := testTeam(ref.TeamID)
			definition.Name = fmt.Sprintf("Writer %d", i)
			_, _, err := New(catalog.path, nil).Update(t.Context(), ref, 1, definition)
			results <- err
		}()
	}
	winners, conflicts := 0, 0
	for range writers {
		err := <-results
		switch {
		case err == nil:
			winners++
		case errors.Is(err, ErrConflict):
			conflicts++
		default:
			t.Error(err)
		}
	}
	if winners != 1 || conflicts != writers-1 {
		t.Fatalf("CAS winners=%d conflicts=%d", winners, conflicts)
	}
	if got, _, err := catalog.Lookup(t.Context(), ref); err != nil || got.Revision != 2 {
		t.Fatalf("final CAS revision: %+v %v", got, err)
	}
}

func TestConcurrentIndependentTeamUpdatesDoNotConflict(t *testing.T) {
	catalog := testCatalog(t)
	const writers = 4
	for i := range writers {
		registerTeam(t, catalog, userRef(fmt.Sprintf("team-%d", i)))
	}
	results := make(chan error, writers)
	for i := range writers {
		go func() {
			ref := userRef(fmt.Sprintf("team-%d", i))
			definition := testTeam(ref.TeamID)
			definition.Name = "Updated independently"
			_, _, err := New(catalog.path, nil).Update(t.Context(), ref, 1, definition)
			results <- err
		}()
	}
	for range writers {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	listing, err := catalog.List(t.Context(), nil)
	if err != nil || len(listing.Records) != writers {
		t.Fatalf("independent update listing: %+v %v", listing, err)
	}
	for _, record := range listing.Records {
		if record.Revision != 2 || record.Definition.Name != "Updated independently" {
			t.Errorf("independent update lost: %+v", record)
		}
	}
}

func TestWriterCancellationWhileAnotherTransactionOwnsLock(t *testing.T) {
	catalog := testCatalog(t)
	ref := userRef("review")
	registerTeam(t, catalog, ref)
	err := catalog.transact(t.Context(), catalogWrite, func(ctx context.Context, _ *sql.Tx, _ string) error {
		waiting, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		_, _, err := New(catalog.path, nil).Update(waiting, ref, 1, testTeam(ref.TeamID))
		if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrUnavailable) {
			t.Errorf("canceled writer did not surface deadline: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _, err := catalog.Lookup(t.Context(), ref); err != nil || got.Revision != 1 {
		t.Fatalf("canceled writer changed data: %+v %v", got, err)
	}
}

func TestBlockedCommitIsExplicitAndLeavesOriginalData(t *testing.T) {
	catalog := testCatalog(t)
	ref := userRef("review")
	registerTeam(t, catalog, ref)
	err := catalog.transact(t.Context(), catalogRead, func(ctx context.Context, _ *sql.Tx, _ string) error {
		waiting, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		definition := testTeam(ref.TeamID)
		definition.Name = "Must not persist"
		_, _, err := New(catalog.path, nil).Update(waiting, ref, 1, definition)
		if !errors.Is(err, ErrUnavailable) {
			t.Errorf("blocked commit was not reported: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := catalog.Lookup(t.Context(), task.TeamReference{Scope: "user", TeamID: "review"})
	if err != nil || got.Revision != 1 || got.Definition.Name != "Review team" {
		t.Fatalf("failed commit changed data: %+v %v", got, err)
	}
}
