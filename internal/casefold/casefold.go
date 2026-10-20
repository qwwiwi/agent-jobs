// Package casefold folds names the way a case-insensitive file system compares them.
package casefold

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Fold maps every character to one member of its Unicode simple case-folding orbit (the smallest
// lower-case one, else the smallest): two strings fold to the same string exactly when
// strings.EqualFold reports them equal. Plain lower-casing misses the
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
		m, lower := r, unicode.IsLower(r)
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			switch fl := unicode.IsLower(f); {
			case fl && !lower:
				m, lower = f, true
			case fl == lower:
				m = min(m, f)
			}
		}
		return m
	}, s)
}
