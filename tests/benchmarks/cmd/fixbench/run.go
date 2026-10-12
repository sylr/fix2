package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// result is what one run measured.
type result struct {
	elapsed  time.Duration
	rtt      []int64 // sorted
	iniCPU   time.Duration
	accCPU   time.Duration
	iniMaxRS int64 // bytes, over the process's life
	accMaxRS int64
}

// runOnce runs the scenario once: it starts the acceptor, then the
// initiator, which logs on and warms up; it measures the acceptor's CPU
// time around the initiator's measured orders, then stops both.
func runOnce(ctx context.Context, cfg *config, p pair, window int) (result, error) {
	var r result
	ctx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()
	port, err := freePort()
	if err != nil {
		return r, err
	}
	common := []string{"-port", strconv.Itoa(port)}
	if cfg.dict != "" {
		common = append(common, "-dict", cfg.dict)
	}

	acc, err := start(ctx, p.acceptor, "acceptor", common...)
	if err != nil {
		return r, err
	}
	defer acc.kill()
	if _, err := acc.expect(ctx, "READY"); err != nil {
		return r, err
	}
	ini, err := start(ctx, p.initiator, "initiator", append(common,
		"-messages", strconv.Itoa(cfg.messages), "-warmup", strconv.Itoa(cfg.warmup), "-window", strconv.Itoa(window))...)
	if err != nil {
		return r, err
	}
	defer ini.kill()
	if _, err := ini.expect(ctx, "WARM"); err != nil {
		return r, err
	}

	acc0, err := acc.usage(ctx)
	if err != nil {
		return r, err
	}
	if err := ini.say("GO"); err != nil {
		return r, err
	}
	line, err := ini.expect(ctx, "RESULT")
	if err != nil {
		return r, err
	}
	acc1, err := acc.usage(ctx)
	if err != nil {
		return r, err
	}
	var res struct {
		ElapsedNs int64   `json:"elapsed_ns"`
		CPUNs     int64   `json:"cpu_ns"`
		RTTNs     []int64 `json:"rtt_ns"`
	}
	if err := json.Unmarshal([]byte(line), &res); err != nil {
		return r, fmt.Errorf("%s: RESULT: %w", ini, err)
	}
	if len(res.RTTNs) != cfg.messages || res.ElapsedNs <= 0 {
		return r, fmt.Errorf("%s: RESULT of %d round trips over %dns, want %d", ini, len(res.RTTNs), res.ElapsedNs, cfg.messages)
	}

	// Closing its stdin stops an agent: the initiator logs out first.
	if err := ini.stop(ctx); err != nil {
		return r, err
	}
	if err := acc.stop(ctx); err != nil {
		return r, err
	}
	slices.Sort(res.RTTNs)
	r = result{
		elapsed:  time.Duration(res.ElapsedNs),
		rtt:      res.RTTNs,
		iniCPU:   time.Duration(res.CPUNs),
		accCPU:   acc1 - acc0,
		iniMaxRS: ini.maxRSS(),
		accMaxRS: acc.maxRSS(),
	}
	return r, nil
}

// freePort returns a port nothing listens on, on the loopback.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// proc is a running agent, which the driver talks to line by line.
type proc struct {
	im    *impl
	role  string
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines chan string // stdout, closed at its end
}

func (p *proc) String() string { return p.role + " " + p.im.name }

func start(ctx context.Context, im *impl, role string, args ...string) (*proc, error) {
	cmd := exec.CommandContext(ctx, im.path, append([]string{role}, args...)...)
	cmd.Stderr = &prefixWriter{prefix: "[" + role + " " + im.name + "] ", w: os.Stderr}
	cmd.WaitDelay = time.Second
	// An agent may be a script starting the engine: killing it kills its
	// process group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	p := &proc{im: im, role: role, cmd: cmd, stdin: stdin, lines: make(chan string, 4)}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	go func() {
		defer close(p.lines)
		// A RESULT line holds every round trip: no bound on its length.
		br := bufio.NewReader(stdout)
		for {
			l, err := br.ReadString('\n')
			if l = strings.TrimRight(l, "\r\n"); l != "" {
				p.lines <- l
			}
			if err != nil {
				return
			}
		}
	}()
	return p, nil
}

// expect reads the next line, which must be word or start with word and a
// space, and returns what follows the word.
func (p *proc) expect(ctx context.Context, word string) (string, error) {
	select {
	case l, ok := <-p.lines:
		if !ok {
			return "", fmt.Errorf("%s exited before saying %s: %w", p, word, p.wait())
		}
		if l == word {
			return "", nil
		}
		if rest, ok := strings.CutPrefix(l, word+" "); ok {
			return rest, nil
		}
		return "", fmt.Errorf("%s said %.100q, want %s", p, l, word)
	case <-ctx.Done():
		return "", fmt.Errorf("%s: waiting for %s: %w", p, word, ctx.Err())
	}
}

func (p *proc) say(line string) error {
	_, err := io.WriteString(p.stdin, line+"\n")
	return err
}

// usage asks the agent for the CPU time it used.
func (p *proc) usage(ctx context.Context) (time.Duration, error) {
	if err := p.say("USAGE"); err != nil {
		return 0, err
	}
	v, err := p.expect(ctx, "USAGE")
	if err != nil {
		return 0, err
	}
	ns, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: USAGE %q: %w", p, v, err)
	}
	return time.Duration(ns), nil
}

// stop closes the agent's stdin and waits for it to exit, successfully.
func (p *proc) stop(ctx context.Context) error {
	p.stdin.Close()
	for {
		select {
		case l, ok := <-p.lines:
			if !ok {
				if err := p.wait(); err != nil {
					return fmt.Errorf("%s: %w", p, err)
				}
				return nil
			}
			return fmt.Errorf("%s said %.100q while stopping", p, l)
		case <-ctx.Done():
			return fmt.Errorf("%s: stopping: %w", p, ctx.Err())
		}
	}
}

func (p *proc) wait() error {
	if p.cmd.ProcessState != nil {
		if !p.cmd.ProcessState.Success() {
			return errors.New(p.cmd.ProcessState.String())
		}
		return nil
	}
	return p.cmd.Wait()
}

// kill kills the agent if it still runs.
func (p *proc) kill() {
	if p.cmd.ProcessState == nil {
		syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		p.cmd.Wait()
	}
}

// maxRSS returns the agent's maximum resident set size, in bytes.
func (p *proc) maxRSS() int64 {
	ru, ok := p.cmd.ProcessState.SysUsage().(*syscall.Rusage)
	if !ok {
		return 0
	}
	if runtime.GOOS == "darwin" {
		return int64(ru.Maxrss) // bytes
	}
	return int64(ru.Maxrss) * 1024 // KiB
}

// prefixWriter prefixes each line written to w.
type prefixWriter struct {
	prefix string
	w      io.Writer
	mid    bool // the last write ended mid-line
}

func (pw *prefixWriter) Write(b []byte) (int, error) {
	n := len(b)
	for len(b) > 0 {
		if !pw.mid {
			io.WriteString(pw.w, pw.prefix)
			pw.mid = true
		}
		i := strings.IndexByte(string(b), '\n')
		if i < 0 {
			pw.w.Write(b)
			break
		}
		pw.w.Write(b[:i+1])
		b = b[i+1:]
		pw.mid = false
	}
	return n, nil
}
