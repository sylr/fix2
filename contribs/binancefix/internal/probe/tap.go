package probe

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sylr/fix2/contribs/binancefix/internal/fixsbe"
	"github.com/sylr/fix2/contribs/binancefix/internal/lesofh"
	"github.com/sylr/fix2/transport"
)

// Message is a message of a session, ours or Binance's, as it went over the
// wire: what the probes show, and judge by.
type Message struct {
	At     time.Time `json:"at"`
	Sent   bool      `json:"sent"` // ours
	SeqNum uint32    `json:"seq_num"`
	Type   string    `json:"type"`

	ClOrdID       string   `json:"cl_ord_id,omitempty"`
	OrigClOrdID   string   `json:"orig_cl_ord_id,omitempty"`
	CancelClOrdID string   `json:"cancel_cl_ord_id,omitempty"`
	ClListID      string   `json:"cl_list_id,omitempty"`
	OrigClListID  string   `json:"orig_cl_list_id,omitempty"`
	ReqID         string   `json:"req_id,omitempty"`
	OrderID       *int64   `json:"order_id,omitempty"`
	ListID        *int64   `json:"list_id,omitempty"`
	ExecType      string   `json:"exec_type,omitempty"`
	OrdStatus     string   `json:"ord_status,omitempty"`
	ErrorCode     *int32   `json:"error_code,omitempty"`
	Text          string   `json:"text,omitempty"`
	Orders        []string `json:"orders,omitempty"` // a list's ClOrdIDs
	// OrderErrorCodes are a ListStatus's entries' error codes, by
	// ClOrdID: a -1007 there leaves that order's status unknown.
	OrderErrorCodes map[string]int32 `json:"order_error_codes,omitempty"`
	// Fields are the message's other fields of interest, as text.
	Fields string `json:"fields,omitempty"`
	// Answers is the request the client matched the message to, by its
	// first ID: the message answered one of its legs.
	Answers string `json:"answers,omitempty"`

	template    uint16
	execType    fixsbe.ExecType
	cumQty      decimal // an ExecutionReport's CumQty
	sendingTime int64   // the header's
	ack         *LogonAck
	listDone    bool // a ListStatus of a list done or refused
}

// String returns m on a line: its direction, sequence number, type, IDs
// and fields.
func (m Message) String() string {
	var b strings.Builder
	if m.Sent {
		b.WriteString("→ ")
	} else {
		b.WriteString("← ")
	}
	fmt.Fprintf(&b, "%d %s", m.SeqNum, m.Type)
	kv := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, " %s=%s", k, v)
		}
	}
	kv("ClOrdID", m.ClOrdID)
	kv("CancelClOrdID", m.CancelClOrdID)
	kv("OrigClOrdID", m.OrigClOrdID)
	kv("ClListID", m.ClListID)
	kv("OrigClListID", m.OrigClListID)
	kv("ReqID", m.ReqID)
	if m.OrderID != nil {
		kv("OrderID", strconv.FormatInt(*m.OrderID, 10))
	}
	if m.ListID != nil {
		kv("ListID", strconv.FormatInt(*m.ListID, 10))
	}
	kv("ExecType", m.ExecType)
	kv("OrdStatus", m.OrdStatus)
	if len(m.Orders) > 0 {
		kv("Orders", strings.Join(m.Orders, ","))
	}
	if m.Fields != "" {
		b.WriteString(" " + m.Fields)
	}
	if m.ErrorCode != nil {
		kv("ErrorCode", strconv.Itoa(int(*m.ErrorCode)))
	}
	if m.Text != "" {
		kv("Text", strconv.Quote(m.Text))
	}
	if m.Answers != "" {
		fmt.Fprintf(&b, " [answers %s]", m.Answers)
	}
	return b.String()
}

// LogonAck is Binance's LogonAck.
type LogonAck struct {
	SeqNum           uint32 `json:"seq_num"`
	HeartBtInt       uint32 `json:"heart_bt_int"` // seconds
	UUID             string `json:"uuid"`
	SchemaDeprecated string `json:"sbe_schema_id_version_deprecated"`
}

// recorder keeps a session's messages, in the order they went over the
// wire, and which request the client matched each received one to.
type recorder struct {
	mu      sync.Mutex
	msgs    []Message
	answers map[uint32]string
}

func newRecorder() *recorder { return &recorder{answers: map[uint32]string{}} }

func (r *recorder) add(m Message) {
	r.mu.Lock()
	r.msgs = append(r.msgs, m)
	r.mu.Unlock()
}

// answer records that the received message seq answered request id.
func (r *recorder) answer(seq uint32, id string) {
	r.mu.Lock()
	r.answers[seq] = id
	r.mu.Unlock()
}

// mark returns the place of the next message.
func (r *recorder) mark() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.msgs)
}

// since returns the messages from mark i on.
func (r *recorder) since(i int) []Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i > len(r.msgs) {
		i = len(r.msgs)
	}
	ms := make([]Message, len(r.msgs)-i)
	copy(ms, r.msgs[i:])
	for j := range ms {
		if !ms[j].Sent {
			ms[j].Answers = r.answers[ms[j].SeqNum]
		}
	}
	return ms
}

// tapDialer records the messages of the connections of d.
type tapDialer struct {
	d   transport.Dialer
	rec *recorder
}

func (t *tapDialer) Dial(ctx context.Context) (transport.Conn, error) {
	c, err := t.d.Dial(ctx)
	if err != nil {
		return nil, err
	}
	return &tapConn{Conn: c, in: frames{rec: t.rec}, out: frames{rec: t.rec, sent: true}}, nil
}

// tapConn records the frames read and written.
type tapConn struct {
	transport.Conn
	in, out frames
}

func (c *tapConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.in.feed(p[:n])
	return n, err
}

// Write records p before writing it: recorded once written, a message
// could follow the answer the reader already recorded. A write that fails
// leaves it recorded, sent or not; the connection then ends, and its error
// tells.
func (c *tapConn) Write(p []byte) (int, error) {
	c.out.feed(p)
	return c.Conn.Write(p)
}

// frames cuts a byte stream into frames, and records their messages.
type frames struct {
	mu     sync.Mutex
	rec    *recorder
	sent   bool
	buf    []byte
	broken bool
}

func (f *frames) feed(p []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken || len(p) == 0 {
		return
	}
	f.buf = append(f.buf, p...)
	for len(f.buf) >= lesofh.HeaderLen {
		n, err := lesofh.ParseHeader(f.buf)
		if err != nil {
			// The client tells of it; nothing more can be cut.
			f.broken = true
			return
		}
		if len(f.buf) < n {
			return
		}
		f.rec.add(describe(f.buf[lesofh.HeaderLen:n], f.sent))
		f.buf = f.buf[n:]
	}
}

// dec returns the text of decimal mantissa m of exponent e.
func dec(m int64, e int8) string { return decimal{m, e}.String() }

func ptr[T any](o fixsbe.Optional[T]) *T {
	if !o.Valid {
		return nil
	}
	v := o.Value
	return &v
}

func opt[T any](o fixsbe.Optional[T]) string {
	if !o.Valid {
		return ""
	}
	return fmt.Sprint(o.Value)
}

// fields joins the non-empty pairs of kv, key=value.
func fields(kv ...string) string {
	var parts []string
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			parts = append(parts, kv[i]+"="+kv[i+1])
		}
	}
	return strings.Join(parts, " ")
}

func optDec(o fixsbe.Optional[int64], e int8) string {
	if !o.Valid {
		return ""
	}
	return dec(o.Value, e)
}

// describe returns the Message of msg, a message with its header.
func describe(msg []byte, sent bool) Message {
	m := Message{At: time.Now(), Sent: sent}
	h, body, err := fixsbe.ReadHeader(msg)
	if err != nil {
		m.Type, m.Text = "?", err.Error()
		return m
	}
	m.SeqNum, m.template, m.sendingTime = h.SeqNum, h.TemplateId, int64(h.SendingTime)
	if h.SchemaId != fixsbe.SchemaID {
		m.Type = fmt.Sprintf("message of schema %d, template %d", h.SchemaId, h.TemplateId)
		return m
	}
	var derr error
	switch h.TemplateId {
	case fixsbe.LogonTemplateID:
		var x fixsbe.Logon
		_, derr = x.DecodeBody(h, body)
		m.Type = "Logon"
		// The API key, Username, is not shown; nor the signature.
		m.Fields = fields("SenderCompID", string(x.SenderCompId), "TargetCompID", string(x.TargetCompId),
			"HeartBtInt", strconv.Itoa(int(x.HeartBtInt)), "MessageHandling", opt(x.MessageHandling), "ResponseMode", opt(x.ResponseMode),
			"SendingTime", strconv.FormatInt(int64(h.SendingTime), 10))
	case fixsbe.LogonAckTemplateID:
		var x fixsbe.LogonAck
		_, derr = x.DecodeBody(h, body)
		m.Type = "LogonAck"
		m.ack = &LogonAck{SeqNum: h.SeqNum, HeartBtInt: x.HeartBtInt, UUID: string(x.UUID), SchemaDeprecated: x.SbeSchemaIdVersionDeprecated.String()}
		m.Fields = fields("HeartBtInt", strconv.Itoa(int(x.HeartBtInt)), "UUID", string(x.UUID),
			"SbeSchemaIdVersionDeprecated", x.SbeSchemaIdVersionDeprecated.String(), "EncryptMethod", opt(x.EncryptMethod))
	case fixsbe.LogoutTemplateID:
		var x fixsbe.Logout
		_, derr = x.DecodeBody(h, body)
		m.Type, m.Text = "Logout", string(x.Text)
	case fixsbe.HeartbeatTemplateID:
		var x fixsbe.Heartbeat
		_, derr = x.DecodeBody(h, body)
		m.Type, m.Fields = "Heartbeat", fields("TestReqID", string(x.TestReqID))
	case fixsbe.TestRequestTemplateID:
		var x fixsbe.TestRequest
		_, derr = x.DecodeBody(h, body)
		m.Type, m.Fields = "TestRequest", fields("TestReqID", string(x.TestReqID))
	case fixsbe.RejectTemplateID:
		var x fixsbe.Reject
		_, derr = x.DecodeBody(h, body)
		m.Type, m.ErrorCode, m.Text = "Reject", ptr(x.ErrorCode), string(x.Text)
		m.Fields = fields("RefSeqNum", opt(x.RefSeqNum), "RefTagID", opt(x.RefTagID), "RefMsgType", string(x.RefMsgType),
			"SessionRejectReason", opt(x.SessionRejectReason))
	case fixsbe.NewsTemplateID:
		var x fixsbe.News
		_, derr = x.DecodeBody(h, body)
		m.Type, m.Text = "News", string(x.Headline)
	case fixsbe.NewOrderSingleTemplateID:
		var x fixsbe.NewOrderSingle
		_, derr = x.DecodeBody(h, body)
		m.Type, m.ClOrdID = "NewOrderSingle", string(x.ClOrdID)
		m.Fields = fields("Symbol", string(x.Symbol), "Side", x.Side.String(), "OrdType", x.OrdType.String(),
			"OrderQty", optDec(x.OrderQty, x.QtyExponent), "Price", optDec(x.Price, x.PriceExponent), "TimeInForce", opt(x.TimeInForce))
	case fixsbe.OrderCancelRequestTemplateID:
		var x fixsbe.OrderCancelRequest
		_, derr = x.DecodeBody(h, body)
		m.Type, m.ClOrdID, m.OrigClOrdID, m.OrigClListID = "OrderCancelRequest", string(x.ClOrdID), string(x.OrigClOrdID), string(x.OrigClListID)
		m.OrderID, m.ListID = ptr(x.OrderID), ptr(x.ListID)
		m.Fields = fields("Symbol", string(x.Symbol))
	case fixsbe.OrderCancelRequestAndNewOrderSingleTemplateID:
		var x fixsbe.OrderCancelRequestAndNewOrderSingle
		_, derr = x.DecodeBody(h, body)
		m.Type, m.ClOrdID, m.CancelClOrdID, m.OrigClOrdID = "OrderCancelRequestAndNewOrderSingle", string(x.ClOrdID), string(x.CancelClOrdID), string(x.OrigClOrdID)
		m.OrderID = ptr(x.OrderID)
		m.Fields = fields("Mode", x.OrderCancelRequestAndNewOrderSingleMode.String(), "Symbol", string(x.Symbol), "Side", x.Side.String(),
			"OrdType", x.OrdType.String(), "OrderQty", optDec(x.OrderQty, x.QtyExponent), "Price", optDec(x.Price, x.PriceExponent))
	case fixsbe.OrderAmendKeepPriorityRequestTemplateID:
		var x fixsbe.OrderAmendKeepPriorityRequest
		_, derr = x.DecodeBody(h, body)
		m.Type, m.ClOrdID, m.OrigClOrdID, m.OrderID = "OrderAmendKeepPriorityRequest", string(x.ClOrdID), string(x.OrigClOrdID), ptr(x.OrderID)
		m.Fields = fields("Symbol", string(x.Symbol), "OrderQty", dec(x.OrderQty, x.QtyExponent))
	case fixsbe.NewOrderListTemplateID:
		var x fixsbe.NewOrderList
		_, derr = x.DecodeBody(h, body)
		m.Type, m.ClListID = "NewOrderList", string(x.ClListID)
		var desc []string
		for _, o := range x.Orders {
			m.Orders = append(m.Orders, string(o.ClOrdID))
			desc = append(desc, fmt.Sprintf("%s:%s/%s/%s@%s", o.ClOrdID, o.Side, o.OrdType, optDec(o.OrderQty, o.QtyExponent), optDec(o.Price, o.PriceExponent)))
		}
		m.Fields = fields("ContingencyType", x.ContingencyType.String(), "Legs", strings.Join(desc, ","))
	case fixsbe.OrderMassCancelRequestTemplateID:
		var x fixsbe.OrderMassCancelRequest
		_, derr = x.DecodeBody(h, body)
		m.Type, m.ClOrdID = "OrderMassCancelRequest", string(x.ClOrdID)
		m.Fields = fields("Symbol", string(x.Symbol), "MassCancelRequestType", x.MassCancelRequestType.String())
	case fixsbe.LimitQueryTemplateID:
		var x fixsbe.LimitQuery
		_, derr = x.DecodeBody(h, body)
		m.Type, m.ReqID = "LimitQuery", string(x.ReqID)
	case fixsbe.ExecutionReportTemplateID:
		var x fixsbe.ExecutionReport
		_, derr = x.DecodeBody(h, body)
		m.Type, m.ClOrdID, m.OrigClOrdID = "ExecutionReport", string(x.ClOrdID), string(x.OrigClOrdID)
		m.OrderID, m.ListID, m.ErrorCode, m.Text = ptr(x.OrderID), ptr(x.ListID), ptr(x.ErrorCode), string(x.ErrorText)
		m.execType, m.ExecType, m.OrdStatus = x.ExecType, x.ExecType.String(), x.OrdStatus.String()
		m.cumQty = decimal{x.CumQty, x.QtyExponent}
		m.Fields = fields("Symbol", string(x.Symbol), "Side", x.Side.String(), "OrdType", x.OrdType.String(),
			"OrderQty", optDec(x.OrderQty, x.QtyExponent), "Price", optDec(x.Price, x.PriceExponent),
			"CumQty", dec(x.CumQty, x.QtyExponent), "LeavesQty", optDec(x.LeavesQty, x.QtyExponent),
			"LastQty", dec(x.LastQty, x.QtyExponent), "LastPx", optDec(x.LastPx, x.PriceExponent), "ExpiryReason", opt(x.ExpiryReason))
	case fixsbe.ExecutionReportAckTemplateID:
		var x fixsbe.ExecutionReportAck
		_, derr = x.DecodeBody(h, body)
		m.Type, m.ClOrdID = "ExecutionReportAck", string(x.ClOrdID)
		m.OrderID, m.ListID, m.ErrorCode, m.Text = ptr(x.OrderID), ptr(x.ListID), ptr(x.ErrorCode), string(x.ErrorText)
		m.execType, m.ExecType, m.OrdStatus = x.ExecType, x.ExecType.String(), x.OrdStatus.String()
	case fixsbe.OrderCancelRejectTemplateID:
		var x fixsbe.OrderCancelReject
		_, derr = x.DecodeBody(h, body)
		m.Type, m.ClOrdID, m.OrigClOrdID, m.OrigClListID = "OrderCancelReject", string(x.ClOrdID), string(x.OrigClOrdID), string(x.OrigClListID)
		code := x.ErrorCode
		m.OrderID, m.ListID, m.ErrorCode, m.Text = ptr(x.OrderID), ptr(x.ListID), &code, string(x.ErrorText)
	case fixsbe.OrderAmendRejectTemplateID:
		var x fixsbe.OrderAmendReject
		_, derr = x.DecodeBody(h, body)
		m.Type, m.ClOrdID, m.OrigClOrdID = "OrderAmendReject", string(x.ClOrdID), string(x.OrigClOrdID)
		code := x.ErrorCode
		m.OrderID, m.ErrorCode, m.Text = ptr(x.OrderID), &code, string(x.ErrorText)
		m.Fields = fields("OrderQty", dec(x.OrderQty, x.QtyExponent))
	case fixsbe.ListStatusTemplateID:
		var x fixsbe.ListStatus
		_, derr = x.DecodeBody(h, body)
		m.Type, m.ClListID, m.OrigClListID, m.ListID = "ListStatus", string(x.ClListID), string(x.OrigClListID), ptr(x.ListID)
		m.listDone = x.ListOrderStatus == fixsbe.ListOrderStatusAllDone || x.ListOrderStatus == fixsbe.ListOrderStatusReject
		var errs []string
		for _, o := range x.Orders {
			m.Orders = append(m.Orders, string(o.ClOrdID))
			if o.ErrorCode.Valid {
				if m.OrderErrorCodes == nil {
					m.OrderErrorCodes = map[string]int32{}
				}
				m.OrderErrorCodes[string(o.ClOrdID)] = o.ErrorCode.Value
			}
			if o.ErrorCode.Valid || len(o.ErrorText) > 0 {
				errs = append(errs, fmt.Sprintf("%s:%s %q", o.ClOrdID, opt(o.ErrorCode), o.ErrorText))
			}
		}
		m.Fields = fields("ListStatusType", x.ListStatusType.String(), "ListOrderStatus", x.ListOrderStatus.String(),
			"ListRejectReason", opt(x.ListRejectReason), "OrderErrors", strings.Join(errs, ","))
	case fixsbe.OrderMassCancelReportTemplateID:
		var x fixsbe.OrderMassCancelReport
		_, derr = x.DecodeBody(h, body)
		m.Type, m.ClOrdID, m.ErrorCode, m.Text = "OrderMassCancelReport", string(x.ClOrdID), ptr(x.ErrorCode), string(x.ErrorText)
		m.Fields = fields("Symbol", string(x.Symbol), "MassCancelResponse", x.MassCancelResponse.String(),
			"TotalAffectedOrders", opt(x.TotalAffectedOrders), "MassCancelRejectReason", opt(x.MassCancelRejectReason))
	case fixsbe.LimitResponseTemplateID:
		var x fixsbe.LimitResponse
		_, derr = x.DecodeBody(h, body)
		m.Type, m.ReqID = "LimitResponse", string(x.ReqID)
		var ls []string
		for _, l := range x.LimitIndicators {
			ls = append(ls, fmt.Sprintf("%s:%d/%d per %s %s", l.LimitType, l.LimitCount, l.LimitMax, opt(l.LimitResetInterval), opt(l.LimitResetIntervalResolution)))
		}
		m.Fields = fields("Limits", strings.Join(ls, ","))
	default:
		m.Type = fmt.Sprintf("template %d", h.TemplateId)
	}
	if derr != nil {
		m.Type += " (not decoded: " + derr.Error() + ")"
	}
	return m
}
