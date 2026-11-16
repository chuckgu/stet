package proxy

import "errors"

// errEnded ends the pump that saw a message which ends the session (see
// endWith). That message is not forwarded.
var errEnded = errors.New("proxy: session ended by stet")

// endWith notes that the proxy ends the session for reason, a Block reason,
// unless it already does, and returns errEnded for the pump that saw it to
// return. relay then ends the rest (see ended). It runs with s.mu held.
func (s *session) endWith(reason string) error {
	if s.end == "" {
		s.end = reason
	}
	return errEnded
}

// ended returns the error the client is given when the proxy ended the
// session: a FATAL with the gate's text for the reason (Blocked, which also
// logs it). It returns nil when the proxy did not end the session. It runs
// without s.mu held, once a pump has stopped.
func (s *session) ended() []byte {
	s.mu.Lock()
	reason := s.end
	info := s.identity()
	s.mu.Unlock()
	if reason == "" {
		return nil
	}
	return errorMessage("FATAL", s.gate.Blocked(info, reason))
}

// admit is the first look an enforced session takes at a client message of
// type t. Once the proxy is ending the session, nothing more goes upstream.
//
// It also keeps the copy rule (spec correction B6 ②③, ruling P10). During
// a COPY FROM STDIN of a simple Query, from the server's CopyInResponse to
// the client's CopyDone or CopyFail, the server reads only copy messages: it
// ignores a Sync or Flush, and ends the session at anything else. A Sync it
// ignores leaves a unit the server never answers, so the session is ended
// when one of those messages arrives in that stretch (protocol_order), and
// it is not forwarded. A Flush is let through: the units follow it either
// way. A message the client sends after a Query before CopyDone or CopyFail,
// which only matters if the Query turns out to start a copy, marks that
// Query (stray) for copyIn, since the server may not have answered it yet.
func (s *session) admit(t byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.end != "" {
		return errEnded
	}
	switch t {
	case 'c', 'f': // CopyDone, CopyFail
		if s.lastQuery != nil {
			s.lastQuery.copyEnded = true
			s.lastQuery = nil
		}
		s.copying = nil
	case 'H':
	default:
		if s.copying != nil {
			return s.endWith(BlockProtocolOrder)
		}
		if s.lastQuery != nil {
			s.lastQuery.stray = true
			s.lastQuery = nil
		}
	}
	return nil
}

// copyIn follows a CopyInResponse or CopyBothResponse from the server of an
// enforced session, answering head (see admit). The server ignores a Sync
// during an extended-protocol copy, so that ends the session at once
// (copy_extended), as does a Query copy behind which the client already sent
// a message the server will read during the copy (protocol_order). It runs
// on the server side with s.mu held.
func (s *session) copyIn(head *unit) error {
	switch {
	case head == nil:
	case !head.query:
		return s.endWith(BlockCopyExtended)
	case head.stray:
		return s.endWith(BlockProtocolOrder)
	case !head.copyEnded:
		s.copying = head
	}
	return nil
}
