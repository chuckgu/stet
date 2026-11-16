package proxy

import (
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

// Only a message that sets or resets client_encoding or
// standard_conforming_strings, and that every client_encoding and either
// standard_conforming_strings lex the same, repairs the settings: plain ASCII
// with no backslash, no comment, no quoted identifier, no special string.
func TestRepairsSettings(t *testing.T) {
	for sql, want := range map[string]bool{
		"set client_encoding to 'UTF8'":                                            true,
		"SET client_encoding = utf8":                                               true,
		"set session client_encoding to default":                                   true,
		"set local standard_conforming_strings = on":                               true,
		"set standard_conforming_strings to 'on';":                                 true,
		"set names 'UTF8'":                                                         true,
		"set names":                                                                true,
		"reset client_encoding; reset standard_conforming_strings":                 true,
		" set client_encoding to 'UTF8' ;\n set standard_conforming_strings = on ": true,
		"set client_encoding to 'it''s'":                                           true,
		"set client_encoding to 'SJIS'":                                            true, // still only a setting

		"":         false,
		";":        false,
		"select 1": false,
		"set client_encoding to 'UTF8'; select 1": false,
		"set search_path to public":               false,
		"reset all":                               false,
		"set client_encoding to 'x\\''; insert into t values (1); -- '": false,
		"set client_encoding to E'UTF8'":                                false,
		"set client_encoding to U&'UTF8'":                               false,
		"set client_encoding to $$UTF8$$":                               false,
		"set \"client_encoding\" to 'UTF8'":                             false,
		"set client_encoding to 'UTF8' -- x":                            false,
		"set client_encoding /* x */ to 'UTF8'":                         false,
		"set client_encoding to 'UTF8' 'x'":                             false,
		"set client_encoding to 'UTF8\xc5\x95'":                         false,
		"set client_encoding to":                                        false,
		"set client_encoding to 'unterminated":                          false,
		"set client_encoding from current":                              false,
		"set names 'UTF8', 'x'":                                         false,
	} {
		if got := repairsSettings(sql); got != want {
			t.Errorf("repairsSettings(%q) = %v, want %v", sql, got, want)
		}
	}
}

// On an enforced session the server's ParameterStatus messages for
// client_encoding and standard_conforming_strings are followed (ruling 22).
// Until the server reported UTF8 and on, every Query and Parse is refused,
// except one that only repairs the settings; so is a Bind that names a
// statement or portal in bytes the server converts, or whose statement may
// be a watched write (its values are converted too), and an Execute that
// names its portal so. A Close of such a name changes nothing in the tables.
// A session that does not enforce never looks.
func TestSettingsTheClassifierCannotReadAreRefused(t *testing.T) {
	const sjis = "s\x82\xa0"
	g := newBlockGate()
	s := newSession(g, SessionInfo{})
	s.enforce = true
	// refused sends m in a unit of its own, which the server answers with a
	// ReadyForQuery alone.
	refused := func(t *testing.T, want bool, m pgproto3.FrontendMessage) {
		t.Helper()
		g.blocked = nil
		r := sendOne(s, m)
		if (r != nil) != want {
			t.Errorf("%T replaced %v, want %v", m, r != nil, want)
		}
		if want && !slices.Equal(g.blocked, []string{BlockSessionSetting}) {
			t.Errorf("Blocked for %q, want session_setting", g.blocked)
		}
		if _, query := m.(*pgproto3.Query); !query {
			send(s, syncMsg)
		}
		serve(s, ready)
		if s.end != "" || len(s.units) != 0 {
			t.Fatalf("session ending for %q with %d units waiting, want neither", s.end, len(s.units))
		}
	}

	// During startup, before the server reported them, the settings are not
	// known.
	if r := sendOne(s, queryMsg("select 1")); r == nil || !slices.Equal(g.blocked, []string{BlockSessionSetting}) {
		t.Errorf("a Query before startup ended: replaced %v, Blocked for %q; want refused for session_setting", r != nil, g.blocked)
	}
	if r := sendOne(s, queryMsg("set client_encoding to 'UTF8'")); r != nil {
		t.Errorf("a Query that repairs the settings was replaced by %q", r)
	}
	serve(s, setting("client_encoding", "UTF8"), setting("standard_conforming_strings", "on"), ready) // startup's
	serve(s, syntaxError(), ready, tag("SET"), ready)
	refused(t, false, queryMsg("select 1"))
	send(s, parseMsg("w", ins), parseMsg("r", "select 1"), parseMsg(sjis, "select 1"), bindMsg("p", "r"), syncMsg)
	serve(s, parseOK, parseOK, parseOK, bindOK, ready)

	for _, value := range []string{"SJIS", "LATIN1", "SQL_ASCII"} {
		serve(s, setting("client_encoding", value), ready)
		refused(t, true, queryMsg("select 1"))
	}
	refused(t, true, parseMsg("x", "select 1"))
	if r := sendOne(s, parseMsg(sjis, "select 1")); r == nil {
		t.Error("a Parse under a converted name was forwarded")
	} else if m, _ := body2(t, r).(*pgproto3.Parse); m == nil || !freshNameRE.MatchString(m.Name) {
		t.Errorf("a Parse under a converted name was replaced by %q, want one under a fresh name", r)
	}
	send(s, syncMsg)
	serve(s, syntaxError(), ready)
	refused(t, false, parseMsg("x", "set client_encoding to 'UTF8'"))
	refused(t, true, bindMsg("", "w"))
	refused(t, false, bindMsg("", "r"))
	refused(t, true, bindMsg("", sjis))
	refused(t, true, bindMsg(sjis, "r"))
	refused(t, false, execMsg("p"))
	refused(t, true, execMsg(sjis))
	send(s, closeMsg('S', sjis), syncMsg)
	serve(s, closeOK, ready)
	if _, ok, _ := s.stmts.get(nil, sjis); !ok {
		t.Errorf("a Close under a converted name dropped %q, want it kept", sjis)
	}

	serve(s, setting("client_encoding", "UTF8"), ready)
	refused(t, false, queryMsg("select 1"))
	serve(s, setting("standard_conforming_strings", "off"), ready)
	refused(t, true, queryMsg("select 1"))
	serve(s, setting("standard_conforming_strings", "on"), ready)
	refused(t, false, queryMsg("select 1"))

	shadow := readySession(g)
	if shadow.serverTap().want('S') {
		t.Error("a session that does not enforce follows ParameterStatus")
	}
	g.blocked = nil
	if r, _ := shadow.fromClient('Q', body(queryMsg("select 1")), func() error { return nil }); r != nil || len(g.blocked) != 0 {
		t.Errorf("a session that does not enforce refused a Query (Blocked for %q)", g.blocked)
	}
}

// body2 decodes the whole message b.
func body2(t *testing.T, b []byte) pgproto3.FrontendMessage {
	t.Helper()
	var m pgproto3.FrontendMessage
	switch b[0] {
	case 'P':
		m = &pgproto3.Parse{}
	case 'Q':
		m = &pgproto3.Query{}
	default:
		t.Fatalf("no decoder for %c", b[0])
	}
	if err := m.Decode(whole(t, b, b[0])); err != nil {
		t.Fatal(err)
	}
	return m
}

// A setting that changes to one the classifier cannot read while messages
// sent after the change are already on their way: those were read under the
// old value, so the session is ended (ruling 22). They are the messages of a
// later unit, and those after an Execute in the same unit or in an unsynced
// unit before it, since the server reports the change only at the
// ReadyForQuery of the unit that ran it. A Sync or Flush carries nothing to
// misread. A change with nothing behind it ends nothing.
func TestSettingChangedAheadOfSentStatementsEndsTheSession(t *testing.T) {
	set := "set client_encoding to 'SJIS'"
	for _, c := range []struct {
		name string
		send []pgproto3.FrontendMessage
		// replies are the server's before its ParameterStatus.
		replies []pgproto3.BackendMessage
		ends    bool
	}{
		{"a Query behind it", msgs(queryMsg(set), queryMsg("select 1")),
			msgs2(tag("SET")), true},
		{"a Bind behind it", msgs(queryMsg(set), bindMsg("", "s1")),
			msgs2(tag("SET")), true},
		{"an Execute after it in its unit", msgs(parseMsg("", set), bindMsg("", ""), execMsg(""), parseMsg("", "select 1"), syncMsg),
			msgs2(parseOK, bindOK, tag("SET"), parseOK), true},
		{"a Query behind its unsynced unit", msgs(parseMsg("", set), bindMsg("", ""), execMsg(""), &pgproto3.Flush{}, queryMsg("select 1")),
			msgs2(parseOK, bindOK, tag("SET"), &pgproto3.RowDescription{}, row, tag("SELECT 1")), true},
		{"nothing behind it", msgs(queryMsg(set)),
			msgs2(tag("SET")), false},
		{"a Sync and a Flush behind it", msgs(queryMsg(set), syncMsg, &pgproto3.Flush{}),
			msgs2(tag("SET")), false},
		{"only its own Execute", msgs(parseMsg("", set), bindMsg("", ""), execMsg(""), syncMsg),
			msgs2(parseOK, bindOK, tag("SET")), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newFakeGate()
			s := enforcedSession(g)
			send(s, c.send...)
			serve(s, c.replies...)
			_, err := s.serverTap().see('S', body(setting("client_encoding", "SJIS")), nil)
			if c.ends != errors.Is(err, errEnded) || !c.ends && err != nil {
				t.Errorf("ParameterStatus: %v, want the session ended %v", err, c.ends)
			}
			if ended := s.ended() != nil; ended != c.ends {
				t.Errorf("session ended %v, want %v", ended, c.ends)
			}
			if want := []string(nil); c.ends {
				if !slices.Equal(g.blocked, []string{BlockSessionSetting}) {
					t.Errorf("Blocked for %q, want session_setting", g.blocked)
				}
			} else if !slices.Equal(g.blocked, want) {
				t.Errorf("Blocked for %q, want never", g.blocked)
			}
		})
	}

	// Back to readable values with statements on their way: they were
	// refused, nothing was misread.
	g := newFakeGate()
	s := enforcedSession(g)
	serve(s, setting("client_encoding", "SJIS"), ready)
	send(s, queryMsg("set client_encoding to 'UTF8'"), queryMsg("select 1"))
	serve(s, tag("SET"), setting("client_encoding", "UTF8"))
	if s.ended() != nil {
		t.Error("a change back to UTF8 ended the session")
	}
}

func msgs2(m ...pgproto3.BackendMessage) []pgproto3.BackendMessage { return m }

// Following the settings adds nothing per row: an enforced session's taps
// are shown no row message, and a row costs the server side one increment
// and no allocation.
func TestEnforcedTapsShowNoRows(t *testing.T) {
	s := enforcedSession(newFakeGate())
	st, ct := s.serverTap(), s.clientTap()
	// DataRow and CopyData from the server; CopyData from the client.
	if st.want('D') || st.want('d') || ct.want('d') {
		t.Error("a tap is shown row messages")
	}
	if !st.want('S') {
		t.Error("the server tap is not shown ParameterStatus")
	}
	before := s.sent
	if allocs := testing.AllocsPerRun(1000, func() { st.each('D') }); allocs != 0 {
		t.Errorf("a row allocates %v times", allocs)
	}
	if s.sent-before != 1001 { // AllocsPerRun runs it once more to warm up
		t.Errorf("rows counted %d, want 1001", s.sent-before)
	}
}

// The copy rule ends an enforced session only for a message the server reads
// during a simple COPY FROM STDIN: between its CopyInResponse (or
// CopyBothResponse) and the client's CopyDone or CopyFail, or the copy's own
// end on the server, by an error say. A Flush in that stretch is let through.
// An extended-protocol copy ends the session as it starts.
func TestCopyRuleHoldsOnlyDuringTheCopy(t *testing.T) {
	copyIn, copyBoth := &pgproto3.CopyInResponse{}, &pgproto3.CopyBothResponse{}
	copyDone, copyFail := &pgproto3.CopyDone{}, &pgproto3.CopyFail{Message: "no"}
	for _, c := range []struct {
		name   string
		client []pgproto3.FrontendMessage // before the server's reply
		server []pgproto3.BackendMessage
		then   pgproto3.FrontendMessage // after it
		ends   string
	}{
		{"a Sync during the copy", msgs(queryMsg("copy")), msgs2(copyIn), syncMsg, BlockProtocolOrder},
		{"a Query during a copy both ways", msgs(queryMsg("copy")), msgs2(copyBoth), queryMsg("select 1"), BlockProtocolOrder},
		{"a Sync before CopyInResponse", msgs(queryMsg("copy"), syncMsg), msgs2(copyIn), nil, BlockProtocolOrder},
		{"a Flush during the copy", msgs(queryMsg("copy")), msgs2(copyIn), &pgproto3.Flush{}, ""},
		{"a Sync after CopyDone", msgs(queryMsg("copy")), msgs2(copyIn), nil, ""},
		{"CopyDone before CopyInResponse", msgs(queryMsg("copy"), copyDone, syncMsg), msgs2(copyIn), nil, ""},
		{"CopyFail before CopyInResponse", msgs(queryMsg("copy"), copyFail, queryMsg("select 1")), msgs2(copyIn), nil, ""},
		{"a Sync after the copy failed on the server", msgs(queryMsg("copy")),
			msgs2(copyIn, serverErr("22P02"), ready), syncMsg, ""},
		{"an extended-protocol copy", msgs(parseMsg("", "copy"), bindMsg("", ""), execMsg(""), syncMsg),
			msgs2(parseOK, bindOK, copyIn), nil, BlockCopyExtended},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := enforcedSession(newFakeGate())
			send(s, c.client...)
			var err error
			tp := s.serverTap()
			for _, m := range c.server {
				b, _ := m.Encode(nil)
				tp.each(b[0])
				if tp.want(b[0]) {
					if _, err = tp.see(b[0], b[5:], nil); err != nil {
						break
					}
				}
			}
			if err == nil && c.then != nil {
				b, _ := c.then.Encode(nil)
				_, err = s.fromClient(b[0], b[5:], func() error { return nil })
			}
			if err == nil && c.name == "a Sync after CopyDone" {
				send(s, copyDone)
				_, err = s.fromClient('S', nil, func() error { return nil })
			}
			if c.ends == "" && err != nil || c.ends != "" && !errors.Is(err, errEnded) || s.end != c.ends {
				t.Errorf("%v, ending for %q; want ending for %q", err, s.end, c.ends)
			}
		})
	}
}
