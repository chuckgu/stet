// Package proxy relays the Postgres wire protocol between clients and an
// upstream Postgres.
//
// Messages are framed and forwarded as raw bytes. Only the messages Stet needs
// to inspect are decoded, so anything the proxy does not understand still
// passes through unchanged.
package proxy

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
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

	startupTimeout = 10 * time.Second
	dialTimeout    = 5 * time.Second
	bufSize        = 32 << 10
)

type Server struct {
	upstream string
	log      *slog.Logger
	gate     Gate
	policy   Policy

	mu     sync.Mutex
	ln     net.Listener
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
}

// New returns a proxy that forwards clients to upstream (host:port). The gate
// is shown the sessions policy watches; the others, and every session when
// gate is nil, are a plain passthrough.
func New(upstream string, log *slog.Logger, gate Gate, policy Policy) *Server {
	return &Server{upstream: upstream, log: log, gate: gate, policy: policy, conns: make(map[net.Conn]struct{})}
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

func (s *Server) handle(client net.Conn) {
	defer client.Close()

	pkt, err := readStartup(client)
	if err != nil {
		s.log.Debug("startup failed", "remote", client.RemoteAddr(), "err", err)
		return
	}
	if binary.BigEndian.Uint32(pkt[4:8]) == cancelRequestCode {
		s.forwardCancel(pkt)
		return
	}

	server, err := net.DialTimeout("tcp", s.upstream, dialTimeout)
	if err != nil {
		s.log.Warn("upstream unreachable", "upstream", s.upstream, "err", err)
		writeFatal(client, "08001", fmt.Sprintf("stet: cannot connect to upstream %s: %v", s.upstream, err))
		return
	}
	if !s.track(server) {
		server.Close()
		return
	}
	defer s.wg.Done()
	defer s.untrack(server)
	defer server.Close()

	if _, err := server.Write(pkt); err != nil {
		return
	}
	start := time.Now()
	s.log.Debug("session started", "remote", client.RemoteAddr())
	if ss := s.watch(pkt); ss == nil {
		relay(client, server, nil, nil)
	} else {
		relay(client, server, ss.clientTap(), ss.serverTap())
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
	return newSession(s.gate, info)
}

// forwardCancel passes a CancelRequest to upstream on a fresh connection.
// Backend keys reach the client unchanged, so no translation is needed.
func (s *Server) forwardCancel(pkt []byte) {
	c, err := net.DialTimeout("tcp", s.upstream, dialTimeout)
	if err != nil {
		s.log.Warn("cancel not forwarded", "upstream", s.upstream, "err", err)
		return
	}
	defer c.Close()
	if _, err := c.Write(pkt); err != nil {
		return
	}
	// Postgres closes the connection after reading the request. Waiting for
	// that keeps the client's view of "cancel sent" honest.
	c.SetReadDeadline(time.Now().Add(dialTimeout))
	io.Copy(io.Discard, c)
}

// readStartup returns the client's StartupMessage or CancelRequest packet.
// SSL and GSS encryption requests are declined so the client continues in
// plaintext.
func readStartup(c net.Conn) ([]byte, error) {
	c.SetReadDeadline(time.Now().Add(startupTimeout))
	defer c.SetReadDeadline(time.Time{})

	for range 3 { // GSSENCRequest, SSLRequest, then the real packet
		pkt, err := readStartupPacket(c)
		if err != nil {
			return nil, err
		}
		switch code := binary.BigEndian.Uint32(pkt[4:8]); {
		case code == sslRequestCode, code == gssEncRequestCode:
			if _, err := c.Write([]byte{'N'}); err != nil {
				return nil, err
			}
		case code == cancelRequestCode, code>>16 == 3:
			return pkt, nil
		default:
			return nil, fmt.Errorf("unsupported protocol code %d", code)
		}
	}
	return nil, errors.New("too many encryption requests")
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

// relay copies messages both ways until either side closes. Taps may be nil.
func relay(client, server net.Conn, fromClient, fromServer *tap) {
	done := make(chan struct{}, 2)
	go func() { pump(server, client, fromClient); done <- struct{}{} }()
	go func() { pump(client, server, fromServer); done <- struct{}{} }()
	<-done
	client.Close()
	server.Close()
	<-done
}

// tap observes messages in one direction. For message types it wants, see
// gets the whole body before the message is forwarded (nil past
// maxInspect), plus a way to flush what is already queued for the peer.
// each, when set, gets the type of every message as soon as its header is
// read; it runs once per result row, so it must stay trivial.
type tap struct {
	want func(t byte) bool
	see  func(t byte, body []byte, flush func() error)
	each func(t byte)
}

const maxInspect = 64 << 20

// pump copies whole protocol messages from src to dst. Writes are batched
// while src already has the next header buffered and flushed before any read
// that may block, so a peer waiting on a response (after Sync, Flush, or
// LISTEN) always gets it.
func pump(dst io.Writer, src io.Reader, tp *tap) error {
	br := bufio.NewReaderSize(src, bufSize)
	bw := bufio.NewWriterSize(dst, bufSize)
	var hdr [5]byte // type byte + int32 length (which counts itself)
	var body []byte
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
		if tp != nil && tp.want(hdr[0]) {
			if size > maxInspect {
				tp.see(hdr[0], nil, bw.Flush)
			} else {
				if int64(cap(body)) < size {
					body = make([]byte, size)
				}
				body = body[:size]
				if _, err := io.ReadFull(br, body); err != nil {
					return err
				}
				tp.see(hdr[0], body, bw.Flush)
				if _, err := bw.Write(hdr[:]); err != nil {
					return err
				}
				if _, err := bw.Write(body); err != nil {
					return err
				}
				continue
			}
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
