// Command fix2-jepsen-node runs one fix2 session, initiator or acceptor,
// for the Jepsen suite of docs/jepsen.md: it logs what the session and its
// store do to an event log, and takes orders from the test over HTTP.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sylr/fix2/fix/session"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "fix2-jepsen-node:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("fix2-jepsen-node", flag.ContinueOnError)
	var opts Options
	var httpAddr, mutants, level, ackProbe string
	fs.StringVar(&opts.Role, "role", "", "initiator or acceptor")
	fs.StringVar(&opts.Listen, "listen", ":9876", "acceptor: the FIX address to listen on")
	fs.StringVar(&opts.Connect, "connect", "", "initiator: the acceptor's FIX address")
	fs.StringVar(&opts.Store, "store", "memstore", "memstore, or sqlite:<path>")
	fs.StringVar(&opts.Events, "events", ".", "directory of the event logs")
	fs.StringVar(&opts.Incarnation, "incarnation", "", "names this process's event log")
	fs.DurationVar(&opts.HeartBtInt, "heartbeat", time.Second, "HeartBtInt")
	fs.DurationVar(&opts.MaxLatency, "max-latency", 0, "MaxLatency, the session's default if 0")
	fs.DurationVar(&opts.StoreTimeout, "store-timeout", 0, "StoreTimeout, the session's default if 0")
	fs.DurationVar(&opts.WriteTimeout, "write-timeout", 0, "WriteTimeout, the session's default if 0")
	fs.StringVar(&ackProbe, "ack-probe", "lazy", "AckProbe: lazy, eager or never")
	fs.DurationVar(&opts.Retention.Interval, "retention-interval", 0, "Retention.Interval: 0 keeps every message")
	fs.IntVar(&opts.Retention.Keep, "retention-keep", 0, "Retention.Keep")
	fs.DurationVar(&opts.Retention.MinAge, "retention-min-age", 0, "Retention.MinAge")
	fs.IntVar(&opts.Retention.Max, "retention-max", 0, "Retention.Max, 0 for no bound")
	fs.BoolVar(&opts.LastProcessed, "last-processed", false, "send LastMsgSeqNumProcessed (369)")
	fs.IntVar(&opts.Sessions, "sessions", 1, "sessions run, the k-th INI<k>/ACC<k> (the first INI/ACC), its store's file with -s<k> before the extension")
	fs.IntVar(&opts.MaxWorkers, "max-workers", 256, "sends running at once")
	fs.DurationVar(&opts.FromAppDelay, "fromapp-delay", 0, "how long FromApp takes for each message")
	fs.StringVar(&httpAddr, "http", ":8080", "the control API's address")
	fs.StringVar(&mutants, "mutant", "", "comma-separated: no-sync, drop-deliver, redeliver")
	fs.StringVar(&level, "log-level", "info", "debug, info, warn or error")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if opts.Incarnation == "" {
		opts.Incarnation = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	switch ackProbe {
	case "lazy":
		opts.AckProbe = session.AckProbeLazy
	case "eager":
		opts.AckProbe = session.AckProbeEager
	case "never":
		opts.AckProbe = session.AckProbeNever
	default:
		return fmt.Errorf("ack-probe %q: want lazy, eager or never", ackProbe)
	}
	if mutants != "" {
		opts.Mutants = strings.Split(mutants, ",")
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return err
	}
	opts.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))

	// The control API is bound first: a node the test cannot reach must not
	// run.
	ln, err := net.Listen("tcp", httpAddr)
	if err != nil {
		return err
	}
	n, err := NewHost(opts)
	if err != nil {
		ln.Close()
		return err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	srv := &http.Server{Handler: n.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			cancel(fmt.Errorf("control API: %w", err))
		}
	}()
	err = n.Run(ctx)
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		err = cause
	}
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	srv.Shutdown(sctx)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
