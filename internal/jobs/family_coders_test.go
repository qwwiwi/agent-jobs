package jobs

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// insertCoder stores a general job of repo with covers of checklist demo v2, as the store keeps it.
func insertCoder(t *testing.T, m *Manager, repo, id, status, size, runner, model string, covers ...string) {
	t.Helper()
	if err := m.Store.Insert(&store.Job{ID: id, Agent: m.Agent, Title: "coder", Kind: "general", Brief: "b",
		RepoPath: mainWorktree(repo), Status: status, Model: model, Runner: runner, Size: size, CreatedAt: time.Now(),
		CoverContract: &store.CoverContract{ChecklistID: "demo", Version: 2}, Covers: covers}); err != nil {
		t.Fatal(err)
	}
}

// The coders a judge's family is compared with are the succeeded feature jobs: size large, or none
// recorded (a job queued before sizes); a small one, a failed or a cancelled one is not.
func TestFamilyCodersSelection(t *testing.T) {
	m := newManager(t)
	repo, _ := gateRepo(t)
	insertCoder(t, m, repo, "large-ok", store.StatusSucceeded, SizeLarge, store.RunnerClaude, "opus", "R01")
	insertCoder(t, m, repo, "large-failed", store.StatusFailed, SizeLarge, store.RunnerCodex, "gpt-5.6-sol", "R01")
	insertCoder(t, m, repo, "legacy-ok", store.StatusSucceeded, "", store.RunnerCodex, "gpt-5.6-sol", "R02")
	insertCoder(t, m, repo, "small-ok", store.StatusSucceeded, SizeSmall, store.RunnerCodex, "gpt-5.6-sol", "R01")
	insertCoder(t, m, repo, "cancelled", store.StatusCancelled, "", store.RunnerCodex, "gpt-5.6-sol", "R01")
	feature, family, err := familyCoders(m.Store, mainWorktree(repo), "demo")
	if err != nil {
		t.Fatal(err)
	}
	ids := func(l []store.CoverJob) []string {
		var out []string
		for _, c := range l {
			out = append(out, c.ID)
		}
		slices.Sort(out)
		return out
	}
	if got := ids(feature); !slices.Equal(got, []string{"cancelled", "large-failed", "large-ok", "legacy-ok"}) {
		t.Errorf("feature jobs %v", got)
	}
	if got := ids(family); !slices.Equal(got, []string{"large-ok", "legacy-ok"}) {
		t.Errorf("family coders %v", got)
	}
}

// judge_start's warning uses the same coders as release_check: a succeeded large claude coder and a
// failed codex coder warn a claude judge, not a codex one.
func TestJudgeFamilyWarningSucceededCodersOnly(t *testing.T) {
	m := newManager(t)
	repo, sha := gateRepo(t)
	approveBrief(t, m, repo)
	insertCoder(t, m, repo, "claude-ok", store.StatusSucceeded, SizeLarge, store.RunnerClaude, "opus", "R01")
	insertCoder(t, m, repo, "codex-failed", store.StatusFailed, SizeLarge, store.RunnerCodex, "gpt-5.6-sol", "R01")
	for _, tc := range []struct {
		model string
		want  string // a coder id in the warning ("" = no warning)
	}{{"gpt-5.6-sol", ""}, {"sonnet", "claude-ok"}} {
		j, err := m.StartGate(GateRequest{Kind: KindJudge, Repo: repo, SHA: sha, Model: tc.model})
		if err != nil {
			t.Fatal(err)
		}
		w, err := m.JudgeFamilyWarning(j)
		if err != nil || tc.want == "" && w != "" || tc.want != "" && (!strings.Contains(w, tc.want) || strings.Contains(w, "codex-failed")) {
			t.Errorf("judge %s: %q %v", tc.model, w, err)
		}
	}
}

// The verdict's family snapshot uses the same coders: a failed coder of the judge's family does not set
// same_family.
func TestJudgeVerdictFamilySucceededCodersOnly(t *testing.T) {
	m := acceptanceManager(t, "")
	repo, sha := gateRepo(t)
	approveBrief(t, m, repo)
	insertCoder(t, m, repo, "codex-ok", store.StatusSucceeded, SizeLarge, store.RunnerCodex, "gpt-5.6-sol", "R01")
	insertCoder(t, m, repo, "claude-failed", store.StatusFailed, SizeLarge, store.RunnerClaude, "opus", "R01")
	j := runToEnd(t, m, judgeJob(t, m, repo, sha).ID)
	if j.Status != store.StatusSucceeded {
		t.Fatalf("judge: %s %s", j.Status, j.Error)
	}
	v, err := m.Store.VerdictByJob(j.ID, j.Attempt)
	if err != nil || v == nil {
		t.Fatalf("no verdict: %v", err)
	}
	if v.SameFamily || !v.FamilyDiffers || !slices.Equal(v.CoderJobs, []string{"codex-ok"}) || !slices.Equal(v.CoderFamilies, []string{"openai"}) {
		t.Fatalf("verdict family: %+v", v)
	}
}
