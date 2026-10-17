package probe

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/sylr/fix2/contribs/binancefix"
)

// LogonResult is what became of a Logon.
type LogonResult struct {
	Endpoint      string `json:"endpoint"`
	SenderCompID  string `json:"sender_comp_id"`
	SignatureTime string `json:"signature_time"`
	// SignedSendingTime is the text of the SendingTime signed.
	SignedSendingTime string `json:"signed_sending_time,omitempty"`
	// Accepted is set when Binance answered with a LogonAck: it took the
	// signature. The client may still have refused the LogonAck (Error).
	Accepted bool      `json:"accepted"`
	LogonAck *LogonAck `json:"logon_ack,omitempty"`
	// Refusal is the Reject or Logout that answered the Logon instead.
	Refusal *Refusal `json:"refusal,omitempty"`
	// Error is why the session was not established, if it was not.
	Error    string    `json:"error,omitempty"`
	Messages []Message `json:"messages"`
}

// Refusal is Binance's refusal of a Logon.
type Refusal struct {
	Message   string `json:"message"` // Reject or Logout
	Text      string `json:"text,omitempty"`
	ErrorCode *int32 `json:"error_code,omitempty"`
	Fields    string `json:"fields,omitempty"`
}

// Logon logs on to o.Endpoint (any: the caller decides) with o's
// SignatureTime, then logs out: the M0 spike.
func Logon(ctx context.Context, o Options) *LogonResult {
	o = o.withDefaults()
	s, lr, err := open(ctx, newRun(o), o.SenderCompID)
	if err == nil {
		s.close()
		lr.Messages = s.rec.since(0)
	}
	return lr
}

// AutoResult is what LogonAuto found.
type AutoResult struct {
	// Accepted is the form Binance accepted, "" if none.
	Accepted string         `json:"accepted,omitempty"`
	Attempts []*LogonResult `json:"attempts"`
	Error    string         `json:"error,omitempty"`
}

// stopCodes are the error codes of a refusal that is not the signature's:
// trying another form would not help.
var stopCodes = []int32{
	-1002, // UNAUTHORIZED
	-1003, // TOO_MANY_REQUESTS
	-1021, // INVALID_TIMESTAMP: the clock is off
	-2014, // BAD_API_KEY_FMT
	-2015, // REJECTED_MBX_KEY: invalid API key, IP or permissions
}

// Cooldown returns the wait between two Logon attempts: twice HeartBtInt,
// Binance refusing a SenderCompID that long after a session ended without
// Logout, and never less than Binance's 15 connection attempts in 30
// seconds allow.
func Cooldown(heartBtInt time.Duration) time.Duration {
	return max(2*heartBtInt, 30*time.Second/15)
}

// LogonAuto tries the forms of the signed SendingTime in turn, until
// Binance accepts one, each attempt Cooldown after the last. It stops at a
// failure that is not a refusal, or a refusal for another cause than the
// signature (stopCodes).
func LogonAuto(ctx context.Context, o Options, forms []binancefix.SignatureTime) *AutoResult {
	o = o.withDefaults()
	res := &AutoResult{Attempts: []*LogonResult{}}
	wait := Cooldown(o.HeartBtInt)
	for i, f := range forms {
		if i > 0 {
			o.Logf("waiting %v (twice HeartBtInt) before the next attempt", wait)
			if err := o.Sleep(ctx, wait); err != nil {
				res.Error = fmt.Sprintf("interrupted: %v", err)
				return res
			}
		}
		o.Logf("attempt %d/%d: signature time %s", i+1, len(forms), f)
		of := o
		of.SignatureTime = f
		lr := Logon(ctx, of)
		res.Attempts = append(res.Attempts, lr)
		switch {
		case lr.Accepted:
			res.Accepted = f.String()
			return res
		case ctx.Err() != nil:
			res.Error = fmt.Sprintf("interrupted: %v", ctx.Err())
			return res
		case lr.Refusal == nil:
			res.Error = fmt.Sprintf("the %s attempt failed without a refusal (%s): stopping", f, lr.Error)
			return res
		case lr.Refusal.ErrorCode != nil && slices.Contains(stopCodes, *lr.Refusal.ErrorCode):
			res.Error = fmt.Sprintf("the %s attempt was refused for another cause than the signature (error code %d, %q): stopping", f, *lr.Refusal.ErrorCode, lr.Refusal.Text)
			return res
		}
	}
	res.Error = "Binance accepted none of the forms"
	return res
}
