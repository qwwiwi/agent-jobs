package mcp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// insertReportJob stores a finished job with the given report.md and returns its id.
func insertReportJob(t *testing.T, m *jobs.Manager, n int, report string, edit func(*store.Job)) string {
	t.Helper()
	now := time.Now()
	j := &store.Job{ID: fmt.Sprintf("261106-15%04d-abcd", n), Agent: "test", Title: "t", Kind: "general", Brief: "b",
		Status: store.StatusSucceeded, Model: "opus", MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1,
		CreatedAt: now, StartedAt: &now, FinishedAt: &now}
	if edit != nil {
		edit(j)
	}
	if err := m.Store.Insert(j); err != nil {
		t.Fatal(err)
	}
	dir := m.JobDir(j.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}
	return j.ID
}

const bigReport = "## Summary\nfixed it\n\n## Findings / Changes\nchanged a.go\n\n## Evidence\n%s\n" +
	"## Not done / uncertain\nBLOCKED: x · why\n\n## Forks\n- a / b / c\n\n## Progress\n- 10:00 UTC a\n- 10:05 UTC b\n"

// The default job_result is compact: no whole report, no repeated Progress; Not done, Summary and Forks
// whole, every section sized, and a hint how to fetch the rest.
func TestJobResultDefaultIsCompact(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))
	id := insertReportJob(t, m, 1, fmt.Sprintf(bigReport, strings.Repeat("evidence line\n", 4000)), nil)
	var res JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": id}, &res)
	if res.Report != "" || res.Progress != "" || res.NotDone != "BLOCKED: x · why" || res.Summary != "fixed it" ||
		res.Forks != "- a / b / c" || res.DoneLine == "" {
		t.Fatalf("default is not compact: report %d bytes, progress %q, not_done %q, summary %q, forks %q",
			len(res.Report), res.Progress, res.NotDone, res.Summary, res.Forks)
	}
	var names []string
	var evidence int
	for _, s := range res.ReportSections {
		names = append(names, s.Name)
		if s.Name == "Evidence" {
			evidence = s.Bytes
		}
	}
	if strings.Join(names, "|") != "Summary|Findings / Changes|Evidence|Not done / uncertain|Forks|Progress" || evidence < 4000*14 ||
		res.ReportBytes < evidence || !strings.Contains(res.Note, "detail=full") || !strings.Contains(res.Note, "sections=") {
		t.Fatalf("sections %v, evidence %d, total %d, note %q", names, evidence, res.ReportBytes, res.Note)
	}
	// An explicit detail=summary is the default.
	var again JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": id, "detail": "summary"}, &again)
	if again.Report != "" || again.NotDone != res.NotDone {
		t.Fatalf("detail=summary differs from the default: %+v", again)
	}
	// The compact payload is a small fraction of the full one.
	var full JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": id, "detail": "full"}, &full)
	if len(full.Report) < 50000 || full.Progress != "- 10:00 UTC a\n- 10:05 UTC b" || full.NotDone != res.NotDone || full.Summary != "" ||
		full.ReportSections != nil || full.Commits != nil {
		t.Fatalf("full view changed: report %d bytes, progress %q, summary %q", len(full.Report), full.Progress, full.Summary)
	}
}

// detail=full keeps the old output: the whole report as written (redacted) and the Not done, Progress
// and Forks fields.
func TestJobResultFullUnchanged(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))
	report := "## Summary\nok\n\n## Not done / uncertain\nnothing\n\n## Forks\nnone\n\n## Progress\n- 10:00 UTC x\n"
	id := insertReportJob(t, m, 2, report, nil)
	var res JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": id, "detail": "full"}, &res)
	if res.Report != report || res.NotDone != "nothing" || res.Progress != "- 10:00 UTC x" || res.Forks != "none" ||
		res.ReportTruncated || res.Note != "" {
		t.Fatalf("full: %+v", res)
	}
	// The report file over the read cap: the same truncation as before.
	huge := "## Summary\n" + strings.Repeat("yyyyyyyyyyyyyyy\n", MaxReportBytes/16+10)
	id = insertReportJob(t, m, 3, huge, nil)
	var cut JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": id, "detail": "full"}, &cut)
	if !cut.ReportTruncated || len(cut.Report) > MaxReportBytes || len(cut.Report) < MaxReportBytes-64 || !strings.Contains(cut.Note, "report cut at") {
		t.Fatalf("full over the cap: truncated %v, %d bytes, note %q", cut.ReportTruncated, len(cut.Report), cut.Note)
	}
}

// A bad detail or a negative max_bytes is an error.
func TestJobResultBadArguments(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))
	id := insertReportJob(t, m, 4, "## Summary\nok\n", nil)
	if msg := callErr(t, cs, "job_result", map[string]any{"id": id, "detail": "everything"}); !strings.Contains(msg, "detail must be") {
		t.Fatalf("bad detail: %q", msg)
	}
	if msg := callErr(t, cs, "job_result", map[string]any{"id": id, "max_bytes": -1}); !strings.Contains(msg, "max_bytes") {
		t.Fatalf("negative max_bytes: %q", msg)
	}
}

// A report without Forks, Progress, Summary or Not done is not an error: the missing fields stay empty.
func TestJobResultCompactMissingSections(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))
	id := insertReportJob(t, m, 5, "## Summary\nonly this\n", nil)
	var res JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": id}, &res)
	if res.ReportMissing || res.Summary != "only this" || res.NotDone != "" || res.Forks != "" || len(res.ReportSections) != 1 {
		t.Fatalf("partial report: %+v", res)
	}
	// No report file at all.
	now := time.Now()
	j := &store.Job{ID: "261106-150006-abcd", Agent: "test", Title: "t", Kind: "general", Brief: "b", Status: store.StatusFailed,
		Model: "opus", MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1, CreatedAt: now, StartedAt: &now, FinishedAt: &now}
	if err := m.Store.Insert(j); err != nil {
		t.Fatal(err)
	}
	var none JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": j.ID}, &none)
	if !none.ReportMissing || none.Report != "" || !strings.Contains(none.Note, "no report.md") {
		t.Fatalf("missing report: %+v", none)
	}
}

// sections=[...] returns just those sections as written; unknown names are reported in the note.
func TestJobResultSections(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))
	id := insertReportJob(t, m, 7, fmt.Sprintf(bigReport, "the evidence"), nil)
	var res JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": id, "sections": []string{"evidence", "Progress", "Nope"}}, &res)
	want := "## Evidence\nthe evidence\n\n## Progress\n- 10:00 UTC a\n- 10:05 UTC b\n"
	if res.Report != want || res.NotDone != "BLOCKED: x · why" || !strings.Contains(res.Note, "no such section: Nope") {
		t.Fatalf("sections: report %q, note %q", res.Report, res.Note)
	}
	// With detail=full the report is the requested sections too, the full fields stay.
	var full JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": id, "detail": "full", "sections": []string{"Summary"}}, &full)
	if full.Report != "## Summary\nfixed it\n" || full.Progress == "" {
		t.Fatalf("full with sections: %+v", full)
	}
}

// Secret-looking values are redacted in the summary view, in sections and in the full view.
func TestJobResultCompactRedacts(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))
	tok := "ghp_" + strings.Repeat("Q", 36)
	report := "## Summary\nused " + tok + "\n\n## Evidence\nlog " + tok + "\n\n## Not done / uncertain\nneeds " + tok + "\n\n## Forks\nnone " + tok + "\n"
	id := insertReportJob(t, m, 8, report, func(j *store.Job) { j.Error = "auth Bearer " + strings.Repeat("z", 24) })
	for _, args := range []map[string]any{
		{"id": id},
		{"id": id, "sections": []string{"Evidence"}},
		{"id": id, "detail": "full"},
	} {
		var res JobResultOutput
		call(t, cs, "job_result", args, &res)
		all := fmt.Sprintf("%+v", res)
		if strings.Contains(all, tok) || strings.Contains(all, strings.Repeat("z", 24)) || !strings.Contains(all, "[redacted:github]") {
			t.Fatalf("args %v: not redacted: %s", args, all)
		}
	}
}

// Truncation: Not done / uncertain is capped with a marker, never dropped; max_bytes caps each text.
func TestJobResultCompactTruncationMarkers(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))
	blocker := "BLOCKED: first blocker\n" + strings.Repeat("detail line\n", 2000)
	report := "## Summary\n" + strings.Repeat("s", 20000) + "\n\n## Not done / uncertain\n" + blocker + "\n## Progress\n- 10:00 UTC a\n"
	id := insertReportJob(t, m, 9, report, nil)
	var res JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": id}, &res)
	if !strings.HasPrefix(res.NotDone, "BLOCKED: first blocker\n") || !strings.Contains(res.NotDone, "[cut: ") ||
		len(res.NotDone) > SummaryFieldBytes+100 || !strings.Contains(res.Summary, "[cut: ") || len(res.Summary) > SummaryFieldBytes+100 ||
		!strings.Contains(res.Note, "cut at the cap: Not done / uncertain, Summary") {
		t.Fatalf("default caps: not_done %d bytes, summary %d bytes, note %q", len(res.NotDone), len(res.Summary), res.Note)
	}
	// max_bytes lowers the cap; a value over the ceiling is clamped.
	var small JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": id, "max_bytes": 100}, &small)
	if len(small.NotDone) > 100+60 || !strings.Contains(small.NotDone, "[cut: 100 of ") {
		t.Fatalf("max_bytes=100: %q", small.NotDone)
	}
	var big JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": id, "max_bytes": 1 << 30}, &big)
	if strings.Contains(big.NotDone, "[cut: ") || !strings.Contains(big.Summary, "ssss") || strings.Contains(big.Summary, "[cut: ") {
		t.Fatalf("max_bytes over the ceiling: not_done %d, summary %d", len(big.NotDone), len(big.Summary))
	}
	// sections and full views get markers too.
	var sec JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": id, "sections": []string{"Summary"}, "max_bytes": 50}, &sec)
	if !strings.Contains(sec.Report, "[cut: 50 of ") {
		t.Fatalf("sections cap: %q", sec.Report)
	}
	var full JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": id, "detail": "full", "max_bytes": 1000}, &full)
	if !strings.Contains(full.Report, "[cut: 1000 of ") || !full.ReportTruncated {
		t.Fatalf("full cap: %d bytes truncated %v", len(full.Report), full.ReportTruncated)
	}
	// The cut never splits a multi-byte rune.
	utf := "## Not done / uncertain\n" + strings.Repeat("ж", 100) + "\n"
	id = insertReportJob(t, m, 10, utf, nil)
	var ru JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": id, "max_bytes": 51}, &ru)
	if !strings.Contains(ru.NotDone, strings.Repeat("ж", 25)+"\n[cut: 50 of ") {
		t.Fatalf("rune boundary: %q", ru.NotDone)
	}
}

// The acceptance log tail comes in the summary view only when the acceptance did not pass; the full
// view keeps it always.
func TestJobResultAcceptanceTailOnFailureOnly(t *testing.T) {
	m := newManager(t)
	m.Cfg.AcceptanceAllowed = true
	cs := connect(t, mustServer(t, m))
	for i, c := range []struct {
		exit     int
		wantTail bool
	}{{0, false}, {1, true}} {
		exit := c.exit
		id := insertReportJob(t, m, 20+i, "## Summary\nok\n", func(j *store.Job) {
			now := time.Now()
			j.Kind, j.AcceptanceCmd, j.AcceptanceExit, j.AcceptanceStartedAt, j.AcceptanceDurationSec = "fix", "make test", &exit, &now, 3
		})
		if err := os.WriteFile(filepath.Join(m.JobDir(id), jobs.AcceptanceLog), []byte("the acceptance output\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var res, full JobResultOutput
		call(t, cs, "job_result", map[string]any{"id": id}, &res)
		call(t, cs, "job_result", map[string]any{"id": id, "detail": "full"}, &full)
		if res.Acceptance == nil || res.Acceptance.Exit != c.exit || (res.Acceptance.LogTail != "") != c.wantTail ||
			full.Acceptance == nil || full.Acceptance.LogTail != "the acceptance output\n" {
			t.Fatalf("exit %d: summary %+v, full %+v", c.exit, res.Acceptance, full.Acceptance)
		}
	}
}

// The compact view lists the job's own commits (base..head), newest first.
func TestJobResultCompactListsCommits(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))
	repo, base := gitRepo(t)
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=T", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("commit", "-q", "--allow-empty", "-m", "First change")
	git("commit", "-q", "--allow-empty", "-m", "Second change")
	head := git("rev-parse", "HEAD")
	git("reset", "-q", "--hard", base)
	id := insertReportJob(t, m, 30, "## Summary\nok\n", func(j *store.Job) { j.RepoPath, j.BaseCommit = repo, base })
	git("update-ref", "refs/agent-jobs/jobs/"+id, head)
	var res, full JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": id}, &res)
	if len(res.Commits) != 2 || !strings.HasSuffix(res.Commits[0], " Second change") || !strings.HasSuffix(res.Commits[1], " First change") {
		t.Fatalf("commits: %v (note %q)", res.Commits, res.Note)
	}
	call(t, cs, "job_result", map[string]any{"id": id, "detail": "full"}, &full)
	if full.Commits != nil {
		t.Fatalf("full view lists commits: %v", full.Commits)
	}
}

// lateBlockerReport is a valid report whose Evidence is bigger than the response cap, so "Not done /
// uncertain" and Forks start past it (the blocker heading at about offset 308000).
func lateBlockerReport() string {
	return "## Summary\nfixed it\n\n## Findings / Changes\nchanged a.go\n\n## Evidence\n" + strings.Repeat("evidence line\n", 22000) +
		"\n## Not done / uncertain\nBLOCKED: late blocker\n\n## Forks\n- late / fork / reason\n\n## Progress\n- 10:00 UTC a\n"
}

// F1: the sections that must not be missed are found wherever they sit in the report, not only in
// the first MaxReportBytes (default, sections and full views).
func TestJobResultRequiredSectionsBeyondResponseCap(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))
	report := lateBlockerReport()
	if at := strings.Index(report, "## Not done"); at < MaxReportBytes {
		t.Fatalf("test report is too small: blocker at %d", at)
	}
	id := insertReportJob(t, m, 20, report, nil)
	var res JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": id}, &res)
	if res.NotDone != "BLOCKED: late blocker" || res.Forks != "- late / fork / reason" || res.Summary != "fixed it" || res.ReportTruncated {
		t.Fatalf("summary: not_done %q, forks %q, summary %q, truncated %v", res.NotDone, res.Forks, res.Summary, res.ReportTruncated)
	}
	var sec JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": id, "sections": []string{"Not done / uncertain", "Forks"}}, &sec)
	if !strings.Contains(sec.Report, "BLOCKED: late blocker") || !strings.Contains(sec.Report, "late / fork") || strings.Contains(sec.Note, "no such section") {
		t.Fatalf("sections: %q note %q", sec.Report, sec.Note)
	}
	var full JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": id, "detail": "full"}, &full)
	if full.NotDone != "BLOCKED: late blocker" || full.Forks != "- late / fork / reason" || full.Progress != "- 10:00 UTC a" ||
		!full.ReportTruncated || !strings.Contains(full.Note, "report cut at") || len(full.Report) > MaxReportBytes {
		t.Fatalf("full: not_done %q, forks %q, progress %q, truncated %v", full.NotDone, full.Forks, full.Progress, full.ReportTruncated)
	}
}

// F1: a report past the scan cap is marked as an incomplete scan, in every view, and never read in full.
func TestJobResultIncompleteScanIsMarked(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))
	report := "## Summary\nfixed it\n\n## Evidence\n" + strings.Repeat("evidence line\n", jobs.MaxRedactBytes/14+10) +
		"\n## Not done / uncertain\nBLOCKED: past the scan cap\n"
	id := insertReportJob(t, m, 21, report, nil)
	for _, args := range []map[string]any{{"id": id}, {"id": id, "sections": []string{"Summary"}}, {"id": id, "detail": "full"}} {
		var res JobResultOutput
		call(t, cs, "job_result", args, &res)
		if !res.ReportTruncated || !strings.Contains(res.Note, "INCOMPLETE SCAN") || strings.Contains(res.NotDone, "past the scan cap") || res.Summary == "" && args["detail"] == nil && args["sections"] == nil {
			t.Fatalf("%v: truncated %v, not_done %q, note %q", args, res.ReportTruncated, res.NotDone, res.Note)
		}
	}
}

// F2: report_truncated follows the real cut (CapText appends a marker, so comparing lengths is wrong),
// in the full, sections and summary views, at the boundary.
func TestJobResultTruncatedFlagAtBoundary(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))
	// Sections of exactly the cap and one byte over it: Summary heading line + body.
	body := func(n int) string { return strings.Repeat("x", n) }
	for i, tc := range []struct {
		extra int
		cut   bool
	}{{0, false}, {1, true}, {11, true}} {
		// "## Summary\n" is 11 bytes; the section text is that plus the body and "\n".
		report := "## Summary\n" + body(1000-11-1+tc.extra) + "\n"
		id := insertReportJob(t, m, 30+i, report, nil)
		var sec JobResultOutput
		call(t, cs, "job_result", map[string]any{"id": id, "sections": []string{"Summary"}, "max_bytes": 1000}, &sec)
		if sec.ReportTruncated != tc.cut || strings.Contains(sec.Report, "[cut: ") != tc.cut {
			t.Fatalf("sections +%d: truncated %v, %d bytes", tc.extra, sec.ReportTruncated, len(sec.Report))
		}
		var full JobResultOutput
		call(t, cs, "job_result", map[string]any{"id": id, "detail": "full", "max_bytes": 1000}, &full)
		if full.ReportTruncated != tc.cut {
			t.Fatalf("full +%d: truncated %v, %d bytes", tc.extra, full.ReportTruncated, len(full.Report))
		}
		var sum JobResultOutput
		call(t, cs, "job_result", map[string]any{"id": id, "max_bytes": 1000 - 12}, &sum)
		if sum.ReportTruncated != tc.cut {
			t.Fatalf("summary +%d: truncated %v, summary %d bytes", tc.extra, sum.ReportTruncated, len(sum.Summary))
		}
	}
	// 1011 bytes cut to 1000: the returned text is longer than 1000 because of the marker, still truncated.
	id := insertReportJob(t, m, 40, "## Summary\n"+body(1011-11-1)+"\n", nil)
	var full JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": id, "detail": "full", "max_bytes": 1000}, &full)
	if !full.ReportTruncated || len(full.Report) <= 1000 {
		t.Fatalf("1011 -> 1000: truncated %v, %d bytes", full.ReportTruncated, len(full.Report))
	}
}

// F3: with an explicit max_bytes the full view caps not_done, progress and forks too; without it they
// keep their old size.
func TestJobResultFullAppliesMaxBytesToEveryField(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))
	report := "## Summary\nfixed it\n\n## Not done / uncertain\nBLOCKED: first\n" + strings.Repeat("n", 20000) +
		"\n\n## Forks\n- a / b / c\n" + strings.Repeat("f", 20000) + "\n\n## Progress\n" + strings.Repeat("- 10:00 UTC step\n", 3000)
	id := insertReportJob(t, m, 50, report, nil)
	var capped JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": id, "detail": "full", "max_bytes": 1000}, &capped)
	for name, v := range map[string]string{"not_done": capped.NotDone, "progress": capped.Progress, "forks": capped.Forks, "report": capped.Report} {
		if !strings.Contains(v, "[cut: 1000 of ") || len(v) > 1000+60 {
			t.Fatalf("%s is not capped: %d bytes", name, len(v))
		}
	}
	if !strings.HasPrefix(capped.NotDone, "BLOCKED: first") || !capped.ReportTruncated {
		t.Fatalf("capped not_done %q, truncated %v", capped.NotDone[:20], capped.ReportTruncated)
	}
	var def JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": id, "detail": "full"}, &def)
	if len(def.NotDone) != len("BLOCKED: first\n")+20000 || len(def.Forks) < 20000 || len(def.Progress) < 50000 ||
		strings.Contains(def.NotDone+def.Forks+def.Progress, "[cut: ") || def.ReportTruncated {
		t.Fatalf("legacy default changed: not_done %d, forks %d, progress %d", len(def.NotDone), len(def.Forks), len(def.Progress))
	}
}
