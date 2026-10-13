package sqlite3_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sylr/fix2/contribs/store/sqlite3"
	"github.com/sylr/fix2/session/store"
	"github.com/sylr/fix2/session/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		s, err := sqlite3.OpenFile(filepath.Join(t.TempDir(), "fix2.db"), "S")
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

// Sessions sharing a database do not see each other's messages.
func TestConformanceSharedDatabase(t *testing.T) {
	db := openDB(t, filepath.Join(t.TempDir(), "fix2.db"))
	n := 0
	storetest.Run(t, func(*testing.T) store.Store {
		n++
		return sqlite3.New(db, fmt.Sprintf("session %d", n))
	})
}

// A caller's DSN asking for immediate transactions does not turn Range's
// reads into writes, which would hold back those its callbacks make.
func TestConformanceTxLock(t *testing.T) {
	db := openDB(t, "file:"+filepath.Join(t.TempDir(), "fix2.db")+"?_txlock=immediate")
	n := 0
	storetest.Run(t, func(*testing.T) store.Store {
		n++
		return sqlite3.New(db, fmt.Sprintf("session %d", n))
	})
}

// A pool of one connection would leave Range waiting for the writer's.
func TestOneConnection(t *testing.T) {
	db := openDB(t, filepath.Join(t.TempDir(), "fix2.db"))
	db.SetMaxOpenConns(1)
	if _, err := sqlite3.New(db, "S").Open(context.Background()); err == nil {
		t.Error("Open on a pool of one connection succeeded")
	}
}

// The writer's connection, and its pragmas, do not go back to the caller's
// pool.
func TestCloseDiscardsWriter(t *testing.T) {
	ctx := context.Background()
	db := openDB(t, filepath.Join(t.TempDir(), "fix2.db"))
	s := sqlite3.New(db, "S")
	if _, err := s.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if n := db.Stats().OpenConnections; n != 0 {
		t.Errorf("%d connections left open", n)
	}
	var autocheckpoint int
	if err := db.QueryRow("PRAGMA wal_autocheckpoint").Scan(&autocheckpoint); err != nil {
		t.Fatal(err)
	}
	if autocheckpoint == 0 {
		t.Error("a connection of the pool does not checkpoint: the writer's came back")
	}
}

// The store checkpoints the write-ahead log, which stays bounded.
func TestCheckpoint(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fix2.db")
	s, st := open(t, path, "S")
	raw := make([]byte, 200)
	for seq := uint64(1); seq <= 6000; seq++ {
		if err := s.Append(ctx, st.Generation, seq, raw); err != nil {
			t.Fatal(err)
		}
	}
	fi, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	// Some 12KB per commit, were it never checkpointed: 70MB.
	t.Logf("write-ahead log of %.1f MB", float64(fi.Size())/(1<<20))
	if fi.Size() > 40<<20 {
		t.Errorf("write-ahead log of %d MB after 6000 messages", fi.Size()>>20)
	}
}

func openDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func open(t testing.TB, path, key string) (*sqlite3.Store, store.State) {
	t.Helper()
	s, err := sqlite3.OpenFile(path, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	st, err := s.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s, st
}

type record struct {
	seq  uint64
	raw  string
	hole store.Hole
}

func collect(t *testing.T, s store.Store, gen, begin, end uint64) []record {
	t.Helper()
	var out []record
	err := s.Range(context.Background(), gen, begin, end, func(seq uint64, raw []byte, hole store.Hole) error {
		out = append(out, record{seq, string(raw), hole})
		return nil
	})
	if err != nil {
		t.Fatalf("Range %d..%d: %v", begin, end, err)
	}
	return out
}

func checkRecords(t *testing.T, got, want []record) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("Range = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Range[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// What a store wrote is there once the database is opened again.
func TestReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fix2.db")
	s, st := open(t, path, "S")
	g := st.Generation
	for seq := uint64(1); seq <= 6; seq++ {
		if err := s.Append(ctx, g, seq, fmt.Appendf(nil, "m%d", seq)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetNextSender(ctx, g, 10); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, g, 10, []byte("m10")); err != nil {
		t.Fatal(err)
	}
	if err := s.Truncate(ctx, g, 3); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNextTarget(ctx, g, 42); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(ctx, g, 10); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, st2 := open(t, path, "S")
	if st2.Generation != g || st2.NextSender != 11 || st2.NextTarget != 42 || !st2.CreatedAt.Equal(st.CreatedAt) {
		t.Errorf("state reopened = %+v, want generation %d, NextSender 11, NextTarget 42, CreatedAt %v", st2, g, st.CreatedAt)
	}
	checkRecords(t, collect(t, s, g, 1, 10), []record{
		{1, "", store.HoleRetention}, {2, "", store.HoleRetention},
		{3, "m3", store.HoleNone}, {4, "m4", store.HoleNone}, {5, "m5", store.HoleNone}, {6, "m6", store.HoleNone},
		{7, "", store.HoleNotStored}, {8, "", store.HoleNotStored}, {9, "", store.HoleNotStored},
		{10, "m10", store.HoleNone},
	})
	// Retries of Appends made before the restart still succeed.
	if err := s.Append(ctx, g, 1, []byte("m1")); err != nil {
		t.Errorf("retrying a truncated Append: %v", err)
	}
	if err := s.Append(ctx, g, 4, []byte("m4")); err != nil {
		t.Errorf("retrying an Append: %v", err)
	}
	if err := s.Append(ctx, g, 8, []byte("m8")); !errors.Is(err, store.ErrSeqMismatch) {
		t.Errorf("Append over a skipped number: err = %v, want ErrSeqMismatch", err)
	}

	at := time.Date(2026, 10, 12, 17, 0, 0, 0, time.FixedZone("CEST", 2*3600))
	if err := s.Reset(ctx, g, at, "w2"); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, g+1, 1, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, st3 := open(t, path, "S")
	if st3.Generation != g+1 || st3.NextSender != 2 || st3.NextTarget != 1 || !st3.CreatedAt.Equal(at) || st3.WindowID != "w2" {
		t.Errorf("state reopened after a Reset = %+v", st3)
	}
	checkRecords(t, collect(t, s, g+1, 1, 10), []record{{1, "new", store.HoleNone}})
}

// A message gone from the database is reported missing, not gap-filled.
func TestMissing(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fix2.db")
	s, st := open(t, path, "S")
	for seq := uint64(1); seq <= 3; seq++ {
		if err := s.Append(ctx, st.Generation, seq, []byte("m")); err != nil {
			t.Fatal(err)
		}
	}
	db := openDB(t, path)
	if _, err := db.Exec("DELETE FROM fix2_messages WHERE seq = 2"); err != nil {
		t.Fatal(err)
	}
	checkRecords(t, collect(t, s, st.Generation, 1, 3), []record{
		{1, "m", store.HoleNone}, {2, "", store.HoleMissing}, {3, "m", store.HoleNone},
	})
}

// Two stores writing the same session conflict rather than interleave.
func TestSecondWriter(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fix2.db")
	a, st := open(t, path, "S")
	b, _ := open(t, path, "S")
	if err := a.Append(ctx, st.Generation, 1, []byte("from a")); err != nil {
		t.Fatal(err)
	}
	if err := b.Append(ctx, st.Generation, 1, []byte("from b")); !errors.Is(err, store.ErrSeqMismatch) {
		t.Errorf("second writer's Append: err = %v, want ErrSeqMismatch", err)
	}
	// Changes leaving NextSender as it was are caught too.
	a2, st2 := open(t, path, "T")
	for seq := uint64(1); seq <= 10; seq++ {
		if err := a2.Append(ctx, st2.Generation, seq, []byte("m")); err != nil {
			t.Fatal(err)
		}
	}
	b2, _ := open(t, path, "T")
	if err := a2.Truncate(ctx, st2.Generation, 8); err != nil {
		t.Fatal(err)
	}
	if err := b2.Truncate(ctx, st2.Generation, 4); !errors.Is(err, store.ErrSeqMismatch) {
		t.Errorf("second writer's Truncate moving first_seq back: err = %v, want ErrSeqMismatch", err)
	}
	if err := a2.SetNextTarget(ctx, st2.Generation, 100); err != nil {
		t.Fatal(err)
	}
	if err := b2.SetNextTarget(ctx, st2.Generation, 2); !errors.Is(err, store.ErrSeqMismatch) {
		t.Errorf("second writer's SetNextTarget moving it back: err = %v, want ErrSeqMismatch", err)
	}
	if err := a.Reset(ctx, st.Generation, time.Now(), ""); err != nil {
		t.Fatal(err)
	}
	if err := b.SetNextTarget(ctx, st.Generation, 2); !errors.Is(err, store.ErrSeqMismatch) {
		t.Errorf("second writer's SetNextTarget in a generation reset: err = %v, want ErrSeqMismatch", err)
	}
	// Another session in the same file is independent.
	c, stc := open(t, path, "other")
	if err := c.Append(ctx, stc.Generation, 1, []byte("from c")); err != nil {
		t.Errorf("Append of another session: %v", err)
	}
}

func TestSync(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fix2.db")
	s, st := open(t, path, "S")
	g := st.Generation
	if err := s.Sync(ctx, g, 0); err != nil {
		t.Errorf("Sync with nothing stored: %v", err)
	}
	for seq := uint64(1); seq <= 3; seq++ {
		if err := s.Append(ctx, g, seq, []byte("m")); err != nil {
			t.Fatal(err)
		}
	}
	db := openDB(t, path)
	flushes := func() (n int) {
		t.Helper()
		if err := db.QueryRow("SELECT stamp FROM fix2_sessions WHERE session_key = 'S'").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Every flush commits a change: SQLite writes nothing for a commit that
	// changes nothing, and flushes nothing.
	flushed := func(what string) {
		t.Helper()
		before := flushes()
		if err := s.Sync(ctx, g, 3); err != nil {
			t.Fatal(err)
		}
		if after := flushes(); after != before+1 {
			t.Errorf("Sync after %s: flushes %d -> %d, the flush wrote nothing", what, before, after)
		}
	}
	flushed("messages")
	if err := s.SetNextTarget(ctx, g, 9); err != nil {
		t.Fatal(err)
	}
	flushed("SetNextTarget alone")
	if before := flushes(); s.Sync(ctx, g, 3) != nil || flushes() != before {
		t.Error("Sync with nothing committed since the last one flushed again")
	}
	if err := s.Sync(ctx, g+1, 3); !errors.Is(err, store.ErrGenerationGone) {
		t.Errorf("Sync of another generation: err = %v, want ErrGenerationGone", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.Append(cctx, g, 4, []byte("m")); !errors.Is(err, context.Canceled) {
		t.Errorf("Append with its context done: err = %v, want context.Canceled", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(ctx, g, 3); !errors.Is(err, store.ErrClosed) {
		t.Errorf("Sync after Close: err = %v, want ErrClosed", err)
	}
}

// A nil message is an empty one, as memstore has it.
func TestAppendNil(t *testing.T) {
	ctx := context.Background()
	s, st := open(t, filepath.Join(t.TempDir(), "fix2.db"), "S")
	if err := s.Append(ctx, st.Generation, 1, nil); err != nil {
		t.Fatalf("Append of a nil message: %v", err)
	}
	if err := s.Append(ctx, st.Generation, 1, nil); err != nil {
		t.Errorf("retrying the Append of a nil message: %v", err)
	}
	checkRecords(t, collect(t, s, st.Generation, 1, 1), []record{{1, "", store.HoleNone}})
}

// An in-memory database cannot be shared by the store's connections.
func TestNotAFile(t *testing.T) {
	s, err := sqlite3.OpenFile(":memory:", "S")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Open(context.Background()); err == nil {
		t.Error("Open of an in-memory database succeeded")
	}
}

// A newer schema is refused.
func TestSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fix2.db")
	open(t, path, "S")
	if _, err := openDB(t, path).Exec("UPDATE fix2_schema SET version = 99"); err != nil {
		t.Fatal(err)
	}
	s, err := sqlite3.OpenFile(path, "S")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Open(context.Background()); err == nil || !strings.Contains(err.Error(), "version 99") {
		t.Errorf("Open of a database with a newer schema: %v, want version 99 refused", err)
	}
}

// userVersion returns the user_version of db.
func userVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// The database's user_version belongs to the application sharing it:
// whatever it is, the store opens and leaves it as it is.
func TestSchemaUserVersionLeftAlone(t *testing.T) {
	for _, v := range []int{0, 1, 7, 99} {
		t.Run(fmt.Sprint(v), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fix2.db")
			db := openDB(t, path)
			for _, q := range []string{"CREATE TABLE application (value TEXT)", fmt.Sprintf("PRAGMA user_version = %d", v)} {
				if _, err := db.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				s, _ := open(t, path, "S")
				s.Close()
				if got := userVersion(t, db); got != v {
					t.Fatalf("user_version = %d, want %d", got, v)
				}
			}
		})
	}
}

// A database of version 1, which had no fix2_schema and set user_version
// to 1, keeps its sessions and messages, and its user_version.
func TestSchemaVersion1(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fix2.db")
	s, st := open(t, path, "S")
	if err := s.Append(ctx, st.Generation, 1, []byte("m1")); err != nil {
		t.Fatal(err)
	}
	s.Close()
	db := openDB(t, path)
	for _, q := range []string{"DROP TABLE fix2_schema", "PRAGMA user_version = 1"} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	s, st = open(t, path, "S")
	if st.NextSender != 2 {
		t.Errorf("NextSender = %d, want 2", st.NextSender)
	}
	if got := collect(t, s, st.Generation, 1, 1); len(got) != 1 || got[0].raw != "m1" {
		t.Errorf("messages = %v, want m1", got)
	}
	var version int
	if err := db.QueryRow("SELECT version FROM fix2_schema").Scan(&version); err != nil || version != 2 {
		t.Errorf("fix2_schema version = %d (%v), want 2", version, err)
	}
	if got := userVersion(t, db); got != 1 {
		t.Errorf("user_version = %d, want 1", got)
	}
}
