package jobs

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// A claude job gets an explicit effort (#40): its own, else the kind's default from effort_by_kind,
// recorded on the row and passed as --effort on the command line (fresh and resumed).
func TestClaudeEffortDefaultsAndArgs(t *testing.T) {
	m := newManager(t)
	for kind, want := range map[string]string{"review": "high", "research": "high", "fix": "high", "general": "medium"} {
		j := enqueue(t, m, "x", func(r *StartRequest) { r.Kind, r.Model = kind, "opus" })
		if j.ReasoningEffort != want {
			t.Errorf("%s: effort %q, want %q", kind, j.ReasoningEffort, want)
		}
	}
	j := enqueue(t, m, "x", func(r *StartRequest) { r.Kind, r.Model, r.ReasoningEffort = "fix", "opus", "xhigh" })
	if j.ReasoningEffort != "xhigh" {
		t.Fatalf("explicit effort %q", j.ReasoningEffort)
	}
	j.SessionID = "s"
	for name, args := range map[string][]string{"fresh": ClaudeArgs(j, "/jd"), "resume": ResumeClaudeArgs(j, "/jd")} {
		i := slices.Index(args, "--effort")
		if i < 0 || i+1 >= len(args) || args[i+1] != "xhigh" {
			t.Errorf("%s args lack --effort xhigh: %v", name, args)
		}
	}
	// A row from before #40 (no effort) runs without the flag.
	j.ReasoningEffort = ""
	if slices.Contains(ClaudeArgs(j, "/jd"), "--effort") {
		t.Error("empty effort must not pass --effort")
	}
	m.Cfg.EffortByKind = map[string]string{"general": "low"}
	if j := enqueue(t, m, "x", func(r *StartRequest) { r.Kind, r.Model = "general", "opus" }); j.ReasoningEffort != "low" {
		t.Fatalf("configured general effort %q", j.ReasoningEffort)
	}
	if j := enqueue(t, m, "x", func(r *StartRequest) { r.Kind, r.Model = "fix", "opus" }); j.ReasoningEffort != "high" {
		t.Fatalf("unconfigured fix effort %q", j.ReasoningEffort)
	}
}

// One validator with per-runner value lists: claude takes xhigh and max, codex does not, and an
// unknown value is refused naming the allowed values of the job's runner.
func TestEffortPerRunnerValues(t *testing.T) {
	m := newManager(t)
	for _, e := range []string{"low", "medium", "high", "xhigh", "max"} {
		if err := ValidateReasoningEffort(store.RunnerClaude, e); err != nil {
			t.Errorf("claude %s: %v", e, err)
		}
	}
	for _, e := range []string{"xhigh", "max"} {
		if err := ValidateReasoningEffort(store.RunnerCodex, e); err == nil {
			t.Errorf("codex %s accepted", e)
		}
	}
	_, err := m.Enqueue(StartRequest{Title: "t", Kind: "fix", Brief: "b", Model: "opus", ReasoningEffort: "turbo"})
	if err == nil || !strings.Contains(err.Error(), "low|medium|high|xhigh|max") {
		t.Fatalf("unknown claude effort: %v", err)
	}
	_, err = m.Enqueue(StartRequest{Title: "t", Kind: "review", Brief: "b", Model: "gpt-6.1-sol", ReasoningEffort: "max"})
	if err == nil || !strings.Contains(err.Error(), "low|medium|high") || strings.Contains(err.Error(), "xhigh") {
		t.Fatalf("max on codex: %v", err)
	}
}

func TestEffortByKindConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) error {
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadConfig(dir)
		return err
	}
	if err := write(`{"effort_by_kind": {"review": "max", "general": "low"}}`); err != nil {
		t.Fatal(err)
	}
	if err := write(`{"effort_by_kind": {"review": "turbo"}}`); err == nil || !strings.Contains(err.Error(), "effort_by_kind.review") {
		t.Fatalf("bad value: %v", err)
	}
	if err := write(`{"effort_by_kind": {"deploy": "high"}}`); err == nil || !strings.Contains(err.Error(), "deploy") {
		t.Fatalf("unknown kind: %v", err)
	}
}

func TestDoneLineShowsEffort(t *testing.T) {
	j := &store.Job{ID: "id", Title: "t", Status: store.StatusSucceeded, Model: "opus", ReasoningEffort: "high",
		ScopeUnit: "u", StartedAt: &time.Time{}}
	if l := DoneLine(j, 0); !strings.Contains(l, "· model opus · effort high") {
		t.Fatalf("done line: %s", l)
	}
	j.ReasoningEffort = ""
	if l := DoneLine(j, 0); strings.Contains(l, "effort") {
		t.Fatalf("done line without effort: %s", l)
	}
}

func TestSummarizeByEffort(t *testing.T) {
	a := statsRow("1", "fix", "opus", store.StatusSucceeded, 1, 1, nil, nil)
	b := statsRow("2", "fix", "opus", store.StatusFailed, 1, 1, nil, nil)
	c := statsRow("3", "fix", "opus", store.StatusSucceeded, 1, 1, nil, nil)
	a.Effort, b.Effort = "high", "high"
	rep, err := Summarize([]*store.JobStats{a, b, c}, "effort")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, g := range rep.Groups {
		got = append(got, g.Key)
	}
	if strings.Join(got, " ") != "high (none)" {
		t.Fatalf("groups: %v", got)
	}
}
