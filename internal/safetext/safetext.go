// Package safetext makes text from outside Stet (a server's message, a
// client's account name, a value in a file) safe to show on a terminal or in
// a log line.
package safetext

import (
	"strings"
	"unicode"
)

// Printable is s with every character a terminal would act on or an eye
// misread replaced by a space: control characters (Cc), the line and
// paragraph separators (U+2028, U+2029) and the direction controls
// (U+202A–U+202E, U+2066–U+2069). Other valid text is unchanged; each byte of
// invalid UTF-8 becomes U+FFFD.
func Printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unsafeRune(r) {
			return ' '
		}
		return r
	}, s)
}

func unsafeRune(r rune) bool {
	return unicode.Is(unicode.Cc, r) || r == 0x2028 || r == 0x2029 ||
		(r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}
