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
// without s.mu held, once both pumps have stopped.
func (s *session) ended() []byte {
	s.mu.Lock()
	reason := s.end
	info := s.identity()
	first := reason != "" && s.firstTime(reason)
	s.mu.Unlock()
	if reason == "" {
		return nil
	}
	return errorMessage("FATAL", s.gate.Blocked(info, reason, first))
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
// way.
//
// A Query may hold several copies, each ended by the client's next CopyDone
// or CopyFail, so the server splits what the client sends after the Query at
// each of them: what comes after the n-th is read during copy n+1, if the
// Query starts one. Until the client sends something else after the Query,
// its CopyDones and CopyFails are counted on it (copyEnds); that message
// marks the Query stray at that count, for copyIn, since the server may not
// have started the copy that will read it yet.
func (s *session) admit(t byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.end != "" {
		return errEnded
	}
	switch t {
	case 'c', 'f': // CopyDone, CopyFail
		if s.lastQuery != nil {
			s.lastQuery.copyEnds++ // the Query may start another copy
		}
		s.copying = nil
	case 'H':
	default:
		if s.copying != nil {
			return s.endWith(BlockProtocolOrder)
		}
		if q := s.lastQuery; q != nil {
			q.stray, q.strayAt = true, q.copyEnds
			s.lastQuery = nil
		}
	}
	return nil
}

// copyIn follows a CopyInResponse or CopyBothResponse from the server of an
// enforced session, answering head (see admit). The server ignores a Sync
// during an extended-protocol copy, so that ends the session at once
// (copy_started): the backstop for a COPY FROM STDIN the classifier did not
// see, since one it sees is refused in its Parse (copy_extended). So does a
// Query copy that will read a message the client
// already sent other than copy messages (protocol_order). A copy the client
// has not ended yet is in progress. It runs on the server side with s.mu
// held.
func (s *session) copyIn(head *unit) error {
	if head == nil {
		return nil
	}
	head.copies++
	switch {
	case !head.query:
		return s.endWith(BlockCopyStarted)
	case head.stray && head.copies > head.strayAt:
		return s.endWith(BlockProtocolOrder)
	case head.copies > head.copyEnds:
		s.copying = head
	}
	return nil
}
