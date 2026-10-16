package memstore_test

import (
	"context"
	"errors"
	"testing"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/fixp/store"
	"github.com/sylr/fix2/fixp/store/memstore"
	"github.com/sylr/fix2/fixp/store/storetest"
)

func TestConformance(t *testing.T) {
	// memstore keeps nothing across processes: "reopening" is using it
	// again, which runs the reopening checks all the same.
	storetest.Run(t, func(*testing.T) store.Store { return memstore.New() },
		func(_ *testing.T, s store.Store) store.Store { return s })
}

func TestClosed(t *testing.T) {
	s := memstore.New()
	ss, err := s.Create(context.Background(), store.Meta{ID: fixp.NewSessionID()})
	if err != nil {
		t.Fatal(err)
	}
	ss.Close()
	if err := ss.Append(context.Background(), 1, nil); !errors.Is(err, memstore.ErrClosed) {
		t.Errorf("closed session: %v", err)
	}
	s.Close()
	if _, err := s.Sessions(context.Background()); !errors.Is(err, memstore.ErrClosed) {
		t.Errorf("closed store: %v", err)
	}
}
