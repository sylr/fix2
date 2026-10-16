package session

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/transport"
	"github.com/sylr/fix2/transport/mem"
)

// A session finalizing, its server crashing at its every store write, the
// failed write lost or made, then restarting: the client ends finalized,
// by the Terminate exchange or, the exchange lost, by the server retiring
// the finished session; the server never keeps it established.
func TestCrashFinalizeAtEveryWrite(t *testing.T) {
	for _, commit := range []bool{false, true} {
		t.Run(fmt.Sprintf("commit=%v", commit), func(t *testing.T) { crashFinalize(t, commit) })
	}
}

func crashFinalize(t *testing.T, commit bool) {
	short := func(c *ServerConfig) { c.UnestablishedTimeout = 300 * time.Millisecond }
	for failAt := 1; ; failAt++ {
		passed := t.Run(fmt.Sprintf("write %d", failAt), func(t *testing.T) {
			var cur atomic.Pointer[mem.Listener]
			cur.Store(mem.Listen("server"))
			dialer := transport.DialerFunc(func(ctx context.Context) (transport.Conn, error) { return cur.Load().Dial(ctx) })
			ss := newCrashStore(failAt)
			ss.commit = commit
			srv, cancelSrv, srvDone := newServer(t, cur.Load(), ss, BaseHandler{}, fixp.Recoverable, short)
			c := newClient(t, dialer, newCrashStore(0), BaseHandler{}, fixp.Idempotent, nil)
			cancel, done := runClient(c)
			defer cancel()
			halted := func() bool {
				s := srv.Session(c.Session().ID())
				return s != nil && s.State() == StateStopped
			}
			eventually(t, func() bool {
				st := c.Session().State()
				return st == StateEstablished || st == StateStopped || halted()
			})
			if c.Session().State() == StateStopped {
				cancelSrv()
				<-srvDone
				<-done
				return // the negotiation refused
			}
			for i := range 2 {
				c.Session().Send(bg, appMsg{1, fmt.Sprint(i)})
			}
			c.Session().Finish(bg)
			var runErr error
			finished := false
			eventually(t, func() bool {
				select {
				case runErr = <-done:
					finished = true
					return true
				default:
					return halted()
				}
			})
			if finished {
				cancelSrv()
				<-srvDone
				if runErr != nil {
					t.Fatalf("client Run: %v", runErr)
				}
				ss.mu.Lock()
				untouched := ss.writes < failAt
				ss.mu.Unlock()
				if untouched {
					skipRest = true // no failure injected: every write passed
				}
				return
			}
			cancelSrv()
			<-srvDone
			restarted := ss.crash()
			cur.Store(mem.Listen("server"))
			srv, cancelSrv, srvDone = newServer(t, cur.Load(), restarted, BaseHandler{}, fixp.Recoverable, short)
			defer func() { cancelSrv(); <-srvDone }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("client Run after the server's restart: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("client not finalized: %s", c.Session().State())
			}
			if c.Session().State() != StateFinalized {
				t.Fatalf("client %s", c.Session().State())
			}
			// The server closes its end once it answered: soon after.
			eventually(t, func() bool {
				s := srv.Session(c.Session().ID())
				return s == nil || s.State() != StateEstablished && s.State() != StateTerminating
			})
		})
		if !passed || skipRest || failAt > 200 {
			skipRest = false
			return
		}
	}
}
