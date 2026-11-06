package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// The job-done line flags a coder job that ended within three minutes without one commit over its
// base (#132): on for both coder kinds and both flagged statuses, and off for each condition it
// needs — a longer run, a job with commits, a review or research kind, a job without a repository,
// a failed or timed-out status — and off when the merge check has not run (nothing to reuse, so no
// guess). The zero-commits finding is the merge check's own, cached like a merge note.
func TestSuspiciousNoteConditions(t *testing.T) {
	m := newManager(t)
	m.mergeChecks = map[string]mergeCheck{} // seeded below like a run merge check would
	cases := []struct {
		name string
		job  *store.Job
		// cache seeds m.mergeChecks for the job's id; nil leaves it without an entry.
		cache *mergeCheck
		want  string
	}{
		{
			name:  "fix succeeded fast with no commits",
			job:   &store.Job{ID: "on-fix", Kind: "fix", Status: store.StatusSucceeded, RepoPath: "/repo", BaseCommit: "b1", DurationSec: 100},
			cache: &mergeCheck{head: "b1", noCommits: true},
			want:  "suspicious: no commits in 2m",
		},
		{
			name:  "general cancelled fast with no commits",
			job:   &store.Job{ID: "on-general", Kind: "general", Status: store.StatusCancelled, RepoPath: "/repo", BaseCommit: "b1", DurationSec: 20},
			cache: &mergeCheck{head: "b1", noCommits: true},
			want:  "suspicious: no commits in 0m",
		},
		{
			name:  "three minutes or more is not fast",
			job:   &store.Job{ID: "off-long", Kind: "fix", Status: store.StatusSucceeded, RepoPath: "/repo", BaseCommit: "b1", DurationSec: 180},
			cache: &mergeCheck{head: "b1", noCommits: true},
			want:  "",
		},
		{
			name:  "a job with commits over its base",
			job:   &store.Job{ID: "off-commits", Kind: "fix", Status: store.StatusSucceeded, RepoPath: "/repo", BaseCommit: "b1", DurationSec: 60},
			cache: &mergeCheck{head: "h1", tip: "t1", note: "merges cleanly onto main at 000000000000"},
			want:  "",
		},
		{
			name:  "review kind is never flagged",
			job:   &store.Job{ID: "off-review", Kind: "review", Status: store.StatusSucceeded, RepoPath: "/repo", BaseCommit: "b1", DurationSec: 60},
			cache: &mergeCheck{head: "b1", noCommits: true},
			want:  "",
		},
		{
			name:  "research kind is never flagged",
			job:   &store.Job{ID: "off-research", Kind: "research", Status: store.StatusSucceeded, RepoPath: "/repo", BaseCommit: "b1", DurationSec: 60},
			cache: &mergeCheck{head: "b1", noCommits: true},
			want:  "",
		},
		{
			name: "no repository",
			job:  &store.Job{ID: "off-norepo", Kind: "fix", Status: store.StatusSucceeded, BaseCommit: "", DurationSec: 60},
			want: "",
		},
		{
			name:  "failed status",
			job:   &store.Job{ID: "off-failed", Kind: "fix", Status: store.StatusFailed, RepoPath: "/repo", BaseCommit: "b1", DurationSec: 60},
			cache: &mergeCheck{head: "b1", noCommits: true},
			want:  "",
		},
		{
			name:  "timed-out status",
			job:   &store.Job{ID: "off-timeout", Kind: "fix", Status: store.StatusTimeout, RepoPath: "/repo", BaseCommit: "b1", DurationSec: 60},
			cache: &mergeCheck{head: "b1", noCommits: true},
			want:  "",
		},
		{
			name: "the merge check has not run",
			job:  &store.Job{ID: "off-unknown", Kind: "fix", Status: store.StatusSucceeded, RepoPath: "/repo", BaseCommit: "b1", DurationSec: 60},
			want: "",
		},
	}
	for _, c := range cases {
		if c.cache != nil {
			m.mergeChecks[c.job.ID] = *c.cache
		}
		if got := m.suspiciousNote(c.job); got != c.want {
			t.Errorf("%s: note %q, want %q", c.name, got, c.want)
		}
		if c.cache == nil {
			continue
		}
		// The delivered line carries the note after the merge note, before «owner waits».
		if c.want != "" {
			j := c.job
			j.OwnerWaits = true
			line := m.DoneLineFor(j)
			if !strings.Contains(line, "· "+c.want+" · "+OwnerWaitsMarker) {
				t.Errorf("%s: done line: %q", c.name, line)
			}
		}
	}
}

// A coder job that runs the whole lifecycle carries the note on its delivered job-done line; a job
// that commits does not. The delivered line is what the fake tmux accepted.
func TestSuspiciousNoteOnDoneLine(t *testing.T) {
	m := newManager(t)
	logf := notifyTo(t, m)
	repo := newRepo(t)
	fast := enqueue(t, m, "plain brief", func(r *StartRequest) { r.RepoPath = repo; r.Kind = "general" })
	slow := enqueue(t, m, "FAKE_COMMIT", func(r *StartRequest) { r.RepoPath = repo; r.Kind = "fix" })
	fast = runToEnd(t, m, fast.ID)
	slow = runToEnd(t, m, slow.ID)
	if fast.Status != store.StatusSucceeded || slow.Status != store.StatusSucceeded {
		t.Fatalf("statuses: %s %s (%s)", fast.Status, slow.Status, slow.Error)
	}
	if fast.DurationSec >= 180 {
		t.Fatalf("the empty job took %ds; the fake is meant to be fast", fast.DurationSec)
	}
	b, err := os.ReadFile(logf)
	if err != nil {
		t.Fatal(err)
	}
	var fastLine, slowLine string
	for _, l := range strings.Split(string(b), "\n") {
		switch {
		case strings.Contains(l, "[jobs:job-done] job "+fast.ID):
			fastLine = l
		case strings.Contains(l, "[jobs:job-done] job "+slow.ID):
			slowLine = l
		}
	}
	if !strings.Contains(fastLine, "· suspicious: no commits in ") {
		t.Fatalf("empty job's line: %q\n%s", fastLine, b)
	}
	if strings.Contains(slowLine, "suspicious") || !strings.Contains(slowLine, "merges cleanly onto main at ") {
		t.Fatalf("committing job's line: %q\n%s", slowLine, b)
	}
}

// #132 fix loop 1: the zero-commits finding never outlives the check that made it. The first check
// of an empty job arms the note; once the job's head gains a commit, a later check that fails — a
// timeout, a missing base branch, a worktree that no longer verifies — must leave the note off: the
// flag fires only on the latest check's own finding, never on a stale one.
func TestSuspiciousStaleFindingNeverFlags(t *testing.T) {
	// breakIt makes the recheck return through one of its not-checked paths, after the job's head
	// has moved past its base.
	cases := []struct {
		name    string
		breakIt func(t *testing.T, m *Manager, repo, work string)
	}{
		{
			name: "timed out check",
			breakIt: func(t *testing.T, m *Manager, repo, work string) {
				old := MergeCheckTimeout
				MergeCheckTimeout = time.Nanosecond
				t.Cleanup(func() { MergeCheckTimeout = old })
			},
		},
		{
			name: "branch missing",
			breakIt: func(t *testing.T, m *Manager, repo, work string) {
				gitT(t, repo, "update-ref", "-d", "refs/heads/main")
			},
		},
		{
			name: "worktree not verified",
			breakIt: func(t *testing.T, m *Manager, repo, work string) {
				if err := os.Remove(filepath.Join(work, ".git")); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newManager(t)
			repo := newRepo(t)
			j := enqueue(t, m, "plain brief", func(r *StartRequest) { r.RepoPath = repo; r.Kind = "fix" })
			if err := m.Tick(); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "exit_code", 15*time.Second, func() bool {
				_, ok := readExitCode(m.JobDir(j.ID))
				return ok
			})
			j = get(t, m, j.ID)
			// The first check finds the head still at the base: the note is armed.
			m.checkMerge(j)
			if no, ok := m.noCommitsOverBase(j); !ok || !no {
				t.Fatalf("empty check: noCommits %t, ok %t", no, ok)
			}
			// The head gains a commit after the check (a failed delivery's retry window).
			work := filepath.Join(m.JobDir(j.ID), "work")
			gitT(t, work, "commit", "-q", "--allow-empty", "-m", "late work")
			c.breakIt(t, m, repo, work)
			m.checkMerge(j)
			if !strings.Contains(j.MergeNote, "not checked") {
				t.Fatalf("recheck note: %q", j.MergeNote)
			}
			// The recheck failed: a fast succeeded line must not carry the stale finding.
			flagged := *j
			flagged.Status = store.StatusSucceeded
			flagged.DurationSec = 60
			if note := m.suspiciousNote(&flagged); note != "" {
				t.Fatalf("stale finding still flags the job: %q", note)
			}
		})
	}
}

// #132 fix loop 1: a requeued (resumed) job drops the stopped attempt's zero-commits finding — the
// next attempt's line is judged by its own merge check, not by the attempt that was stopped empty.
func TestSuspiciousNoteClearedOnResume(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, true)
	repo := newRepo(t)
	st := startAndStop(t, m, func(r *StartRequest) { r.RepoPath = repo; r.Kind = "fix" })
	j := get(t, m, st.ID)
	// The merge check of the stopped attempt found the head still at the base.
	m.checkMerge(j)
	if no, ok := m.noCommitsOverBase(j); !ok || !no {
		t.Fatalf("stopped job: noCommits %t, ok %t", no, ok)
	}
	if _, err := m.Resume(st.ID, "NEW_FACT_1: go on", Limits{MaxBudgetUSD: 4}); err != nil {
		t.Fatal(err)
	}
	if no, ok := m.noCommitsOverBase(get(t, m, st.ID)); ok && no {
		t.Fatal("the resume kept the stopped attempt's zero-commits finding")
	}
	flagged := *get(t, m, st.ID)
	flagged.Status = store.StatusSucceeded
	flagged.DurationSec = 60
	if note := m.suspiciousNote(&flagged); note != "" {
		t.Fatalf("resumed job flagged on the old finding: %q", note)
	}
}
