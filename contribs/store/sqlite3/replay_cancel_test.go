package sqlite3_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/contribs/store/sqlite3"
	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/session/transport"
	"github.com/sylr/fix2/session/transport/mem"
)

// counterparty is the scripted end of a session's connection.
type counterparty struct {
	t      *testing.T
	conn   transport.Conn
	framer *fix2.Framer
	seq    int
}

// accept accepts the session's next connection on l.
func accept(t *testing.T, l *mem.Listener, seq int) *counterparty {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := l.Accept(ctx)
	if err != nil {
		t.Fatalf("no connection: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return &counterparty{t: t, conn: conn, framer: fix2.NewFramer(conn, fix2.FramerOptions{}), seq: seq}
}

// send writes a message of msgType with fields after the header, at the
// next sequence number.
func (c *counterparty) send(msgType string, fields ...string) {
	c.t.Helper()
	b := fix2.NewBuilder(session.FIX44, msgType).
		SetString(49, "ACC").SetString(56, "INI").SetInt(34, c.seq).
		SetString(52, string(fix2.AppendUTCTimestamp(nil, time.Now(), fix2.Milliseconds)))
	c.seq++
	for i := 0; i+1 < len(fields); i += 2 {
		tag, _ := strconv.Atoi(fields[i])
		b.SetString(tag, fields[i+1])
	}
	if _, err := c.conn.Write(b.Build()); err != nil {
		c.t.Fatal(err)
	}
}

// expect reads the next message and checks its MsgType and fields.
func (c *counterparty) expect(msgType string, fields ...string) {
	c.t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	frame, err := c.framer.Next()
	if err != nil {
		c.t.Fatalf("expected MsgType %s: %v", msgType, err)
	}
	m, err := fix2.Parse(frame)
	if err != nil {
		c.t.Fatal(err)
	}
	if got := string(m.MsgType()); got != msgType {
		c.t.Fatalf("got MsgType %s, want %s", got, msgType)
	}
	for i := 0; i+1 < len(fields); i += 2 {
		tag, _ := strconv.Atoi(fields[i])
		if got := string(m.Get(tag)); got != fields[i+1] {
			c.t.Fatalf("MsgType %s: tag %d is %q, want %q", msgType, tag, got, fields[i+1])
		}
	}
}

// waitFor polls cond until it holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// poolReplay is an initiator session on a database whose pool allows the
// two connections its store needs, the spare one held by the application,
// logged on with an order sent; its counterparty asked for it again, and
// the replay waits for a connection.
type poolReplay struct {
	s     *session.Session
	l     *mem.Listener
	peer  *counterparty
	spare *sql.Conn
	run   context.CancelFunc
	ran   chan error
}

func replayWaitingForPool(t *testing.T) *poolReplay {
	t.Helper()
	db := openDB(t, filepath.Join(t.TempDir(), "fix2.db"))
	db.SetMaxOpenConns(2)
	r := &poolReplay{l: mem.Listen("acceptor"), ran: make(chan error, 1)}
	t.Cleanup(func() { r.l.Close() })
	i := session.NewInitiator(session.InitiatorOptions{ReconnectMin: 10 * time.Millisecond, ReconnectMax: 10 * time.Millisecond})
	var err error
	r.s, err = i.Add(session.Config{
		ID:            session.SessionID{BeginString: session.FIX44, SenderCompID: "INI", TargetCompID: "ACC"},
		Store:         sqlite3.New(db, "replay"),
		Handler:       session.BaseHandler{},
		LogoutTimeout: 100 * time.Millisecond,
	}, r.l)
	if err != nil {
		t.Fatal(err)
	}
	var ctx context.Context
	ctx, r.run = context.WithCancel(context.Background())
	go func() { r.ran <- i.Run(ctx) }()
	t.Cleanup(func() {
		r.release()
		r.run()
		<-r.ran
	})
	r.peer = accept(t, r.l, 1)
	r.peer.expect("A")
	r.peer.send("A", "98", "0", "108", "30")
	waitFor(t, "the Logon", func() bool { return r.s.State() == session.StateLoggedOn })
	if _, err := r.s.Send(context.Background(), fix2.NewBuilder(session.FIX44, "D").SetString(11, "O1")); err != nil {
		t.Fatal(err)
	}
	r.peer.expect("D", "34", "2")

	// An application sharing the database holds the spare connection.
	if r.spare, err = db.Conn(context.Background()); err != nil {
		t.Fatal(err)
	}
	waits := db.Stats().WaitCount
	r.peer.send("2", "7", "1", "16", "0")
	waitFor(t, "the replay to wait for a connection", func() bool { return db.Stats().WaitCount > waits })
	return r
}

// release gives the spare connection back to the pool.
func (r *poolReplay) release() {
	if r.spare != nil {
		r.spare.Close()
		r.spare = nil
	}
}

// A replay waiting for a connection the application holds is cancelled when
// its connection ends: the session reconnects without the connection being
// given back, and resends once it is.
func TestReplayWaitingForPoolReconnects(t *testing.T) {
	r := replayWaitingForPool(t)
	r.peer.conn.Close()

	peer := accept(t, r.l, r.peer.seq)
	peer.expect("A", "34", "3")
	peer.send("A", "98", "0", "108", "30")
	waitFor(t, "the Logon", func() bool { return r.s.State() == session.StateLoggedOn })
	if err := r.s.Err(); err != nil {
		t.Errorf("Err = %v, want nil", err)
	}

	r.release()
	peer.send("2", "7", "1", "16", "0")
	peer.expect("4", "34", "1", "36", "2", "123", "Y")
	peer.expect("D", "34", "2", "43", "Y", "11", "O1")
	peer.expect("4", "34", "3", "36", "4", "123", "Y")
}

// A replay waiting for a connection the application holds keeps neither Run
// nor the session from stopping.
func TestReplayWaitingForPoolStops(t *testing.T) {
	r := replayWaitingForPool(t)
	r.run()
	select {
	case err := <-r.ran:
		r.ran <- err // for the cleanup
	case <-time.After(5 * time.Second):
		t.Fatal("Run waited for the replay")
	}
	select {
	case <-r.s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done waited for the replay")
	}
	if err := r.s.Err(); err != nil {
		t.Errorf("Err = %v, want nil", err)
	}
}

// A replay waiting for a connection the application holds holds back our
// answer to the counterparty's Logout, or our Logout crossing its,
// LogoutTimeout at most: Stop, or Logout, returns then, the connection
// still held.
func TestLogoutWhileReplayWaitsForPool(t *testing.T) {
	for _, crossing := range []bool{false, true} {
		name := "counterparty's Logout"
		if crossing {
			name = "crossing Logouts"
		}
		t.Run(name, func(t *testing.T) {
			r := replayWaitingForPool(t)
			done := make(chan error, 1)
			if crossing {
				go func() { done <- r.s.Logout(context.Background(), "end of day") }()
				waitFor(t, "our Logout", func() bool { return r.s.State() == session.StateLoggingOut })
				r.peer.send("5")
			} else {
				r.peer.send("5", "58", "bye")
				waitFor(t, "the Logout", func() bool { return r.s.State() == session.StateLoggingOut })
				go func() { done <- r.s.Stop(context.Background()) }()
			}
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("logging out: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("logging out waited for the replay")
			}
			if !crossing {
				select {
				case <-r.s.Done():
				case <-time.After(5 * time.Second):
					t.Fatal("Done waited for the replay")
				}
			}
			if err := r.s.Err(); err != nil {
				t.Errorf("Err = %v, want nil", err)
			}
		})
	}
}

// A session whose store waits to open, the application holding every
// connection of the pool, is not added, after StoreTimeout.
func TestOpenWaitingForPool(t *testing.T) {
	db := openDB(t, filepath.Join(t.TempDir(), "fix2.db"))
	db.SetMaxOpenConns(2)
	for range 2 {
		conn, err := db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
	}
	i := session.NewInitiator(session.InitiatorOptions{})
	added := make(chan error, 1)
	go func() {
		_, err := i.Add(session.Config{
			ID:           session.SessionID{BeginString: session.FIX44, SenderCompID: "INI", TargetCompID: "ACC"},
			Store:        sqlite3.New(db, "open"),
			Handler:      session.BaseHandler{},
			StoreTimeout: 50 * time.Millisecond,
		}, mem.Listen("acceptor"))
		added <- err
	}()
	select {
	case err := <-added:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Add: %v, want the store's Open past StoreTimeout", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Add waited for the store's Open")
	}
}
