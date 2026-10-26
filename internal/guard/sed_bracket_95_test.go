package guard

import "testing"

// Issue #95: GNU sed reads a delimiter inside a regex bracket expression as data (`s/[/]/x/`), so a
// regex or an address with an unescaped [ is not parsed: every e of the script counts.
func TestSedBracketExpression(t *testing.T) {
	cases := []struct {
		name    string
		script  string
		mention bool
	}{
		{"s regex bracket hides delimiter", `s/[/]/;s#x/;eclaude #/#`, true},
		{"s regex negated bracket", `s/[^/]/;s#x/;eclaude #/#`, true},
		{"s regex bracket leading ]", `s/[]/]/;s#x/;eclaude #/#`, true},
		{"address bracket hides delimiter", `\#[#s#]#eclaude #`, true},
		{"address bracket then e", `\#[#]#eclaude`, true},
		{"slash address bracket", `/[/]/s#x/;eclaude #/#`, true},
		// Brackets are literal in a replacement and in y; escaped ones are literal in a regex.
		{"bracket in replacement", `s/eclaude/[x]/`, false},
		{"bracket in y", `y/[e/]x/`, false},
		{"escaped bracket in regex", `s/\[eclaude\]/x/`, false},
		{"bracket delimiter", `s[eclaude[x[`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sedExecMention([]string{c.script, "f"}, true); got != c.mention {
				t.Errorf("sedExecMention(%q) = %v, want %v (spans %v)", c.script, got, c.mention, sedDataSpans(c.script))
			}
		})
	}
}

// The same scripts through Check: a sed script with [ is also a glob word for a program that may
// write, so these stay blocked whatever the sed reader says.
func TestCheckSedBracketExpression(t *testing.T) {
	cfg := Config{GitName: "Job Bot", GitEmail: "bot@example.com"}
	for _, cmd := range []string{
		`sed 's/[/]/;s#x/;eclaude #/#' f`,
		`sed 's/[^/]/;s#x/;eclaude #/#' f`,
		`sed 's/[]/]/;s#x/;eclaude #/#' f`,
		`sed '\#[#s#]#eclaude #' f`,
		`sed '\#[#]#eclaude' f`,
		`sed -n -e 's/[/]/;s#x/;eclaude #/#' cmd/agent-jobs/x.go`,
	} {
		if Check(cmd, cfg) == "" {
			t.Errorf("%q allowed, want blocked", cmd)
		}
	}
}
