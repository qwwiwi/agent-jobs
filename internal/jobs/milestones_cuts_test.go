package jobs

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/guard"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// cutCheck is one bureaucracy cut of the R08 audit: the bold title of its DECISIONS.md row and a check
// that calls the code the row says was relaxed, so a cut that is only written down, or reverted later,
// fails here.
type cutCheck struct {
	title  string
	inCode func(t *testing.T)
}

// r08Cuts maps every cut row of DECISIONS.md (marked «checklist R08» or «cut bureaucracy that costs
// time without catching defects») to its code. A new cut row needs an entry here.
var r08Cuts = []cutCheck{
	{"Cut guard noise that costs job turns.", func(t *testing.T) {
		state := t.TempDir()
		own := filepath.Join(state, "jobs", "own")
		cfg := guard.Config{StateDir: state, OwnDir: own, GitName: "Job Bot", GitEmail: "bot@example.com", OwnerNames: []string{"Alex"}}
		db := filepath.Join(state, "jobs.db")
		for _, cmd := range []string{
			"cat > notes.md <<'EOF'\nthe database is " + db + "\nEOF", // a literal here-doc written by cat
			"tee notes.md <<'EOF'\nreports: " + state + "/jobs/*/report.md\nEOF",
			`echo "$(git log --oneline -3)"`, // read-only git in a substitution
			`python3 -I -c "print(2*3)"`,     // * inside inline code
		} {
			if r := guard.Check(cmd, cfg); r != "" {
				t.Errorf("relaxed form refused: %q: %s", cmd, r)
			}
		}
		// The refusals the row keeps: state reads, outward actions, git config and identity, package runners.
		for _, cmd := range []string{"cat " + db, "git push origin main", `echo "$(git push origin main)"`, "gh pr merge 3",
			`git -c user.name=Someone commit -m x`, `echo "$(git config core.hooksPath /tmp/h)"`, "npx tsx x.ts"} {
			if guard.Check(cmd, cfg) == "" {
				t.Errorf("kept refusal allowed: %q", cmd)
			}
		}
	}},
	{"Release gate deadlock cut", func(t *testing.T) {
		if !enforcedBlind(store.RunnerCodex, CodexSandboxReadOnly) || enforcedBlind(store.RunnerCodex, CodexSandboxFullAccess) ||
			!enforcedBlind(store.RunnerClaude, "") {
			t.Error("a codex judge under the read-only sandbox is not enforced_blind, or one with full access is")
		}
		// The coders the family is read from are the succeeded feature jobs only.
		m := newManager(t)
		repo, _ := gateRepo(t)
		insertCoder(t, m, repo, "large-ok", store.StatusSucceeded, SizeLarge, store.RunnerClaude, "opus", "R01")
		insertCoder(t, m, repo, "large-failed", store.StatusFailed, SizeLarge, store.RunnerCodex, "gpt-5.6-sol", "R01")
		insertCoder(t, m, repo, "small-ok", store.StatusSucceeded, SizeSmall, store.RunnerCodex, "gpt-5.6-sol", "R01")
		_, family, err := familyCoders(m.Store, mainWorktree(repo), "demo")
		if err != nil {
			t.Fatal(err)
		}
		if len(family) != 1 || family[0].ID != "large-ok" {
			t.Errorf("family coders %+v, want only the succeeded large job", family)
		}
		if slices.Contains(ReleaseConditions, "family") {
			t.Errorf("release_check still has a family condition: %v", ReleaseConditions)
		}
	}},
	{"The `trace` condition of `release_check` is cut.", func(t *testing.T) {
		if slices.Contains(ReleaseConditions, "trace") {
			t.Errorf("release_check still has a trace condition: %v", ReleaseConditions)
		}
	}},
	{"Owner waivers bind to (checklist hash, requirement)", func(t *testing.T) {
		// release_check at a newer commit still takes a waiver stored with the sha of an older one.
		m := newManager(t)
		repo := sizeRepo(t, m)
		older := gitT(t, repo, "rev-parse", "HEAD")
		first, err := m.ReleaseCheck(repo, older)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Store.PutWaiver(store.Waiver{Repo: first.Repo, ChecklistID: first.ChecklistID, ChecklistSHA256: first.ChecklistSHA256,
			ChecklistVersion: first.Version, SHA: older, Requirement: "R01", Quote: "waive R01", CreatedAt: m.Now()}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, "fix.go"), []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitT(t, repo, "add", "fix.go")
		gitT(t, repo, "commit", "-qm", "a fix after the waiver")
		newer := gitT(t, repo, "rev-parse", "HEAD")
		if newer == older {
			t.Fatal("no new commit")
		}
		r, err := m.ReleaseCheck(repo, newer)
		if err != nil {
			t.Fatal(err)
		}
		waived := map[string]string{}
		for _, req := range r.Requirements {
			if req.Waived {
				waived[req.ID] = req.WaiverQuote
			}
		}
		if len(waived) != 1 || waived["R01"] != "waive R01" {
			t.Errorf("release_check at %s after a waiver at %s: waived %v, want only R01\n%+v", newer, older, waived, r.Requirements)
		}
	}},
	{"Merge gate: a command the reader cannot read no longer refuses.", func(t *testing.T) {
		m, repo := mergeGateRepo(t)
		a, err := m.MergeGateCheck(repo, "sh -c 'gh pr merge 5'", "")
		if err != nil || a.Refusal != "" || !strings.HasPrefix(a.Warning, unsureWarn) {
			t.Errorf("unreadable merge: %+v %v, want a warning and no refusal", a, err)
		}
		if r := gateAnswer(t, m, repo, "gh pr merge 5", ""); !strings.HasPrefix(r, MergeGatePrefix) {
			t.Errorf("parsed merge without a release pass: %q, want refused", r)
		}
	}},
	{"The Requirements block and REDPROOF are informational.", func(t *testing.T) {
		report := "## Summary\nok\n\n## Findings / Changes\n- x\n\n### Requirements\n- R01 · done · web/login.go:12\n- R02 · done\n\n" +
			"## Not done / uncertain\nnothing\n"
		if r := ParseRequirements(report, []string{"R01", "R02"}); len(r.Claimed) != 0 || len(r.Lines) != 2 || !r.Lines[1].Verified {
			t.Errorf("a done line without ran or REDPROOF does not count: %+v", r)
		}
		rules, err := template("prompts/requirements.md")
		if err != nil {
			t.Fatal(err)
		}
		fix, err := template("prompts/kind-fix.md")
		if err != nil {
			t.Fatal(err)
		}
		if n := len(strings.Split(strings.TrimSpace(rules), "\n")); n > 5 || !strings.Contains(rules, "- R01 · done|partial|missing · evidence") ||
			strings.Contains(rules, "REDPROOF") {
			t.Errorf("prompts/requirements.md is not the five-line format (%d lines):\n%s", n, rules)
		}
		if strings.Contains(fix, "REDPROOF") || !strings.Contains(fix, "First reproduce the problem with a failing test.") {
			t.Errorf("kind-fix.md asks for REDPROOF or lost the failing test first:\n%s", fix)
		}
	}},
	{"Job size is optional.", func(t *testing.T) {
		m := newManager(t)
		if m.Config().SizeRequired {
			t.Error("size_required is on by default")
		}
		repo := sizeRepo(t, m)
		withCovers := enqueue(t, m, "b", func(r *StartRequest) {
			r.Kind, r.RepoPath, r.Covers, r.SizeRule = "fix", repo, []string{"R01"}, true
		})
		plain := enqueue(t, m, "b", func(r *StartRequest) { r.Kind, r.RepoPath, r.SizeRule = "fix", repo, true })
		if a, b := get(t, m, withCovers.ID).Size, get(t, m, plain.ID).Size; a != SizeLarge || b != SizeSmall {
			t.Errorf("sizes without size: %q with covers, %q without; want large, small", a, b)
		}
	}},
	{"The brief reminder speaks only when a checklist decision is pending.", func(t *testing.T) {
		m := newManager(t)
		if err := m.Store.BindHookSession("s1", m.Now()); err != nil {
			t.Fatal(err)
		}
		repo := sizeRepo(t, m)
		for _, c := range []struct{ cwd, prompt string }{{repo, "build the login page"}, {newRepo(t), "build it"},
			{repo, "[jobs:job-done] job x succeeded"}} {
			if line, err := m.BriefReminder(UserPromptInput{SessionID: "s1", Cwd: c.cwd, Prompt: c.prompt}); err != nil || line != "" {
				t.Errorf("nothing pending (%s, %q): %q %v", c.cwd, c.prompt, line, err)
			}
		}
	}},
	{"The slicing check before approval is advisory.", func(t *testing.T) {
		m := newManager(t)
		repo := sizeRepo(t, m)
		if _, _, err := m.SlicingGate(mainWorktree(repo), "demo", 2, "h", true, nil, m.Now()); err != nil {
			t.Errorf("approve without a slicing result refused: %v", err)
		}
	}},
}

// Every bureaucracy cut of the R08 audit is a row of DECISIONS.md with its measured reason, and each is
// in the code: r08Cuts calls the relaxed code of every row.
func TestMilestoneCutsDecided(t *testing.T) {
	t.Run("[R08] each cut in DECISIONS has its measured reason and is in the code", func(t *testing.T) {
		doc := readDoc(t, "docs/DECISIONS.md")
		var rows []string
		for _, line := range strings.Split(doc, "\n") {
			if strings.HasPrefix(line, "| ") && (strings.Contains(line, "cut bureaucracy that costs time without catching defects") ||
				strings.Contains(line, "(checklist R08)")) {
				rows = append(rows, line)
			}
		}
		if len(rows) < len(r08Cuts) {
			t.Errorf("%d cut rows in DECISIONS.md, %d cuts in the table", len(rows), len(r08Cuts))
		}
		for _, r := range rows {
			if !strings.Contains(r, "Measured reason:") {
				t.Errorf("a cut without a measured reason: %.120s", r)
			}
			if !slices.ContainsFunc(r08Cuts, func(c cutCheck) bool { return strings.Contains(r, "**"+c.title) }) {
				t.Errorf("a cut row with no code check in r08Cuts: %.120s", r)
			}
		}
		for _, c := range r08Cuts {
			if !slices.ContainsFunc(rows, func(r string) bool { return strings.Contains(r, "**"+c.title) }) {
				t.Errorf("DECISIONS.md has no cut row %q", c.title)
			}
		}
		guardRow := ""
		if i := slices.IndexFunc(rows, func(r string) bool { return strings.Contains(r, "**Cut guard noise") }); i >= 0 {
			guardRow = rows[i]
		}
		if !strings.Contains(guardRow, "the money, auth, secrets") || !strings.Contains(guardRow, "refusals are unchanged") {
			t.Errorf("the guard cut does not keep the money and auth refusals: %q", guardRow)
		}
		for _, c := range r08Cuts {
			t.Run(c.title, c.inCode)
		}
	})
}
