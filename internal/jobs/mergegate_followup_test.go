package jobs

import (
	"strings"
	"testing"
)

// While a repository is gated, a gh pr call with the word merge as a data argument, an extglob-like text
// in a quoted here-document body, and a quoted dynamic -R value pass; a real or unreadable merge, and an
// extglob-like text in a comment (comments are read strictly), are still refused.
func TestMergeGateFollowUpFalseBlocks(t *testing.T) {
	m, repo := mergeGateRepo(t)
	for _, cmd := range []string{
		`gh pr create --title merge --body x`,
		`gh pr create --title "Wave F: merge gate" --body-file f`,
		`gh pr list --label merge`,
		`gh pr comment 78 --body merge`,
		`gh pr view merge --json headRefOid`,
		"gh pr create --title t --body \"$(cat <<'EOF'\n- **(F9)** bold\nEOF\n)\"",
		"git commit -m \"$(cat <<'EOF'\nfix gh pr merge reader: C++(17)\nEOF\n)\"",
		"cat > /tmp/x.md <<'EOF'\ngh pr merge docs?(x)\nEOF",
		`gh -R "$R" pr view 5`,
	} {
		if r := gateAnswer(t, m, repo, cmd, ""); r != "" {
			t.Errorf("%q: %q", cmd, r)
		}
	}
	for _, tc := range []struct{ cmd, want string }{
		{`gh pr merge 5 --squash`, "without --match-head-commit"},
		{`gh pr "" merge`, "cannot tell"},
		{`gh pr $S 5`, "cannot tell"},
		{"cat <<EOF\n$(gh pr merge 5)\nEOF", "cannot tell"},
		{"bash <<'EOF'\ngh pr merge 5\nEOF", "cannot tell"},
		{"cat > /tmp/m.sh <<'EOF'\ngh pr merge 5\nEOF\nsh < /tmp/m.sh", "cannot tell"},
		{`gh -R "$R" pr merge 5`, "cannot tell"},
		{`gh -R $R pr view 5`, "cannot tell"},
		{"gh pr view 5 # ok?(x)", "cannot tell"},
		{"[[ x == @(#) ]]; gh pr merge 5", "cannot tell"},
		{"gh pr view merge ${x:- #}; gh pr merge 5", "blocked"},
	} {
		if r := gateAnswer(t, m, repo, tc.cmd, ""); !strings.Contains(r, tc.want) {
			t.Errorf("%q: %q, want %q", tc.cmd, r, tc.want)
		}
	}
}

// While a repository is gated, a # inside ${…}, a here-document with a $'…' delimiter and a brace
// expansion before the gh pr subcommand hide no merge.
func TestMergeGateInertRangesExact(t *testing.T) {
	m, repo := mergeGateRepo(t)
	for _, cmd := range []string{
		"eval ${x:- #}; gh pr merge 5",
		"eval ${x# #}; gh pr merge 5",
		"cat <<$'EOF'\nx\nEOF\ngh pr m@(erge) 5",
		`gh -R {o/r,pr,merge,--subject} pr view --body merge --squash`,
		`bash -c 'gh -R {o/r,pr,merge,--subject} pr view --body merge --squash'`,
	} {
		if r := gateAnswer(t, m, repo, cmd, ""); r == "" {
			t.Errorf("%q: passed, want refused", cmd)
		}
	}
	for _, tc := range []struct{ cmd, want string }{
		{"eval ${x:- #}; gh pr merge 5", "cannot tell"},
		{"cat <<$'EOF'\nx\nEOF\ngh pr m@(erge) 5", "cannot tell"},
	} {
		if r := gateAnswer(t, m, repo, tc.cmd, ""); !strings.Contains(r, tc.want) {
			t.Errorf("%q: %q, want %q", tc.cmd, r, tc.want)
		}
	}
}
