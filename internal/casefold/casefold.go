// Package casefold folds names the way a case-insensitive file system compares them.
package casefold

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Fold maps every character to one member of its Unicode case-folding orbit, lower-cased: two
// strings equal under strings.EqualFold fold to the same string. Plain lower-casing misses the
// characters that fold to ASCII letters without being their lower case: U+017F (long s) folds to
// `s` and U+212A (Kelvin sign) to `k`, so `JOBſ.DB` names `jobs.db` on a case-insensitive volume.
func Fold(s string) string {
	return strings.Map(func(r rune) rune {
		if r < utf8.RuneSelf {
			if 'A' <= r && r <= 'Z' {
				r += 'a' - 'A'
			}
			return r
		}
		m := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			m = min(m, f)
		}
		return unicode.ToLower(m)
	}, s)
}
