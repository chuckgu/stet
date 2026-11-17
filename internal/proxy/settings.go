package proxy

import (
	"bytes"
	"slices"
	"strings"
)

// The server lexes a statement's bytes by two settings of its session:
// client_encoding, from which it converts them before it parses them, and
// standard_conforming_strings, which says whether a backslash escapes in an
// ordinary string literal. The classifier reads them as UTF-8 with
// standard_conforming_strings on. Under SJIS, BIG5, GBK, UHC, GB18030, JOHAB
// or SHIFT_JIS_2004 a multibyte character can end in a backslash byte, and
// with standard_conforming_strings off \' is an escaped quote: either way the
// two can end a string literal at different places, and a write can hide
// from the classifier inside what it takes for one (ruling 22). Names and
// bind values are converted from client_encoding too.
//
// So an enforced session follows the server's ParameterStatus messages for
// both settings, which the server sends at startup and before the
// ReadyForQuery that follows any change, and while they are not what the
// classifier assumes it refuses what the server may read otherwise (see
// misread and fromClient). Shadow sessions do not look: they classify as
// before, and may record such a statement wrongly.
//
// An enforced session also follows TimeZone, DateStyle and IntervalStyle,
// by which the server reads a date, time or interval sent as text, and hands
// them to the gate, which keeps them with a write it holds so that the write
// runs later as it would have then (spec 4).

// readable reports whether the server reads this session's statements as the
// classifier does: it last reported client_encoding UTF8 (or UNICODE) and
// standard_conforming_strings on. Until it reported both, during startup,
// neither is known. It runs with s.mu held.
func (s *session) readable() bool { return s.utf8 && s.standard }

// misread reports whether the server of this enforced session may read sql
// otherwise than the classifier: the session is not readable, and sql does
// more than repair the settings (see repairsSettings). It runs with s.mu
// held, once the message holding sql has joined its unit: a report that
// makes the session unreadable is then either seen here or sees that unit
// (sentAfter).
func (s *session) misread(sql string) bool {
	return !s.readable() && !repairsSettings(sql)
}

// parameter follows a ParameterStatus with body body from the server of an
// enforced session, while head is the unit being answered.
//
// The server reports a change at the ReadyForQuery of the unit that ran it,
// while it may already have read messages sent after the change, which the
// proxy inspected under the old value. When the report makes the session
// unreadable and such messages were sent (sentAfter), the session is ended.
// A client that changes a setting and sends its next message before the
// server reports the change still gets that one message read under the old
// value: the known residue. It runs on the server side with s.mu held.
func (s *session) parameter(head *unit, body []byte) error {
	name, rest, _ := bytes.Cut(body, []byte{0})
	value, _, _ := bytes.Cut(rest, []byte{0})
	was := s.readable()
	switch string(name) {
	case "client_encoding":
		// The server reports UTF8 by its canonical name, except the spelling
		// UNICODE, which it keeps for old JDBC drivers.
		s.utf8 = string(value) == "UTF8" || string(value) == "UNICODE"
	case "standard_conforming_strings":
		s.standard = string(value) == "on"
	case "server_encoding": // reported at startup, never changes
		s.serverUTF8 = string(value) == "UTF8"
		return nil
	case "TimeZone":
		s.timeZone = string(value)
		return nil
	case "DateStyle":
		s.dateStyle = string(value)
		return nil
	case "IntervalStyle":
		s.intervalStyle = string(value)
		return nil
	default:
		return nil
	}
	if was && !s.readable() && s.sentAfter(head) {
		return s.endWith(BlockSettingChanged)
	}
	return nil
}

// sentAfter reports whether the server may have read, after a change made by
// head, the unit being answered, a message sent before the change was
// reported: one after an Execute of head, one of the unit after an unsynced
// unit that executed (head.joined), or one of a later unit. A Sync or Flush
// holds nothing to read, and a FunctionCall is refused. It runs with s.mu
// held.
func (s *session) sentAfter(head *unit) bool {
	if head == nil {
		return false
	}
	if head.late || head.joined {
		return true
	}
	return slices.ContainsFunc(s.units[1:], func(u *unit) bool { return u.query || u.msgs > 0 })
}

// repairsSettings reports whether sql only sets or resets client_encoding or
// standard_conforming_strings, so that a session that cannot be read can
// still be repaired. It is one or more statements
//
//	SET [SESSION | LOCAL] {client_encoding | standard_conforming_strings} {TO | =} value
//	SET [SESSION | LOCAL] NAMES [value]
//	RESET {client_encoding | standard_conforming_strings}
//
// separated by semicolons, where a value is a word or a quoted string. sql
// must be ASCII with no backslash, and hold no comment, quoted identifier or
// special string: every client_encoding and either
// standard_conforming_strings lex such text alike, so the server reads what
// this reads, whatever the settings are.
func repairsSettings(sql string) bool {
	for i := range len(sql) {
		if c := sql[i]; c >= 0x80 || c == '\\' {
			return false
		}
	}
	toks, ok := settingTokens(sql)
	if !ok {
		return false
	}
	n := 0
	for len(toks) > 0 {
		end := slices.Index(toks, ";")
		if end < 0 {
			end = len(toks)
		}
		if end > 0 { // not an empty statement
			if !settingStatement(toks[:end]) {
				return false
			}
			n++
		}
		toks = toks[min(end+1, len(toks)):]
	}
	return n > 0
}

// settingTokens splits sql into words, quoted strings (with their quotes),
// "=" and ";", or reports false when it holds anything else.
func settingTokens(sql string) ([]string, bool) {
	var toks []string
	for i := 0; i < len(sql); {
		switch c := sql[i]; {
		case strings.IndexByte(" \t\n\r\f\v", c) >= 0:
			i++
		case c == ';' || c == '=':
			toks = append(toks, sql[i:i+1])
			i++
		case c == '\'':
			j := i + 1
			for {
				k := strings.IndexByte(sql[j:], '\'')
				if k < 0 {
					return nil, false // not terminated
				}
				j += k + 1
				if j == len(sql) || sql[j] != '\'' {
					break
				}
				j++ // '' is a quote inside the string
			}
			toks = append(toks, sql[i:j])
			i = j
		case wordByte(c):
			j := i
			for j < len(sql) && wordByte(sql[j]) {
				j++
			}
			toks = append(toks, sql[i:j])
			i = j
		default:
			return nil, false
		}
	}
	return toks, true
}

func wordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

// settingStatement reports whether t, the tokens of one statement, is one of
// those repairsSettings takes.
func settingStatement(t []string) bool {
	is := func(i int, words ...string) bool {
		return i < len(t) && slices.ContainsFunc(words, func(w string) bool { return strings.EqualFold(t[i], w) })
	}
	value := func(i int) bool { return i < len(t) && t[i] != "=" }
	switch {
	case is(0, "reset"):
		return len(t) == 2 && is(1, "client_encoding", "standard_conforming_strings")
	case is(0, "set"):
		i := 1
		if is(i, "session", "local") {
			i++
		}
		if is(i, "names") {
			return len(t) == i+1 || len(t) == i+2 && value(i+1)
		}
		return len(t) == i+3 && is(i, "client_encoding", "standard_conforming_strings") && is(i+1, "to", "=") && value(i+2)
	}
	return false
}

// foreign reports whether the server of this enforced session may compare
// name otherwise than the proxy and the classifier, which clip names to 63
// bytes of UTF-8: name is not ASCII, and the server's server_encoding is not
// (known to be) UTF8, so the server converts the name to it and clips it
// there. It runs with s.mu held.
func (s *session) foreign(name string) bool { return !s.serverUTF8 && !ascii(name) }

// foreignNames reports whether st runs by name (SQL EXECUTE, FETCH, MOVE) a
// prepared statement or portal the server may compare otherwise (foreign).
// It runs with s.mu held.
func (s *session) foreignNames(st Statement) bool {
	return slices.ContainsFunc(st.Executes, s.foreign) || slices.ContainsFunc(st.Portals, s.foreign)
}

// ascii reports whether name holds only ASCII bytes, which every
// client_encoding converts to themselves: the server knows the statement or
// portal by the same bytes the proxy does.
func ascii(name string) bool {
	for i := range len(name) {
		if name[i] >= 0x80 {
			return false
		}
	}
	return true
}
