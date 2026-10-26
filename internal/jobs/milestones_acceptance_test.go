package jobs

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
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

// headingLevel is the number of #s of a markdown heading line, 0 for any other line.
func headingLevel(line string) int {
	n := len(line) - len(strings.TrimLeft(line, "#"))
	if n == 0 || !strings.HasPrefix(line[n:], " ") {
		return 0
	}
	return n
}

// docSection is the text of doc from the line heading up to the next heading of the same or a higher
// level outside a code fence ("" when doc has no such heading).
func docSection(doc, heading string) string {
	_, rest, ok := strings.Cut(doc, "\n"+heading+"\n")
	if !ok {
		return ""
	}
	lines := strings.Split(rest, "\n")
	fenced := false
	for i, line := range lines {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
		}
		if l := headingLevel(line); !fenced && l > 0 && l <= headingLevel(heading) {
			return strings.Join(lines[:i], "\n")
		}
	}
	return rest
}

// The docs keep the two terms apart: the checklist keeps its name as the acceptance list, a milestone is
// a stage of work.
func TestMilestoneTermsDocumented(t *testing.T) {
	t.Run("[R06] GUIDE and README define checklist as the acceptance list and milestone as a stage of work", func(t *testing.T) {
		for name, want := range map[string][]string{
			"docs/GUIDE.md": {
				"- **checklist**: the acceptance list of a repository (`brief/CHECKLIST.md`, Wave F); it keeps its name and does not change.",
				"- **milestone**: a stage of work. A large project is a sequence of milestones",
			},
			"README.md": {
				"A large project is split into **milestones** (stages of work, ordered per repository)",
				"The **checklist** (`brief/CHECKLIST.md`) stays the acceptance list",
			},
		} {
			doc := readDoc(t, name)
			for _, s := range want {
				if !strings.Contains(doc, s) {
					t.Errorf("%s lacks %q", name, s)
				}
			}
		}
	})
}

// Every bureaucracy cut of the R08 audit is a row of DECISIONS.md with its measured reason, and the
// guard cut keeps the money and auth refusals.
func TestMilestoneCutsDecided(t *testing.T) {
	t.Run("[R08] DECISIONS lists each cut with its measured reason", func(t *testing.T) {
		doc := readDoc(t, "docs/DECISIONS.md")
		row := func(title string) string {
			for _, line := range strings.Split(doc, "\n") {
				if strings.HasPrefix(line, "| ") && strings.Contains(line, "**"+title) {
					return line
				}
			}
			return ""
		}
		for _, title := range []string{
			"Cut guard noise that costs job turns.",
			"Release gate deadlock cut",
			"The `trace` condition of `release_check` is cut.",
			"Owner waivers bind to (checklist hash, requirement)",
			"Merge gate: a command the reader cannot read no longer refuses.",
			"The Requirements block and REDPROOF are informational.",
			"Job size is optional.",
			"The brief reminder speaks only when a checklist decision is pending.",
			"The slicing check before approval is advisory.",
		} {
			r := row(title)
			if r == "" {
				t.Errorf("DECISIONS.md has no row %q", title)
				continue
			}
			if !strings.Contains(r, "Measured reason:") {
				t.Errorf("row %q has no measured reason", title)
			}
		}
		if r := row("Cut guard noise that costs job turns."); !strings.Contains(r, "(checklist R08)") ||
			!strings.Contains(r, "the money, auth, secrets") || !strings.Contains(r, "refusals are unchanged") {
			t.Errorf("the guard cut does not name R08 or keep the money and auth refusals: %q", r)
		}
		n := 0
		for _, line := range strings.Split(doc, "\n") {
			if strings.Contains(line, "cut bureaucracy that costs time without catching defects") || strings.Contains(line, "(checklist R08)") {
				n++
				if !strings.Contains(line, "Measured reason:") {
					t.Errorf("a cut without a measured reason: %.120s", line)
				}
			}
		}
		if n < 9 {
			t.Errorf("%d cut rows, want at least 9", n)
		}
	})
}

// README, GUIDE and the decisions describe milestones in English (the Russian copies are separate files),
// PLAN has the milestones phase 2, and the decision keeps «no noticeable latency» as a constraint.
func TestMilestoneDocsInEnglish(t *testing.T) {
	t.Run("[R09i] README, GUIDE, PLAN and DECISIONS describe milestones in English", func(t *testing.T) {
		cyrillic := regexp.MustCompile(`\p{Cyrillic}`)
		decisions := readDoc(t, "docs/DECISIONS.md")
		var approved string
		for _, line := range strings.Split(decisions, "\n") {
			if strings.Contains(line, "**Milestones build approved by the owner.**") {
				approved = line
			}
		}
		if !strings.Contains(approved, "no noticeable latency on job_start or the daemon tick") {
			t.Errorf("DECISIONS.md: the milestones decision lacks the latency constraint: %q", approved)
		}
		for _, c := range []struct{ name, text string }{
			{"README.md ## Milestones", docSection(readDoc(t, "README.md"), "## Milestones: stages, tasks, jobs")},
			{"GUIDE.md ### Milestones", docSection(readDoc(t, "docs/GUIDE.md"), "### Milestones")},
			{"PLAN.md ## Milestones phase 2", docSection(readDoc(t, "docs/PLAN.md"), "## Milestones phase 2 (approved, not built)")},
			{"DECISIONS.md milestones row", approved},
		} {
			if !strings.Contains(c.text, "milestone") {
				t.Errorf("%s: no milestones section", c.name)
			}
			if w := cyrillic.FindString(c.text); w != "" {
				t.Errorf("%s: not English (%q)", c.name, w)
			}
		}
	})
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
