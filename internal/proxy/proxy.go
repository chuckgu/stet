// Package proxy relays the Postgres wire protocol between clients and an
// upstream Postgres.
//
// Messages are framed and forwarded as raw bytes. Only the messages Stet needs
// to inspect are decoded, so anything the proxy does not understand still
// passes through unchanged.
package proxy

import (
	"bufio"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"runtime/debug"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

const (
	sslRequestCode    = 80877103
	gssEncRequestCode = 80877104
	cancelRequestCode = 80877102

	// Postgres refuses startup packets larger than this (MAX_STARTUP_PACKET_LENGTH).
	maxStartupLen = 10000

	fatalTimeout = 5 * time.Second // to write the error of a session the proxy ends
	bufSize      = 32 << 10
)

// startupTimeout bounds what a client sends before its session starts: the
// encryption requests, a TLS handshake and the startup packet, on one clock.
// A variable only so tests can shorten it (SetStartupTimeout).
var startupTimeout = 10 * time.Second

type Server struct {
	// ClientTLS, when set, is what the proxy answers a client's SSLRequest
	// with. nil: the request is declined and the client goes on in plaintext.
	ClientTLS *tls.Config

	up     Upstream
	log    *slog.Logger
	gate   Gate
	policy Policy

	mu     sync.Mutex
	ln     net.Listener
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
}

// New returns a proxy that forwards clients to the server, reached as up
// says. The gate is shown the sessions policy watches; the others, and every
// session when gate is nil, are a plain passthrough.
func New(up Upstream, log *slog.Logger, gate Gate, policy Policy) *Server {
	return &Server{up: up, log: log, gate: gate, policy: policy, conns: make(map[net.Conn]struct{})}
}

// Serve accepts connections on ln until Close is called, then returns nil.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ln.Close()
	}
	s.ln = ln
	s.mu.Unlock()

	for {
		c, err := ln.Accept()
		if err != nil {
			if s.isClosed() {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			s.log.Warn("accept failed", "err", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if !s.track(c) {
			c.Close()
			continue
		}
		go func() {
			defer s.wg.Done()
			defer s.untrack(c)
			// A panic in a session's taps ends that session in relay; this
			// keeps one elsewhere (abandon reporting, say) to the session too.
			defer func() {
				if v := recover(); v != nil {
					s.log.Error("connection ended by a panic", "remote", c.RemoteAddr(), "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
				}
			}()
			s.handle(c)
		}()
	}
}

// Close stops accepting, drops every open session on both sides, and waits
// for their goroutines to finish.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	var err error
	if s.ln != nil {
		err = s.ln.Close()
	}
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
	return err
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// track registers a connection so Close can drop it. Each tracked connection
// holds one wg count until untracked.
func (s *Server) track(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conns[c] = struct{}{}
	s.wg.Add(1)
	return true
}

func (s *Server) untrack(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

// handle serves one client connection, c. Server.Close ends it by closing c,
// the TCP connection, also when the session runs over TLS on top of it.
func (s *Server) handle(c net.Conn) {
	defer c.Close()

	client, pkt, err := readStartup(c, s.ClientTLS)
	if err != nil {
		s.log.Debug("startup failed", "remote", c.RemoteAddr(), "err", err)
		return
	}
	defer client.Close() // over TLS, says close_notify first
	_, clientTLS := client.(*tls.Conn)
	if binary.BigEndian.Uint32(pkt[4:8]) == cancelRequestCode {
		s.forwardCancel(pkt)
		return
	}

	server, secure, release, err := s.dialUpstream()
	if err != nil {
		if s.isClosed() { // Close ended the dial, and closed the client too
			return
		}
		s.log.Warn("upstream unreachable", "upstream", s.up.Addr, "err", err)
		writeFatal(client, "08001", "stet: "+err.Error())
		return
	}
	defer release()
	defer server.Close()

	if _, err := server.Write(pkt); err != nil {
		return
	}
	start := time.Now()
	s.log.Debug("session started", "remote", client.RemoteAddr(), "upstream_tls", secure, "client_tls", clientTLS)
	// Over a TLS upstream the server offers SCRAM-SHA-256-PLUS, which the
	// client, on a plaintext leg, cannot use: authTap takes it out. A client
	// on TLS gets the list as the server sent it (spec-v0.2-server 3).
	takePlusOut := secure && !clientTLS
	if ss := s.watch(pkt); ss == nil {
		var fromServer *tap
		var ended func() []byte
		if takePlusOut {
			fromServer, ended = authTap(nil)
		}
		relay(client, server, nil, fromServer, ended, nil, nil)
	} else {
		fromServer, ended := ss.serverTap(), ss.ended
		if takePlusOut {
			var refused func() []byte
			fromServer, refused = authTap(fromServer)
			ended = func() []byte {
				if fatal := refused(); fatal != nil {
					return fatal
				}
				return ss.ended()
			}
		}
		relay(client, server, ss.clientTap(), fromServer, ended, func(v any, stack []byte) {
			s.log.Error("session ended by a panic", "session", ss.info.ID, "login_role", ss.info.User,
				"database", ss.info.Database, "agent", ss.info.ApplicationName, "panic", fmt.Sprint(v), "stack", string(stack))
		}, ss.stuck)
		ss.abandon()
	}
	s.log.Debug("session ended", "remote", client.RemoteAddr(), "duration", time.Since(start))
}

// watch returns the session to tap when the gate is shown the session that
// startup packet pkt opens, and nil when the proxy only relays it.
func (s *Server) watch(pkt []byte) *session {
	if s.gate == nil {
		return nil
	}
	info, ok := startupInfo(pkt)
	if !s.policy.watches(info, ok) {
		return nil
	}
	info.ID = newSessionID()
	ss := newSession(s.gate, info)
	ss.enforce = s.policy.Enforce
	ss.log = s.log
	return ss
}

// dialUpstream is s.up.dial with the TCP connection tracked from the moment it
// opens, so Close also ends a dial still waiting on the server's answer to
// the TLS request or its handshake. release, called once conn is closed,
// gives up that tracking.
//
// When the dial does not succeed, the tracking is given up here, from a
// defer, so also when dial panics (in a certificate check, say): the tracked
// connection holds a count in s.wg, which Close would wait for forever. The
// connection is closed first, which dial does not do when it panics.
func (s *Server) dialUpstream() (conn net.Conn, secure bool, release func(), err error) {
	var raw net.Conn
	untrack := func() {
		if raw != nil {
			s.untrack(raw)
			s.wg.Done()
		}
	}
	dialed := false
	defer func() {
		if !dialed {
			if raw != nil {
				raw.Close()
			}
			untrack()
		}
	}()
	up := s.up
	up.opened = func(c net.Conn) bool {
		if !s.track(c) {
			return false
		}
		raw = c
		return true
	}
	conn, secure, err = up.dial()
	if err != nil {
		return nil, false, nil, err
	}
	dialed = true
	return conn, secure, untrack, nil
}

// forwardCancel passes a CancelRequest to upstream on a fresh connection,
// over TLS as a session would be. Backend keys reach the client unchanged,
// so no translation is needed.
func (s *Server) forwardCancel(pkt []byte) {
	c, _, release, err := s.dialUpstream()
	if err != nil {
		if !s.isClosed() {
			s.log.Warn("cancel not forwarded", "upstream", s.up.Addr, "err", err)
		}
		return
	}
	defer release()
	defer c.Close()
	if _, err := c.Write(pkt); err != nil {
		return
	}
	// Postgres closes the connection after reading the request. Waiting for
	// that keeps the client's view of "cancel sent" honest.
	c.SetReadDeadline(time.Now().Add(dialTimeout))
	io.Copy(io.Discard, c)
}

// readStartup returns the client's StartupMessage or CancelRequest packet,
// and the connection to go on with: c, or TLS over c.
//
// Without cfg, SSL and GSS encryption requests are declined (N) so the client
// continues in plaintext. With cfg, an SSLRequest is answered S and TLS is
// set up with cfg; what follows is read inside TLS, where another SSLRequest
// ends the connection. A GSSENCRequest is always declined. With cfg, a
// session (not a cancel request) that starts without TLS from an address
// other than loopback is refused with a FATAL. The handshake shares the one
// startupTimeout.
func readStartup(c net.Conn, cfg *tls.Config) (net.Conn, []byte, error) {
	c.SetDeadline(time.Now().Add(startupTimeout))
	defer c.SetDeadline(time.Time{})

	conn := c
	for range 3 { // GSSENCRequest, SSLRequest, then the real packet
		pkt, err := readStartupPacket(conn)
		if err != nil {
			return nil, nil, err
		}
		switch code := binary.BigEndian.Uint32(pkt[4:8]); {
		case code == sslRequestCode && cfg != nil:
			if conn != c {
				return nil, nil, errTLSTwice
			}
			if conn, err = startClientTLS(c, cfg); err != nil {
				return nil, nil, err
			}
		case code == sslRequestCode, code == gssEncRequestCode:
			if _, err := conn.Write([]byte{'N'}); err != nil {
				return nil, nil, err
			}
		case code == cancelRequestCode:
			return conn, pkt, nil
		case code>>16 == 3:
			if cfg != nil && conn == c && !plaintextAllowed(c.RemoteAddr()) {
				writeFatal(c, "28000", needsTLS)
				return nil, nil, errNeedsTLS
			}
			return conn, pkt, nil
		default:
			return nil, nil, fmt.Errorf("unsupported protocol code %d", code)
		}
	}
	return nil, nil, errors.New("too many encryption requests")
}

func readStartupPacket(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n < 8 || n > maxStartupLen {
		return nil, fmt.Errorf("invalid startup packet length %d", n)
	}
	pkt := make([]byte, n)
	copy(pkt, hdr[:])
	if _, err := io.ReadFull(r, pkt[4:]); err != nil {
		return nil, err
	}
	return pkt, nil
}

// stuckTick is how often relay looks, after a panic, whether the other side
// can still stop (see relay). A variable only so tests can shorten it.
var stuckTick = time.Second

// relay copies messages both ways until either side closes. Taps may be nil.
//
// When a side stops because the proxy itself ends the session (a tap's see
// returned errEnded), the server connection is closed and the other side
// stopped, and then ended, when set, returns the error to give the client:
// it is the last thing the client reads. A message the server was sending
// when its connection closed may reach the client cut short, ahead of it.
//
// A panic in a tap ends the session there (panicked, when set, is told of it
// with its stack): both connections are closed at once, so the message the
// tap was inspecting, and anything queued behind it, is never forwarded, and
// the client gets no error from Stet; ended is not asked. The other side then
// stops at its next read or write, unless the tap panicked holding the
// session's lock and the other side waits for that lock: it never stops.
// stuck, when set, reports that state (see session.stuck): relay looks every
// stuckTick, and stops waiting once it finds it at two looks in a row. That
// side's goroutine is then left parked for the life of the process; it holds
// no count in Server.wg, so Close does not wait for it. Either way the caller
// reports what was pending (abandon) once relay returns.
func relay(client, server net.Conn, fromClient, fromServer *tap, ended func() []byte, panicked func(v any, stack []byte), stuck func() bool) {
	type stop struct {
		err      error
		panicked bool
	}
	done := make(chan stop, 2)
	run := func(dst, src net.Conn, tp *tap) {
		defer func() {
			if v := recover(); v != nil {
				if panicked != nil {
					panicked(v, debug.Stack())
				}
				client.Close()
				server.Close()
				done <- stop{panicked: true}
			}
		}()
		done <- stop{err: pump(dst, src, tp)}
	}
	go run(server, client, fromClient)
	go run(client, server, fromServer)
	first := <-done
	if first.panicked {
		client.Close()
		server.Close()
		tick := time.NewTicker(stuckTick)
		defer tick.Stop()
		for looks := 0; looks < 2; {
			select {
			case <-done:
				return
			case <-tick.C:
				if stuck != nil && stuck() {
					looks++
				} else {
					looks = 0
				}
			}
		}
		return
	}
	if ended == nil || !errors.Is(first.err, errEnded) {
		client.Close()
		server.Close()
		<-done
		return
	}
	server.Close()
	client.SetReadDeadline(time.Now()) // stops the pump reading the client
	// A client that stopped reading does not hold the pump writing to it,
	// nor the error, past the deadline.
	client.SetWriteDeadline(time.Now().Add(fatalTimeout))
	if (<-done).panicked {
		return
	}
	if fatal := ended(); fatal != nil {
		client.Write(fatal)
	}
	client.Close()
}

// tap observes messages in one direction. For message types it wants, see
// gets the whole body before the message is forwarded (nil past
// maxInspect), plus a way to flush what is already queued for the peer. A
// nil want wants nothing.
// each, when set, gets the type of every message as soon as its header is
// read. It runs for every result row, so for a row (DataRow, CopyData) it may
// do no more than increment a counter; for the few other types it acts on
// (the session's completions) it may take a lock.
type tap struct {
	want func(t byte) bool
	// see may return one whole message (type, length, body) to send in
	// place of the one it was shown; nil forwards it unchanged. For a message
	// past maxInspect a replacement makes pump read and discard the body. A
	// non-nil err ends the pump: the messages before it are flushed, and it
	// is not sent.
	see  func(t byte, body []byte, flush func() error) (replace []byte, err error)
	each func(t byte)
}

// maxInspect is the largest message body a tap is shown. A variable only so
// tests can lower it (SetMaxInspect).
var maxInspect int64 = 64 << 20

// pump copies whole protocol messages from src to dst. Writes are batched
// while src already has the next header buffered and flushed before any read
// that may block, so a peer waiting on a response (after Sync, Flush, or
// LISTEN) always gets it.
func pump(dst io.Writer, src io.Reader, tp *tap) error {
	br := bufio.NewReaderSize(src, bufSize)
	bw := bufio.NewWriterSize(dst, bufSize)
	var hdr [5]byte // type byte + int32 length (which counts itself)
	// Never nil, so an empty body that was read is not taken for one past maxInspect.
	body := []byte{}
	for {
		if br.Buffered() < len(hdr) {
			if err := bw.Flush(); err != nil {
				return err
			}
		}
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return err
		}
		n := binary.BigEndian.Uint32(hdr[1:])
		if n < 4 {
			return fmt.Errorf("invalid message length %d", n)
		}
		size := int64(n - 4)

		if tp != nil && tp.each != nil {
			tp.each(hdr[0])
		}
		if tp != nil && tp.want != nil && tp.want(hdr[0]) {
			read := size <= maxInspect // past it, see is shown no body
			var shown []byte
			if read {
				if int64(cap(body)) < size {
					body = make([]byte, size)
				}
				body = body[:size]
				if _, err := io.ReadFull(br, body); err != nil {
					return err
				}
				shown = body
			}
			replace, err := tp.see(hdr[0], shown, bw.Flush)
			if err != nil {
				bw.Flush()
				return err
			}
			switch {
			case replace != nil:
				if !read {
					if _, err := io.CopyN(io.Discard, br, size); err != nil {
						return err
					}
				}
				if _, err := bw.Write(replace); err != nil {
					return err
				}
				continue
			case read:
				if _, err := bw.Write(hdr[:]); err != nil {
					return err
				}
				if _, err := bw.Write(body); err != nil {
					return err
				}
				continue
			}
			// Past maxInspect and not replaced: streamed below.
		}
		if _, err := bw.Write(hdr[:]); err != nil {
			return err
		}
		if _, err := io.CopyN(bw, br, size); err != nil {
			return err
		}
	}
}

func writeFatal(w io.Writer, code, msg string) {
	buf, err := (&pgproto3.ErrorResponse{
		Severity:            "FATAL",
		SeverityUnlocalized: "FATAL",
		Code:                code,
		Message:             msg,
	}).Encode(nil)
	if err == nil {
		w.Write(buf)
	}
}
