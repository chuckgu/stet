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

// admit is the first look an enforced session takes at a client message:
// once the proxy is ending the session, nothing more goes upstream.
func (s *session) admit() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.end != "" {
		return errEnded
	}
	return nil
}
