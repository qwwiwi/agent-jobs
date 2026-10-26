package guard

import (
	"slices"
	"testing"
)

// bash reads `$$` as one token (the PID): a `{` right after it is a plain character, so in `$${x:-;cmd;}`
// the `;` separates commands and cmd runs. The ${…} reader must not start at the second `$`.
func TestDollarPidBeforeBrace(t *testing.T) {
	for _, src := range []string{
		"echo $${x:-;echo M1;: }",
		"echo $$$${x:-;echo M1;: }",
		`false && echo "$${x:-"; echo M1; echo "}"`,
		"false && echo ${y:-$${x:-a}; echo M1; echo }",
		`echo ${y:-"$${x:-"}; echo M1; echo "}`,
	} {
		sc, _ := parseScript(src)
		found := false
		for _, pl := range sc.pipelines {
			for _, c := range pl.cmds {
				if slices.Equal(c.words, []string{"echo", "M1"}) {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("%q: the reader does not list `echo M1`", src)
		}
	}

	_, _, cfg := noiseSetup(t)
	runRefused(t, cfg, []refusalCase{
		{"pid then brace", "echo $${x:-;git push origin main;: }", ""},
		{"two pids then brace", "echo $$$${x:-;git push origin main;: }", ""},
		{"pid in double quotes", `false && echo "$${x:-"; git push origin main; echo "}"`, ""},
		{"pid inside braces", "false && echo ${y:-$${x:-a}; git push origin main; echo }", ""},
		{"pid inside quoted braces", `echo ${y:-"$${x:-"}; git push origin main; echo "}`, ""},
	})
	runAllowed(t, cfg, []string{
		`ls "${HOME:-/tmp}"`,
		"echo ${#arr[@]}",
		"echo $$",
		`echo "$$"`,
		"kill -0 $$",
		"git format-patch -1 --stdout",
		`git log -1 "${REV:-HEAD}"`,
	})
}

// Forms the reader does not follow are refused: a `$` before a backslash-newline (bash joins the line
// first, so `$\` + newline + `${…}` is `$${…}`), and `${` before a blank, newline or `|` (bash 5.3 runs
// `${ cmd; }` and `${| cmd; }` as a command substitution).
func TestDollarVagueForms(t *testing.T) {
	_, _, cfg := noiseSetup(t)
	const want = "cannot verify"
	runRefused(t, cfg, []refusalCase{
		{"dollar continued to brace", "echo $\\\n{x:-a}", want},
		{"dollar continued to pid brace", "echo $\\\n${x:-;git push origin main;: }", ""},
		{"dollar continued in double quotes", "echo \"$\\\n${x:-\"; git push origin main; echo \"}\"", ""},
		{"dollar continued inside braces", "echo ${y:-$\\\n${x:-a}; git push origin main; echo }", ""},
		{"funsub blank", "echo ${ git push origin main; }", ""},
		{"funsub newline", "echo ${\ngit push origin main; }", ""},
		{"funsub pipe", "echo ${| git push origin main; }", ""},
		{"funsub in double quotes", `echo "${ git push origin main; }"`, ""},
		{"funsub nested", "echo ${y:-${ git push origin main; }}", ""},
		{"funsub plain", "echo ${ date; }", want},
	})
}
