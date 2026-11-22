package safetext

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// unsafeSet is the characters Printable replaces, written out by hand.
func unsafeSet(r rune) bool {
	return r <= 0x1F || (r >= 0x7F && r <= 0x9F) || // Cc
		r == 0x2028 || r == 0x2029 || // line and paragraph separators
		(r >= 0x202A && r <= 0x202E) || // LRE, RLE, PDF, LRO, RLO
		(r >= 0x2066 && r <= 0x2069) // LRI, RLI, FSI, PDI
}

// Every code point: those in the set become one space, all others are kept.
func TestPrintableReplacesExactlyTheUnsafeCharacters(t *testing.T) {
	for r := rune(0); r <= utf8.MaxRune; r++ {
		if !utf8.ValidRune(r) {
			continue // surrogates
		}
		in := string(r)
		want := in
		if unsafeSet(r) {
			want = " "
		}
		if got := Printable(in); got != want {
			t.Errorf("Printable(%U) = %q, want %q", r, got, want)
		}
	}
}

func TestPrintable(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"Empty", "", ""},
		{"Plain", `password authentication failed for user "stet_scram"`, `password authentication failed for user "stet_scram"`},
		{"Text beyond ASCII", "계정 ‘agent’ — ok ✓ 😀", "계정 ‘agent’ — ok ✓ 😀"},
		{"Escape sequence", "a\x1b[2Jb", "a [2Jb"},
		{"Line breaks and tabs", "a\r\nb\tc", "a  b c"},
		{"Separators and direction controls", "a b c‮d⁦e⁩f", "a b c d e f"},
		{"NUL and DEL", "a\x00b\x7fc", "a b c"},
		{"C1 control", "a\u0085b\u009bc", "a b c"},
		// Invalid UTF-8 becomes U+FFFD, one per bad byte, as strings.Map makes it.
		{"Invalid UTF-8", "a\xffb\xc3", "a�b�"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Printable(tc.in)
			if got != tc.want {
				t.Errorf("Printable(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("Printable(%q) = %q, not valid UTF-8", tc.in, got)
			}
			if strings.ContainsFunc(got, unsafeSet) {
				t.Errorf("Printable(%q) = %q still holds an unsafe character", tc.in, got)
			}
		})
	}
}
