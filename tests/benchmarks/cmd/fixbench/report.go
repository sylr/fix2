package main

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// key identifies the runs of a window and pair.
type key struct {
	window int
	pair   pair
}

// stats are a run's figures. Times are in nanoseconds.
type stats struct {
	throughput float64 // orders acknowledged per second
	nsPerOp    float64 // elapsed time per order
	p50, p90   float64 // round trip quantiles
	p99, p999  float64
	max        float64
	accCPU     float64 // CPU time per order, acceptor
	iniCPU     float64 // and initiator
	accRSS     float64 // maximum resident set size, bytes
	iniRSS     float64
}

func (r result) stats(n int) stats {
	return stats{
		throughput: float64(n) / r.elapsed.Seconds(),
		nsPerOp:    float64(r.elapsed.Nanoseconds()) / float64(n),
		p50:        quantile(r.rtt, 0.50),
		p90:        quantile(r.rtt, 0.90),
		p99:        quantile(r.rtt, 0.99),
		p999:       quantile(r.rtt, 0.999),
		max:        float64(r.rtt[len(r.rtt)-1]),
		accCPU:     float64(r.accCPU.Nanoseconds()) / float64(n),
		iniCPU:     float64(r.iniCPU.Nanoseconds()) / float64(n),
		accRSS:     float64(r.accMaxRS),
		iniRSS:     float64(r.iniMaxRS),
	}
}

// quantile returns the q quantile of sorted, by the nearest rank.
func quantile(sorted []int64, q float64) float64 {
	i := int(math.Ceil(q*float64(len(sorted)))) - 1
	return float64(sorted[max(0, min(i, len(sorted)-1))])
}

// median returns the median of f over runs.
func median(runs []stats, f func(stats) float64) float64 {
	vs := make([]float64, len(runs))
	for i, s := range runs {
		vs[i] = f(s)
	}
	slices.Sort(vs)
	if n := len(vs); n%2 == 0 {
		return (vs[n/2-1] + vs[n/2]) / 2
	}
	return vs[len(vs)/2]
}

// writeHeader writes the configuration lines of the Go benchmark format.
func writeHeader(w io.Writer, cfg *config) {
	fmt.Fprintf(w, "goos: %s\ngoarch: %s\n", runtime.GOOS, runtime.GOARCH)
	if cpu := cpuModel(); cpu != "" {
		fmt.Fprintf(w, "cpu: %s\n", cpu)
	}
	fmt.Fprintf(w, "pkg: github.com/sylr/fix2/tests/benchmarks\n")
	fmt.Fprintf(w, "messages: %d\nwarmup: %d\n", cfg.messages, cfg.warmup)
	fmt.Fprintf(w, "dictionary: %s\n", dictName(cfg.dict))
	for _, im := range cfg.impls {
		fmt.Fprintf(w, "%s: %s\n", im.name, im.version)
	}
}

// writeRun writes a run in the Go benchmark format.
func writeRun(w io.Writer, k key, n int, s stats) {
	fmt.Fprintf(w, "BenchmarkOrderAck/window=%d/acceptor=%s/initiator=%s\t%d\t%.1f ns/op\t%.0f msgs/s"+
		"\t%.0f p50-ns\t%.0f p90-ns\t%.0f p99-ns\t%.0f p99.9-ns\t%.0f max-ns"+
		"\t%.1f acceptor-cpu-ns/op\t%.1f initiator-cpu-ns/op\t%.0f acceptor-maxrss-B\t%.0f initiator-maxrss-B\n",
		k.window, k.pair.acceptor.name, k.pair.initiator.name, n, s.nsPerOp, s.throughput,
		s.p50, s.p90, s.p99, s.p999, s.max, s.accCPU, s.iniCPU, s.accRSS, s.iniRSS)
}

// writeSummary writes, for each window, the medians of each pair's runs.
func writeSummary(w io.Writer, cfg *config, pairs []pair, results map[key][]result) {
	fmt.Fprintf(w, "\n%s/%s", runtime.GOOS, runtime.GOARCH)
	if cpu := cpuModel(); cpu != "" {
		fmt.Fprintf(w, ", %s", cpu)
	}
	fmt.Fprintf(w, ", %d CPUs; dictionary %s; %d orders per run after %d of warm-up; medians of %d runs\n",
		runtime.NumCPU(), dictName(cfg.dict), cfg.messages, cfg.warmup, cfg.runs)
	for _, im := range cfg.impls {
		fmt.Fprintf(w, "%s: %s\n", im.name, im.version)
	}
	for _, win := range cfg.windows {
		if !slices.ContainsFunc(pairs, func(p pair) bool { return len(results[key{window: win, pair: p}]) > 0 }) {
			continue
		}
		fmt.Fprintf(w, "\n### window %d\n\n", win)
		fmt.Fprintln(w, "| acceptor | initiator | orders/s (min–max) | vs baseline | p50 | p90 | p99 | p99.9 | max | CPU/order acceptor | CPU/order initiator | max RSS acceptor | max RSS initiator |")
		fmt.Fprintln(w, "|---|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|")
		runsOf := func(p pair) []stats {
			var runs []stats
			for _, r := range results[key{window: win, pair: p}] {
				runs = append(runs, r.stats(cfg.messages))
			}
			return runs
		}
		throughput := func(s stats) float64 { return s.throughput }
		// The baseline pair is the first; a matrix stopped early may lack it.
		vs := func(float64) string { return "–" }
		if runs := runsOf(pairs[0]); len(runs) > 0 {
			base := median(runs, throughput)
			vs = func(t float64) string { return fmt.Sprintf("%.2fx", t/base) }
		}
		for _, p := range pairs {
			runs := runsOf(p)
			if len(runs) == 0 {
				continue
			}
			med := func(f func(stats) float64) float64 { return median(runs, f) }
			tput := med(throughput)
			lo, hi := math.Inf(1), math.Inf(-1)
			for _, s := range runs {
				lo, hi = min(lo, s.throughput), max(hi, s.throughput)
			}
			fmt.Fprintf(w, "| %s | %s | %.0f (%.0f–%.0f) | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n",
				p.acceptor.name, p.initiator.name, tput, lo, hi, vs(tput),
				fmtNs(med(func(s stats) float64 { return s.p50 })),
				fmtNs(med(func(s stats) float64 { return s.p90 })),
				fmtNs(med(func(s stats) float64 { return s.p99 })),
				fmtNs(med(func(s stats) float64 { return s.p999 })),
				fmtNs(med(func(s stats) float64 { return s.max })),
				fmtNs(med(func(s stats) float64 { return s.accCPU })),
				fmtNs(med(func(s stats) float64 { return s.iniCPU })),
				fmtBytes(med(func(s stats) float64 { return s.accRSS })),
				fmtBytes(med(func(s stats) float64 { return s.iniRSS })))
		}
	}
}

func dictName(path string) string {
	if path == "" {
		return "none"
	}
	return filepath.Base(path)
}

func fmtNs(ns float64) string {
	switch {
	case ns < 1e3:
		return fmt.Sprintf("%.0fns", ns)
	case ns < 1e6:
		return fmt.Sprintf("%.1fµs", ns/1e3)
	case ns < 1e9:
		return fmt.Sprintf("%.2fms", ns/1e6)
	default:
		return fmt.Sprintf("%.2fs", ns/1e9)
	}
}

func fmtBytes(b float64) string { return fmt.Sprintf("%.0f MiB", b/(1<<20)) }

// cpuModel returns the processor's name, "" if unknown.
func cpuModel() string {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	case "linux":
		f, err := os.Open("/proc/cpuinfo")
		if err != nil {
			return ""
		}
		defer f.Close()
		s := bufio.NewScanner(f)
		for s.Scan() {
			if k, v, ok := strings.Cut(s.Text(), ":"); ok && strings.TrimSpace(k) == "model name" {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}
