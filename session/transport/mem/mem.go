// Package mem provides in-memory session connections: reliable, ordered and
// buffered like TCP, with deadlines, for tests and for sessions within one
// process.
package mem

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/sylr/fix2/session/transport"
)

// maxBuffered bounds the bytes in flight in one direction: a writer waits
// beyond, as on a TCP connection whose peer does not read.
const maxBuffered = 1 << 20

// addr is a mem address.
type addr string

func (a addr) Network() string { return "mem" }
func (a addr) String() string  { return string(a) }

// pipe is one direction of a connection.
type pipe struct {
	mu     sync.Mutex
	buf    []byte
	closed bool          // closed by the writer: reads drain then EOF
	broken bool          // closed by the reader: writes fail
	signal chan struct{} // closed and replaced on every change
}

func newPipe() *pipe { return &pipe{signal: make(chan struct{})} }

func (p *pipe) changed() {
	close(p.signal)
	p.signal = make(chan struct{})
}

// conn is one end of a connection.
type conn struct {
	in, out       *pipe
	local, remote addr

	mu            sync.Mutex
	readDeadline  time.Time
	writeDeadline time.Time
	deadline      chan struct{} // closed and replaced when a deadline changes
	closed        chan struct{}
	closeOnce     sync.Once
}

// Pipe returns the two ends of a new connection.
func Pipe() (transport.Conn, transport.Conn) {
	a, b := newPipe(), newPipe()
	c1 := &conn{in: a, out: b, local: "mem:1", remote: "mem:2", deadline: make(chan struct{}), closed: make(chan struct{})}
	c2 := &conn{in: b, out: a, local: "mem:2", remote: "mem:1", deadline: make(chan struct{}), closed: make(chan struct{})}
	return c1, c2
}

// wait returns when p changes, the deadline passes or changes, or c closes.
func (c *conn) wait(signal <-chan struct{}, deadline func() time.Time) error {
	c.mu.Lock()
	d, dl := deadline(), c.deadline
	c.mu.Unlock()
	var timer <-chan time.Time
	if !d.IsZero() {
		wait := time.Until(d)
		if wait <= 0 {
			return os.ErrDeadlineExceeded
		}
		t := time.NewTimer(wait)
		defer t.Stop()
		timer = t.C
	}
	select {
	case <-signal:
	case <-dl:
	case <-timer:
		return os.ErrDeadlineExceeded
	case <-c.closed:
		return net.ErrClosed
	}
	return nil
}

func (c *conn) Read(b []byte) (int, error) {
	for {
		select {
		case <-c.closed:
			return 0, net.ErrClosed
		default:
		}
		p := c.in
		p.mu.Lock()
		if len(p.buf) > 0 {
			n := copy(b, p.buf)
			p.buf = p.buf[n:]
			p.changed()
			p.mu.Unlock()
			return n, nil
		}
		if p.closed {
			p.mu.Unlock()
			return 0, io.EOF
		}
		signal := p.signal
		p.mu.Unlock()
		if err := c.wait(signal, func() time.Time { return c.readDeadline }); err != nil {
			return 0, err
		}
	}
}

func (c *conn) Write(b []byte) (int, error) {
	written := 0
	for written < len(b) {
		select {
		case <-c.closed:
			return written, net.ErrClosed
		default:
		}
		p := c.out
		p.mu.Lock()
		if p.broken || p.closed {
			p.mu.Unlock()
			return written, errors.New("mem: write to a closed connection")
		}
		if room := maxBuffered - len(p.buf); room > 0 {
			n := min(room, len(b)-written)
			p.buf = append(p.buf, b[written:written+n]...)
			written += n
			p.changed()
			p.mu.Unlock()
			continue
		}
		signal := p.signal
		p.mu.Unlock()
		if err := c.wait(signal, func() time.Time { return c.writeDeadline }); err != nil {
			return written, err
		}
	}
	return written, nil
}

// Close closes the connection: the peer reads what was written, then EOF.
func (c *conn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.out.mu.Lock()
		c.out.closed = true
		c.out.changed()
		c.out.mu.Unlock()
		c.in.mu.Lock()
		c.in.broken = true
		c.in.changed()
		c.in.mu.Unlock()
	})
	return nil
}

func (c *conn) setDeadline(read, write bool, t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if read {
		c.readDeadline = t
	}
	if write {
		c.writeDeadline = t
	}
	close(c.deadline)
	c.deadline = make(chan struct{})
}

func (c *conn) SetDeadline(t time.Time) error      { c.setDeadline(true, true, t); return nil }
func (c *conn) SetReadDeadline(t time.Time) error  { c.setDeadline(true, false, t); return nil }
func (c *conn) SetWriteDeadline(t time.Time) error { c.setDeadline(false, true, t); return nil }
func (c *conn) LocalAddr() net.Addr                { return c.local }
func (c *conn) RemoteAddr() net.Addr               { return c.remote }

// Listener accepts in-memory connections dialed with its Dial method.
type Listener struct {
	name    string
	conns   chan transport.Conn
	closed  chan struct{}
	closeMu sync.Once
}

var _ transport.Listener = (*Listener)(nil)

// Listen returns a new Listener named name.
func Listen(name string) *Listener {
	return &Listener{name: name, conns: make(chan transport.Conn), closed: make(chan struct{})}
}

// Dial connects to l, as a transport.Dialer.
func (l *Listener) Dial(ctx context.Context) (transport.Conn, error) {
	a, b := Pipe()
	select {
	case l.conns <- b:
		return a, nil
	case <-l.closed:
		return nil, net.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Accept implements transport.Listener.
func (l *Listener) Accept(ctx context.Context) (transport.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Addr implements transport.Listener.
func (l *Listener) Addr() net.Addr { return addr(l.name) }

// Close implements transport.Listener.
func (l *Listener) Close() error {
	l.closeMu.Do(func() { close(l.closed) })
	return nil
}
