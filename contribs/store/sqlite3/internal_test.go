package sqlite3

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sylr/fix2/fix/session/store"
)

func open(t *testing.T) (*Store, store.State) {
	t.Helper()
	return openFile(t, filepath.Join(t.TempDir(), "fix2.db"))
}

func openFile(t *testing.T, path string) (*Store, store.State) {
	t.Helper()
	s, err := OpenFile(path, "S")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	st, err := s.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s, st
}

// Once a commit or a flush failed, every call fails: a later flush may
// succeed without the earlier writes having reached the disk.
func TestFailureSticks(t *testing.T) {
	ctx := context.Background()
	s, st := open(t)
	g := st.Generation
	if err := s.Append(ctx, g, 1, []byte("m")); err != nil {
		t.Fatal(err)
	}
	_ = s.fail(fmt.Errorf("%w: fsync: input/output error", store.ErrUnknownOutcome))
	for name, err := range map[string]error{
		"Append":        s.Append(ctx, g, 2, []byte("m")),
		"SetNextTarget": s.SetNextTarget(ctx, g, 2),
		"SetNextSender": s.SetNextSender(ctx, g, 5),
		"Truncate":      s.Truncate(ctx, g, 2),
		"Reset":         s.Reset(ctx, g, st.CreatedAt, ""),
		"Sync":          s.Sync(ctx, g, 1),
		"Range":         s.Range(ctx, g, 1, 1, func(uint64, []byte, store.Hole) error { return nil }),
		"Open":          func() error { _, err := s.Open(ctx); return err }(),
	} {
		if !errors.Is(err, store.ErrUnknownOutcome) {
			t.Errorf("%s after a failure: err = %v, want ErrUnknownOutcome", name, err)
		}
	}
}

// A commit that fails, a flush aside, is reported once: the next call
// carries on from what the database holds.
func TestCommitFailureSettles(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "fix2.db")+"?_foreign_keys=1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := New(db, "S")
	defer s.Close()
	st, err := s.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A foreign key checked when committing fails the commits that set
	// NextTarget to 13 or store message 2.
	for _, q := range []string{
		"CREATE TABLE parent (id INTEGER PRIMARY KEY)",
		"CREATE TABLE child (p INTEGER REFERENCES parent (id) DEFERRABLE INITIALLY DEFERRED)",
		"CREATE TRIGGER fail_target AFTER UPDATE OF next_target ON fix2_sessions WHEN NEW.next_target = 13 BEGIN INSERT INTO child VALUES (1); END",
		"CREATE TRIGGER fail_append AFTER INSERT ON fix2_messages WHEN NEW.seq = 2 AND NEW.raw = CAST('bad' AS BLOB) BEGIN INSERT INTO child VALUES (1); END",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	g := st.Generation
	stamp := func() (n int) {
		t.Helper()
		if err := db.QueryRow("SELECT stamp FROM fix2_sessions").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := stamp()
	if err := s.SetNextTarget(ctx, g, 13); !errors.Is(err, store.ErrUnknownOutcome) {
		t.Fatalf("SetNextTarget failing to commit: err = %v, want ErrUnknownOutcome", err)
	}
	// Settling wrote: a commit SQLite logged but could not index is
	// overwritten, rather than left to come back after a crash.
	if after := stamp(); after != before+1 {
		t.Errorf("stamp %d -> %d after a failed commit: settling wrote nothing", before, after)
	}
	if err := s.SetNextTarget(ctx, g, 14); err != nil {
		t.Errorf("SetNextTarget after one failed to commit: %v", err)
	}
	if err := s.Append(ctx, g, 1, []byte("m1")); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, g, 2, []byte("bad")); !errors.Is(err, store.ErrUnknownOutcome) {
		t.Fatalf("Append failing to commit: err = %v, want ErrUnknownOutcome", err)
	}
	// It was rolled back: 2 is still the next.
	if err := s.Append(ctx, g, 2, []byte("m2")); err != nil {
		t.Errorf("Append after one failed to commit: %v", err)
	}
	if err := s.Sync(ctx, g, 2); err != nil {
		t.Errorf("Sync after a commit failed: %v", err)
	}
	if st, _ := s.Open(ctx); st.NextTarget != 14 || st.NextSender != 3 {
		t.Errorf("state = %+v, want NextTarget 14 and NextSender 3", st)
	}
}

// The store checkpoints the write-ahead log, which starts over.
func TestCheckpoint(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fix2.db")
	s, st := openFile(t, path)
	before := walCheckpoints(t, path)
	raw := make([]byte, 200)
	for seq := uint64(1); seq <= checkpointEvery+1; seq++ {
		if seq == checkpointEvery+1 {
			// The checkpoint over, this write copies the rest and starts
			// the log over.
			s.background.Wait()
		}
		if err := s.Append(ctx, st.Generation, seq, raw); err != nil {
			t.Fatal(err)
		}
	}
	if walCheckpoints(t, path) == before {
		t.Errorf("the write-ahead log did not start over after %d messages", checkpointEvery+1)
	}
}

// A Range reads in short transactions, a chunk at a time: the checkpoints
// that run while its callback waits complete, and the write-ahead log
// starts over. A Range in one transaction would hold every checkpoint back;
// begun with the log copied whole, reading the database file, it would let
// the log start over once, at the first write.
func TestRangeLetsCheckpointsRun(t *testing.T) {
	for _, checkpointed := range []bool{false, true} {
		t.Run(fmt.Sprintf("checkpointed=%v", checkpointed), func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "fix2.db")
			s, st := openFile(t, path)
			raw := make([]byte, 200)
			next := uint64(1)
			write := func() error {
				err := s.Append(ctx, st.Generation, next, raw)
				next++
				return err
			}
			const n = 6000
			for range n {
				if err := write(); err != nil {
					t.Fatal(err)
				}
			}
			// The log copied whole, a Range in one transaction would read
			// the database file; a commit more, the log.
			s.background.Wait()
			if checkpointed {
				if _, err := s.conn.ExecContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)"); err != nil {
					t.Fatal(err)
				}
			} else if err := write(); err != nil { // one the database file lacks
				t.Fatal(err)
			}
			// A resend of n messages, during which as many are sent.
			var before uint32
			startedOver := false
			err := s.Range(ctx, st.Generation, 1, n, func(seq uint64, _ []byte, _ store.Hole) error {
				if err := write(); err != nil {
					return err
				}
				switch seq {
				case 1:
					before = walCheckpoints(t, path)
				case n:
					// The last checkpoint over, the next write copies the
					// rest and starts the log over, unless a reader holds it
					// back.
					s.background.Wait()
					if err := write(); err != nil {
						return err
					}
					startedOver = walCheckpoints(t, path) != before
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if !startedOver {
				t.Errorf("the write-ahead log did not start over during a Range, %d messages written meanwhile", n)
			}
		})
	}
}

// walCheckpoints returns the checkpoint sequence number in the header of
// the write-ahead log of the database at path: how many times the log
// started over, once checkpointed whole. The log's size is no measure of
// that: it is reused, not truncated, and grows by every commit made while a
// checkpoint runs, as long as the disk makes it.
func walCheckpoints(t *testing.T, path string) uint32 {
	t.Helper()
	f, err := os.Open(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var hdr [16]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		t.Fatal(err)
	}
	return binary.BigEndian.Uint32(hdr[12:])
}

// Checkpoints flush, whatever the DSN says.
func TestCheckpointConnFlushes(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "fix2.db")+"?_sync=OFF")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := New(db, "S")
	defer s.Close()
	if _, err := s.Open(ctx); err != nil {
		t.Fatal(err)
	}
	conn, err := s.checkpointConn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer discard(conn)
	var mode int
	if err := conn.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode == 0 {
		t.Error("the checkpoint's connection does not flush")
	}
}

// A Range callback may close the store while a checkpoint runs, or has just
// run: the Range holds no connection then, which the checkpoint takes.
func TestCloseFromRangeDuringCheckpoint(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "fix2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(2) // the writer's, and the Range's or the checkpoint's
	s := New(db, "S")
	st, err := s.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, st.Generation, 1, []byte("m")); err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	err = s.Range(ctx, st.Generation, 1, 1, func(uint64, []byte, store.Hole) error {
		// A checkpoint starts every checkpointEvery commits; it may be
		// over before it is seen running.
		for seq := uint64(2); seq <= 3*checkpointEvery && !s.checkpointing.Load() && !s.catchUp.Load(); seq++ {
			if err := s.Append(ctx, st.Generation, seq, []byte("m")); err != nil {
				return err
			}
		}
		if !s.checkpointing.Load() && !s.catchUp.Load() {
			t.Fatal("no checkpoint ran")
		}
		go func() { closed <- s.Close() }()
		select {
		case err := <-closed:
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("Close waits for a checkpoint that waits for the Range")
		}
		return nil
	})
	if !errors.Is(err, store.ErrClosed) {
		t.Errorf("Range closed by its callback: err = %v, want ErrClosed", err)
	}
}

// Sync flushes whatever was committed, not only messages.
func TestSyncFlushesEveryCommit(t *testing.T) {
	ctx := context.Background()
	s, st := open(t)
	if err := s.Sync(ctx, st.Generation, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNextTarget(ctx, st.Generation, 7); err != nil {
		t.Fatal(err)
	}
	if !s.dirty {
		t.Fatal("SetNextTarget left nothing to flush")
	}
	if err := s.Sync(ctx, st.Generation, 0); err != nil {
		t.Fatal(err)
	}
	if s.dirty {
		t.Error("Sync left SetNextTarget's commit unflushed")
	}
}

// A Range waits neither for a write nor for a flush: holding the writer's
// lock, as a flush that hangs does, holds no Range back.
func TestRangeWaitsForNoWrite(t *testing.T) {
	ctx := context.Background()
	s, st := open(t)
	if err := s.Append(ctx, st.Generation, 1, []byte("m1")); err != nil {
		t.Fatal(err)
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	done := make(chan error, 1)
	go func() {
		done <- s.Range(ctx, st.Generation, 1, 1, func(uint64, []byte, store.Hole) error { return nil })
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Range waited for the writer's lock")
	}
}

// A Range waiting for a Truncate gives up when its ctx is done.
func TestRangeCancelledWaitingForTruncate(t *testing.T) {
	s, st := open(t)
	s.tmu <- struct{}{} // a Truncate running
	defer func() { <-s.tmu }()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := s.Range(ctx, st.Generation, 1, 1, func(uint64, []byte, store.Hole) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Range: %v, want %v", err, context.DeadlineExceeded)
	}
}

// After a commit whose outcome is unknown, the store does not take as
// deleted what it cannot know was: a Reset may have brought first back to 1.
func TestSettleForgetsDeleted(t *testing.T) {
	s, _ := open(t)
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.deleted = 1001 // as after truncating generation 1 before 1001
	if err := s.settle(); err != nil {
		t.Fatal(err)
	}
	if s.deleted > s.first {
		t.Errorf("deleted = %d past first = %d", s.deleted, s.first)
	}
}
