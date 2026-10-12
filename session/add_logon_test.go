package session_test

import (
	"context"
	"testing"
	"time"

	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/session/store"
	"github.com/sylr/fix2/session/store/memstore"
)

// A Logon for a session Add is adding, its store still opening, waits for
// it and gets it once Add returns: arriving before Add, or while Resolve
// runs for it.
func TestLogonWhileAdding(t *testing.T) {
	id := session.SessionID{BeginString: session.FIX44, SenderCompID: "INI", TargetCompID: "ACC"}
	for _, tc := range []struct {
		name    string
		resolve bool // Add starts while Resolve runs
	}{
		{"no Resolve", false},
		{"during Resolve", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newGatedStore(t, 1)
			h := newRecorder()
			var a *session.Acceptor
			added := make(chan error, 1)
			add := func() {
				go func() {
					_, err := a.Add(session.Config{ID: id, Handler: h, Store: st})
					added <- err
				}()
				select {
				case <-st.entered:
				case <-time.After(5 * time.Second):
					t.Error("Add did not open its store")
				}
			}
			var opts session.AcceptorOptions
			resolved := memstore.New()
			if tc.resolve {
				opts.Resolve = func(ctx context.Context, req *session.LogonRequest) (*session.Config, error) {
					add()
					return &session.Config{ID: req.ID, Handler: newRecorder(), Store: resolved}, nil
				}
			}
			a, l, _ := runAcceptor(t, opts)
			if !tc.resolve {
				add()
			}
			p := dialPeer(t, l)
			p.send("A", 0, "98", "0", "108", "30")
			time.Sleep(50 * time.Millisecond)
			st.release()
			if err := <-added; err != nil {
				t.Fatalf("Add: %v", err)
			}
			p.expect("A")
			h.wait(t, func() bool { return h.logons == 1 })
		})
	}
}

// panicStore panics when opened.
type panicStore struct{ *memstore.Store }

func (panicStore) Open(context.Context) (store.State, error) { panic("store broken") }

// An Add whose store panics when opened releases the ID it reserved.
func TestAddPanicReleasesID(t *testing.T) {
	id := session.SessionID{BeginString: session.FIX44, SenderCompID: "INI", TargetCompID: "ACC"}
	for _, tc := range managers {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.new()
			func() {
				defer func() {
					if recover() == nil {
						t.Error("Add did not panic")
					}
				}()
				m.add(session.Config{ID: id, Handler: session.BaseHandler{}, Store: panicStore{memstore.New()}})
			}()
			if _, err := m.add(session.Config{ID: id, Handler: session.BaseHandler{}}); err != nil {
				t.Errorf("Add after an Add that panicked: %v", err)
			}
		})
	}
}
