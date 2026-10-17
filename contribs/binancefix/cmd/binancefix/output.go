package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/sylr/fix2/contribs/binancefix"
	"github.com/sylr/fix2/contribs/binancefix/internal/probe"
)

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func writeMessages(w io.Writer, indent string, ms []probe.Message) {
	for _, m := range ms {
		fmt.Fprintf(w, "%s%s\n", indent, m)
	}
}

// logonOutcome describes what became of a Logon, on a line.
func logonOutcome(lr *probe.LogonResult) string {
	switch {
	case lr.Accepted && lr.Error == "":
		a := lr.LogonAck
		return fmt.Sprintf("accepted: LogonAck seq %d, HeartBtInt %ds, UUID %s, SbeSchemaIdVersionDeprecated %s", a.SeqNum, a.HeartBtInt, a.UUID, a.SchemaDeprecated)
	case lr.Accepted:
		a := lr.LogonAck
		return fmt.Sprintf("accepted by Binance (LogonAck seq %d, HeartBtInt %ds, UUID %s, SbeSchemaIdVersionDeprecated %s), refused by the client: %s",
			a.SeqNum, a.HeartBtInt, a.UUID, a.SchemaDeprecated, lr.Error)
	case lr.Refusal != nil:
		r := lr.Refusal
		s := "refused by a " + r.Message
		if r.ErrorCode != nil {
			s += fmt.Sprintf(", error code %d", *r.ErrorCode)
		}
		if r.Text != "" {
			s += fmt.Sprintf(": %q", r.Text)
		}
		if r.Fields != "" {
			s += " (" + r.Fields + ")"
		}
		return s
	}
	return "failed: " + lr.Error
}

func writeLogonText(w io.Writer, lr *probe.LogonResult) {
	fmt.Fprintf(w, "logon to %s as %s, signature time %s", lr.Endpoint, lr.SenderCompID, lr.SignatureTime)
	if lr.SignedSendingTime != "" {
		fmt.Fprintf(w, " (signed %q)", lr.SignedSendingTime)
	}
	fmt.Fprintln(w)
	writeMessages(w, "  ", lr.Messages)
	fmt.Fprintln(w, logonOutcome(lr))
}

func writeLogon(w io.Writer, lr *probe.LogonResult, asJSON bool) error {
	if asJSON {
		return writeJSON(w, lr)
	}
	writeLogonText(w, lr)
	return nil
}

func writeAuto(w io.Writer, res *probe.AutoResult, asJSON bool) error {
	if asJSON {
		return writeJSON(w, res)
	}
	for i, lr := range res.Attempts {
		fmt.Fprintf(w, "attempt %d: ", i+1)
		writeLogonText(w, lr)
		fmt.Fprintln(w)
	}
	switch res.Accepted {
	case "":
		fmt.Fprintf(w, "no signature time accepted: %s\n", res.Error)
	case binancefix.SignatureTimeMicros.String():
		fmt.Fprintf(w, "Binance accepted signature time %s, the client's default\n", res.Accepted)
	default:
		fmt.Fprintf(w, "Binance accepted signature time %s, not %s, the client's default (settled on the testnet on 2026-10-17): Binance changed; make %s the default (see the README)\n",
			res.Accepted, binancefix.SignatureTimeMicros, res.Accepted)
	}
	return nil
}

func writeReport(w io.Writer, rep *probe.Report, asJSON bool) error {
	if asJSON {
		return writeJSON(w, rep)
	}
	price := rep.PriceFar
	if price == "" {
		price = "market only (no --price-far)"
	}
	fmt.Fprintf(w, "probe %s as %s, signature time %s: %s, orders of %s at %s\n", rep.Endpoint, rep.SenderCompID, rep.SignatureTime,
		rep.Symbol, rep.Qty, price)
	if rep.Logon != nil {
		fmt.Fprintf(w, "logon: %s\n", logonOutcome(rep.Logon))
	}
	count := map[probe.Verdict]int{}
	for _, r := range rep.Results {
		count[r.Verdict]++
		fmt.Fprintf(w, "\n(%s) %s: %s\n    %s\n", r.Letter, r.Probe, strings.ToUpper(string(r.Verdict)), r.Assumption)
		for _, c := range r.Checks {
			fmt.Fprintf(w, "  %-12s %s: %s\n", c.Verdict, c.Name, c.Detail)
		}
		fmt.Fprintln(w, "  messages:")
		writeMessages(w, "    ", r.Messages)
	}
	if len(rep.Cleanup) > 0 {
		fmt.Fprintln(w, "\ncleanup:")
		for _, c := range rep.Cleanup {
			fmt.Fprintf(w, "  %s (%s): %s", c.ClOrdID, c.Probe, c.Outcome)
			if c.Detail != "" {
				fmt.Fprintf(w, ", %s", c.Detail)
			}
			fmt.Fprintln(w)
		}
	}
	if len(rep.Leftover) > 0 {
		fmt.Fprintf(w, "\nWARNING: orders possibly left open, to cancel by hand (https://testnet.binance.vision): %s\n", strings.Join(rep.Leftover, ", "))
	}
	if rep.Position != "" {
		fmt.Fprintf(w, "\nWARNING: position not traded back, to trade by hand (https://testnet.binance.vision): %s\n", rep.Position)
	}
	fmt.Fprintf(w, "\nsummary: %d confirmed, %d contradicted, %d inconclusive\n", count[probe.Confirmed], count[probe.Contradicted], count[probe.Inconclusive])
	if rep.Error != "" {
		fmt.Fprintf(w, "error: %s\n", rep.Error)
	}
	return nil
}
