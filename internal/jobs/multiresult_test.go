package jobs

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// twoResultStream is the shape of issue #154: the report result, then a background task's notification
// wakes the session for one more turn and claude prints a second, short result.
const twoResultStream = `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"id":"a1","content":[{"type":"text","text":"## Summary\nall done\n\n## Not done / uncertain\nnothing"}]}}
{"type":"result","subtype":"success","is_error":false,"num_turns":87,"total_cost_usd":3.2,"result":"## Summary\nall done\n\n## Not done / uncertain\nnothing"}
{"type":"assistant","message":{"id":"a2","content":[{"type":"text","text":"This is a notification from my own wait timer."}]}}
{"type":"result","subtype":"success","is_error":false,"num_turns":1,"total_cost_usd":3.25,"result":"This is a notification from my own wait timer."}
`

// The wrapper's tee keeps the report of the last result that has one, sums the turns of every result
// (claude counts num_turns per query) and keeps the session's running cost.
func TestTeeTrailingResultKeepsReport(t *testing.T) {
	tee := &resultTee{w: io.Discard}
	_, _ = tee.Write([]byte(twoResultStream))
	tee.end()
	r := tee.res
	if !r.Found || r.Subtype != "success" || r.IsError || r.Results != 2 {
		t.Fatalf("result: %+v", r)
	}
	if !strings.HasPrefix(r.Text, "## Summary\nall done") {
		t.Fatalf("text taken from the trailing notification turn: %q", r.Text)
	}
	if r.Turns != 88 || r.CostUSD != 3.25 {
		t.Fatalf("turns/cost %d/%v, want the run total 88/3.25", r.Turns, r.CostUSD)
	}

	// ParseStream (stream.jsonl) reads the same way.
	path := filepath.Join(t.TempDir(), "stream.jsonl")
	if err := os.WriteFile(path, []byte(twoResultStream), 0o600); err != nil {
		t.Fatal(err)
	}
	if p := ParseStream(path); !strings.HasPrefix(p.Text, "## Summary\nall done") || p.Turns != 88 || p.CostUSD != 3.25 {
		t.Fatalf("ParseStream: %+v", p)
	}

	// A trailing result that is a report itself replaces the earlier one.
	again := twoResultStream + `{"type":"result","subtype":"success","is_error":false,"num_turns":2,"total_cost_usd":3.3,"result":"## Summary\nrewritten\n\n## Not done / uncertain\nnothing"}` + "\n"
	tee = &resultTee{w: io.Discard}
	_, _ = tee.Write([]byte(again))
	tee.end()
	if !strings.HasPrefix(tee.res.Text, "## Summary\nrewritten") || tee.res.Turns != 90 || tee.res.Results != 3 {
		t.Fatalf("third result: %+v", tee.res)
	}
}

// A job whose session got a trailing notification turn after its report succeeds with that report and
// the run's total turns (issue #154).
func TestTrailingNotificationResultKeepsReport(t *testing.T) {
	m := newManager(t)
	noAutoContinue(m)
	j := runToEnd(t, m, enqueue(t, m, "FAKE_BG_NOTIFY", nil).ID)
	if j.Status != store.StatusSucceeded || j.Error != "" {
		t.Fatalf("job: %s %q", j.Status, j.Error)
	}
	if j.Turns != 88 || j.CostUSD != 3.25 {
		t.Fatalf("turns/cost %d/%v, want the run total 88/3.25", j.Turns, j.CostUSD)
	}
	if r := readFile(t, filepath.Join(m.JobDir(j.ID), "report.md")); !strings.HasPrefix(r, "## Summary\nbg done") {
		t.Fatalf("report.md lost the report: %q", r)
	}
	if line := DoneLine(j, 0); strings.Contains(line, "report has no") || strings.Contains(line, "failed") {
		t.Fatalf("done line: %s", line)
	}
}

// mergeResult keeps the earlier text only when it has strictly more report sections than the later one,
// judged by the same section parser as the final validation (issue #154, review H1).
func TestMergeResultReportSelection(t *testing.T) {
	full := "## Summary\nok\n\n## Not done / uncertain\nnothing"
	intro := "Final report follows.\n\n## summary\nok\n\n## not done / uncertain\nnone"
	summaryOnly := "## Summary\nonly this"
	for _, tc := range []struct{ name, prev, next, want string }{
		{"plain notification", full, "timer fired", full},
		{"report with intro and lowercase headings", intro, "timer fired", intro},
		{"notification titled like a summary", full, "## Summary of background task\ntimer fired", full},
		{"summary-only notification", full, summaryOnly, full},
		{"newer complete report", full, intro, intro},
		{"summary-only beats a plain note", summaryOnly, "timer fired", summaryOnly},
		{"nothing is a report", "working", "timer fired", "timer fired"},
	} {
		got := mergeResult(StreamResult{Found: true, Turns: 5, Text: tc.prev}, StreamResult{Found: true, Turns: 1, Text: tc.next})
		if got.Text != tc.want || got.Turns != 6 {
			t.Errorf("%s: text %q turns %d, want %q / 6", tc.name, got.Text, got.Turns, tc.want)
		}
	}
}

// The assistant-text fallback and the idle close judge a report by its sections, like mergeResult: an
// introductory line and lowercase headings count, a fenced quote of the heading does not (issue #154).
func TestIsReportBySections(t *testing.T) {
	for text, want := range map[string]bool{
		"## Summary\nok": true,
		"Final report follows.\n\n## summary\nok": true,
		"timer fired": false,
		"## Summary of background task\ntimer fired":    false,
		"Quote:\n```\n## Summary\nx\n```\nnothing else": false,
	} {
		if got := isReport(text); got != want {
			t.Errorf("isReport(%q) = %v, want %v", text, got, want)
		}
	}
	path := filepath.Join(t.TempDir(), "stream.jsonl")
	line := `{"type":"assistant","message":{"id":"x","content":[{"type":"text","text":"Final report follows.\n\n## summary\nok"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := lastReportText(path); !strings.Contains(got, "## summary\nok") {
		t.Fatalf("lastReportText: %q", got)
	}
}
