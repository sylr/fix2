package e2e

import (
	"errors"
	"fmt"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/dictionary"
	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/tests/dicts/fix44"
	"github.com/sylr/fix2/tests/dicts/fix50sp2"
	"github.com/sylr/fix2/tests/dicts/fixt11"
)

// Values of Side (54), ExecType (150) and OrdStatus (39), the same in FIX 4.4
// and FIX 5.0 SP2.
const (
	buy  = "1"
	sell = "2"

	execNew   = "0"
	execTrade = "F"

	statusNew     = "0"
	statusPartial = "1"
	statusFilled  = "2"
)

// order is a NewOrderSingle (35=D), whatever its FIX version: a limit order.
type order struct {
	ClOrdID      string
	Symbol       string
	Side         string
	Qty, Price   float64
	TransactTime time.Time
}

// report is an ExecutionReport (35=8), whatever its FIX version.
type report struct {
	OrderID, ExecID, ClOrdID string
	ExecType, OrdStatus      string
	Symbol, Side             string
	OrderQty, Price          float64
	LastQty, LastPx          float64
	LeavesQty, CumQty, AvgPx float64
	TransactTime             time.Time
}

// equal reports whether o and p hold the same values.
func (o order) equal(p order) bool {
	same := o.TransactTime.Equal(p.TransactTime)
	o.TransactTime, p.TransactTime = time.Time{}, time.Time{}
	return same && o == p
}

// equal reports whether r and o hold the same values.
func (r report) equal(o report) bool {
	same := r.TransactTime.Equal(o.TransactTime)
	r.TransactTime, o.TransactTime = time.Time{}, time.Time{}
	return same && r == o
}

// dialect writes and reads orders and execution reports in one FIX version,
// with the package fixgen generated from its limited dictionary.
type dialect struct {
	name         string
	beginString  string
	dictionaries session.Dictionaries
	newOrder     func(order) session.Outgoing
	readOrder    func(*fix2.Message) (order, error)
	newReport    func(report) session.Outgoing
	readReport   func(*fix2.Message) (report, error)
}

var dialects = []dialect{
	{
		name:         "FIX44",
		beginString:  session.FIX44,
		dictionaries: session.Dictionaries{Session: fix44.Dictionary()},
		newOrder: func(o order) session.Outgoing {
			return fix44.BuildNewOrderSingle(fix44.Version.BeginString).
				SetClOrdID(o.ClOrdID).
				SetSymbol(o.Symbol).
				SetSide(fix44.Side(o.Side)).
				SetTransactTime(o.TransactTime).
				SetOrderQty(o.Qty).
				SetOrdType(fix44.OrdTypeLimit).
				SetPrice(o.Price)
		},
		readOrder: func(m *fix2.Message) (o order, err error) {
			nos := fix44.WrapNewOrderSingle(m)
			if ordType := get(&err, "OrdType", nos.OrdType); ordType != fix44.OrdTypeLimit && err == nil {
				err = fmt.Errorf("OrdType %q, want a limit order", ordType)
			}
			o.ClOrdID = get(&err, "ClOrdID", nos.ClOrdID)
			o.Symbol = get(&err, "Symbol", nos.Symbol)
			o.Side = string(get(&err, "Side", nos.Side))
			o.Qty = parsed(&err, "OrderQty", nos.OrderQty)
			o.Price = parsed(&err, "Price", nos.Price)
			o.TransactTime = parsed(&err, "TransactTime", nos.TransactTime)
			return o, err
		},
		newReport: func(r report) session.Outgoing {
			return fix44.BuildExecutionReport(fix44.Version.BeginString).
				SetOrderID(r.OrderID).
				SetClOrdID(r.ClOrdID).
				SetExecID(r.ExecID).
				SetExecType(fix44.ExecType(r.ExecType)).
				SetOrdStatus(fix44.OrdStatus(r.OrdStatus)).
				SetSymbol(r.Symbol).
				SetSide(fix44.Side(r.Side)).
				SetOrderQty(r.OrderQty).
				SetOrdType(fix44.OrdTypeLimit).
				SetPrice(r.Price).
				SetLastQty(r.LastQty).
				SetLastPx(r.LastPx).
				SetLeavesQty(r.LeavesQty).
				SetCumQty(r.CumQty).
				SetAvgPx(r.AvgPx).
				SetTransactTime(r.TransactTime)
		},
		readReport: func(m *fix2.Message) (r report, err error) {
			er := fix44.WrapExecutionReport(m)
			r.OrderID = get(&err, "OrderID", er.OrderID)
			r.ExecID = get(&err, "ExecID", er.ExecID)
			r.ClOrdID = get(&err, "ClOrdID", er.ClOrdID)
			r.ExecType = string(get(&err, "ExecType", er.ExecType))
			r.OrdStatus = string(get(&err, "OrdStatus", er.OrdStatus))
			r.Symbol = get(&err, "Symbol", er.Symbol)
			r.Side = string(get(&err, "Side", er.Side))
			r.OrderQty = parsed(&err, "OrderQty", er.OrderQty)
			r.Price = parsed(&err, "Price", er.Price)
			r.LastQty = parsed(&err, "LastQty", er.LastQty)
			r.LastPx = parsed(&err, "LastPx", er.LastPx)
			r.LeavesQty = parsed(&err, "LeavesQty", er.LeavesQty)
			r.CumQty = parsed(&err, "CumQty", er.CumQty)
			r.AvgPx = parsed(&err, "AvgPx", er.AvgPx)
			r.TransactTime = parsed(&err, "TransactTime", er.TransactTime)
			return r, err
		},
	},
	{
		name:        "FIX50SP2",
		beginString: session.FIXT11,
		dictionaries: session.Dictionaries{
			Session:          fixt11.Dictionary(),
			Apps:             map[string]*dictionary.Dict{fix50sp2.Version.ApplVerID: fix50sp2.Dictionary()},
			DefaultApplVerID: fix50sp2.Version.ApplVerID,
		},
		newOrder: func(o order) session.Outgoing {
			return fix50sp2.BuildNewOrderSingle(fix50sp2.Version.BeginString).
				SetClOrdID(o.ClOrdID).
				SetSymbol(o.Symbol).
				SetSide(fix50sp2.Side(o.Side)).
				SetTransactTime(o.TransactTime).
				SetOrderQty(o.Qty).
				SetOrdType(fix50sp2.OrdTypeLimit).
				SetPrice(o.Price)
		},
		readOrder: func(m *fix2.Message) (o order, err error) {
			nos := fix50sp2.WrapNewOrderSingle(m)
			if ordType := get(&err, "OrdType", nos.OrdType); ordType != fix50sp2.OrdTypeLimit && err == nil {
				err = fmt.Errorf("OrdType %q, want a limit order", ordType)
			}
			o.ClOrdID = get(&err, "ClOrdID", nos.ClOrdID)
			o.Symbol = get(&err, "Symbol", nos.Symbol)
			o.Side = string(get(&err, "Side", nos.Side))
			o.Qty = parsed(&err, "OrderQty", nos.OrderQty)
			o.Price = parsed(&err, "Price", nos.Price)
			o.TransactTime = parsed(&err, "TransactTime", nos.TransactTime)
			return o, err
		},
		newReport: func(r report) session.Outgoing {
			return fix50sp2.BuildExecutionReport(fix50sp2.Version.BeginString).
				SetOrderID(r.OrderID).
				SetClOrdID(r.ClOrdID).
				SetExecID(r.ExecID).
				SetExecType(fix50sp2.ExecType(r.ExecType)).
				SetOrdStatus(fix50sp2.OrdStatus(r.OrdStatus)).
				SetSymbol(r.Symbol).
				SetSide(fix50sp2.Side(r.Side)).
				SetOrderQty(r.OrderQty).
				SetOrdType(fix50sp2.OrdTypeLimit).
				SetPrice(r.Price).
				SetLastQty(r.LastQty).
				SetLastPx(r.LastPx).
				SetLeavesQty(r.LeavesQty).
				SetCumQty(r.CumQty).
				SetAvgPx(r.AvgPx).
				SetTransactTime(r.TransactTime)
		},
		readReport: func(m *fix2.Message) (r report, err error) {
			er := fix50sp2.WrapExecutionReport(m)
			r.OrderID = get(&err, "OrderID", er.OrderID)
			r.ExecID = get(&err, "ExecID", er.ExecID)
			r.ClOrdID = get(&err, "ClOrdID", er.ClOrdID)
			r.ExecType = string(get(&err, "ExecType", er.ExecType))
			r.OrdStatus = string(get(&err, "OrdStatus", er.OrdStatus))
			r.Symbol = get(&err, "Symbol", er.Symbol)
			r.Side = string(get(&err, "Side", er.Side))
			r.OrderQty = parsed(&err, "OrderQty", er.OrderQty)
			r.Price = parsed(&err, "Price", er.Price)
			r.LastQty = parsed(&err, "LastQty", er.LastQty)
			r.LastPx = parsed(&err, "LastPx", er.LastPx)
			r.LeavesQty = parsed(&err, "LeavesQty", er.LeavesQty)
			r.CumQty = parsed(&err, "CumQty", er.CumQty)
			r.AvgPx = parsed(&err, "AvgPx", er.AvgPx)
			r.TransactTime = parsed(&err, "TransactTime", er.TransactTime)
			return r, err
		},
	},
}

// get returns the value of a generated getter, joining to *err that field
// name is missing.
func get[T any](err *error, name string, getter func() (T, bool)) T {
	v, ok := getter()
	if !ok {
		*err = errors.Join(*err, fmt.Errorf("%s missing", name))
	}
	return v
}

// parsed is get for the getters of values that may not parse: numbers and
// times.
func parsed[T any](err *error, name string, getter func() (T, bool, error)) T {
	v, ok, perr := getter()
	switch {
	case perr != nil:
		*err = errors.Join(*err, fmt.Errorf("%s: %w", name, perr))
	case !ok:
		*err = errors.Join(*err, fmt.Errorf("%s missing", name))
	}
	return v
}
