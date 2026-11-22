package proxy

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

// saslBody is the body of an Authentication message of code: names, each
// ended by a zero byte, then the zero byte that ends the list.
func saslBody(code uint32, names ...string) []byte {
	b := binary.BigEndian.AppendUint32(nil, code)
	for _, n := range names {
		b = append(append(b, n...), 0)
	}
	return append(b, 0)
}

// saslMessage is the whole AuthenticationSASL message (type, length, body)
// that lists names.
func saslMessage(names ...string) []byte {
	body := saslBody(10, names...)
	m := binary.BigEndian.AppendUint32([]byte{'R'}, uint32(4+len(body)))
	return append(m, body...)
}

func TestStripPlus(t *testing.T) {
	const plus, scram = "SCRAM-SHA-256-PLUS", "SCRAM-SHA-256"
	listed := func(code uint32) []byte { return saslBody(code, plus, scram) }
	for _, tc := range []struct {
		name string
		body []byte
		want []byte // nil: not changed
		left int
	}{
		{"PlusAndScram", saslBody(10, plus, scram), saslMessage(scram), 1},
		{"ScramAlone", saslBody(10, scram), nil, 0},
		{"PlusAlone", saslBody(10, plus), saslMessage(), 0},
		{"OrderKept", saslBody(10, plus, "OAUTHBEARER", scram), saslMessage("OAUTHBEARER", scram), 2},
		{"PlusLast", saslBody(10, scram, plus), saslMessage(scram), 1},
		{"EmptyList", saslBody(10), nil, 0},
		{"Ok", listed(0), nil, 0},
		{"CleartextPassword", listed(3), nil, 0},
		{"MD5Password", listed(5), nil, 0},
		{"SASLContinue", listed(11), nil, 0},
		{"SASLFinal", listed(12), nil, 0},
		{"Empty", []byte{}, nil, 0},
		{"Nil", nil, nil, 0}, // a body past maxInspect
		{"ShortCode", []byte{0, 0, 10}, nil, 0},
		{"ListNotEnded", saslBody(10, plus, scram)[:len(saslBody(10, plus, scram))-1], nil, 0},
		{"NameNotEnded", append(binary.BigEndian.AppendUint32(nil, 10), plus+"\x00"+scram...), nil, 0},
		{"BytesAfterList", append(saslBody(10, plus, scram), 'x'), nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := bytes.Clone(tc.body)
			replace, left, changed := stripPlus(tc.body)
			if !bytes.Equal(tc.body, before) {
				t.Errorf("the body was changed in place: % x, was % x", tc.body, before)
			}
			if tc.want == nil {
				if changed || replace != nil {
					t.Errorf("changed %v, replace % x; want it forwarded as the server sent it", changed, replace)
				}
				return
			}
			if !changed || !bytes.Equal(replace, tc.want) || left != tc.left {
				t.Fatalf("changed %v, replace % x, left %d; want % x, left %d", changed, replace, left, tc.want, tc.left)
			}
			if n := binary.BigEndian.Uint32(replace[1:5]); replace[0] != 'R' || int(n) != len(replace)-1 {
				t.Errorf("type %q, length %d, for a message of %d bytes after its type", replace[0], n, len(replace)-1)
			}
		})
	}
}

// through hands tp a message as pump does: each with its type, then see
// when want takes it. shown reports whether see was called.
func through(tp *tap, m byte, body []byte) (replace []byte, shown bool, err error) {
	if tp.each != nil {
		tp.each(m)
	}
	if tp.want == nil || !tp.want(m) {
		return nil, false, nil
	}
	replace, err = tp.see(m, body, func() error { return nil })
	return replace, true, err
}

// recorder is a server-side tap that wants the types in wants, as the
// session's does, and records what it is given.
type recorder struct {
	wants string
	each  []byte // the type of every message
	seen  []byte // the types see was shown
	ready []byte // what see returns for a ReadyForQuery
}

func (r *recorder) tap() *tap {
	return &tap{
		want: func(m byte) bool { return strings.IndexByte(r.wants, m) >= 0 },
		see: func(m byte, _ []byte, _ func() error) ([]byte, error) {
			r.seen = append(r.seen, m)
			if m == 'Z' {
				return r.ready, nil
			}
			return nil, nil
		},
		each: func(m byte) { r.each = append(r.each, m) },
	}
}

func TestAuthTapBecomesInner(t *testing.T) {
	const plus, scram = "SCRAM-SHA-256-PLUS", "SCRAM-SHA-256"
	sasl, final, ok := saslBody(10, plus, scram), saslBody(12, plus, scram), saslBody(0)

	t.Run("Relayed", func(t *testing.T) {
		tp, refused := authTap(nil)
		if r, _, err := through(tp, 'R', sasl); err != nil || !bytes.Equal(r, saslMessage(scram)) {
			t.Errorf("AuthenticationSASL: replace % x, err %v; want % x", r, err, saslMessage(scram))
		}
		for _, m := range []struct {
			t    byte
			body []byte
		}{{'R', final}, {'R', ok}, {'S', nil}, {'K', nil}, {'Z', []byte{'I'}}} {
			if r, _, err := through(tp, m.t, m.body); r != nil || err != nil {
				t.Errorf("%q: replace % x, err %v; want it forwarded", m.t, r, err)
			}
		}
		if tp.want != nil || tp.see != nil || tp.each != nil {
			t.Errorf("after the ReadyForQuery: want %v, see %v, each %v; want all nil", tp.want != nil, tp.see != nil, tp.each != nil)
		}
		if f := refused(); f != nil {
			t.Errorf("refused() = % x, want nil", f)
		}
	})

	t.Run("Watched", func(t *testing.T) {
		rec := &recorder{wants: "CIsEZK", ready: []byte("replaced")}
		inner := rec.tap()
		tp, refused := authTap(inner)
		if r, shown, err := through(tp, 'R', sasl); !shown || err != nil || !bytes.Equal(r, saslMessage(scram)) {
			t.Errorf("AuthenticationSASL: shown %v, replace % x, err %v; want % x", shown, r, err, saslMessage(scram))
		}
		for _, m := range []byte{'R', 'S', 'K'} {
			through(tp, m, ok)
		}
		r, _, err := through(tp, 'Z', []byte{'I'})
		if err != nil || string(r) != "replaced" {
			t.Errorf("ReadyForQuery: replace %q, err %v; want inner's %q", r, err, rec.ready)
		}
		if string(rec.each) != "RRSKZ" || string(rec.seen) != "KZ" {
			t.Errorf("up to the ReadyForQuery inner's each got %q and its see %q, want RRSKZ and KZ", rec.each, rec.seen)
		}

		if tp.want == nil || tp.see == nil || tp.each == nil {
			t.Fatalf("after the ReadyForQuery: want %v, see %v, each %v; want inner's", tp.want != nil, tp.see != nil, tp.each != nil)
		}
		for m := range 256 {
			if got, want := tp.want(byte(m)), inner.want(byte(m)); got != want {
				t.Errorf("want(%q) = %v, inner wants it: %v", byte(m), got, want)
			}
		}
		rec.each, rec.seen = nil, nil
		for _, m := range []byte{'R', 'D', 'E', 'Z'} {
			through(tp, m, sasl)
		}
		if string(rec.each) != "RDEZ" || string(rec.seen) != "EZ" {
			t.Errorf("after the ReadyForQuery inner's each got %q and its see %q, want RDEZ and EZ", rec.each, rec.seen)
		}
		if f := refused(); f != nil {
			t.Errorf("refused() = % x, want nil", f)
		}
	})

	// An inner that wants Authentication messages is shown each as the
	// client gets it.
	t.Run("InnerWantsAuthentication", func(t *testing.T) {
		var shown [][]byte
		inner := &tap{
			want: func(m byte) bool { return m == 'R' },
			see: func(_ byte, body []byte, _ func() error) ([]byte, error) {
				shown = append(shown, bytes.Clone(body))
				return nil, nil
			},
		}
		tp, _ := authTap(inner)
		through(tp, 'R', sasl)
		through(tp, 'R', final)
		if len(shown) != 2 || !bytes.Equal(shown[0], saslBody(10, scram)) || !bytes.Equal(shown[1], final) {
			t.Errorf("inner was shown % x, want % x and % x", shown, saslBody(10, scram), final)
		}
	})
}

// A list with nothing left once SCRAM-SHA-256-PLUS is out ends the session:
// the message is not forwarded, and refused gives the client a FATAL 28000.
func TestAuthTapRefusesChannelBoundOnly(t *testing.T) {
	for _, inner := range []*tap{nil, (&recorder{wants: "CIsEZK"}).tap()} {
		tp, refused := authTap(inner)
		if f := refused(); f != nil {
			t.Fatalf("refused() = % x before anything was refused", f)
		}
		r, _, err := through(tp, 'R', saslBody(10, "SCRAM-SHA-256-PLUS"))
		if !errors.Is(err, errEnded) || r != nil {
			t.Fatalf("replace % x, err %v; want errEnded", r, err)
		}
		want := errorMessage("FATAL", Refusal{Code: "28000", Message: "stet: the server offers only channel-bound authentication, which cannot pass through stet"})
		if f := refused(); !bytes.Equal(f, want) {
			t.Errorf("refused() = %q, want %q", f, want)
		}
	}
}
