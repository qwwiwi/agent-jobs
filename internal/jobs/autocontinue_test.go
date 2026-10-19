package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// noAutoContinue turns the automatic continuation off for a test that checks a stopped attempt as is.
func noAutoContinue(m *Manager) {
	off := false
	m.Cfg.AutoContinue = &off
}

// waitExit waits until the current attempt's wrapper wrote exit_code.
func waitExit(t *testing.T, m *Manager, id string) {
	t.Helper()
	waitFor(t, "exit_code", 15*time.Second, func() bool {
		_, ok := readExitCode(m.JobDir(id))
		return ok
	})
}

func tick(t *testing.T, m *Manager) {
	t.Helper()
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
}

// An attempt that ends error_max_turns is continued in the same tick, in the same session, with half
// of the original limits and the tail of PROGRESS.md in its update; the stopped attempt keeps a stop
// report and its cost; the job-done line names the continuation.
func TestAutoContinueAfterTurnLimit(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, true)
	j := enqueue(t, m, "FAKE_MAX_TURNS_ONCE", nil)
	tick(t, m)
	sid := get(t, m, j.ID).SessionID
	waitExit(t, m, j.ID)
	tick(t, m) // collects attempt 1, queues and launches attempt 2
	got := get(t, m, j.ID)
	if got.Status != store.StatusRunning || got.Attempt != 2 || got.RunMode != store.RunResume {
		t.Fatalf("after the turn-limit stop: %s attempt %d mode %s %q", got.Status, got.Attempt, got.RunMode, got.Error)
	}
	// The continuation resumes attempt 1's session: claude gets --resume with its id.
	waitExit(t, m, j.ID)
	launches := strings.Split(readFile(t, filepath.Join(m.JobDir(j.ID), "argv-all.txt")), "---\n")
	if sid == "" || got.SessionID != sid || len(launches) < 2 || !strings.Contains(launches[0], "--session-id\n"+sid+"\n") ||
		!strings.Contains(launches[1], "--resume\n"+sid+"\n") {
		t.Fatalf("attempt 2 did not resume session %q (row %q):\n%v", sid, got.SessionID, launches)
	}
	if got.MaxTurns != 40 || got.MaxBudgetUSD != 5 || got.TimeoutMinutes != 23 {
		t.Fatalf("continuation limits: %d turns $%g %d min", got.MaxTurns, got.MaxBudgetUSD, got.TimeoutMinutes)
	}
	for _, s := range []string{autoContinueTag + "attempt 1 stopped at its turn limit after 7 turns (cost $0.40)",
		"- done: step one of FAKE_MAX_TURNS_ONCE", "- next: step two", "Continue from here; do not redo what is listed as done."} {
		if !strings.Contains(got.PendingContext, s) {
			t.Fatalf("update lacks %q:\n%s", s, got.PendingContext)
		}
	}
	dir := m.JobDir(j.ID)
	if r := readFile(t, filepath.Join(dir, AttemptReportName(1))); !strings.Contains(r, "Attempt 1 stopped at its turn limit after 7 turns") ||
		!strings.Contains(r, "- next: step two") || reportContractNote(r) != "" {
		t.Fatalf("stop report of attempt 1:\n%s", r)
	}
	tick(t, m)
	got = get(t, m, j.ID)
	if got.Status != store.StatusSucceeded || got.Attempt != 2 || got.Turns != 3 {
		t.Fatalf("continuation: %s attempt %d turns %d %q", got.Status, got.Attempt, got.Turns, got.Error)
	}
	if !strings.Contains(readFile(t, filepath.Join(dir, "stdin.txt")), "- next: step two") {
		t.Fatal("the resumed session did not get the progress tail")
	}
	atts, err := m.Store.Attempts(j.ID)
	if err != nil || len(atts) != 1 || atts[0].Status != store.StatusFailed || atts[0].Turns != 7 || atts[0].CostUSD != 0.4 {
		t.Fatalf("archived attempt 1: %+v %v", atts, err)
	}
	st, err := m.Store.GetStats(j.ID)
	if err != nil || st.Turns != 10 || st.CostUSD < 0.649 || st.CostUSD > 0.651 {
		t.Fatalf("stats do not count attempt 1: %+v %v", st, err)
	}
	if line := DoneLine(got); !strings.Contains(line, "auto-continued (attempt 2; attempt 1 stopped at its turn limit after 7 turns (cost $0.40))") {
		t.Fatalf("done line: %s", line)
	}
}

// The continuation that stops again is final: it is not continued a second time, says why, and keeps
// its own stop report.
func TestAutoContinueOnlyOnce(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, true)
	j := enqueue(t, m, "FAKE_MAX_TURNS_ALWAYS", nil)
	tick(t, m)
	waitExit(t, m, j.ID)
	tick(t, m)
	if got := get(t, m, j.ID); got.Attempt != 2 {
		t.Fatalf("not continued: %s attempt %d", got.Status, got.Attempt)
	}
	waitExit(t, m, j.ID)
	tick(t, m)
	tick(t, m)
	got := get(t, m, j.ID)
	if got.Status != store.StatusFailed || got.Attempt != 2 || !strings.Contains(got.Error, "not auto-continued: it was auto-continued once already") {
		t.Fatalf("second stop: %s attempt %d %q", got.Status, got.Attempt, got.Error)
	}
	if r := readFile(t, filepath.Join(m.JobDir(j.ID), "report.md")); !strings.Contains(r, "Attempt 2 stopped at its turn limit") {
		t.Fatalf("stop report of attempt 2:\n%s", r)
	}
	if line := DoneLine(got); !strings.Contains(line, "auto-continued (attempt 2;") {
		t.Fatalf("done line: %s", line)
	}
}

// auto_continue false: the turn-limit stop stays failed (with its stop report); a stopped job is
// never continued.
func TestAutoContinueOffAndStopped(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, true)
	noAutoContinue(m)
	j := runToEnd(t, m, enqueue(t, m, "FAKE_MAX_TURNS_ONCE", nil).ID)
	tick(t, m)
	if j = get(t, m, j.ID); j.Status != store.StatusFailed || j.Attempt != 1 || strings.Contains(j.Error, "auto-continue") {
		t.Fatalf("auto_continue off: %s attempt %d %q", j.Status, j.Attempt, j.Error)
	}
	if r := readFile(t, filepath.Join(m.JobDir(j.ID), "report.md")); !strings.Contains(r, "stopped at its turn limit") {
		t.Fatalf("no stop report:\n%s", r)
	}
	if strings.Contains(DoneLine(j), "auto-continued") {
		t.Fatalf("done line: %s", DoneLine(j))
	}
	if st, err := m.Store.GetStats(j.ID); err != nil || st.HasReport {
		t.Fatalf("the stop report counts as the job's report in the stats: %+v %v", st, err)
	}

	m2 := newManager(t)
	s := startAndStop(t, m2, nil)
	tick(t, m2)
	if s = get(t, m2, s.ID); s.Status != store.StatusStopped || s.Attempt != 1 {
		t.Fatalf("stopped job: %s attempt %d", s.Status, s.Attempt)
	}
}

// A timed-out attempt is continued once too; its unknown cost counts at its whole budget.
func TestAutoContinueAfterTimeout(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, true)
	j := enqueue(t, m, "FAKE_WIP FAKE_SLEEP_ONCE=30", func(r *StartRequest) { r.TimeoutMinutes = 10 })
	tick(t, m)
	waitFor(t, "work in progress", 15*time.Second, func() bool {
		b, err := os.ReadFile(filepath.Join(m.JobDir(j.ID), "stream.jsonl"))
		return err == nil && strings.Contains(string(b), "WIP_PROGRESS_NOTE")
	})
	later := time.Now().Add(11 * time.Minute)
	m.Now = func() time.Time { return later }
	tick(t, m)
	got := get(t, m, j.ID)
	if got.Attempt != 2 || got.MaxTurns != 40 || got.MaxBudgetUSD != 5 || got.TimeoutMinutes != 5 {
		t.Fatalf("after the timeout: %s attempt %d limits %d/$%g/%d %q", got.Status, got.Attempt, got.MaxTurns,
			got.MaxBudgetUSD, got.TimeoutMinutes, got.Error)
	}
	for _, s := range []string{"attempt 1 timed out", "counted at its $10 budget", "WIP_PROGRESS_NOTE"} {
		if !strings.Contains(got.PendingContext, s) {
			t.Fatalf("update lacks %q:\n%s", s, got.PendingContext)
		}
	}
	waitExit(t, m, j.ID)
	tick(t, m)
	got = get(t, m, j.ID)
	atts, err := m.Store.Attempts(j.ID)
	if got.Status != store.StatusSucceeded || err != nil || len(atts) != 1 || atts[0].Status != store.StatusTimeout || !atts[0].CostUnknown {
		t.Fatalf("continuation: %s %q attempts %+v %v", got.Status, got.Error, atts, err)
	}
	// Attempt 1's unknown cost counts at its whole $10 budget, plus the continuation's own $0.25.
	if _, cost, _ := attemptsUsage(got, atts); cost < 10.249 || cost > 10.251 {
		t.Fatalf("accounted cost $%g, want $10.25", cost)
	}
	if line := DoneLine(got); !strings.Contains(line, "auto-continued (attempt 2; attempt 1 timed out") {
		t.Fatalf("done line: %s", line)
	}
}

// A FIFO (or link) the job left as report.md is replaced by the stop report, never opened; a report
// the job wrote is kept.
func TestStopReportReplacesFIFO(t *testing.T) {
	m := newManager(t)
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "report.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ProgressFile), []byte("- done: a\n- next: b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		m.writeStopReport(&store.Job{ID: "x", Attempt: 1}, dir, stopTimeout, 3)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("writeStopReport blocked on a FIFO report.md")
	}
	r := readFile(t, filepath.Join(dir, "report.md"))
	if !strings.Contains(r, "Attempt 1 timed out after 3 turns") || !strings.Contains(r, "- next: b") || !isStopReport(filepath.Join(dir, "report.md")) {
		t.Fatalf("stop report:\n%s", r)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("## Summary\nown\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.writeStopReport(&store.Job{ID: "x", Attempt: 1}, dir, stopTimeout, 3)
	if r := readFile(t, filepath.Join(dir, "report.md")); r != "## Summary\nown\n" {
		t.Fatalf("the job's own report was replaced: %q", r)
	}
}

// The allowance is a share of the first attempt's limits, never more than the ceilings leave.
func TestAutoContinueLimits(t *testing.T) {
	j := &store.Job{MaxTurns: 80, MaxBudgetUSD: 10, TimeoutMinutes: 45, Turns: 80, CostUSD: 3, DurationSec: 600}
	l, err := autoContinueLimits(j, nil, 0.5)
	if err != nil || l != (Limits{MaxTurns: 40, MaxBudgetUSD: 5, TimeoutMinutes: 23}) {
		t.Fatalf("half: %+v %v", l, err)
	}
	if l, _ := autoContinueLimits(&store.Job{MaxTurns: 1, MaxBudgetUSD: 0.01, TimeoutMinutes: 1}, nil, 0.1); l.MaxTurns != 1 ||
		l.MaxBudgetUSD != 0.01 || l.TimeoutMinutes != 1 {
		t.Fatalf("floor: %+v", l)
	}
	// A resumed job: the first attempt's limits count; the ceilings cap what is left.
	earlier := []store.Attempt{{MaxTurns: 200, MaxBudgetUSD: 50, TimeoutMinutes: 180, Turns: 150, CostUSD: 30, DurationSec: 100 * 60}}
	j2 := &store.Job{MaxTurns: 40, MaxBudgetUSD: 10, TimeoutMinutes: 60, Turns: 40, CostUSD: 10, DurationSec: 60 * 60}
	if l, err := autoContinueLimits(j2, earlier, 1); err != nil || l != (Limits{MaxTurns: 10, MaxBudgetUSD: 10, TimeoutMinutes: 20}) {
		t.Fatalf("capped: %+v %v", l, err)
	}
	j2.Turns = 50
	if _, err := autoContinueLimits(j2, earlier, 1); err == nil || !strings.Contains(err.Error(), "no turns (200 of 200 used)") {
		t.Fatalf("no turns left: %v", err)
	}
}

// Report contract (issue #46): a final message without the Summary section keeps the job succeeded
// with a note in its row, on the job-done line and in the stats; report_contract fail fails it; a
// report that follows the contract gets no note.
func TestReportContractNote(t *testing.T) {
	m := newManager(t)
	j := runToEnd(t, m, enqueue(t, m, "FAKE_MIDTASK", nil).ID)
	if j.Status != store.StatusSucceeded || j.Error != NoteNoSummary {
		t.Fatalf("mid-task report: %s %q", j.Status, j.Error)
	}
	if line := DoneLine(j); !strings.HasSuffix(line, " · "+NoteNoSummary) {
		t.Fatalf("done line: %s", line)
	}
	ok := runToEnd(t, m, enqueue(t, m, "do the thing", nil).ID)
	if ok.Status != store.StatusSucceeded || ok.Error != "" || strings.Contains(DoneLine(ok), "report has no") {
		t.Fatalf("contract report: %s %q %s", ok.Status, ok.Error, DoneLine(ok))
	}
	rep, err := m.Stats(time.Time{}, "kind")
	if err != nil || rep.Totals.ReportOffContract != 1 || len(rep.Groups) != 1 || rep.Groups[0].ReportOffContract != 1 {
		t.Fatalf("stats: %+v %v", rep.Totals, err)
	}
	m.Cfg.ReportContract = ReportContractFail
	if f := runToEnd(t, m, enqueue(t, m, "FAKE_MIDTASK", nil).ID); f.Status != store.StatusFailed || f.Error != NoteNoSummary {
		t.Fatalf("report_contract fail: %s %q", f.Status, f.Error)
	}
}

func TestReportContractNoteSections(t *testing.T) {
	for report, want := range map[string]string{
		"Now let me check the remaining callers in store.go.":      NoteNoSummary,
		"## Summary\nok\n\n## Findings / Changes\n-":               NoteNoNotDone,
		"```\n## Summary\n```\n## Not done / uncertain\nnothing":   NoteNoSummary,
		"## Summary\nok\n\n## Not done / uncertain\nnothing\n":     "",
		"intro\n\n## summary\nok\n\n## not done / uncertain\nnone": "",
	} {
		if got := reportContractNote(report); got != want {
			t.Errorf("%q: %q, want %q", report, got, want)
		}
	}
}

func TestConfigReportContractAndAutoContinue(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{}`)
	if c, err := LoadConfig(dir); err != nil || !c.AutoContinueOn() || c.AutoContinueShare() != 0.5 || c.ReportContract != "" {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	write(`{"auto_continue": false, "auto_continue_fraction": 0.25, "report_contract": "fail"}`)
	if c, err := LoadConfig(dir); err != nil || c.AutoContinueOn() || c.AutoContinueShare() != 0.25 || c.ReportContract != ReportContractFail {
		t.Fatalf("set: %+v %v", c, err)
	}
	for _, bad := range []string{`{"auto_continue": null}`, `{"auto_continue_fraction": 1.5}`, `{"auto_continue_fraction": -0.1}`,
		`{"report_contract": "strict"}`} {
		write(bad)
		if _, err := LoadConfig(dir); err == nil {
			t.Fatalf("%s accepted", bad)
		}
	}
}

func TestPreambleAsksForProgressFile(t *testing.T) {
	m := newManager(t)
	j := runToEnd(t, m, enqueue(t, m, "do the thing", nil).ID)
	if p := readFile(t, filepath.Join(m.JobDir(j.ID), "prompt.md")); !strings.Contains(p, "Keep `PROGRESS.md` in the job directory up to date") {
		t.Fatal("prompt.md lacks the progress-file rule")
	}
}

// A FIFO the stopped attempt left at settings.json or mcp.json is replaced when the continuation
// rewrites them, never opened: the tick does not block.
func TestAutoContinueReplacesFIFOJobFiles(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, true)
	j := enqueue(t, m, "FAKE_MAX_TURNS_ONCE", nil)
	tick(t, m)
	waitExit(t, m, j.ID)
	dir := m.JobDir(j.ID)
	for _, name := range []string{"settings.json", "mcp.json"} {
		p := filepath.Join(dir, name)
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(p, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- m.Tick() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		// Release the blocked writer so the daemon goroutine ends.
		for _, name := range []string{"settings.json", "mcp.json"} {
			if f, err := os.OpenFile(filepath.Join(dir, name), os.O_RDONLY|syscall.O_NONBLOCK, 0); err == nil {
				defer f.Close()
			}
		}
		<-done
		t.Fatal("auto-continue blocked on a FIFO job file")
	}
	got := get(t, m, j.ID)
	if got.Attempt != 2 || got.RunMode != store.RunResume {
		t.Fatalf("not continued: %s attempt %d %q", got.Status, got.Attempt, got.Error)
	}
	for _, name := range []string{"settings.json", "mcp.json"} {
		if fi, err := os.Lstat(filepath.Join(dir, name)); err != nil || !fi.Mode().IsRegular() {
			t.Fatalf("%s not replaced by a regular file: %v %v", name, fi, err)
		}
	}
	waitExit(t, m, j.ID)
	tick(t, m)
}

// A tail cut inside one long line drops that partial line: a secret whose prefix was cut off is never
// quoted.
func TestProgressTailDropsCutLine(t *testing.T) {
	dir := t.TempDir()
	long := "api_key=" + strings.Repeat("S", progressTailBytes+100)
	if err := os.WriteFile(filepath.Join(dir, ProgressFile), []byte(long+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := progressTail(dir); got != "" {
		t.Fatalf("cut single line quoted (%d bytes)", len(got))
	}
	if err := os.WriteFile(filepath.Join(dir, ProgressFile), []byte(long+"\n- next: b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := progressTail(dir); got != "- next: b" {
		t.Fatalf("tail: %.80q", got)
	}
}

// exit_code written after collect's first check but before the timeout's kill: the attempt ended by
// itself, so its own verdict is recorded (here error_max_budget_usd), not a timeout, and it is not
// auto-continued.
func TestTimeoutLateExitCodeCollectsVerdict(t *testing.T) {
	m := newManager(t)
	m.KillGrace = 200 * time.Millisecond
	start := time.Now().Add(-20 * time.Minute)
	w := liveChild(t)
	j := &store.Job{ID: "261019-100000-late", Agent: m.Agent, Title: "t", Kind: "fix", Brief: "b", Status: store.StatusRunning,
		Model: "sonnet", MaxTurns: 10, MaxBudgetUSD: 1, TimeoutMinutes: 10, CreatedAt: start, StartedAt: &start,
		LaunchToken: "tok", PID: w.Process.Pid, Attempt: 1}
	if err := m.Store.Insert(j); err != nil {
		t.Fatal(err)
	}
	dir := m.JobDir(j.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	beforeTimeoutClaim = func(id string) {
		if id != j.ID {
			return
		}
		res := `{"type":"result","subtype":"error_max_budget_usd","is_error":true,"num_turns":4,"total_cost_usd":1.02,"result":""}` + "\n"
		if err := os.WriteFile(filepath.Join(dir, "stream.jsonl"), []byte(res), 0o600); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "exit_code"), []byte("1\n"), 0o600); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { beforeTimeoutClaim = func(string) {} })
	if err := m.collect(); err != nil {
		t.Fatal(err)
	}
	got := get(t, m, j.ID)
	if got.Status != store.StatusFailed || got.Attempt != 1 || strings.Contains(got.Error, "timeout") ||
		got.CostUnknown || got.Turns != 4 || got.PendingContext != "" {
		t.Fatalf("late exit_code: %s attempt %d turns %d unknown %v %q", got.Status, got.Attempt, got.Turns, got.CostUnknown, got.Error)
	}
	if _, err := os.Lstat(filepath.Join(dir, "report.md")); err == nil {
		t.Fatal("a stop report was written for an attempt that ended by itself")
	}
}

// A compliant report longer than the read limit is not judged on its first part: the check is
// inconclusive and adds no note.
func TestReportContractLongReport(t *testing.T) {
	m := newManager(t)
	dir := t.TempDir()
	long := "## Summary\nok\n\n## Evidence\n" + strings.Repeat("x\n", maxContractReportBytes) + "\n## Not done / uncertain\nnothing\n"
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte(long), 0o600); err != nil {
		t.Fatal(err)
	}
	if st, msg := m.reportContract(&store.Job{ID: "x"}, store.StatusSucceeded, "", dir); st != store.StatusSucceeded || msg != "" {
		t.Fatalf("long report: %s %q", st, msg)
	}
}
