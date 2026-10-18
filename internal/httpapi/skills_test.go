package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

func TestSkillsRating(t *testing.T) {
	f := newFixture(t)
	root := t.TempDir()
	dir := filepath.Join(root, "s", "golang")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\ndescription: SKILL-BODY-SECRET.\n---\nSKILL-BODY-SECRET\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, _ := json.Marshal(map[string]any{"skills": []map[string]any{{"name": "golang", "path": "s/golang", "status": "core",
		"kinds": []string{"code"}, "last_verified": time.Now().Format("2006-01-02")}}})
	if err := os.WriteFile(filepath.Join(root, "index.json"), idx, 0o644); err != nil {
		t.Fatal(err)
	}
	f.m.Cfg.SkillRoots = []string{root}
	fin := f.now.Add(-time.Hour)
	for _, r := range []*store.JobStats{
		{ID: "a", Repo: "r1", Status: store.StatusSucceeded, CostUSD: 1, Skills: map[string]int{"golang": 1}, SkillsDelivered: []string{"golang"}},
		{ID: "b", Repo: "r2", Status: store.StatusFailed, CostUSD: 2, Skills: map[string]int{"golang": 1, "other": 1}},
		{ID: "c", Repo: "r1", Status: store.StatusSucceeded, CostUSD: 3, Skills: map[string]int{"other": 1},
			FinishedAt: ptr(f.now.Add(-40 * 24 * time.Hour))},
	} {
		r.Kind, r.Model, r.CreatedAt, r.RecordedAt = "fix", "opus", fin, fin
		if r.FinishedAt == nil {
			r.FinishedAt = &fin
		}
		if err := f.m.Store.PutStats(r); err != nil {
			t.Fatal(err)
		}
	}
	res, b := f.do(t, http.MethodGet, "/v1/skills", f.token)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", res.StatusCode, b)
	}
	for _, bad := range []string{"SKILL-BODY-SECRET", root, f.m.StateDir, "description", "path"} {
		if strings.Contains(string(b), bad) {
			t.Fatalf("response carries %q: %s", bad, b)
		}
	}
	var raw struct {
		Skills []map[string]any `json:"skills"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	g := raw.Skills[0]
	if g["name"] != "golang" || g["status"] != "core" || g["used"] != 2.0 || g["delivered"] != 1.0 ||
		g["success_rate"] != 0.5 || g["median_cost_usd"] != 1.0 || g["stale"] != false || g["last_used"] == "" {
		t.Fatalf("golang %v", g)
	}
	if br := g["by_repo"].([]any); len(br) != 2 {
		t.Fatalf("by_repo %v", br)
	}
	// "other": used once in the window (job c is older than 30 days), not in the catalog.
	if o := raw.Skills[1]; o["name"] != "other" || o["used"] != 1.0 || o["status"] != "not_in_catalog" {
		t.Fatalf("other %v", o)
	}
	if o := f.get(t, "/v1/skills?since=60d", http.StatusOK); o["skills"].([]any)[0].(map[string]any)["used"] != 2.0 {
		t.Fatalf("since=60d %v", o)
	}
	if o := f.get(t, "/v1/skills?repo=r2", http.StatusOK); len(o["skills"].([]any)) != 2 ||
		o["skills"].([]any)[0].(map[string]any)["used"] != 1.0 {
		t.Fatalf("repo=r2 %v", o)
	}
	if o := f.get(t, "/v1/skills?limit=1", http.StatusOK); len(o["skills"].([]any)) != 1 || o["truncated"] != true {
		t.Fatalf("limit=1 %v", o)
	}
	for _, q := range []string{"since=400d", "since=bad", "limit=0", "limit=501", "limit=x", "repo=a/b", "repo=.."} {
		f.get(t, "/v1/skills?"+q, http.StatusBadRequest)
	}
}

func ptr(t time.Time) *time.Time { return &t }

func TestJobViewSkillNames(t *testing.T) {
	f := newFixture(t)
	j := &store.Job{ID: "261018-100009-cccc", Agent: "test", Title: "t", Kind: "fix", Brief: "b", Status: store.StatusQueued,
		Model: "opus", MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1, CreatedAt: time.Now(), Skills: []string{"a", "b"}}
	if err := f.m.Store.Insert(j); err != nil {
		t.Fatal(err)
	}
	out := f.get(t, "/v1/jobs/"+j.ID, http.StatusOK)
	if s, _ := out["skills"].([]any); len(s) != 2 || s[0] != "a" {
		t.Fatalf("skills %v", out["skills"])
	}
}
