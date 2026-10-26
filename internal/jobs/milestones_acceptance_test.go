package jobs

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

func readDoc(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../../" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// milestoneOverheadBound is how much slower, per call, a step may be with a milestone than without one.
// Generous on purpose: the calls take a few milliseconds, so a real regression (a scan per task, a query
// per job) shows, while a loaded host does not make the test flaky.
const milestoneOverheadBound = 50 * time.Millisecond

func timed(t *testing.T, f func() error) time.Duration {
	t.Helper()
	start := time.Now()
	if err := f(); err != nil {
		t.Fatal(err)
	}
	return time.Since(start)
}

func median(d []time.Duration) time.Duration {
	s := slices.Clone(d)
	slices.Sort(s)
	return s[len(s)/2]
}

// The milestone MVP adds no noticeable latency: job_start (Enqueue), the daemon's launch step (the
// milestone recheck and the claim) and a daemon tick are timed without a milestone and with one of 20
// tasks (10 ready, 10 waiting on them), and the median overhead per call stays under the bound.
func TestMilestoneLatency(t *testing.T) {
	t.Run("[R09i] job_start, claim and the daemon tick stay fast with a 20-task milestone", func(t *testing.T) {
		m := newManager(t)
		ctx := context.Background()
		var tasks []MilestoneTaskInput
		for i := 1; i <= 20; i++ {
			task := MilestoneTaskInput{Key: fmt.Sprintf("T%d", i), Title: fmt.Sprintf("task %d", i)}
			if i > 10 {
				task.DependsOn = deps(fmt.Sprintf("T%d", i-10))
			}
			tasks = append(tasks, task)
		}
		v, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "latency", Tasks: tasks})
		if err != nil {
			t.Fatal(err)
		}
		req := func(task string) StartRequest {
			r := StartRequest{Title: "latency job", Kind: "general", Brief: "measure", Model: "sonnet"}
			if task != "" {
				r.Milestone, r.Task = v.ID, task
			}
			return r
		}
		check := func(what string, plain, withMilestone []time.Duration) {
			t.Helper()
			p, w := median(plain), median(withMilestone)
			t.Logf("%s: median %v without a milestone, %v with one (overhead %v, %d runs each)", what, p, w, w-p, len(plain))
			if w-p > milestoneOverheadBound {
				t.Errorf("%s: a milestone adds %v per call, bound %v", what, w-p, milestoneOverheadBound)
			}
		}
		const n = 15

		// job_start, plain and on a ready task in turn; the queued jobs are cancelled before the ticks.
		var plain, withMilestone []time.Duration
		var queued []*store.Job
		for i := 0; i < n; i++ {
			for _, task := range []string{"", fmt.Sprintf("T%d", i%10+1)} {
				var j *store.Job
				d := timed(t, func() (err error) { j, err = m.Enqueue(req(task)); return err })
				if task == "" {
					plain = append(plain, d)
				} else {
					withMilestone = append(withMilestone, d)
				}
				queued = append(queued, j)
			}
		}
		check("job_start", plain, withMilestone)

		// The launch step of a tick: the claim of a plain job, the milestone recheck plus the claim of a job
		// on a task; each claimed job is put back in the queue for the next run.
		pj, mj := queued[0], queued[1]
		plain, withMilestone = nil, nil
		for i := 0; i < n; i++ {
			token := newToken()
			plain = append(plain, timed(t, func() error { return m.claim(pj, token) }))
			if err := m.Store.Unclaim(pj.ID, token); err != nil {
				t.Fatal(err)
			}
			token = newToken()
			withMilestone = append(withMilestone, timed(t, func() error {
				wait, cancel, err := m.milestoneLaunch(ctx, mj)
				if err != nil || wait != "" || cancel != "" {
					return fmt.Errorf("launch check of a ready task: wait %q cancel %q err %v", wait, cancel, err)
				}
				return m.claim(mj, token)
			}))
			if err := m.Store.Unclaim(mj.ID, token); err != nil {
				t.Fatal(err)
			}
		}
		check("claim", plain, withMilestone)

		// A daemon tick: idle, then with a queued job on each of the 10 waiting tasks (each tick rechecks
		// all of them). Two ticks before each run settle the job-done lines of the cancelled jobs.
		for _, j := range queued {
			m.cancelQueued(j, "latency test")
		}
		ticks := func() []time.Duration {
			var out []time.Duration
			for i := 0; i < n+2; i++ {
				if d := timed(t, m.Tick); i >= 2 {
					out = append(out, d)
				}
			}
			return out
		}
		plain = ticks()
		for i := 11; i <= 20; i++ {
			if err := m.Store.Insert(&store.Job{ID: fmt.Sprintf("wait-%d", i), Agent: "test", Title: "t", Kind: "general", Brief: "b",
				Status: store.StatusQueued, Model: "sonnet", MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1, CreatedAt: time.Now(),
				MilestoneID: v.ID, TaskKey: fmt.Sprintf("T%d", i)}); err != nil {
				t.Fatal(err)
			}
		}
		withMilestone = ticks()
		for i := 11; i <= 20; i++ {
			if j := get(t, m, fmt.Sprintf("wait-%d", i)); j.Status != store.StatusQueued {
				t.Fatalf("job of a waiting task: %s %q, want queued", j.Status, j.Error)
			}
		}
		check("tick", plain, withMilestone)
	})
}
