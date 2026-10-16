package main

import (
	"errors"
	"strconv"
	"testing"

	"github.com/sylr/fix2/contribs/coinbasemd/internal/fixt"
	"github.com/sylr/fix2/contribs/coinbasemd/internal/md"
	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/dictionary"
)

// A group Coinbase's dictionary requires within an optional component,
// NoRelatedSym in SecListGrp or NoEvents in EvntGrp, is required only when
// the component is there: a SecurityList without instruments, and an
// instrument or a SecurityDefinition without events, are valid.
//
// Instruments without events were rejected until md was regenerated with
// the fixgen of 7d09b28, which emits such groups as conditions.
func TestDictionary_OptionalComponentGroups(t *testing.T) {
	d := dictionary.Compose(fixt.Dictionary(), md.Dictionary())
	list := []string{"320", "R", "322", "R1", "560", "0", "393", "1", "893", "Y"}
	security := []string{"55", "BTC-USD", "15", "USD", "562", "1", "969", "0.01", "29003", "0.00000001", "1682", "full_trading"}
	definition := []string{"980", "A", "779", "20240101"}
	event := []string{"864", "1", "865", "5", "1145", "20240101"}
	for _, tc := range []struct {
		name    string
		msgType string
		fields  [][]string
		missing int // the required tag reported missing, 0 if valid
	}{
		{"SecurityList without instruments", "y", [][]string{list}, 0},
		{"SecurityList instrument without events", "y", [][]string{list, {"146", "1"}, security}, 0},
		{"SecurityList instrument with an event", "y", [][]string{list, {"146", "1"}, security, event}, 0},
		{"SecurityList event without EventTime", "y", [][]string{list, {"146", "1"}, security, event[:4]}, 1145},
		{"SecurityDefinition without instrument", "d", [][]string{definition}, 0},
		{"SecurityDefinition without events", "d", [][]string{definition, security}, 0},
		{"SecurityDefinition with an event", "d", [][]string{definition, security, event}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := fix.NewBuilder("FIXT.1.1", tc.msgType).
				SetString(49, "Coinbase").SetString(56, testKey).SetInt(34, 2).
				SetString(52, "20240101-00:00:00.000")
			for _, fields := range tc.fields {
				for i := 0; i+1 < len(fields); i += 2 {
					tag, _ := strconv.Atoi(fields[i])
					b.Append(tag, []byte(fields[i+1]))
				}
			}
			m, err := fix.Parse(b.Build())
			if err != nil {
				t.Fatal(err)
			}
			// The checks the session runs: see client.go.
			err = d.ValidateWith(&m, dictionary.CheckAll&^dictionary.CheckGroupOrder)
			if tc.missing == 0 {
				if err != nil {
					t.Fatalf("rejected: %v", err)
				}
				return
			}
			var ve *dictionary.ValidationError
			if !errors.As(err, &ve) || !errors.Is(err, dictionary.ErrRequiredTag) || ve.Tag != tc.missing {
				t.Fatalf("got %v, want required tag %d missing", err, tc.missing)
			}
		})
	}
}
