package unit_test

// Concurrency tests for the storage layer.
//
// These exist because of a specific failure: with the default deferred transaction mode, a
// transaction that reads before it writes discovers the write lock too late and SQLite answers
// SQLITE_BUSY without waiting — which under a parallel test suite (or a real server with two
// active users) surfaces as 500s on ordinary joins and invites. A unit test for a locking bug
// has to be a race, so these drive many writers and readers at once and assert that none of
// them sees a lock error.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/enimatek-nl/qwatrix/internal/store"
)

// TestStoreSurvivesConcurrentWriters runs writers and readers at the same database and insists
// that every operation succeeds.
//
// The failure this guards is a lock error rather than a wrong answer: correctness under one
// writer is already covered by every other test, while "database is locked" only appears when
// several requests overlap, which is exactly what a homeserver does and what a compliance
// suite does hardest.
func TestStoreSurvivesConcurrentWriters(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "concurrent.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}

	const (
		writers = 8
		readers = 8
		rounds  = 40
	)

	var (
		wg       sync.WaitGroup
		lockFail atomic.Int64
		otherErr atomic.Int64
		written  atomic.Int64
	)
	report := func(err error) {
		if err == nil {
			return
		}
		if isLockError(err) {
			lockFail.Add(1)
			t.Logf("lock failure: %v", err)
			return
		}
		otherErr.Add(1)
		t.Logf("unexpected error: %v", err)
	}

	ctx := context.Background()
	start := make(chan struct{})

	for worker := 0; worker < writers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-start
			for round := 0; round < rounds; round++ {
				localpart := fmt.Sprintf("writer%d-%d", worker, round)
				err := st.Do(ctx, func(ctx context.Context, tx store.Tx) error {
					if err := st.Users.Create(ctx, &store.User{
						Localpart:    localpart,
						UserID:       "@" + localpart + ":test.local",
						PasswordHash: "x",
					}); err != nil {
						return err
					}
					// A read inside the same transaction is what makes a deferred
					// transaction upgrade its lock, which is the case that fails.
					if _, err := st.Users.Get(ctx, localpart); err != nil {
						return err
					}
					_, err := st.Events.Insert(ctx, tx, &store.EventRow{
						EventID:        fmt.Sprintf("$%d-%d", worker, round),
						RoomID:         "!concurrent:test.local",
						Type:           "m.room.message",
						JSON:           "{}",
						RoomVersion:    "11",
						Sender:         "@" + localpart + ":test.local",
						ReceivedMS:     time.Now().UnixMilli(),
						OriginServerTS: time.Now().UnixMilli(),
					})
					return err
				})
				if err != nil {
					report(err)
					continue
				}
				written.Add(1)
			}
		}(worker)
	}

	for worker := 0; worker < readers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for round := 0; round < rounds*2; round++ {
				// Reads run on their own connections so that a writer has to contend with
				// live readers, the way WAL mode is meant to allow.
				if _, err := st.Users.List(ctx, 50, ""); err != nil {
					report(err)
				}
				if _, err := st.Events.MaxPosition(ctx); err != nil {
					report(err)
				}
			}
		}()
	}

	close(start)
	wg.Wait()

	if got := written.Load(); got != writers*rounds {
		t.Errorf("%d of %d write transactions succeeded", got, writers*rounds)
	}
	if got := lockFail.Load(); got > 0 {
		t.Errorf("%d operation(s) failed with a lock error; the database must wait rather than refuse", got)
	}
	if got := otherErr.Load(); got > 0 {
		t.Errorf("%d operation(s) failed unexpectedly", got)
	}
}

// TestStoreTransactionIsSerialisedButNeverDeadlocked checks that the write lock is held for
// the transaction and not for the caller's whole lifetime: a transaction that starts another
// write through the same store is a programming error we want to surface, while an unrelated
// reader must keep working while a transaction is open.
func TestStoreTransactionKeepsReadersWorking(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "readers.db"))
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	if err := st.Users.Create(context.Background(), &store.User{
		Localpart: "host", UserID: "@host:test.local", PasswordHash: "x",
	}); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	go func() {
		// A write transaction that stays open while another connection reads.
		_ = st.Do(context.Background(), func(ctx context.Context, tx store.Tx) error {
			close(entered)
			<-release
			return st.Users.Update(ctx, "host", map[string]any{"display_name": "updated"})
		})
	}()

	<-entered
	readDone := make(chan error, 1)
	go func() {
		_, err := st.Users.Get(context.Background(), "host")
		readDone <- err
	}()

	select {
	case err := <-readDone:
		if err != nil {
			t.Errorf("reading while a write transaction was open: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("a read blocked behind an open write transaction; WAL should let it through")
	}
	close(release)
}

// isLockError reports whether an error is SQLite refusing a lock.
func isLockError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "database is locked") ||
		strings.Contains(text, "database table is locked") ||
		strings.Contains(text, "sqlite_busy") ||
		strings.Contains(text, "deadlock")
}
