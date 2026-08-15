// Package textutil holds small string helpers shared across packages.
package textutil

import "unicode/utf8"

// Truncate returns s unchanged when it is at most max bytes; otherwise it
// returns the longest rune-aligned prefix that fits in max bytes followed by an
// ellipsis. The prefix never splits a multi-byte rune. max bounds the prefix,
// not the appended ellipsis; a negative max is treated as zero.
func Truncate(s string, max int) string {
	if max < 0 {
		max = 0
	}
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
