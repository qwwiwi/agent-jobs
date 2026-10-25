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
		{`gh pr "" merge`, unsureWarn},
		{`gh pr $S 5`, unsureWarn},
		{"cat <<EOF\n$(gh pr merge 5)\nEOF", unsureWarn},
		{"bash <<'EOF'\ngh pr merge 5\nEOF", unsureWarn},
		{"cat > /tmp/m.sh <<'EOF'\ngh pr merge 5\nEOF\nsh < /tmp/m.sh", unsureWarn},
		{`gh -R "$R" pr merge 5`, unsureWarn},
		{`gh -R $R pr view 5`, unsureWarn},
		{"gh pr view 5 # ok?(x)", unsureWarn},
		{"[[ x == @(#) ]]; gh pr merge 5", unsureWarn},
		// Read strictly, the comment hides the real call; the merge word with a variable in the gated
		// repository is refused (write the words literally).
		{"gh pr view merge ${x:- #}; gh pr merge 5", MergeGatePrefix},
	} {
		if r := gateAnswer(t, m, repo, tc.cmd, ""); !strings.Contains(r, tc.want) {
			t.Errorf("%q: %q, want %q", tc.cmd, r, tc.want)
		}
	}
}

// While a repository is gated, a # inside ${…}, a here-document with a $'…' delimiter and a brace
// expansion before the gh pr subcommand hide no merge: each is refused or, unreadable, warned about.
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
		{"eval ${x:- #}; gh pr merge 5", unsureWarn},
		{"cat <<$'EOF'\nx\nEOF\ngh pr m@(erge) 5", unsureWarn},
	} {
		if r := gateAnswer(t, m, repo, tc.cmd, ""); !strings.Contains(r, tc.want) {
			t.Errorf("%q: %q, want %q", tc.cmd, r, tc.want)
		}
	}
}

// While a repository is gated, a merge the reader cannot read runs (no refusal) with one warning line,
// and a parsed gh pr merge without a release pass is still refused (owner decision 2026-10-25), also one
// into the gated repository whose sha or PR number is a variable.
func TestMergeGateUnreadableRunsParsedBlocked(t *testing.T) {
	m, repo := mergeGateRepo(t)
	for _, cmd := range []string{"sh -c 'gh pr merge 5'", `gh pr $M 5`, `gh pr merge 5 -R "$REPO"`} {
		r, err := m.MergeGate(repo, cmd, "")
		if err != nil || r != "" {
			t.Fatalf("%q refused: %q %v", cmd, r, err)
		}
		a, err := m.MergeGateCheck(repo, cmd, "")
		if err != nil || a.Refusal != "" || !strings.HasPrefix(a.Warning, unsureWarn) || strings.Contains(a.Warning, "\n") {
			t.Fatalf("%q: %+v %v", cmd, a, err)
		}
	}
	// The target resolves to the gated repository, only the sha or PR word is a variable: refused.
	for _, cmd := range []string{`gh pr merge 5 --match-head-commit "$SHA"`, `gh pr merge $PR`} {
		if r := gateAnswer(t, m, repo, cmd, ""); !strings.HasPrefix(r, MergeGatePrefix) || !strings.Contains(r, "literally") {
			t.Fatalf("%q: %q", cmd, r)
		}
	}
	if r := gateAnswer(t, m, repo, "gh pr merge 5", ""); !strings.HasPrefix(r, MergeGatePrefix) {
		t.Fatalf("parsed merge without a pass: %q", r)
	}
}
