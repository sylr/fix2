// Command fixbench runs the benchmark matrix: for each window, each pair of
// agents, an acceptor and an initiator, each in a process of its own, runs
// the scenario over TCP on the loopback, several times. The pairs are the
// baseline acceptor and initiator, then for each other implementation X: X
// against X, the baseline against X, X against the baseline. Each round
// runs every window and pair once, the pairs in an order of their own, so
// that drift (thermal throttling, other load) spreads over all of them.
//
// It prints a summary per window and, with -o, every run in the Go
// benchmark format, for benchstat.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// maxWindow is the largest window: see -windows.
const maxWindow = 10000

// impl is an implementation's agent.
type impl struct {
	name, path string
	version    string
}

// pair is an acceptor and an initiator.
type pair struct{ acceptor, initiator *impl }

func (p pair) String() string { return p.acceptor.name + "/" + p.initiator.name }

type config struct {
	impls     []*impl
	baseline  string
	dict      string
	scenarios []string
	messages  int
	warmup    int
	windows   []int
	runs      int
	timeout   time.Duration
	seed      uint64
	output    string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fixbench:", err)
		os.Exit(1)
	}
}

func run() error {
	var cfg config
	var scenarios, windows string
	flag.Func("impl", "an implementation's agent, `name=path`; repeatable (default fix2, quickfixgo, quickfixj and quickfixcpp, in bin/)", func(v string) error {
		name, path, ok := strings.Cut(v, "=")
		if !ok || name == "" || path == "" {
			return errors.New("want name=path")
		}
		cfg.impls = append(cfg.impls, &impl{name: name, path: path})
		return nil
	})
	flag.StringVar(&cfg.baseline, "baseline", "fix2", "the implementation the others are compared to")
	flag.StringVar(&cfg.dict, "dict", "spec/FIX44.xml", "QuickFIX FIX 4.4 dictionary both ends validate with, the one the agents' code is generated from")
	flag.StringVar(&scenarios, "scenarios", "orders,marketdata", "scenarios to run, comma separated: orders, marketdata")
	flag.IntVar(&cfg.messages, "messages", 50000, "requests measured per run")
	flag.IntVar(&cfg.warmup, "warmup", 50000, "requests sent before measuring, each run")
	flag.StringVar(&windows, "windows", "1,10,100,1000", "requests awaiting their answer at most, comma separated")
	flag.IntVar(&cfg.runs, "runs", 5, "runs of each window and pair")
	flag.DurationVar(&cfg.timeout, "timeout", 5*time.Minute, "bound of each run")
	flag.Uint64Var(&cfg.seed, "seed", 1, "seed of the order of the pairs in each round")
	flag.StringVar(&cfg.output, "o", "", "write every run to `file`, in the Go benchmark format")
	flag.Parse()
	if flag.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %q", flag.Args())
	}

	if len(cfg.impls) == 0 {
		cfg.impls = []*impl{{name: "fix2", path: "bin/fix2"}, {name: "quickfixgo", path: "bin/quickfixgo"}, {name: "quickfixj", path: "bin/quickfixj"}, {name: "quickfixcpp", path: "bin/quickfixcpp"}}
	}
	for s := range strings.SplitSeq(scenarios, ",") {
		s = strings.TrimSpace(s)
		if _, ok := scenarioNames[s]; !ok {
			return fmt.Errorf("-scenarios: unknown scenario %q", s)
		}
		if !slices.Contains(cfg.scenarios, s) {
			cfg.scenarios = append(cfg.scenarios, s)
		}
	}
	for w := range strings.SplitSeq(windows, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(w))
		if err != nil || n <= 0 {
			return fmt.Errorf("-windows: invalid window %q", w)
		}
		// fix2's acceptor sends from its callback, which never waits: its
		// queue of messages to write, 10000 by default, bounds the window.
		if n > maxWindow {
			return fmt.Errorf("-windows: %d is above %d, the most fix2's default queues serve", n, maxWindow)
		}
		if !slices.Contains(cfg.windows, n) {
			cfg.windows = append(cfg.windows, n)
		}
	}
	if cfg.messages <= 0 || cfg.warmup < 0 || cfg.runs <= 0 {
		return errors.New("-messages and -runs must be > 0, -warmup >= 0")
	}
	// Every agent validates with the dictionary; the QuickFIX engines also
	// need it to parse repeating groups.
	if _, err := os.Stat(cfg.dict); err != nil {
		return fmt.Errorf("-dict: %w", err)
	}
	for _, im := range cfg.impls {
		v, err := exec.Command(im.path, "version").Output()
		if err != nil {
			var stderr string
			if ee, ok := errors.AsType[*exec.ExitError](err); ok {
				stderr = strings.TrimSpace(string(ee.Stderr))
			}
			return fmt.Errorf("agent %s (%s): %w: %s; build the agents with make build, in nix develop", im.name, im.path, err, stderr)
		}
		im.version = strings.TrimSpace(string(v))
	}
	pairs, err := matrix(cfg.impls, cfg.baseline)
	if err != nil {
		return err
	}

	var out *os.File
	if cfg.output != "" {
		if out, err = os.Create(cfg.output); err != nil {
			return err
		}
		defer out.Close()
		writeHeader(out, &cfg)
	}

	ctx := context.Background()
	results := map[key][]result{}
	total, done := cfg.runs*len(cfg.scenarios)*len(cfg.windows)*len(pairs), 0
	for round := range cfg.runs {
		for si, sc := range cfg.scenarios {
			for _, w := range cfg.windows {
				order := append([]pair(nil), pairs...)
				rng := rand.New(rand.NewPCG(cfg.seed, uint64(round)<<40|uint64(si)<<32|uint64(w)))
				rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
				for _, p := range order {
					done++
					k := key{scenario: sc, window: w, pair: p}
					r, err := runOnce(ctx, &cfg, k)
					if err != nil {
						// A failed run invalidates the matrix: stop, telling
						// what was measured so far.
						writeSummary(os.Stdout, &cfg, pairs, results)
						return fmt.Errorf("%s, window %d, %s, round %d: %w", sc, w, p, round+1, err)
					}
					results[k] = append(results[k], r)
					s := r.stats(cfg.messages)
					fmt.Fprintf(os.Stderr, "[%d/%d] %s window=%d %s: %.0f msgs/s, p50 %s, p99 %s\n",
						done, total, sc, w, p, s.throughput, fmtNs(s.p50), fmtNs(s.p99))
					if out != nil {
						writeRun(out, k, cfg.messages, s)
					}
				}
			}
		}
	}
	writeSummary(os.Stdout, &cfg, pairs, results)
	return nil
}

// matrix returns the pairs to run: the baseline against itself, then for
// each other implementation X, X against X, the baseline acceptor against
// the X initiator and the X acceptor against the baseline initiator.
func matrix(impls []*impl, baseline string) ([]pair, error) {
	var base *impl
	seen := map[string]bool{}
	for _, im := range impls {
		if seen[im.name] {
			return nil, fmt.Errorf("-impl %s given twice", im.name)
		}
		seen[im.name] = true
		if im.name == baseline {
			base = im
		}
	}
	if base == nil {
		return nil, fmt.Errorf("-baseline %s is not an -impl", baseline)
	}
	pairs := []pair{{base, base}}
	for _, im := range impls {
		if im != base {
			pairs = append(pairs, pair{im, im}, pair{base, im}, pair{im, base})
		}
	}
	return pairs, nil
}

// scenarioNames are the scenarios, by the name agents know them by, with
// the name of their benchmarks.
var scenarioNames = map[string]string{
	"orders":     "Orders",
	"marketdata": "MarketData",
}
