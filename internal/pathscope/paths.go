// Package pathscope compares declared repository path scopes without reading the filesystem.
package pathscope

import (
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Validate accepts slash-separated repository paths and Go path.Match globs, plus ** as a whole
// component for zero or more directories. A trailing slash denotes a directory scope (all descendants).
func Validate(p string) error {
	if p == "" || len(p) > 256 || !utf8.ValidString(p) || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.ContainsFunc(p, unicode.IsControl) {
		return fmt.Errorf("paths must be repository-relative paths or globs, at most 256 bytes")
	}
	for _, part := range strings.Split(strings.TrimSuffix(p, "/"), "/") {
		if part == "" || part == "." || part == ".." || (strings.Contains(part, "**") && part != "**") {
			return fmt.Errorf("invalid path component %q (use ** only as a whole component)", part)
		}
		if _, err := path.Match(part, ""); err != nil {
			return fmt.Errorf("invalid path glob %q: %w", p, err)
		}
	}
	return nil
}

// Intersect reports whether two valid scopes can name the same path, including files not yet
// created. It intersects two small glob automata, rather than expanding globs against today's tree.
func Intersect(a, b string) bool {
	if Validate(a) != nil || Validate(b) != nil {
		return false
	}
	parts := func(p string) []string {
		out := strings.Split(strings.TrimSuffix(p, "/"), "/")
		if strings.HasSuffix(p, "/") {
			out = append(out, "**")
		}
		return out
	}
	as, bs := parts(a), parts(b)
	type state struct{ a, b int }
	todo := []state{{}}
	seen := map[state]bool{}
	for len(todo) > 0 {
		s := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if seen[s] {
			continue
		}
		seen[s] = true
		if s.a == len(as) && s.b == len(bs) {
			return true
		}
		if s.a < len(as) && as[s.a] == "**" {
			todo = append(todo, state{s.a + 1, s.b})
		}
		if s.b < len(bs) && bs[s.b] == "**" {
			todo = append(todo, state{s.a, s.b + 1})
		}
		if s.a == len(as) || s.b == len(bs) {
			continue
		}
		x, y := as[s.a], bs[s.b]
		mx, my := x, y
		if x == "**" {
			mx = "*"
		}
		if y == "**" {
			my = "*"
		}
		if segmentsIntersect(mx, my) {
			next := state{s.a + 1, s.b + 1}
			if x == "**" {
				next.a = s.a
			}
			if y == "**" {
				next.b = s.b
			}
			todo = append(todo, next)
		}
	}
	return false
}

type token struct {
	ranges []rune
	star   bool
}

func tokens(p string) []token {
	var out []token
	for len(p) > 0 {
		r, n := utf8.DecodeRuneInString(p)
		p = p[n:]
		t := token{ranges: []rune{r, r}}
		switch r {
		case '*', '?':
			t.ranges, t.star = []rune{1, '/' - 1, '/' + 1, utf8.MaxRune}, r == '*'
		case '[':
			end := strings.IndexByte(p, ']')
			body := []rune(p[:end])
			negate := body[0] == '^'
			if negate {
				body = body[1:]
			}
			t.ranges = nil
			for len(body) > 0 {
				lo, hi := body[0], body[0]
				body = body[1:]
				if len(body) >= 2 && body[0] == '-' {
					hi, body = body[1], body[2:]
				}
				if lo <= hi {
					t.ranges = append(t.ranges, lo, hi)
				}
			}
			if negate {
				t.ranges = complement(t.ranges)
			}
			p = p[end+1:]
		}
		out = append(out, t)
	}
	return out
}

// complement normalizes possibly overlapping/unsorted class ranges before inverting them.
func complement(rs []rune) []rune {
	type interval struct{ lo, hi rune }
	var intervals []interval
	for i := 0; i < len(rs); i += 2 {
		intervals = append(intervals, interval{rs[i], rs[i+1]})
	}
	slices.SortFunc(intervals, func(a, b interval) int { return int(a.lo - b.lo) })
	var out []rune
	next := rune(1)
	for _, r := range intervals {
		if r.lo > next {
			out = append(out, next, r.lo-1)
		}
		next = max(next, r.hi+1)
	}
	if next <= utf8.MaxRune {
		out = append(out, next, utf8.MaxRune)
	}
	return out
}

func tokensIntersect(a, b token) bool {
	for i := 0; i < len(a.ranges); i += 2 {
		for j := 0; j < len(b.ranges); j += 2 {
			lo, hi := max(a.ranges[i], b.ranges[j], 1), min(a.ranges[i+1], b.ranges[j+1])
			if lo <= hi && !(lo == '/' && hi == '/') {
				return true
			}
		}
	}
	return false
}

func segmentsIntersect(a, b string) bool {
	if a == b && !strings.ContainsAny(a, "*?[") {
		return true
	}
	if !strings.ContainsAny(a, "*?[") {
		ok, _ := path.Match(b, a)
		return ok
	}
	if !strings.ContainsAny(b, "*?[") {
		ok, _ := path.Match(a, b)
		return ok
	}
	as, bs := tokens(a), tokens(b)
	type state struct {
		a, b     int
		consumed bool
	}
	todo := []state{{}}
	seen := map[state]bool{}
	for len(todo) > 0 {
		s := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if seen[s] {
			continue
		}
		seen[s] = true
		if s.a == len(as) && s.b == len(bs) && s.consumed {
			return true
		}
		if s.a < len(as) && as[s.a].star {
			todo = append(todo, state{s.a + 1, s.b, s.consumed})
		}
		if s.b < len(bs) && bs[s.b].star {
			todo = append(todo, state{s.a, s.b + 1, s.consumed})
		}
		if s.a == len(as) || s.b == len(bs) || !tokensIntersect(as[s.a], bs[s.b]) {
			continue
		}
		next := state{s.a + 1, s.b + 1, true}
		if as[s.a].star {
			next.a = s.a
		}
		if bs[s.b].star {
			next.b = s.b
		}
		todo = append(todo, next)
	}
	return false
}
