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

// A Range reads in short transactions, a chunk at a time: the writes its
// callback waits through are checkpointed meanwhile, and the write-ahead
// log stays bounded, as in TestCheckpoint.
func TestRangeLetsCheckpointsRun(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fix2.db")
	s, st := open(t, path, "S")
	raw := make([]byte, 200)
	const n = 6000
	for seq := uint64(1); seq <= n; seq++ {
		if err := s.Append(ctx, st.Generation, seq, raw); err != nil {
			t.Fatal(err)
		}
	}
	// A resend of everything, during which as many messages are sent.
	next := uint64(n + 1)
	err := s.Range(ctx, st.Generation, 1, n, func(uint64, []byte, store.Hole) error {
		err := s.Append(ctx, st.Generation, next, raw)
		next++
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	// Some 12KB per commit, were it never checkpointed: 70MB.
	t.Logf("write-ahead log of %.1f MB", float64(fi.Size())/(1<<20))
	if fi.Size() > 40<<20 {
		t.Errorf("write-ahead log of %d MB after %d messages written during a Range", fi.Size()>>20, n)
	}
}

// A Range keeps its snapshot across a Truncate its callback makes, in the
// chunks it reads after it too; the messages kept for it are deleted by a
// later Truncate.
func TestRangeSnapshotAcrossChunks(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fix2.db")
	s, st := open(t, path, "S")
	const n = 3*500 + 7 // several chunks
	for seq := uint64(1); seq <= n; seq++ {
		if err := s.Append(ctx, st.Generation, seq, []byte(fmt.Sprint("m", seq))); err != nil {
			t.Fatal(err)
		}
	}
	got := 0
	err := s.Range(ctx, st.Generation, 1, n, func(seq uint64, raw []byte, hole store.Hole) error {
		if seq == 1 {
			if err := s.Truncate(ctx, st.Generation, n-10); err != nil {
				return err
			}
		}
		if hole != store.HoleNone || string(raw) != fmt.Sprint("m", seq) {
			t.Errorf("message %d: %q, %v, want m%d", seq, raw, hole, seq)
		}
		got++
		return nil
	})
	if err != nil || got != n {
		t.Fatalf("Range: %d messages, %v", got, err)
	}
	for _, r := range collect(t, s, st.Generation, 1, n-11) {
		if r.hole != store.HoleRetention {
			t.Fatalf("after the Range, message %d: %v, want truncated", r.seq, r.hole)
		}
	}
	countBelow := func(seq uint64) int {
		var c int
		if err := openDB(t, path).QueryRow("SELECT count(*) FROM fix2_messages WHERE seq < ?", seq).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	if c := countBelow(n - 10); c == 0 {
		t.Fatal("the messages the Range read were deleted under it")
	}
	if err := s.Truncate(ctx, st.Generation, n-5); err != nil {
		t.Fatal(err)
	}
	if c := countBelow(n - 5); c != 0 {
		t.Errorf("%d messages left before %d after a Truncate with no Range", c, n-5)
	}
}

// Messages, numbers skipped and truncated ones are told apart across the
// chunks a Range reads.
func TestRangeChunkBoundaries(t *testing.T) {
	ctx := context.Background()
	s, st := open(t, filepath.Join(t.TempDir(), "fix2.db"), "S")
	// 1-700 stored, 701-1300 skipped, 1301-2100 stored, the first 100
	// truncated.
	seq := uint64(1)
	for ; seq <= 700; seq++ {
		if err := s.Append(ctx, st.Generation, seq, []byte(fmt.Sprint("m", seq))); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetNextSender(ctx, st.Generation, 1301); err != nil {
		t.Fatal(err)
	}
	for seq = 1301; seq <= 2100; seq++ {
		if err := s.Append(ctx, st.Generation, seq, []byte(fmt.Sprint("m", seq))); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Truncate(ctx, st.Generation, 101); err != nil {
		t.Fatal(err)
	}
	got := collect(t, s, st.Generation, 1, 2100)
	if len(got) != 2100 {
		t.Fatalf("Range: %d records, want 2100", len(got))
	}
	for _, r := range got {
		want := record{seq: r.seq, raw: fmt.Sprint("m", r.seq)}
		switch {
		case r.seq <= 100:
			want = record{seq: r.seq, hole: store.HoleRetention}
		case r.seq >= 701 && r.seq <= 1300:
			want = record{seq: r.seq, hole: store.HoleNotStored}
		}
		if r != want {
			t.Fatalf("record %+v, want %+v", r, want)
		}
	}
}

// A Reset committed while a Range reads stops the Range at its next chunk
// with ErrGenerationGone, rather than reporting the messages it deleted
// missing.
func TestRangeAcrossReset(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fix2.db")
	db := openDB(t, path)
	a := sqlite3.New(db, "S")
	defer a.Close()
	st, err := a.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const n = 1200 // several chunks
	for seq := uint64(1); seq <= n; seq++ {
		if err := a.Append(ctx, st.Generation, seq, []byte("m")); err != nil {
			t.Fatal(err)
		}
	}
	err = a.Range(ctx, st.Generation, 1, n, func(seq uint64, _ []byte, hole store.Hole) error {
		if hole == store.HoleMissing {
			return fmt.Errorf("message %d missing", seq)
		}
		if seq == 1 {
			// Committed by another store: a's generation has not changed
			// yet, as between a Reset's commit and its state.
			b := sqlite3.New(db, "S")
			defer b.Close()
			if _, err := b.Open(ctx); err != nil {
				return err
			}
			return b.Reset(ctx, st.Generation, time.Now(), "")
		}
		return nil
	})
	if !errors.Is(err, store.ErrGenerationGone) {
		t.Errorf("Range: %v, want %v", err, store.ErrGenerationGone)
	}
}

// The messages a Truncate kept for a Range are deleted by the next one,
// even at the same first_seq, or when the store opens.
func TestTruncateDeletesWhatItKept(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fix2.db")
	s, st := open(t, path, "S")
	for seq := uint64(1); seq <= 100; seq++ {
		if err := s.Append(ctx, st.Generation, seq, []byte("m")); err != nil {
			t.Fatal(err)
		}
	}
	truncatingRange := func() {
		t.Helper()
		err := s.Range(ctx, st.Generation, 1, 100, func(seq uint64, _ []byte, _ store.Hole) error {
			if seq == 1 {
				return s.Truncate(ctx, st.Generation, 101)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	stored := func() int {
		t.Helper()
		var c int
		if err := openDB(t, path).QueryRow("SELECT count(*) FROM fix2_messages").Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	truncatingRange()
	if c := stored(); c != 100 {
		t.Fatalf("%d messages stored after the Range, want the 100 it read", c)
	}
	if err := s.Truncate(ctx, st.Generation, 101); err != nil {
		t.Fatal(err)
	}
	if c := stored(); c != 0 {
		t.Errorf("%d messages stored after a Truncate at the same first_seq, want 0", c)
	}
	// Left behind by a process that stopped: deleted when the store opens.
	if _, err := openDB(t, path).Exec("INSERT INTO fix2_messages (session_id, seq, raw) SELECT session_id, 5, x'00' FROM fix2_sessions"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	open(t, path, "S")
	if c := stored(); c != 0 {
		t.Errorf("%d messages stored below first_seq once opened, want 0", c)
	}
}

// A Range of a generation a Reset ended keeps nothing of the next one from
// a Truncate.
func TestTruncateIgnoresPinsOfEarlierGenerations(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fix2.db")
	s, st := open(t, path, "S")
	for seq := uint64(1); seq <= 10; seq++ {
		if err := s.Append(ctx, st.Generation, seq, []byte("m")); err != nil {
			t.Fatal(err)
		}
	}
	err := s.Range(ctx, st.Generation, 1, 10, func(seq uint64, _ []byte, _ store.Hole) error {
		if seq != 1 {
			return nil
		}
		if err := s.Reset(ctx, st.Generation, time.Now(), ""); err != nil {
			return err
		}
		gen := st.Generation + 1
		for seq := uint64(1); seq <= 10; seq++ {
			if err := s.Append(ctx, gen, seq, []byte("n")); err != nil {
				return err
			}
		}
		if err := s.Truncate(ctx, gen, 11); err != nil {
			return err
		}
		var c int
		if err := openDB(t, path).QueryRow("SELECT count(*) FROM fix2_messages").Scan(&c); err != nil {
			return err
		}
		if c != 0 {
			t.Errorf("%d messages of the new generation kept for a Range of the previous one", c)
		}
		return nil
	})
	if !errors.Is(err, store.ErrGenerationGone) {
		t.Errorf("Range: %v, want %v", err, store.ErrGenerationGone)
	}
}

// The numbers skipped a Truncate kept for a Range are deleted when the
// store opens, as the messages are.
func TestOpenDeletesSkippedKept(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fix2.db")
	s, st := open(t, path, "S")
	if err := s.SetNextSender(ctx, st.Generation, 101); err != nil {
		t.Fatal(err)
	}
	err := s.Range(ctx, st.Generation, 1, 100, func(seq uint64, _ []byte, _ store.Hole) error {
		if seq == 1 {
			return s.Truncate(ctx, st.Generation, 101)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	open(t, path, "S")
	var c int
	if err := openDB(t, path).QueryRow("SELECT count(*) FROM fix2_skipped").Scan(&c); err != nil {
		t.Fatal(err)
	}
	if c != 0 {
		t.Errorf("%d ranges of numbers skipped kept once opened, want 0", c)
	}
}
