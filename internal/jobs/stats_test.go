package jobs

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// streamFixture is a stream.jsonl as claude -p --output-format stream-json writes it: tool calls of
// the job and of a subagent, a Skill call, a skill read, two guard refusals (one of them a refused
// subagent) and the result.
func streamFixture(t *testing.T) []byte {
	t.Helper()
	ev := func(typ string, blocks ...map[string]any) string {
		b, err := json.Marshal(map[string]any{"type": typ, "message": map[string]any{"content": blocks}})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	use := func(id, name string, input map[string]any) map[string]any {
		return map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}
	}
	res := func(id string, isErr bool, content any) map[string]any {
		return map[string]any{"type": "tool_result", "tool_use_id": id, "is_error": isErr, "content": content}
	}
	lines := []string{
		`{"type":"system","subtype":"init","session_id":"s"}`,
		ev("assistant", map[string]any{"type": "text", "text": "start"}, use("1", "Bash", map[string]any{"command": "ls"})),
		ev("user", res("1", false, "a\nb")),
		ev("assistant", use("2", "Skill", map[string]any{"skill": "deploy"})),
		ev("assistant", use("3", "Read", map[string]any{"file_path": "/home/u/.claude/skills/review/SKILL.md"})),
		ev("assistant", use("4", "Bash", map[string]any{"command": "git push"})),
		ev("user", res("4", true, "PreToolUse:Bash hook error: [agent-jobs guard]: blocked by guard: `git push` reaches outside the job.")),
		ev("assistant", use("5", "Agent", map[string]any{"description": "d", "prompt": "p"})),
		ev("assistant", use("6", "Read", map[string]any{"file_path": "/repo/main.go"})),
		ev("user", res("6", true, "File does not exist.")),
		ev("assistant", use("7", "Agent", map[string]any{"description": "d", "prompt": "p", "run_in_background": true})),
		ev("user", res("7", true, []map[string]any{{"type": "text", "text": "PreToolUse:Agent hook error: agent-jobs guard: " +
			"a job's subagent runs in the foreground inside the job: run_in_background"}})),
		`not json at all`,
		`{"type":"result","subtype":"success","is_error":false,"num_turns":9,"total_cost_usd":1.5}`,
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

func TestParseStreamStats(t *testing.T) {
	st, err := ParseStreamStats(bytes.NewReader(streamFixture(t)))
	if err != nil {
		t.Fatal(err)
	}
	wantTools := map[string]int{"Bash": 2, "Skill": 1, "Read": 2, "Agent": 2}
	if !maps.Equal(st.ToolCalls, wantTools) {
		t.Fatalf("tools %v, want %v", st.ToolCalls, wantTools)
	}
	if !maps.Equal(st.Skills, map[string]int{"deploy": 1, "review": 1}) {
		t.Fatalf("skills %v", st.Skills)
	}
	if st.GuardRefusals != 2 || !maps.Equal(st.GuardRules, map[string]int{"outward": 1, "subagent-isolation": 1}) {
		t.Fatalf("refusals %d %v", st.GuardRefusals, st.GuardRules)
	}
	if st.Subagents != 1 || st.ResultSubtype != "success" || st.Turns != 9 || !st.Found {
		t.Fatalf("subagents %d, result %q %d", st.Subagents, st.ResultSubtype, st.Turns)
	}
}

// An over-long line is skipped, not held whole: the lines after it still count.
func TestParseStreamStatsSkipsHugeLine(t *testing.T) {
	huge := `{"type":"user","message":{"content":[{"type":"tool_result","content":"` +
		strings.Repeat("x", MaxStreamLineBytes+10) + `"}]}}`
	in := huge + "\n" + `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"1","name":"Bash","input":{}}]}}` +
		"\n" + `{"type":"result","subtype":"error_max_turns"}`
	st, err := ParseStreamStats(strings.NewReader(in))
	if err != nil || st.ToolCalls["Bash"] != 1 || st.ResultSubtype != "error_max_turns" {
		t.Fatalf("%+v %v", st, err)
	}
}

func TestSkillFromPath(t *testing.T) {
	for p, want := range map[string]string{
		"/home/u/.claude/skills/review/SKILL.md":    "review",
		"/x/plugins/p/skills/deploy/scripts/run.sh": "deploy",
		"skills/notes.md":                           "notes",
		"/home/u/skills":                            "",
		"/repo/internal/jobs/stats.go":              "",
		"/a/skills/inner/skills/leaf/SKILL.md":      "leaf",
	} {
		if got := skillFromPath(p); got != want {
			t.Fatalf("%s: %q, want %q", p, got, want)
		}
	}
}

// Backfill writes the missing rows from job folders and from the archive (stream.jsonl.gz), keeps
// present rows, and fills in the store fields for a job whose stream is gone.
func TestBackfillStats(t *testing.T) {
	m := newManager(t)
	fin := time.Now().Add(-100 * time.Hour)
	insertJob(t, m, "dir", store.StatusSucceeded, &fin, func(j *store.Job) {
		j.Kind, j.RepoPath, j.Turns, j.CostUSD, j.DurationSec = "fix", "/home/u/src/agent-jobs", 9, 1.5, 300
	})
	writeFile(t, filepath.Join(m.JobDir("dir"), "stream.jsonl"), streamFixture(t), 0o600)
	writeFile(t, filepath.Join(m.JobDir("dir"), "report.md"), []byte("## Summary\n"), 0o600)

	insertJob(t, m, "gz", store.StatusFailed, &fin, nil)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(streamFixture(t))
	_ = zw.Close()
	adir := filepath.Join(m.ArchiveDir(), fin.UTC().Format("2006-01"), "gz")
	writeFile(t, filepath.Join(adir, "stream.jsonl.gz"), gz.Bytes(), 0o600)
	writeFile(t, filepath.Join(adir, "report.md"), []byte("r"), 0o600)

	insertJob(t, m, "gone", store.StatusTimeout, &fin, func(j *store.Job) { j.Turns = 4 })
	insertJob(t, m, "running", store.StatusRunning, nil, nil)

	rep, err := m.BackfillStats(false)
	if err != nil || rep.Recorded != 3 || len(rep.Errors) != 0 {
		t.Fatalf("backfill: %+v %v", rep, err)
	}
	d, err := m.Store.GetStats("dir")
	if err != nil {
		t.Fatal(err)
	}
	if d.Source != "job_dir" || d.Repo != "agent-jobs" || d.Kind != "fix" || d.Turns != 9 || d.CostUSD != 1.5 ||
		d.DurationSec != 300 || !d.HasReport || d.GuardRefusals != 2 || d.Skills["deploy"] != 1 || d.ResultSubtype != "success" ||
		d.FinishedAt == nil || d.FinishedAt.Unix() != fin.Unix() {
		t.Fatalf("dir row: %+v", d)
	}
	g, _ := m.Store.GetStats("gz")
	if g == nil || g.Source != "archive" || g.ToolCalls["Bash"] != 2 || !g.HasReport || g.Status != store.StatusFailed {
		t.Fatalf("gz row: %+v", g)
	}
	o, _ := m.Store.GetStats("gone")
	if o == nil || o.Source != "none" || o.Turns != 4 || o.HasReport || len(o.ToolCalls) != 0 {
		t.Fatalf("gone row: %+v", o)
	}
	if _, err := m.Store.GetStats("running"); err == nil {
		t.Fatal("a running job got a stats row")
	}
	again, err := m.BackfillStats(false)
	if err != nil || again.Recorded != 0 || again.Present != 3 {
		t.Fatalf("second backfill: %+v %v", again, err)
	}
	if all, err := m.BackfillStats(true); err != nil || all.Recorded != 3 {
		t.Fatalf("backfill all: %+v %v", all, err)
	}
}

func statsRow(id, kind, model, status string, cost float64, dur int, skills map[string]int, rules map[string]int) *store.JobStats {
	n := 0
	for _, c := range rules {
		n += c
	}
	return &store.JobStats{ID: id, Kind: kind, Model: model, Status: status, CostUSD: cost, DurationSec: dur,
		Skills: skills, GuardRules: rules, GuardRefusals: n, ToolCalls: map[string]int{"Bash": 1}, HasReport: status == store.StatusSucceeded}
}

func TestSummarizeGrouping(t *testing.T) {
	rows := []*store.JobStats{
		statsRow("1", "fix", "opus", store.StatusSucceeded, 1, 100, map[string]int{"deploy": 2}, nil),
		statsRow("2", "fix", "opus", store.StatusFailed, 2, 200, map[string]int{"deploy": 1, "review": 1}, map[string]int{"outward": 2}),
		statsRow("3", "fix", "sonnet", store.StatusSucceeded, 3, 300, nil, map[string]int{"pipe-status": 1}),
		statsRow("4", "review", "sonnet", store.StatusSucceeded, 4, 400, map[string]int{"review": 1}, map[string]int{"outward": 1}),
		statsRow("5", "research", "sonnet", store.StatusTimeout, 10, 1000, nil, nil),
	}
	rep, err := Summarize(rows, "")
	if err != nil {
		t.Fatal(err)
	}
	tot := rep.Totals
	if rep.By != "kind" || tot.Jobs != 5 || tot.SuccessRate != 0.6 || tot.CostTotalUSD != 20 || tot.CostMedianUSD != 3 ||
		tot.CostP90USD != 10 || tot.DurationMedianSec != 300 || tot.DurationP90Sec != 1000 || tot.GuardRefusals != 4 ||
		tot.WithoutReport != 2 || tot.ByStatus[store.StatusTimeout] != 1 {
		t.Fatalf("totals: %+v", tot)
	}
	keys := func(r StatsReport) string {
		var out []string
		for _, g := range r.Groups {
			out = append(out, fmt.Sprintf("%s:%d:%.3f", g.Key, g.Jobs, g.SuccessRate))
		}
		return strings.Join(out, " ")
	}
	if got := keys(rep); got != "fix:3:0.667 research:1:0.000 review:1:1.000" {
		t.Fatalf("by kind: %s", got)
	}
	if byModel, _ := Summarize(rows, "model"); keys(byModel) != "sonnet:3:0.667 opus:2:0.500" {
		t.Fatalf("by model: %s", keys(byModel))
	}
	bySkill, _ := Summarize(rows, "skill")
	if got := keys(bySkill); got != "(none):2:0.500 deploy:2:0.500 review:2:0.500" {
		t.Fatalf("by skill: %s", got)
	}
	if got := fmt.Sprint(rep.GuardRules); got != "[{outward 3} {pipe-status 1}]" {
		t.Fatalf("guard rules: %s", got)
	}
	if got := fmt.Sprint(rep.Skills); got != "[{deploy 2 3 1 0.5 0 0} {review 2 2 1 0.5 0 0}]" {
		t.Fatalf("skills: %s", got)
	}
	if _, err := Summarize(rows, "repo"); err == nil {
		t.Fatal("unknown --by accepted")
	}
	if empty, err := Summarize(nil, "kind"); err != nil || empty.Totals.Jobs != 0 || empty.Groups == nil {
		t.Fatalf("empty: %+v %v", empty, err)
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"":           {},
		"7d":         now.Add(-7 * 24 * time.Hour),
		"36h":        now.Add(-36 * time.Hour),
		"2026-10-01": time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	} {
		if got, err := ParseSince(in, now); err != nil || !got.Equal(want) {
			t.Fatalf("%q: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"x", "-3d", "1.5d", "2026-13-01"} {
		if _, err := ParseSince(bad, now); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

// The summary row is written when a job reaches a terminal status: on collect, on cancel and on a
// refused launch.
func TestStatsRowWrittenOnJobEnd(t *testing.T) {
	m := newManager(t)
	j := enqueue(t, m, "FAKE_TOOLS do it", nil)
	runToEnd(t, m, j.ID)
	st, err := m.Store.GetStats(j.ID)
	if err != nil {
		t.Fatalf("no stats row after the job ended: %v", err)
	}
	if st.Status != store.StatusSucceeded || st.Turns != 3 || st.CostUSD != 0.25 || !st.HasReport ||
		st.Skills["deploy"] != 1 || st.ToolCalls["Bash"] != 1 || st.GuardRules["outward"] != 1 ||
		st.ResultSubtype != "success" || st.Source != "job_dir" || st.Model != j.Model {
		t.Fatalf("row: %+v", st)
	}

	q := enqueue(t, m, "queued then cancelled", nil)
	if err := m.Cancel(q.ID); err != nil {
		t.Fatal(err)
	}
	if st, err := m.Store.GetStats(q.ID); err != nil || st.Status != store.StatusCancelled {
		t.Fatalf("cancelled row: %+v %v", st, err)
	}

	m.Cfg.OwnerRulesPath = filepath.Join(t.TempDir(), "missing.md") // the launch is refused
	r := enqueue(t, m, "refused", nil)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if st, err := m.Store.GetStats(r.ID); err != nil || st.Status != store.StatusFailed || st.Source != "none" {
		t.Fatalf("refused row: %+v %v", st, err)
	}
	rep, err := m.Stats(time.Time{}, "kind")
	if err != nil || rep.Totals.Jobs != 3 {
		t.Fatalf("stats: %+v %v", rep, err)
	}
}

// TestSummarizeUnknownCost: a job whose cost is unknown is counted apart and left out of the cost
// total, median and p90 (it would otherwise count as $0).
func TestSummarizeUnknownCost(t *testing.T) {
	unknown := statsRow("3", "review", "gpt-6.1-sol", store.StatusSucceeded, 0, 100, nil, nil)
	unknown.CostUnknown = true
	rows := []*store.JobStats{
		statsRow("1", "review", "opus", store.StatusSucceeded, 2, 100, nil, nil),
		statsRow("2", "review", "opus", store.StatusSucceeded, 4, 100, nil, nil),
		unknown,
	}
	rep, err := Summarize(rows, "model")
	if err != nil {
		t.Fatal(err)
	}
	tot := rep.Totals
	if tot.Jobs != 3 || tot.CostUnknownJobs != 1 || tot.CostTotalUSD != 6 || tot.CostMedianUSD != 2 || tot.CostP90USD != 4 {
		t.Fatalf("totals: %+v", tot)
	}
	for _, g := range rep.Groups {
		if g.Key == "gpt-6.1-sol" && (g.CostUnknownJobs != 1 || g.CostTotalUSD != 0) {
			t.Fatalf("codex group: %+v", g)
		}
	}
}

// TestStatsRowCodexCostUnknown: the stats row of a codex job carries cost_unknown through the store.
func TestStatsRowCodexCostUnknown(t *testing.T) {
	m := newCodexManager(t)
	j := runToEnd(t, m, enqueue(t, m, "review it", codexReview).ID)
	st, err := m.Store.GetStats(j.ID)
	if err != nil || !st.CostUnknown {
		t.Fatalf("stats row: %+v %v", st, err)
	}
	rep, err := m.Stats(time.Time{}, "kind")
	if err != nil || rep.Totals.CostUnknownJobs != 1 {
		t.Fatalf("stats: %+v %v", rep, err)
	}
}
