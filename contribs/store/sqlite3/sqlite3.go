// Package sqlite3 is a store.Store keeping sessions in a SQLite database,
// through github.com/mattn/go-sqlite3 (cgo): a session using it resumes
// where it stopped when the process restarts.
//
// A database holds any number of sessions, each under a key of its own,
// session.SessionID.String() for one, and one Store at a time per key. Its
// tables are prefixed with fix2_ and may live alongside others; the
// database's user_version is left to them. It must be a
// file: the store switches it to WAL mode, where Range reads a snapshot
// while the session keeps writing.
//
// Every call commits a transaction of its own, which a crash of the process
// does not lose, but a crash of the machine may: Sync makes them durable,
// with one commit that flushes the write-ahead log, for everything
// committed since the previous one. Every 1000 commits, a connection of the
// pool checkpoints the log into the database in the background; the next
// write copies what was committed meanwhile, with two flushes of its own,
// which lets the log start over.
//
// A flush or a checkpoint that fails leaves the store in doubt, as the
// operating system may not report the failure twice: every call from then on
// fails with store.ErrUnknownOutcome, and the session halts. Another commit
// that fails returns store.ErrUnknownOutcome once: the store reads its state
// back, and the next call carries on from whichever outcome it was.
//
// The session waits for the disk under its lock, in Append: when a Sync
// flushes meanwhile, and in the write that completes a checkpoint. A disk
// that hangs holds the session's lock, and Close, until it answers; the
// session halts after session.Config.StoreTimeout without waiting for it. On macOS, a
// flush reaches the drive, not its platters: SQLite only asks for that with
// PRAGMA fullfsync, which slows every Sync tenfold. The store does not set
// it, by design: on macOS, a Sync is not promised to survive a power loss.
package sqlite3

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	sqlite "github.com/mattn/go-sqlite3"

	"github.com/sylr/fix2/fix/session/store"
)

// schemaVersion is the version of the tables this package writes, kept in
// fix2_schema: the database's user_version belongs to the application.
// Version 1 had no fix2_schema; version 2 adds it.
const schemaVersion = 2

// checkpointEvery is the number of commits after which the store
// checkpoints the write-ahead log, in the background.
const checkpointEvery = 1000

var schema = []string{
	`CREATE TABLE IF NOT EXISTS fix2_sessions (
		session_id  INTEGER PRIMARY KEY, -- what the other tables refer to
		session_key TEXT NOT NULL UNIQUE, -- the key given to New or OpenFile
		generation  INTEGER NOT NULL,
		next_sender INTEGER NOT NULL,
		next_target INTEGER NOT NULL,
		first_seq   INTEGER NOT NULL, -- the messages below were truncated away
		stamp       INTEGER NOT NULL, -- bumped for a commit to write (see settle, sync)
		created_at  TEXT NOT NULL,
		window_id   TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS fix2_messages (
		session_id INTEGER NOT NULL,
		seq        INTEGER NOT NULL,
		raw        BLOB NOT NULL,
		PRIMARY KEY (session_id, seq)
	)`,
	// The version of these tables, one row.
	`CREATE TABLE IF NOT EXISTS fix2_schema (
		id      INTEGER PRIMARY KEY CHECK (id = 1),
		version INTEGER NOT NULL
	)`,
	// The sequence numbers SetNextSender skipped, first_seq to last_seq.
	`CREATE TABLE IF NOT EXISTS fix2_skipped (
		session_id INTEGER NOT NULL,
		first_seq  INTEGER NOT NULL,
		last_seq   INTEGER NOT NULL,
		PRIMARY KEY (session_id, first_seq)
	)`,
}

// errConflict reports the database's state not being the one the store
// last wrote: a second writer on the same session.
var errConflict = fmt.Errorf("%w: the database changed under the store, another writer?", store.ErrSeqMismatch)

// Store is a store.Store in a SQLite database. Writes go through a
// connection of its own, one at a time; Range reads through others, in a
// short transaction for each chunk of messages, so that checkpoints are not
// held back while its callback runs (see Range).
type Store struct {
	db    *sql.DB
	key   string
	owned bool // Close closes db

	wmu       sync.Mutex // held while writing, on conn
	conn      *sql.Conn
	sessionID int64 // the session's row in fix2_sessions
	commits   int   // since the last checkpoint

	checkpointing atomic.Bool
	catchUp       atomic.Bool        // a checkpoint ran: the next write completes it
	background    sync.WaitGroup     // the checkpoint running
	closing       context.Context    // done once Close is called
	cancel        context.CancelFunc // of closing

	// mu guards what follows, which only writers, holding wmu, change; and
	// failed, which a checkpoint sets too.
	mu    sync.RWMutex
	state store.State
	first uint64 // the messages below were truncated away
	// deleted is how far they were deleted: Truncate keeps those a Range
	// reads (see pins), deleting them later.
	deleted uint64
	dirty   bool  // committed since the last flush
	failed  error // why the store is in doubt
	opened  bool
	closed  bool
	// pins counts the Ranges running by generation and first sequence
	// number read: Truncate deletes no message of the current generation at
	// or after the lowest, which a Range must still see. Changed holding tmu
	// too.
	pins map[pinned]int
	// tmu, held by Truncate and by a Range pinning, orders them: a Truncate
	// is in a Range's snapshot, or sees its pin. A channel, so that a Range
	// stops waiting for it when its ctx is done.
	tmu chan struct{}
}

// pinned is a generation and the first sequence number a Range reads in it.
type pinned struct{ gen, seq uint64 }

// rangeChunkRows and rangeChunkBytes bound the messages a Range reads in one
// transaction, before handing them to its callback.
const (
	rangeChunkRows  = 512
	rangeChunkBytes = 1 << 20
)

var (
	_ store.Store     = (*Store)(nil)
	_ store.Syncer    = (*Store)(nil)
	_ store.Truncater = (*Store)(nil)
)

// New returns the Store of the session key in db, a database file opened
// with the "sqlite3" driver this package registers, without cache=shared.
// Open creates the tables if need be. db stays the caller's: Close leaves it
// open. The Store keeps one of db's connections while open, which it
// discards when it closes, and Range and the checkpoints take others, Range
// only while it reads a chunk, not while its callback runs: a db limited to
// one connection is refused, and one shared by n stores must allow n+1 at
// least.
func New(db *sql.DB, key string) *Store {
	return &Store{db: db, key: key, tmu: make(chan struct{}, 1)}
}

// OpenFile opens the database file at path, creating it if need be, and
// returns the Store of the session key in it. Closing the Store closes the
// database.
func OpenFile(path, key string) (*Store, error) {
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, fmt.Errorf("sqlite3: opening %s: %w", path, err)
	}
	return &Store{db: db, key: key, owned: true, tmu: make(chan struct{}, 1)}, nil
}

// Open implements store.Store.
func (s *Store) Open(ctx context.Context) (store.State, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	switch {
	case s.closed:
		return store.State{}, store.ErrClosed
	case s.opened:
		if err := s.current(s.state.Generation); err != nil {
			return store.State{}, err // in doubt
		}
		return s.state, nil
	case s.db.Stats().MaxOpenConnections == 1:
		return store.State{}, fmt.Errorf("sqlite3: the database allows one connection, the store needs two at least")
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return store.State{}, fmt.Errorf("sqlite3: connecting: %w", err)
	}
	s.conn = conn
	st, first, err := s.load(ctx)
	if err != nil {
		discard(conn)
		s.conn = nil
		return store.State{}, err
	}
	s.closing, s.cancel = context.WithCancel(context.Background())
	s.mu.Lock()
	s.state, s.first, s.deleted, s.opened = st, first, first, true
	s.mu.Unlock()
	return st, nil
}

// load prepares the database and reads the session's state, creating it if
// the database has none.
func (s *Store) load(ctx context.Context) (st store.State, first uint64, err error) {
	var mode string
	if err := s.conn.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&mode); err != nil {
		return st, 0, fmt.Errorf("sqlite3: switching to WAL mode: %w", err)
	}
	if mode != "wal" {
		return st, 0, fmt.Errorf("sqlite3: the database is in %s mode, not WAL: it must be a file", mode)
	}
	// Commits do not wait for the disk, Sync does; nor do they checkpoint,
	// the store does in the background.
	for _, q := range []string{"PRAGMA synchronous = NORMAL", "PRAGMA wal_autocheckpoint = 0"} {
		if _, err := s.conn.ExecContext(ctx, q); err != nil {
			return st, 0, fmt.Errorf("sqlite3: %w", err)
		}
	}
	var createdAt string
	err = s.write(ctx, func(ctx context.Context) error {
		version, err := s.storedSchemaVersion(ctx)
		if err != nil {
			return err
		}
		if version > schemaVersion {
			return fmt.Errorf("the database's fix2 tables are version %d, this store knows up to %d", version, schemaVersion)
		}
		for _, q := range schema {
			if _, err := s.conn.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		if _, err := s.conn.ExecContext(ctx, `INSERT INTO fix2_schema (id, version) VALUES (1, ?)
			ON CONFLICT (id) DO UPDATE SET version = excluded.version`, schemaVersion); err != nil {
			return err
		}
		if _, err := s.conn.ExecContext(ctx, `INSERT INTO fix2_sessions
			(session_key, generation, next_sender, next_target, first_seq, stamp, created_at, window_id)
			VALUES (?, 1, 1, 1, 1, 0, ?, '') ON CONFLICT (session_key) DO NOTHING`,
			s.key, time.Now().Format(time.RFC3339Nano)); err != nil {
			return err
		}
		if err := s.conn.QueryRowContext(ctx, `SELECT session_id, generation, next_sender, next_target, first_seq, created_at, window_id
			FROM fix2_sessions WHERE session_key = ?`, s.key).Scan(
			&s.sessionID, &st.Generation, &st.NextSender, &st.NextTarget, &first, &createdAt, &st.WindowID); err != nil {
			return err
		}
		// What a Truncate kept for a Range that never ended.
		if _, err = s.conn.ExecContext(ctx, "DELETE FROM fix2_messages WHERE session_id = ? AND seq < ?", s.sessionID, first); err != nil {
			return err
		}
		_, err = s.conn.ExecContext(ctx, "DELETE FROM fix2_skipped WHERE session_id = ? AND last_seq < ?", s.sessionID, first)
		return err
	})
	if err != nil {
		return st, 0, fmt.Errorf("sqlite3: loading session %q: %w", s.key, err)
	}
	if st.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return st, 0, fmt.Errorf("sqlite3: loading session %q: created_at: %w", s.key, err)
	}
	return st, first, nil
}

// storedSchemaVersion returns the version of the fix2 tables of the
// database, 0 if it has none.
func (s *Store) storedSchemaVersion(ctx context.Context) (int, error) {
	hasTable := func(name string) (bool, error) {
		var ok bool
		err := s.conn.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sqlite_master
			WHERE type = 'table' AND name = ?)`, name).Scan(&ok)
		return ok, err
	}
	if ok, err := hasTable("fix2_schema"); err != nil || ok {
		var version int
		if err == nil {
			err = s.conn.QueryRowContext(ctx, "SELECT version FROM fix2_schema WHERE id = 1").Scan(&version)
		}
		if errors.Is(err, sql.ErrNoRows) {
			err = errors.New("fix2_schema holds no version")
		}
		return version, err
	}
	// Version 1 kept its version in user_version, which is left as it is.
	if ok, err := hasTable("fix2_sessions"); err != nil || !ok {
		return 0, err
	}
	return 1, nil
}

// write runs fn in a transaction on the writer connection (see tx). If the
// commit fails, the store settles its outcome (see settle). A ctx done
// before it starts stops it; once started, it is not interrupted, which
// would leave the commit in doubt.
func (s *Store) write(ctx context.Context, fn func(ctx context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx = context.WithoutCancel(ctx)
	if s.catchUp.CompareAndSwap(true, false) {
		// A checkpoint copied most of the log in the background; copying
		// what was committed since, with no write meanwhile, lets this
		// transaction start it over.
		if _, err := s.conn.ExecContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)"); err != nil {
			return s.fail(fmt.Errorf("%w: sqlite3: checkpointing: %w", store.ErrUnknownOutcome, err))
		}
	}
	if err := s.tx(ctx, fn); err != nil {
		if errors.Is(err, store.ErrUnknownOutcome) && s.opened {
			if serr := s.settle(); serr != nil {
				return serr
			}
		}
		return err
	}
	s.mu.Lock()
	s.dirty = true
	s.mu.Unlock()
	if s.commits++; s.commits >= checkpointEvery {
		s.commits = 0
		s.checkpoint()
	}
	return nil
}

// tx runs fn in a transaction on the writer connection. If fn fails, the
// transaction is rolled back and nothing is written. If the commit fails,
// whether it was is unknown: the error wraps store.ErrUnknownOutcome.
func (s *Store) tx(ctx context.Context, fn func(ctx context.Context) error) error {
	if _, err := s.conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("sqlite3: beginning a transaction: %w", err)
	}
	if err := fn(ctx); err != nil {
		// SQLite may have rolled it back already.
		_, _ = s.conn.ExecContext(ctx, "ROLLBACK")
		return err
	}
	if _, err := s.conn.ExecContext(ctx, "COMMIT"); err != nil {
		// SQLite may leave the transaction open, and its locks held.
		_, _ = s.conn.ExecContext(ctx, "ROLLBACK")
		return fmt.Errorf("%w: sqlite3: committing: %w", store.ErrUnknownOutcome, err)
	}
	return nil
}

// settle makes the outcome of a commit that failed final, and reads the
// session's state back, so that the next call carries on from whichever it
// was: a SetNextTarget set again settles. SQLite may have written the
// commit to the log without being able to index it: the store would not see
// it, but it would come back after a crash. A commit that writes, bumping
// stamp, overwrites it in the log, or follows it. A store that cannot
// settle is in doubt.
func (s *Store) settle() error {
	var (
		st        store.State
		first     uint64
		createdAt string
	)
	err := s.tx(context.Background(), func(ctx context.Context) error {
		if err := s.update(ctx, "UPDATE fix2_sessions SET stamp = stamp + 1 WHERE session_id = ?", s.sessionID); err != nil {
			return err
		}
		return s.conn.QueryRowContext(ctx, `SELECT generation, next_sender, next_target, first_seq, created_at, window_id
			FROM fix2_sessions WHERE session_id = ?`, s.sessionID).Scan(
			&st.Generation, &st.NextSender, &st.NextTarget, &first, &createdAt, &st.WindowID)
	})
	if err == nil {
		st.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	}
	if err != nil {
		return s.fail(fmt.Errorf("%w: sqlite3: settling a failed commit: %w", store.ErrUnknownOutcome, err))
	}
	s.mu.Lock()
	// What was deleted is not known: no further than first, which a Reset
	// may have brought back to 1, is safe.
	s.state, s.first, s.deleted, s.dirty = st, first, min(s.deleted, first), true
	s.mu.Unlock()
	return nil
}

// fail puts the store in doubt because of err, an ErrUnknownOutcome, unless
// it already is, and returns why it is.
func (s *Store) fail(err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed == nil {
		s.failed = err
	}
	return s.failed
}

// checkpoint copies the write-ahead log into the database, in the
// background, on a connection of the pool: it waits for no write, and none
// waits for it. A checkpoint already running is enough. The log only starts
// over once a checkpoint copied it whole, which one running alongside the
// writes never does: the next write first copies what was committed
// meanwhile (see write). Close cancels it.
func (s *Store) checkpoint() {
	if !s.checkpointing.CompareAndSwap(false, true) {
		return
	}
	s.background.Go(func() {
		defer s.checkpointing.Store(false)
		ctx := s.closing
		conn, err := s.checkpointConn(ctx)
		if err != nil {
			return // nothing was written: the next checkpoint tries again
		}
		defer discard(conn)
		// PASSIVE never waits for a lock: what readers still need is
		// copied next time.
		if _, err := conn.ExecContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)"); err != nil {
			if ctx.Err() == nil {
				_ = s.fail(fmt.Errorf("%w: sqlite3: checkpointing: %w", store.ErrUnknownOutcome, err))
			}
			return
		}
		s.catchUp.Store(true)
	})
}

// checkpointConn returns a connection of the pool that flushes when it
// checkpoints, whatever the DSN says: one with synchronous=OFF would let the
// log start over before what it copied reached the disk. The caller
// discards it.
func (s *Store) checkpointConn(ctx context.Context) (*sql.Conn, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA synchronous = NORMAL"); err != nil {
		discard(conn)
		return nil, err
	}
	return conn, nil
}

// update runs query, an UPDATE of the session's row that must change it:
// a row left as it was is one another writer changed.
func (s *Store) update(ctx context.Context, query string, args ...any) error {
	res, err := s.conn.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("sqlite3: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return errConflict
	}
	return nil
}

// check returns an error if the store cannot act on generation gen. The
// caller holds mu: a checkpoint sets failed without wmu.
func (s *Store) check(gen uint64) error {
	switch {
	case s.closed:
		return store.ErrClosed
	case !s.opened:
		return fmt.Errorf("sqlite3: not opened")
	case s.failed != nil:
		return s.failed
	case gen != s.state.Generation:
		return fmt.Errorf("%w: generation %d, current is %d", store.ErrGenerationGone, gen, s.state.Generation)
	}
	return nil
}

// current returns an error if gen is not the current generation.
func (s *Store) current(gen uint64) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.check(gen)
}

// Append implements store.Store.
func (s *Store) Append(ctx context.Context, gen, seq uint64, raw []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if err := s.current(gen); err != nil {
		return err
	}
	switch next := s.state.NextSender; {
	case seq < next:
		return s.appended(ctx, seq, raw)
	case seq > next:
		return fmt.Errorf("%w: appending %d, next is %d", store.ErrSeqMismatch, seq, next)
	}
	if raw == nil {
		raw = []byte{} // not NULL
	}
	err := s.write(ctx, func(ctx context.Context) error {
		if err := s.update(ctx, "UPDATE fix2_sessions SET next_sender = ? WHERE session_id = ? AND generation = ? AND next_sender = ?",
			seq+1, s.sessionID, gen, seq); err != nil {
			return err
		}
		_, err := s.conn.ExecContext(ctx, "INSERT INTO fix2_messages (session_id, seq, raw) VALUES (?, ?, ?)", s.sessionID, seq, raw)
		if serr := (sqlite.Error{}); errors.As(err, &serr) && serr.Code == sqlite.ErrConstraint {
			return errConflict
		}
		if err != nil {
			return fmt.Errorf("sqlite3: storing message %d: %w", seq, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.state.NextSender = seq + 1
	s.mu.Unlock()
	return nil
}

// appended returns nil if the message seq, already appended, is raw: a retry
// of an Append that succeeded. A message truncated away cannot be compared.
func (s *Store) appended(ctx context.Context, seq uint64, raw []byte) error {
	if seq < s.first {
		return nil
	}
	var stored []byte
	err := s.conn.QueryRowContext(ctx, "SELECT raw FROM fix2_messages WHERE session_id = ? AND seq = ?", s.sessionID, seq).Scan(&stored)
	switch {
	case err == nil && bytes.Equal(stored, raw):
		return nil
	case err == nil, errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: message %d is already stored", store.ErrSeqMismatch, seq)
	}
	return fmt.Errorf("sqlite3: reading message %d: %w", seq, err)
}

// SetNextTarget implements store.Store.
func (s *Store) SetNextTarget(ctx context.Context, gen, next uint64) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if err := s.current(gen); err != nil {
		return err
	}
	if err := s.write(ctx, func(ctx context.Context) error {
		return s.update(ctx, "UPDATE fix2_sessions SET next_target = ? WHERE session_id = ? AND generation = ? AND next_sender = ? AND next_target = ?",
			next, s.sessionID, gen, s.state.NextSender, s.state.NextTarget)
	}); err != nil {
		return err
	}
	s.mu.Lock()
	s.state.NextTarget = next
	s.mu.Unlock()
	return nil
}

// SetNextSender implements store.Store. The numbers skipped are recorded
// as one range.
func (s *Store) SetNextSender(ctx context.Context, gen, next uint64) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if err := s.current(gen); err != nil {
		return err
	}
	cur := s.state.NextSender
	switch {
	case next < cur:
		return fmt.Errorf("%w: NextSender %d after %d", store.ErrBackwards, next, cur)
	case next == cur:
		return nil
	}
	err := s.write(ctx, func(ctx context.Context) error {
		if err := s.update(ctx, "UPDATE fix2_sessions SET next_sender = ? WHERE session_id = ? AND generation = ? AND next_sender = ?",
			next, s.sessionID, gen, cur); err != nil {
			return err
		}
		_, err := s.conn.ExecContext(ctx, "INSERT INTO fix2_skipped (session_id, first_seq, last_seq) VALUES (?, ?, ?)", s.sessionID, cur, next-1)
		if err != nil {
			return fmt.Errorf("sqlite3: skipping %d to %d: %w", cur, next-1, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.state.NextSender = next
	s.mu.Unlock()
	return nil
}

// Range implements store.Store. It reads the messages a chunk at a time
// (rangeChunkRows or rangeChunkBytes), each in a short transaction on a
// connection of the pool, given back before fn sees the chunk: a callback
// that takes long, as a resend to a slow counterparty does, holds back no
// checkpoint. The snapshot it reports is NextSender and first_seq read when
// it starts, and the messages Truncate keeps for it (see pin). It checks
// the generation against the store's before each message.
func (s *Store) Range(ctx context.Context, gen, begin, end uint64, fn func(seq uint64, raw []byte, hole store.Hole) error) error {
	if err := s.current(gen); err != nil {
		return err
	}
	begin = max(begin, 1)
	// Pinned before the snapshot is read: a Truncate is either in it, or
	// deletes nothing this Range reads.
	unpin, err := s.pin(ctx, gen, begin)
	if err != nil {
		return err
	}
	defer unpin()
	var next, first uint64
	var skipped [][2]uint64
	err = s.read(ctx, gen, func(conn *sql.Conn) error {
		var dbGen uint64
		err := conn.QueryRowContext(ctx, "SELECT generation, next_sender, first_seq FROM fix2_sessions WHERE session_id = ?", s.sessionID).Scan(&dbGen, &next, &first)
		if err != nil {
			return fmt.Errorf("sqlite3: reading session %q: %w", s.key, err)
		}
		if dbGen != gen {
			return fmt.Errorf("%w: generation %d, current is %d", store.ErrGenerationGone, gen, dbGen)
		}
		if end = min(end, next-1); begin > end {
			return nil
		}
		skipped, err = s.skipped(ctx, conn, begin, end)
		return err
	})
	if err != nil || begin > end {
		return cmp.Or(err, s.current(gen))
	}
	// The messages stored, a chunk at a time: the transaction reading one
	// ends before fn sees it. Those below first, a Truncate deferred may
	// have left, are not read.
	var rows []storedRow
	var buf []byte
	for seq := begin; seq <= end; {
		hi, ri := end, 0 // ri: the row of rows seq may be
		if lo := max(seq, first); lo <= end {
			rows, buf = rows[:0], buf[:0]
			full := false
			err := s.read(ctx, gen, func(conn *sql.Conn) error {
				// A Reset deleting the messages commits before the store's
				// generation changes: one committed is seen here.
				var dbGen uint64
				if err := conn.QueryRowContext(ctx, "SELECT generation FROM fix2_sessions WHERE session_id = ?", s.sessionID).Scan(&dbGen); err != nil {
					return fmt.Errorf("sqlite3: reading session %q: %w", s.key, err)
				}
				if dbGen != gen {
					return fmt.Errorf("%w: generation %d, current is %d", store.ErrGenerationGone, gen, dbGen)
				}
				r, err := conn.QueryContext(ctx, "SELECT seq, raw FROM fix2_messages WHERE session_id = ? AND seq BETWEEN ? AND ? ORDER BY seq", s.sessionID, lo, end)
				if err != nil {
					return fmt.Errorf("sqlite3: reading messages %d to %d: %w", lo, end, err)
				}
				defer r.Close()
				var rowSeq uint64
				var raw sql.RawBytes // valid until the next row
				for !full && r.Next() {
					if err := r.Scan(&rowSeq, &raw); err != nil {
						return fmt.Errorf("sqlite3: reading messages: %w", err)
					}
					start := len(buf)
					buf = append(buf, raw...)
					rows = append(rows, storedRow{seq: rowSeq, start: start, end: len(buf)})
					full = len(rows) == rangeChunkRows || len(buf) >= rangeChunkBytes
				}
				if err := r.Err(); err != nil {
					return fmt.Errorf("sqlite3: reading messages: %w", err)
				}
				return nil
			})
			if err != nil {
				return err
			}
			if full {
				// Read on from the message after the chunk's last.
				hi = rows[len(rows)-1].seq
			}
		}
		for ; seq <= hi; seq++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := s.current(gen); err != nil {
				return err
			}
			for len(skipped) > 0 && skipped[0][1] < seq {
				skipped = skipped[1:]
			}
			var err error
			switch {
			case seq < first:
				err = fn(seq, nil, store.HoleRetention)
			case ri < len(rows) && rows[ri].seq == seq:
				r := rows[ri]
				err = fn(seq, buf[r.start:r.end:r.end], store.HoleNone)
				ri++
			case len(skipped) > 0 && skipped[0][0] <= seq:
				err = fn(seq, nil, store.HoleNotStored)
			default:
				err = fn(seq, nil, store.HoleMissing)
			}
			if err != nil {
				return err
			}
		}
	}
	return s.current(gen)
}

// storedRow is a message a Range read, at buf[start:end].
type storedRow struct {
	seq        uint64
	start, end int
}

// pin keeps Truncate from deleting the messages of generation gen from seq
// on until the returned function is called. It waits for a Truncate
// running, not for a write: a disk that hangs holds a Range back only if a
// Truncate hangs.
func (s *Store) pin(ctx context.Context, gen, seq uint64) (func(), error) {
	if err := s.lockTruncate(ctx); err != nil {
		return nil, err
	}
	defer s.unlockTruncate()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pins == nil {
		s.pins = make(map[pinned]int)
	}
	p := pinned{gen, seq}
	s.pins[p]++
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.pins[p]--; s.pins[p] == 0 {
			delete(s.pins, p)
		}
	}, nil
}

// lockTruncate takes tmu, unless ctx is done first.
func (s *Store) lockTruncate(ctx context.Context) error {
	select {
	case s.tmu <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Store) unlockTruncate() { <-s.tmu }

// read runs fn in a read transaction on a connection of the pool, which it
// gives back once fn returns.
func (s *Store) read(ctx context.Context, gen uint64, fn func(conn *sql.Conn) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		if cerr := s.current(gen); cerr != nil {
			return cerr // closed meanwhile
		}
		return fmt.Errorf("sqlite3: connecting: %w", err)
	}
	defer release(conn)
	// DEFERRED whatever the driver's _txlock: a read takes no lock that
	// would hold back the writes.
	if _, err := conn.ExecContext(ctx, "BEGIN DEFERRED"); err != nil {
		return fmt.Errorf("sqlite3: beginning a transaction: %w", err)
	}
	return fn(conn)
}

// skipped returns the ranges of sequence numbers SetNextSender skipped
// that overlap begin..end, in order.
func (s *Store) skipped(ctx context.Context, conn *sql.Conn, begin, end uint64) ([][2]uint64, error) {
	rows, err := conn.QueryContext(ctx, "SELECT first_seq, last_seq FROM fix2_skipped WHERE session_id = ? AND last_seq >= ? AND first_seq <= ? ORDER BY first_seq", s.sessionID, begin, end)
	if err != nil {
		return nil, fmt.Errorf("sqlite3: reading the numbers skipped: %w", err)
	}
	defer rows.Close()
	var out [][2]uint64
	for rows.Next() {
		var r [2]uint64
		if err := rows.Scan(&r[0], &r[1]); err != nil {
			return nil, fmt.Errorf("sqlite3: reading the numbers skipped: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite3: reading the numbers skipped: %w", err)
	}
	return out, nil
}

// release ends the transaction of conn, a reader, and returns it to the
// pool; or discards it, if it is still in a transaction.
func release(conn *sql.Conn) {
	_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
	_ = conn.Raw(func(dc any) error {
		if c, ok := dc.(*sqlite.SQLiteConn); ok && !c.AutoCommit() {
			return driver.ErrBadConn
		}
		return nil
	})
	_ = conn.Close()
}

// discard closes conn rather than return it to the pool: the writer's
// pragmas are not for the connections of others.
func discard(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}

// Reset implements store.Store. The messages of the previous generation are
// deleted.
func (s *Store) Reset(ctx context.Context, expectGen uint64, createdAt time.Time, windowID string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	switch err := s.current(expectGen + 1); {
	case err == nil:
		return nil // already done
	case !errors.Is(err, store.ErrGenerationGone):
		return err
	case s.state.Generation != expectGen:
		return fmt.Errorf("%w: resetting generation %d, current is %d", store.ErrGenerationGone, expectGen, s.state.Generation)
	}
	err := s.write(ctx, func(ctx context.Context) error {
		for _, q := range []string{"DELETE FROM fix2_messages WHERE session_id = ?", "DELETE FROM fix2_skipped WHERE session_id = ?"} {
			if _, err := s.conn.ExecContext(ctx, q, s.sessionID); err != nil {
				return fmt.Errorf("sqlite3: dropping generation %d: %w", expectGen, err)
			}
		}
		return s.update(ctx, `UPDATE fix2_sessions SET generation = ?, next_sender = 1, next_target = 1, first_seq = 1,
			created_at = ?, window_id = ? WHERE session_id = ? AND generation = ? AND next_sender = ?`,
			expectGen+1, createdAt.Format(time.RFC3339Nano), windowID, s.sessionID, expectGen, s.state.NextSender)
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.state = store.State{Generation: expectGen + 1, NextSender: 1, NextTarget: 1, CreatedAt: createdAt, WindowID: windowID}
	s.first, s.deleted = 1, 1
	s.mu.Unlock()
	return nil
}

// Sync implements store.Syncer. It commits with the write-ahead log flushed
// to disk, which makes every commit before durable too, the messages up to
// upToSeq and whatever else: nothing, if nothing was committed since the
// last flush. A Sync whose ctx is done returns while the flush goes on: it
// holds the writes back until it is done.
func (s *Store) Sync(ctx context.Context, gen, _ uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.RLock()
	err, dirty := s.check(gen), s.dirty
	s.mu.RUnlock()
	if err != nil || !dirty {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- s.sync(gen) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// sync makes everything committed so far durable. A flush that fails puts
// the store in doubt, whether its Sync still waits for it or not: the
// operating system may not report the failure to the next one.
func (s *Store) sync(gen uint64) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if err := s.current(gen); err != nil {
		return err
	}
	if !s.dirty {
		return nil
	}
	ctx := context.Background()
	if _, err := s.conn.ExecContext(ctx, "PRAGMA synchronous = FULL"); err != nil {
		return fmt.Errorf("sqlite3: %w", err)
	}
	// The commit must write for SQLite to flush: an UPDATE leaving the row
	// as it was writes nothing.
	err := s.write(ctx, func(ctx context.Context) error {
		return s.update(ctx, "UPDATE fix2_sessions SET stamp = stamp + 1 WHERE session_id = ? AND generation = ? AND next_sender = ?",
			s.sessionID, gen, s.state.NextSender)
	})
	// Should this fail, commits keep flushing until the next Sync: slower,
	// not less durable.
	_, _ = s.conn.ExecContext(ctx, "PRAGMA synchronous = NORMAL")
	if errors.Is(err, store.ErrUnknownOutcome) {
		return s.fail(err)
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed != nil {
		// A checkpoint failed meanwhile: what it lost, this flush may not
		// have covered.
		return s.failed
	}
	s.dirty = false
	return nil
}

// Truncate implements store.Truncater. The messages are deleted: a Range
// already started keeps reading them from its snapshot.
func (s *Store) Truncate(ctx context.Context, gen, before uint64) error {
	if err := s.lockTruncate(ctx); err != nil {
		return err
	}
	defer s.unlockTruncate()
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if err := s.current(gen); err != nil {
		return err
	}
	first := max(s.first, min(before, s.state.NextSender))
	// A Range running reads on from its pin, past first_seq: what it must
	// still see is deleted by a Truncate after it, at the same first_seq
	// if need be.
	deleteBefore := first
	s.mu.RLock()
	for p := range s.pins {
		// A Range of an earlier generation reads nothing more.
		if p.gen == gen {
			deleteBefore = min(deleteBefore, p.seq)
		}
	}
	s.mu.RUnlock()
	if first == s.first && deleteBefore <= s.deleted {
		return nil
	}
	err := s.write(ctx, func(ctx context.Context) error {
		if deleteBefore > s.deleted {
			if _, err := s.conn.ExecContext(ctx, "DELETE FROM fix2_messages WHERE session_id = ? AND seq < ?", s.sessionID, deleteBefore); err != nil {
				return fmt.Errorf("sqlite3: truncating before %d: %w", first, err)
			}
			if _, err := s.conn.ExecContext(ctx, "DELETE FROM fix2_skipped WHERE session_id = ? AND last_seq < ?", s.sessionID, deleteBefore); err != nil {
				return fmt.Errorf("sqlite3: truncating before %d: %w", first, err)
			}
		}
		return s.update(ctx, "UPDATE fix2_sessions SET first_seq = ? WHERE session_id = ? AND generation = ? AND next_sender = ? AND first_seq = ?",
			first, s.sessionID, gen, s.state.NextSender, s.first)
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.first, s.deleted = first, max(s.deleted, deleteBefore)
	s.mu.Unlock()
	return nil
}

// Close implements store.Store. It waits for a write, a flush a Sync
// stopped waiting for, or a checkpoint, to finish.
func (s *Store) Close() error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.mu.Lock()
	closed := s.closed
	s.closed = true
	s.mu.Unlock()
	if closed {
		return nil
	}
	if s.cancel != nil {
		s.cancel() // a checkpoint may wait for a connection a Range holds
	}
	s.background.Wait()
	if s.conn != nil {
		discard(s.conn)
	}
	if s.owned {
		if err := s.db.Close(); err != nil {
			return fmt.Errorf("sqlite3: closing: %w", err)
		}
	}
	return nil
}
