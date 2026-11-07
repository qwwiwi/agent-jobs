package pathscope

import (
	"path"
	"testing"
)

func TestIntersect(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"src/a.go", "src/a.go", true}, {"src", "src/a.go", false},
		{"src/*.go", "src/a.go", true}, {"src/*.go", "src/a*.go", true},
		{"src/a*.go", "src/*b.go", true}, {"src/a*.go", "src/b*.go", false},
		{"src/[a-c]?.go", "src/b*.go", true}, {"src/[a-c].go", "src/[x-z].go", false},
		{"src/[^a].go", "src/a.go", false}, {"src/[^a].go", "src/b.go", true},
		{"src/**/*.go", "src/a.go", true}, {"src/**/*.go", "src/x/y/a.go", true},
		{"src/**/a.go", "src/*/a.go", true}, {"src/*.go", "docs/*.go", false},
		{"src/*/a.go", "src/a.go", false}, {"new/*.go", "new/new_file.go", true},
		{"src/**", "docs/**", false}, {"src/??.go", "src/x.go", false},
		{"src/[z-a]", "src/[z-a]", false}, {"src/**", "src/[z-a]", false},
		{"src/[[:alpha:]]", "src/[a-z]*", true},
		{"src/é.go", "src/?.go", true}, {"src/", "src/a.go", true},
	} {
		t.Run(c.a+"|"+c.b, func(t *testing.T) {
			if got := Intersect(c.a, c.b); got != c.want {
				t.Fatalf("Intersect(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
			}
			if got := Intersect(c.b, c.a); got != c.want {
				t.Fatalf("asymmetric intersection: %v", got)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	for _, bad := range []string{"", "/src/a.go", "../a", "src/../a", "src/./a", "a//b", "a\\b", "a[", "a/**x", "a\nb"} {
		if err := Validate(bad); err == nil {
			t.Errorf("accepted invalid path %q", bad)
		}
	}
	for _, good := range []string{"src", "src/", "src/**/*.go", "**", "a/[a-c]?.go"} {
		if err := Validate(good); err != nil {
			t.Errorf("rejected %q: %v", good, err)
		}
	}
}

// Every pattern has at most three required characters. Enumerating up to six characters over
// representatives a/b/c therefore covers a shortest witness of every pair in this finite grammar.
func TestGlobIntersectionAgainstFiniteLanguage(t *testing.T) {
	patterns := []string{"a", "b", "*", "?", "a*", "*b", "a?", "?b", "[ab]", "[^a]", "[a-b]*", "a*b", "a?b", "[z-a]", "[^ab]"}
	var names []string
	var enumerate func(string, int)
	enumerate = func(prefix string, left int) {
		if prefix != "" {
			names = append(names, prefix)
		}
		if left > 0 {
			for _, r := range "abc" {
				enumerate(prefix+string(r), left-1)
			}
		}
	}
	enumerate("", 6)
	for _, a := range patterns {
		for _, b := range patterns {
			want := false
			for _, name := range names {
				am, _ := path.Match(a, name)
				bm, _ := path.Match(b, name)
				if am && bm {
					want = true
					break
				}
			}
			if got := Intersect(a, b); got != want {
				t.Fatalf("Intersect(%q,%q)=%v, finite-language witness=%v", a, b, got, want)
			}
		}
	}
}
