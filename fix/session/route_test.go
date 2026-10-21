package session_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
)

// user is a recorder whose Logons carry Username (553).
type user struct {
	*recorder
	name string
}

func (u user) ToAdmin(_ context.Context, _ *session.Session, msgType string, b *fix.Builder) error {
	if msgType == "A" {
		b.SetString(553, u.name)
	}
	return nil
}

// qualified returns session ACC->INI told apart by qualifier q.
func qualified(q string) session.SessionID {
	id := dynAccID
	id.Qualifier = q
	return id
}

// byUsername routes a Logon to the candidate qualified by its Username,
// sending what it was asked to asked.
func byUsername(asked chan<- []session.SessionID) func(context.Context, *session.LogonRequest, []session.SessionID) (session.SessionID, error) {
	return func(_ context.Context, req *session.LogonRequest, candidates []session.SessionID) (session.SessionID, error) {
		if req.RemoteAddr == nil {
			return session.SessionID{}, errors.New("no RemoteAddr")
		}
		asked <- slices.Clone(candidates)
		return qualified(string(req.Logon.Get(553))), nil
	}
}

// next returns what Route was asked of next.
func next(t *testing.T, asked <-chan []session.SessionID) []session.SessionID {
	t.Helper()
	select {
	case got := <-asked:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("Route not asked")
		return nil
	}
}

// Two configured sessions with the same CompIDs, told apart by their
// Qualifier: Route picks each Logon's by its Username.
func TestRoute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		asked := make(chan []session.SessionID, 100)
		a, l, _ := runAcceptor(t, session.AcceptorOptions{Route: byUsername(asked)})
		accs := map[string]*recorder{"alice": newRecorder(), "bob": newRecorder()}
		for q, r := range accs {
			if _, err := a.Add(session.Config{ID: qualified(q), Handler: r}); err != nil {
				t.Fatal(err)
			}
		}
		alice, bob := user{newRecorder(), "alice"}, user{newRecorder(), "bob"}
		inis, _ := initiateWith(t, l, alice, bob)

		for q, r := range accs {
			r.wait(t, func() bool { return r.logons == 1 })
			if s := a.Session(qualified(q)); s.State() != session.StateLoggedOn {
				t.Errorf("session %v %v", s.ID(), s.State())
			}
		}
		alice.wait(t, func() bool { return alice.logons == 1 })
		bob.wait(t, func() bool { return bob.logons == 1 })

		// Each session receives what its own counterparty sends.
		for i, u := range []user{alice, bob} {
			if _, err := inis[i].Send(context.Background(), fix.NewBuilder(session.FIX44, "D").SetString(11, u.name).SetString(55, "X")); err != nil {
				t.Fatal(err)
			}
		}
		for q, r := range accs {
			r.wait(t, func() bool { return len(r.apps) == 1 })
			r.mu.Lock()
			if r.apps[0] != q {
				t.Errorf("session %s received %q", q, r.apps[0])
			}
			r.mu.Unlock()
		}

		want := []session.SessionID{qualified("alice"), qualified("bob")}
		for range 2 {
			got := next(t, asked)
			slices.SortFunc(got, func(x, y session.SessionID) int { return strings.Compare(x.Qualifier, y.Qualifier) })
			if !slices.Equal(got, want) {
				t.Errorf("Route asked of %v, want %v", got, want)
			}
		}
	})
}

// Route is asked of a single configured session too.
func TestRouteSingleCandidate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		asked := make(chan []session.SessionID, 100)
		a, l, _ := runAcceptor(t, session.AcceptorOptions{Route: byUsername(asked)})
		acc := newRecorder()
		if _, err := a.Add(session.Config{ID: qualified("alice"), Handler: acc}); err != nil {
			t.Fatal(err)
		}
		initiateWith(t, l, user{newRecorder(), "alice"})
		acc.wait(t, func() bool { return acc.logons == 1 })
		if got := next(t, asked); !slices.Equal(got, []session.SessionID{qualified("alice")}) {
			t.Errorf("Route asked of %v, want [%v]", got, qualified("alice"))
		}
	})
}

func TestRouteRefused(t *testing.T) {
	// A session the acceptor has, though not for this Logon: another wire
	// identity.
	elsewhere := session.SessionID{BeginString: session.FIX44, SenderCompID: "ACC", TargetCompID: "ELSEWHERE"}
	for _, tc := range []struct {
		name  string
		route func(context.Context, *session.LogonRequest, []session.SessionID) (session.SessionID, error)
		log   string
	}{{
		name: "error",
		route: func(context.Context, *session.LogonRequest, []session.SessionID) (session.SessionID, error) {
			return session.SessionID{}, errors.New("wrong certificate")
		},
		log: "routing refused: wrong certificate",
	}, {
		name: "unknown session",
		route: func(context.Context, *session.LogonRequest, []session.SessionID) (session.SessionID, error) {
			return qualified("carol"), nil
		},
		log: "routing chose " + qualified("carol").String() + ", not a candidate",
	}, {
		name: "another wire identity's session",
		route: func(context.Context, *session.LogonRequest, []session.SessionID) (session.SessionID, error) {
			return elsewhere, nil
		},
		log: "routing chose " + elsewhere.String() + ", not a candidate",
	}} {
		for _, qs := range [][]string{{"alice"}, {"alice", "bob"}} {
			t.Run(tc.name+"/"+strings.Join(qs, "+"), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					a, l, log := runAcceptor(t, session.AcceptorOptions{Route: tc.route})
					var accs []*recorder
					for _, q := range qs {
						r := newRecorder()
						if _, err := a.Add(session.Config{ID: qualified(q), Handler: r}); err != nil {
							t.Fatal(err)
						}
						accs = append(accs, r)
					}
					if _, err := a.Add(session.Config{ID: elsewhere, Handler: session.BaseHandler{}}); err != nil {
						t.Fatal(err)
					}
					alice := user{newRecorder(), "alice"}
					_, ilog := initiateWith(t, l, alice)
					log.wait(t, tc.log)
					// The connection is closed: the initiator tries again.
					ilog.wait(t, "connection ended, reconnecting")
					for _, r := range append(accs, alice.recorder) {
						r.mu.Lock()
						if r.logons != 0 {
							t.Errorf("logged on %d times", r.logons)
						}
						r.mu.Unlock()
					}
				})
			})
		}
	}
}

// Without Route, sessions must differ on the wire.
func TestRouteRequired(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := session.NewAcceptor(session.AcceptorOptions{})
		if _, err := a.Add(session.Config{ID: qualified("alice"), Handler: session.BaseHandler{}}); err != nil {
			t.Fatal(err)
		}
		_, err := a.Add(session.Config{ID: qualified("bob"), Handler: session.BaseHandler{}})
		if err == nil || !strings.Contains(err.Error(), "set AcceptorOptions.Route") {
			t.Fatalf("Add of a second session with the same wire identity: %v", err)
		}
		if n := len(a.Sessions()); n != 1 {
			t.Errorf("%d sessions, want 1", n)
		}
	})
}
