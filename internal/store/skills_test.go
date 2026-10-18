package store

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// E7: a database from before the skill columns opens, gains them (jobs, job_stats, schedules) with
// empty values, and round-trips the skill lists.
func TestSkillColumnsMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	old := schedulesSchema
	if _, err := raw.Exec(stage1Schema + old); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO schedules (cron, prompt, created_at, updated_at) VALUES ('* * * * *', 'p', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	j, err := st.Get("old-1")
	if err != nil {
		t.Fatal(err)
	}
	if j.Skills != nil || j.SkillsDelivered != nil || j.SkillsNote != "" {
		t.Fatalf("old row skills %v %v %q", j.Skills, j.SkillsDelivered, j.SkillsNote)
	}
	if sc, err := st.GetSchedule(1); err != nil || sc.Skills != nil {
		t.Fatalf("old schedule %+v %v", sc, err)
	}
	now := time.Now()
	nj := &Job{ID: "new-1", Agent: "a", Title: "t", Kind: "fix", Brief: "b", Status: StatusQueued, Model: "m",
		MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1, CreatedAt: now, Skills: []string{"a", "b"}}
	if err := st.Insert(nj); err != nil {
		t.Fatal(err)
	}
	if err := st.Claim("new-1", "tok", now); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSkillsDelivered("new-1", "other", []string{"a"}, ""); err == nil {
		t.Fatal("recorded with a foreign token")
	}
	if err := st.SetSkillsDelivered("new-1", "tok", []string{"a"}, "note"); err != nil {
		t.Fatal(err)
	}
	g, _ := st.Get("new-1")
	if !slices.Equal(g.Skills, []string{"a", "b"}) || !slices.Equal(g.SkillsDelivered, []string{"a"}) || g.SkillsNote != "note" {
		t.Fatalf("round trip %v %v %q", g.Skills, g.SkillsDelivered, g.SkillsNote)
	}
	if err := st.PutStats(&JobStats{ID: "new-1", Kind: "fix", Model: "m", Status: StatusSucceeded, CreatedAt: now,
		RecordedAt: now, SkillsDelivered: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	if s, err := st.GetStats("new-1"); err != nil || !slices.Equal(s.SkillsDelivered, []string{"a"}) {
		t.Fatalf("stats %+v %v", s, err)
	}
	sc, err := st.AddSchedule(&Schedule{Cron: "* * * * *", Prompt: "p", Enabled: true, Kind: "fix", Skills: []string{"+a", "-b"}}, now)
	if err != nil || !slices.Equal(sc.Skills, []string{"+a", "-b"}) {
		t.Fatalf("schedule %+v %v", sc, err)
	}
}
