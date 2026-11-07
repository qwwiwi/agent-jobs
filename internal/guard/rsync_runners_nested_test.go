package guard

import "testing"

// Issue #179, fix loop 2 (H2): a program the runner walk does not model (setpriv, …) reached
// through a modeled runner keeps the enclosing runner's context: every positional rsync word
// behind it is a possible command and is checked, as main did for `setsid … rsync`.
func TestRsyncUnmodeledBehindRunner(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	t.Setenv("CDPATH", "")
	cfg := Config{Dir: "/home/u/work"}
	cases := []struct {
		name  string
		cmd   string
		block bool
	}{
		{"setsid setpriv -- rsync remote", `setsid setpriv -- rsync ./src/ host:/dst/`, true},
		{"flock setpriv -- rsync remote", `flock ./lock setpriv -- rsync ./src/ host:/dst/`, true},
		{"find exec setpriv -- rsync remote", `find . -exec setpriv -- rsync ./src/ host:/dst/ \;`, true},
		{"setsid setpriv option rsync remote", `setsid setpriv --nnp rsync ./src/ host:/dst/`, true},
		{"setsid setpriv parallel rsync stdin", `echo host:/dst/ | setsid setpriv -- parallel rsync ./src/`, true},
		{"xargs setsid setpriv rsync stdin", `echo host:/dst/ | xargs setsid setpriv -- rsync ./src/`, true},
		// Local copies through the same shape stay allowed.
		{"setsid setpriv -- rsync local", `setsid setpriv -- rsync ./src/ ./dst/`, false},
		{"flock setpriv -- rsync local", `flock ./lock setpriv -- rsync -a ./src/ ./dst/`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Check(c.cmd, cfg)
			if (got != "") != c.block {
				t.Fatalf("Check(%q) = %q, want block=%v", c.cmd, got, c.block)
			}
		})
	}
}
