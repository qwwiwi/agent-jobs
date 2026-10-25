package guard

import (
	"go/ast"
	goparser "go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// mustRefuseMerges run a gh pr merge in bash with default options (or may): the reader reports a merge
// call or is unsure.
var mustRefuseMerges = []string{
	// The shell reader's word or command boundaries are not bash's: a relaxation must not hide the merge.
	"[[ x == @(#) ]]; gh pr merge 5",
	"(( 1 << \"E\" ))\n[[ x == @(x) ]]; gh pr merge 5",
	"gh pr view merge ${x:- #}; gh pr merge 5",
	"gh pr view merge ${x# #}; gh pr merge 5",
	"gh pr create --title merge --body ${x:- #}; gh pr merge 5",
	"gh pr view merge <<$'EOF'\nx\nEOF\ngh pr merge 5",
	"gh pr merge 5 --squash",
	`gh pr "" merge`,
	`gh pr $S 5`,
	"bash <<'EOF'\ngh pr merge 5\nEOF",
	`gh -R "$R" pr merge 5`,
	`gh -R {o/r,pr,merge,--subject} pr view --body merge --squash`,
	"echo \"<<'EOF'\"\ngh pr view merge ${x:- #}; gh pr merge 5\nEOF",
	"cat <<'EOF'\nx\nEOF\ngh pr view merge ${x:- #}; gh pr merge 5",
	"cat <<'EOF' $'\\' #'\nx\nEOF\ngh pr view merge; gh pr merge 5",
	"gh pr view merge $( echo # )\n); gh pr merge 5",
	"gh pr view merge `echo #`; gh pr merge 5",
	"gh pr view merge $\"#\"; gh pr merge 5",
	"a=(gh pr view merge #); gh pr merge 5",
	"case x in x) gh pr view merge ;; esac; gh pr merge 5",
	"ev\\al 'gh pr merge 5'",
	// Separators, quotes and here-documents around a gh pr call with merge as a data argument.
	"gh pr view merge \"$(echo ')')\"; gh pr merge 5",
	// bash 5.2 ends a here-document in $( ) at a line `E)` (with a warning) and runs the lines after it.
	"gh pr view merge \"$(cat <<'E'\nx\nE)\"\ngh pr merge 5",
	"gh pr view merge \"$(cat <<'E'\nx\nE)\"\ngh pr merge 5\nE\n)\"",
	"gh pr view merge $(cat <<'E'\nx\nE)\ngh pr merge 5\nE\n)",
	"gh pr view merge $[ ' ] ; gh pr merge 5 ; ' ]",
	"gh pr view merge $[1]; gh pr merge 5",
	"gh pr view merge \"$(echo \"(\")\"; gh pr merge 5",
	"gh pr view merge $(echo \\)); gh pr merge 5",
	"gh pr view merge \"a\\\"b\"; gh pr merge 5",
	"gh pr view merge 'a'\\''b'; gh pr merge 5",
	"gh pr view merge \"$(echo \"a;b\")\"; gh pr merge 5",
	"gh pr view merge $(echo 'a)'); gh pr merge 5",
	"gh pr view merge $(echo \")\"); gh pr merge 5",
	"gh pr view merge \"$(echo \\\")\\\")\"; gh pr merge 5",
	"gh pr view merge \"\\$(\" ; gh pr merge 5",
	"{ gh pr view merge; }; gh pr merge 5",
	"gh pr view merge & gh pr merge 5", "gh pr view merge && gh pr merge 5", "gh pr view merge|gh pr merge 5",
	"gh pr view merge;gh pr merge 5", "gh pr view merge\ngh pr merge 5", "gh pr view merge 2>&1; gh pr merge 5",
	"gh pr view merge >x; gh pr merge 5", "gh pr view merge &>x; gh pr merge 5", "gh pr view merge |& gh pr merge 5",
	"gh pr view merge <<'E'; gh pr merge 5\nx\nE", "gh pr view merge <<'E' && gh pr merge 5\nx\nE",
	"gh pr view merge <<'E' | gh pr merge 5\nx\nE", "cat <<'A'; cat <<'B'\na\nA\nb\nB\ngh pr merge 5",
	"gh pr view merge \"$(cat <<'E'\nx\nE\n)\" ; gh pr merge 5", "gh pr view merge\\\\\ngh pr merge 5",
	"gh pr view merge \\\n; gh pr merge 5", "gh pr view merge $[ 1 ]; gh pr merge 5",
	"gh pr view merge \"$(echo '\"')\"; gh pr merge 5", "gh pr view merge \"$( (echo a) )\"; gh pr merge 5",
	"gh pr view merge $(echo $(echo ')')); gh pr merge 5", "gh pr view merge \"x\n\"; gh pr merge 5",
	"gh pr view merge 'x\n'; gh pr merge 5", "gh pr view merge <<'E'\nx\nE\ngh pr merge 5",
	"gh pr view merge <<\"E\"\n'\nE\ngh pr merge 5", "gh pr view merge <<'E'\n\"\nE\ngh pr merge 5",
	"gh pr view merge <<'E'\n)\nE\ngh pr merge 5", "(gh pr view merge <<'E'\n)\nE\n); gh pr merge 5",
	"gh pr view merge \"$(cat <<'E'\n\"\nE\n)\"; gh pr merge 5", "gh pr view merge \"$(cat <<'E'\n'\nE\n)\"; gh pr merge 5",
	"gh pr view merge $(cat <<'E'\n)\nE\n); gh pr merge 5", "gh pr view merge 2<<'E'\nx\nE\ngh pr merge 5",
	"gh pr view merge <<'E'\nE\ngh pr merge 5\nE",
	"gh pr view merge\tx;\tgh pr merge 5", "gh pr view merge ; gh pr merge \"5\"",
	// An empty or unset value attached to -R leaves a bare -R: gh takes the next word as the repository.
	`gh -R"$X" pr --repo o/r pr merge 5`,
	`gh "-R$X" pr --repo o/r pr merge 5`,
	`gh -R"$X" pr -R o/r pr merge 5 --squash`,
	`gh -R"$X" pr --repo=o/r pr merge 5 --body merge`,
	// xargs rewrites the argv it runs.
	"echo merge | xargs -I% gh pr % 5 --title merge",
	"echo merge | xargs --replace=% gh pr % 5 --title merge",
	"echo merge | xargs -I% -- gh pr % 5 --title merge",
	"echo merge | xargs -I @@ gh pr @@ 5 --body merge",
}

// mustPassMerges run no gh pr merge: the relaxed reading reports neither a call nor unsure.
var mustPassMerges = []string{
	`gh pr create --title merge --body x`,
	`gh pr list --label merge`,
	`gh pr comment 78 --body merge`,
	`gh pr view merge --json headRefOid`,
	`gh pr create --title "Wave F: merge gate" --body-file f`,
	"gh pr create --title t --body \"$(cat <<'EOF'\n- **(F9)** bold\nEOF\n)\"",
	"git commit -m \"$(cat <<'EOF'\nfix gh pr merge reader: C++(17)\nEOF\n)\"",
	`gh -R "$R" pr view 5`,
}

func TestGhPrMergesRelaxationsMonotone(t *testing.T) {
	for _, cmd := range mustRefuseMerges {
		if calls, unsure := GhPrMerges(cmd, "/w"); unsure == "" && len(calls) == 0 {
			t.Errorf("%q: read as no merge", cmd)
		}
	}
	for _, cmd := range mustPassMerges {
		noMerge(t, cmd)
	}
}

// testStrings are the string literals of this package's test files that name gh or merge: the inputs of
// every test table.
func testStrings(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*_test.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("test files: %v", err)
	}
	var out []string
	fset := token.NewFileSet()
	for _, f := range files {
		af, err := goparser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil {
					if l := strings.ToLower(s); strings.Contains(l, "gh") || strings.Contains(l, "merge") {
						out = append(out, s)
					}
				}
			}
			return true
		})
	}
	return out
}

// GhPrMerges's control flow: for every input of every test table, a command that is not relaxable is
// read exactly as strictly, and a command the strict reading passes stays passed. It checks only where
// the relaxed reading is taken, not that the relaxed reading misses no merge bash runs (mustRefuseMerges
// pins known cases of that).
func TestGhPrMergesStrictUnlessRelaxableAndStrictPassKept(t *testing.T) {
	inputs := append(testStrings(t), mustRefuseMerges...)
	inputs = append(inputs, mustPassMerges...)
	if len(inputs) < 300 {
		t.Fatalf("only %d inputs collected", len(inputs))
	}
	relaxed := 0
	for _, cmd := range inputs {
		if len(cmd) > maxCommandBytes {
			continue
		}
		sCalls, sUnsure := readMerges(cmd, "/w", parseAll(cmd), false)
		calls, unsure := GhPrMerges(cmd, "/w")
		same := reflect.DeepEqual(calls, sCalls) && unsure == sUnsure
		switch {
		case !relaxable(cmd, parseAll(cmd)) && !same:
			t.Errorf("%q: not relaxable, but read %+v %q, strictly %+v %q", cmd, calls, unsure, sCalls, sUnsure)
		case sUnsure == "" && len(sCalls) == 0 && !same:
			t.Errorf("%q: passed strictly, but read %+v %q", cmd, calls, unsure)
		case !same:
			relaxed++
		}
	}
	if relaxed == 0 {
		t.Errorf("no input read relaxed")
	}
}

// The relaxable check refuses each form whose boundaries the shell reader may misplace, and takes the
// quoted here-documents it models.
func TestRelaxable(t *testing.T) {
	for _, cmd := range []string{
		"gh pr view 5 ${x}", "gh pr view $'x'", "gh pr view $\"x\"", "(( x ))", "x=$((1))", "[[ x ]]",
		"echo `x`", "eval x", "source f", ". f", "cat <<<x", "cat <<-'E'\nx\nE", "cat <<E\nx\nE",
		"cat << 'E'\nx\nE", "cat <<\\E\nx\nE", "cat <<'E'\nx\n", "cat <<'E'\nx\\\nE", "cat <<'E-1'\nx\nE-1",
		"shopt -s extglob", "set -o posix", "set +o posix", "set -euo pipefail", "bash -O extglob -c x",
		"bash +O extglob", "echo @(x)", "echo !(x)", "echo +(x)", "echo *(x)", "echo ?(x)", "x # c",
		"echo 'a", "echo \"a", "echo )", "echo (", "a=(1 2)", "diff <(a) b", "tee >(a)", "case x in x) ;; esac",
		"coproc x", "alias g=gh", "ev\\al x", "e\"v\"al x", "x\r", "cat <<'E' 'a\nb'\nx\nE", "cat <<'E' \"a\nb\"\nx\nE",
		"sh -c 'cat <<\"E\"\nx\nE'",
		"echo \"<<'E'\"\nx\nE", "echo \"$(cat <<'E' )\"\nx\nE", "echo \"$(cat <<'E'\n\"\nE\n)\"",
		"echo $(cat <<'E'\n)\nE\n)",
		"echo \"$(cat <<'E'\nx\nE)\"", "echo $[1]",
		"cat <<'E' \"$(echo\n)\"\nx\nE", "cat <<'A' \"$(cat <<'B'\nb\nB\n)\"\na\nA",
	} {
		if relaxable(cmd, parseAll(cmd)) {
			t.Errorf("%q: relaxable", cmd)
		}
	}
	for _, cmd := range []string{
		"gh pr view 5", `gh -R "$R" pr view 5`, "echo \"$(cat <<'EOF'\n- **(F9)** bold\nEOF\n)\"",
		"cat <<\"E\" >f\n#x ${y} @(z)\nE\ngh pr view 5", "{ gh pr view 5; } && (cd x)", "echo \"#\" '#' a\\#",
	} {
		if !relaxable(cmd, parseAll(cmd)) {
			t.Errorf("%q: not relaxable", cmd)
		}
	}
}
