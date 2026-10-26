package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// G02 is deferred (owner decision 2026-10-26): every entry point refuses a non-empty sources list before
// anything is stored or queued, and the daemon never clones, even for a stored job with sources.

const g02Off = "G02 disabled until OS-level isolation (owner decision 2026-10-26): "

// refusedSources fails the test unless err is the sources-disabled refusal.
func refusedSources(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, ErrSourcesDisabled) || !strings.Contains(err.Error(), "sources is disabled until OS-level isolation lands") {
		t.Fatalf("%s: want the sources-disabled refusal, got %v", what, err)
	}
}

// countJobs is the number of job rows in the store.
func countJobs(t *testing.T, m *Manager) int {
	t.Helper()
	all, err := m.Store.List(1000)
	if err != nil {
		t.Fatal(err)
	}
	return len(all)
}

// storedJobWithSources inserts a job row with sources directly, as an older daemon could have left it.
func storedJobWithSources(t *testing.T, m *Manager) *store.Job {
	t.Helper()
	j, err := m.newJob(StartRequest{Title: "old", Kind: "research", Brief: "look at lib", Model: "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	j.Sources = []string{"https://github.com/a/lib"}
	if err := m.Store.Insert(j); err != nil {
		t.Fatal(err)
	}
	return j
}

func TestSourcesDisabledJobStart(t *testing.T) {
	m := newManager(t)
	t.Run("[G02 off] job_start refuses a non-empty sources list and queues nothing", func(t *testing.T) {
		_, err := m.Enqueue(StartRequest{Title: "t", Brief: "b", Kind: "research", Model: "sonnet",
			Sources: []string{"https://github.com/a/lib"}})
		refusedSources(t, "job_start", err)
		if n := countJobs(t, m); n != 0 {
			t.Fatalf("%d jobs stored after the refusal", n)
		}
	})
	t.Run("[G02 off] an empty or absent sources list starts as before", func(t *testing.T) {
		enqueue(t, m, "x", func(r *StartRequest) { r.Sources = []string{} })
		enqueue(t, m, "x", nil)
		if n := countJobs(t, m); n != 2 {
			t.Fatalf("%d jobs stored, want 2", n)
		}
	})
}

func TestSourcesDisabledMilestones(t *testing.T) {
	m := newManager(t)
	ctx := context.Background()
	t.Run("[G02 off] milestone_create refuses planned sources and stores nothing", func(t *testing.T) {
		_, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "S", Tasks: []MilestoneTaskInput{
			{Key: "T1", Title: "study", Sources: deps("https://github.com/a/lib")}}})
		refusedSources(t, "milestone_create", err)
		all, err := m.Store.ListMilestones(ctx, store.MilestoneFilter{})
		if err != nil || len(all) != 0 {
			t.Fatalf("milestones after the refusal: %d %v", len(all), err)
		}
	})
	t.Run("[G02 off] milestone_edit refuses planned sources; an omitted list keeps legacy ones, an empty list clears them", func(t *testing.T) {
		legacy := "https://github.com/a/lib"
		id, err := m.Store.CreateMilestone(&store.Milestone{Title: "legacy", Status: store.MilestoneOpen},
			[]store.MilestoneTask{{Key: "T1", Title: "study", Sources: []string{legacy}}}, m.Now())
		if err != nil {
			t.Fatal(err)
		}
		sourcesOf := func(what string) []string {
			t.Helper()
			tasks, err := m.Store.MilestoneTasks(ctx, id)
			if err != nil || len(tasks) != 1 {
				t.Fatalf("%s: tasks %+v %v", what, tasks, err)
			}
			return tasks[0].Sources
		}
		_, err = m.SetMilestone(ctx, MilestoneSet{ID: id, Tasks: []MilestoneTaskInput{
			{Key: "T1", Sources: deps("https://github.com/a/other")}}})
		refusedSources(t, "milestone_edit", err)
		if got := sourcesOf("after the refusal"); len(got) != 1 || got[0] != legacy {
			t.Fatalf("sources after the refusal: %v", got)
		}
		if _, err := m.SetMilestone(ctx, MilestoneSet{ID: id, Tasks: []MilestoneTaskInput{
			{Key: "T1", Title: "study more"}}}); err != nil {
			t.Fatalf("milestone_edit without sources: %v", err)
		}
		if got := sourcesOf("sources omitted"); len(got) != 1 || got[0] != legacy {
			t.Fatalf("omitted sources did not keep the stored ones: %v", got)
		}
		if _, err := m.SetMilestone(ctx, MilestoneSet{ID: id, Tasks: []MilestoneTaskInput{
			{Key: "T1", Sources: deps()}}}); err != nil {
			t.Fatalf("milestone_edit with sources: []: %v", err)
		}
		if got := sourcesOf("sources: []"); len(got) != 0 {
			t.Fatalf("sources: [] did not clear the stored ones: %v", got)
		}
	})
	t.Run("[G02 off] a job on a task with stored planned sources is refused; sources: [] runs it without", func(t *testing.T) {
		id, err := m.Store.CreateMilestone(&store.Milestone{Title: "old", Status: store.MilestoneOpen},
			[]store.MilestoneTask{{Key: "T1", Title: "study", Sources: []string{"https://github.com/a/lib"}}}, m.Now())
		if err != nil {
			t.Fatal(err)
		}
		before := countJobs(t, m)
		_, err = m.Enqueue(StartRequest{Title: "t", Brief: "b", Kind: "research", Model: "sonnet", Milestone: id, Task: "T1"})
		refusedSources(t, "job on a planned task", err)
		if !strings.Contains(err.Error(), "sources: []") {
			t.Fatalf("no hint on the planned sources: %v", err)
		}
		if n := countJobs(t, m); n != before {
			t.Fatalf("a job was stored: %d -> %d", before, n)
		}
		j := enqueue(t, m, "x", func(r *StartRequest) { r.Milestone, r.Task, r.Kind, r.Sources = id, "T1", "research", []string{} })
		if len(j.Sources) != 0 {
			t.Fatalf("explicit empty sources: %v", j.Sources)
		}
	})
}

func TestSourcesDisabledResumeAmend(t *testing.T) {
	m := newManager(t)
	t.Run("[G02 off] amend refuses a queued job that carries sources", func(t *testing.T) {
		j := storedJobWithSources(t, m)
		_, err := m.Amend(j.ID, "more")
		refusedSources(t, "amend", err)
	})
	t.Run("[G02 off] resume refuses a job that carries sources", func(t *testing.T) {
		j := storedJobWithSources(t, m)
		if err := m.Store.Finish(j.ID, "", store.StatusFailed, 0, 0, 0, "test", m.Now()); err != nil {
			t.Fatal(err)
		}
		_, err := m.Resume(j.ID, "more", Limits{})
		refusedSources(t, "resume", err)
		if got := get(t, m, j.ID); got.Status != store.StatusFailed {
			t.Fatalf("job is %s after the refused resume", got.Status)
		}
	})
}

// A legacy job row with sources that stopped at its turn limit or timed out is not auto-continued:
// queueResume refuses it before anything is requeued, and the job keeps its status with the reason.
func TestSourcesDisabledAutoContinue(t *testing.T) {
	for _, c := range []struct{ why, status string }{
		{stopTurnLimit, store.StatusFailed},
		{stopTimeout, store.StatusTimeout},
	} {
		t.Run("[G02 off] "+c.why+": a job with sources is not requeued", func(t *testing.T) {
			m := newManager(t)
			j, err := m.newJob(StartRequest{Title: "old", Kind: "research", Brief: "look at lib", Model: "sonnet"})
			if err != nil {
				t.Fatal(err)
			}
			started := m.Now().Add(-time.Minute)
			j.Sources = []string{"https://github.com/a/lib"}
			j.Status, j.Attempt, j.StartedAt, j.Turns = c.status, 1, &started, 7
			if err := m.Store.Insert(j); err != nil {
				t.Fatal(err)
			}
			dir := m.JobDir(j.ID)
			if err := os.MkdirAll(filepath.Join(dir, "work"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "prompt.md"), []byte("task"), 0o600); err != nil {
				t.Fatal(err)
			}
			unlock, err := m.lock()
			if err != nil {
				t.Fatal(err)
			}
			m.autoContinue(j.ID, c.why)
			unlock()
			got := get(t, m, j.ID)
			if got.Status != c.status || got.Attempt != 1 {
				t.Fatalf("job is %s attempt %d after the auto-continue: %q", got.Status, got.Attempt, got.Error)
			}
			if !strings.Contains(got.Error, "not auto-continued: ") || !strings.Contains(got.Error, "sources is disabled until OS-level isolation lands") {
				t.Fatalf("no disabled reason on the job: %q", got.Error)
			}
			if queued, err := m.Store.ListByStatus(store.StatusQueued); err != nil || len(queued) != 0 {
				t.Fatalf("queued jobs: %d %v", len(queued), err)
			}
			if atts, err := m.Store.Attempts(j.ID); err != nil || len(atts) != 0 {
				t.Fatalf("attempts archived: %+v %v", atts, err)
			}
		})
	}
}

func TestSourcesDisabledDaemonNeverClones(t *testing.T) {
	t.Run("[G02 off] a stored queued job with sources is cancelled with the reason; nothing is cloned", func(t *testing.T) {
		m := newManager(t)
		var clones atomic.Int32
		prev := cloneOne
		t.Cleanup(func() { cloneOne = prev })
		cloneOne = func(_ context.Context, u, target string, _ int64, _ string) sourceResult {
			clones.Add(1)
			return sourceResult{URL: u, Name: filepath.Base(target), Path: target}
		}
		j := storedJobWithSources(t, m)
		m.prefetchSources()
		if err := m.Tick(); err != nil {
			t.Fatal(err)
		}
		m.WaitPrefetch()
		if n := clones.Load(); n != 0 {
			t.Fatalf("cloned %d times", n)
		}
		if _, err := os.Lstat(filepath.Join(m.JobDir(j.ID), SourcesDir)); !os.IsNotExist(err) {
			t.Fatalf("sources/ exists: %v", err)
		}
		got := get(t, m, j.ID)
		if got.Status != store.StatusCancelled || !strings.Contains(got.Error, "sources is disabled until OS-level isolation lands") {
			t.Fatalf("job %s: %q", got.Status, got.Error)
		}
	})
	t.Run("[G02 off] cloneSources refuses a job with sources", func(t *testing.T) {
		m := newManager(t)
		j := &store.Job{ID: "x", Sources: []string{"https://github.com/a/lib"}}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := m.cloneSources(ctx, j, t.TempDir())
		refusedSources(t, "cloneSources", err)
	})
}
