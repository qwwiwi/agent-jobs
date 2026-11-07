package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// start --review attaches the review and turns the control review on by default; --control-review=false
// turns it off; the flag's default alone never trips the "needs review" refusal.
func TestStartCLIReviewFlags(t *testing.T) {
	t.Setenv("AGENT_JOBS_HOME", t.TempDir())
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	base := []string{"--agent", "test", "--title", "t", "--model", "sonnet", "--brief", "b"}
	if r := captureStdoutErr(t, func() error { return cmdStart(append(append([]string{}, base...), "--kind", "general")) }); r.err != nil {
		t.Fatalf("a plain start must not trip on the control-review default: %v", r.err)
	}
	rev := captureStdoutErr(t, func() error { return cmdStart(append(append([]string{}, base...), "--kind", "review", "--repo", repo)) })
	if rev.err != nil {
		t.Fatal(rev.err)
	}
	reviewID := strings.TrimSpace(rev.stdout)
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Store.Claim(reviewID, "tok", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.JobDir(reviewID), 0o700); err != nil {
		t.Fatal(err)
	}
	report := "## Summary\nVerdict: BLOCK\n\n## Findings / Changes\n\n### Blocking\n- `a.go:1` — F1: broken\n\n### Non-blocking\nnone\n"
	if err := os.WriteFile(filepath.Join(m.JobDir(reviewID), "report.md"), []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Finish(reviewID, "tok", store.StatusSucceeded, 1, 0, 1, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	fix := func(extra ...string) (*store.Job, error) {
		r := captureStdoutErr(t, func() error {
			return cmdStart(append(append([]string{}, base...), append([]string{"--kind", "fix", "--repo", repo}, extra...)...))
		})
		if r.err != nil {
			return nil, r.err
		}
		return m.Store.Get(strings.TrimSpace(r.stdout))
	}
	on, err := fix("--review", reviewID)
	if err != nil || on.Review != reviewID || !on.ControlReview || len(on.ReviewBlocking) != 1 || on.ReviewBlocking[0].ID != "F1" {
		t.Fatalf("--review: %+v %v", on, err)
	}
	off, err := fix("--review", reviewID, "--control-review=false")
	if err != nil || off.Review != reviewID || off.ControlReview {
		t.Fatalf("--control-review=false: %+v %v", off, err)
	}
	if _, err := fix("--control-review=true"); err == nil || !strings.Contains(err.Error(), "control_review") {
		t.Fatalf("--control-review without --review: %v", err)
	}
}
