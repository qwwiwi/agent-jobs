package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/guard"
	"github.com/qwwiwi/agent-jobs/internal/skills"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// skillCatalog writes a catalog root (index.json + skill dirs) and returns it.
func skillCatalog(t *testing.T, entries ...map[string]any) string {
	t.Helper()
	root := t.TempDir()
	for _, e := range entries {
		name := e["name"].(string)
		e["path"] = "skills/" + name
		if e["last_verified"] == nil {
			e["last_verified"] = time.Now().Format("2006-01-02")
		}
		dir := filepath.Join(root, "skills", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "---\nname: " + name + "\ndescription: The " + name + " skill. Details.\n---\n\nUse " + name + ".\n"
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := json.Marshal(map[string]any{"version": 1, "skills": entries})
	if err := os.WriteFile(filepath.Join(root, "index.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func skillsManager(t *testing.T) (*Manager, string) {
	t.Helper()
	m := newManager(t)
	root := skillCatalog(t,
		map[string]any{"name": "golang", "status": "core", "kinds": []string{"code"}},
		map[string]any{"name": "git-workflows", "status": "core", "kinds": []string{"code", "review"}},
		map[string]any{"name": "researching", "status": "core", "kinds": []string{"research"}},
		map[string]any{"name": "old", "status": "deprecated", "kinds": []string{"code"}},
		map[string]any{"name": "drafty", "status": "draft"},
		map[string]any{"name": "extra", "status": "active"},
	)
	m.Cfg.SkillRoots = []string{root}
	return m, root
}

func TestStartSelectsSkills(t *testing.T) {
	m, _ := skillsManager(t)
	repo := newRepo(t)
	j := enqueue(t, m, "x", func(r *StartRequest) { r.Kind, r.RepoPath = "fix", repo })
	if !slices.Equal(j.Skills, []string{"git-workflows", "golang"}) {
		t.Fatalf("defaults %v", j.Skills)
	}
	j = enqueue(t, m, "x", func(r *StartRequest) { r.Kind, r.Skills = "research", []string{"+extra", "-researching", "drafty"} })
	if got := get(t, m, j.ID).Skills; !slices.Equal(got, []string{"drafty", "extra"}) {
		t.Fatalf("edited %v (stored)", got)
	}
	for edit, want := range map[string]string{"golnag": "close: golang", "old": "deprecated"} {
		_, err := m.Enqueue(StartRequest{Title: "t", Kind: "fix", Brief: "b", Model: "sonnet", Skills: []string{edit}})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", edit, err)
		}
	}
	m.Cfg.SkillsMaxPerJob = 2
	if _, err := m.Enqueue(StartRequest{Title: "t", Kind: "fix", Brief: "b", Model: "sonnet", Skills: []string{"extra"}}); err == nil ||
		!strings.Contains(err.Error(), "skills_max_per_job") {
		t.Fatalf("over count: %v", err)
	}
}

func TestSkillsDeliveredAndRemoved(t *testing.T) {
	m, root := skillsManager(t)
	repo := newRepo(t)
	// The repository has its own git-workflows: it wins.
	own := filepath.Join(repo, ".claude", "skills", "git-workflows")
	if err := os.MkdirAll(own, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(own, "SKILL.md"), []byte("repo version\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", ".")
	gitT(t, repo, "commit", "-qm", "repo skill")
	j := enqueue(t, m, "FAKE_SKILLS", func(r *StartRequest) { r.Kind, r.RepoPath = "fix", repo })
	j = runToEnd(t, m, j.ID)
	if j.Status != store.StatusSucceeded {
		t.Fatalf("status %s %q", j.Status, j.Error)
	}
	dir := m.JobDir(j.ID)
	seen, err := os.ReadFile(filepath.Join(dir, "skills-seen.txt"))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(string(seen), "---\n")
	if len(parts) != 3 || parts[0] != "git-workflows\ngolang\n" {
		t.Fatalf("job saw skills %q", seen)
	}
	realRoot, _ := filepath.EvalSymlinks(root)
	if !strings.Contains(parts[1], filepath.Join(realRoot, "skills", "golang")) {
		t.Fatalf("golang link target %q", parts[1])
	}
	if strings.TrimSpace(parts[2]) != "" {
		t.Fatalf("git status shows the links: %q", parts[2])
	}
	if !slices.Equal(j.Skills, []string{"git-workflows", "golang"}) || !slices.Equal(j.SkillsDelivered, []string{"golang"}) ||
		!strings.Contains(j.SkillsNote, "repository wins") {
		t.Fatalf("row skills %v delivered %v note %q", j.Skills, j.SkillsDelivered, j.SkillsNote)
	}
	prompt, _ := os.ReadFile(filepath.Join(dir, "prompt.md"))
	if !strings.Contains(string(prompt), "Skills for this job (in .claude/skills; load one when it fits the task): git-workflows, golang.") {
		t.Fatal("brief does not name the skills")
	}
	// Settings: deny rules and the guard protect the catalog and the link.
	sb, _ := os.ReadFile(filepath.Join(dir, "settings.json"))
	settings := string(sb)
	for _, want := range []string{"Edit(//" + strings.TrimPrefix(realRoot, "/") + "/**)",
		"Write(//" + strings.TrimPrefix(filepath.Join(dir, "work", ".claude", "skills", "golang"), "/") + "/**)",
		"--protect '" + root + "'"} {
		if !strings.Contains(settings, want) {
			t.Errorf("settings.json lacks %s", want)
		}
	}
	// Ended: links gone, the repository's own skill kept, info/exclude block removed.
	work := filepath.Join(dir, "work")
	if _, err := os.Lstat(filepath.Join(work, ".claude", "skills", "golang")); !os.IsNotExist(err) {
		t.Fatal("link left after the job ended")
	}
	if b, err := os.ReadFile(filepath.Join(work, ".claude", "skills", "git-workflows", "SKILL.md")); err != nil || string(b) != "repo version\n" {
		t.Fatal("the repository's own skill changed")
	}
	if b, _ := os.ReadFile(filepath.Join(repo, ".git", "info", "exclude")); strings.Contains(string(b), j.ID) {
		t.Fatalf("info/exclude keeps the job's block:\n%s", b)
	}
	// Nothing from .claude/skills/golang in the commit or the patches.
	if files := gitT(t, work, "diff", "--name-only", j.BaseCommit, "HEAD"); strings.Contains(files, ".claude") ||
		!strings.Contains(files, "skillwork.txt") {
		t.Fatalf("job diff: %q", files)
	}
	real := time.Now
	m.Now = func() time.Time { return real().Add(31 * time.Minute) }
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"commits.patch", "uncommitted.patch", "diffstat.txt", "untracked.tar"} {
		if b, _ := os.ReadFile(filepath.Join(dir, f)); strings.Contains(string(b), ".claude/skills/golang") {
			t.Fatalf("%s carries the skill link", f)
		}
	}
	// Stats: delivered and not used.
	st, err := m.Store.GetStats(j.ID)
	if err != nil || !slices.Equal(st.SkillsDelivered, []string{"golang"}) {
		t.Fatalf("stats %+v %v", st, err)
	}
	m.Now = real
	rep, _ := m.Stats(time.Time{}, "skill")
	if len(rep.Skills) != 1 || rep.Skills[0].Name != "golang" || rep.Skills[0].Delivered != 1 || rep.Skills[0].DeliveredUnused != 1 {
		t.Fatalf("stats skills %+v", rep.Skills)
	}
}

func TestSkillsInfoExcludeDuringRun(t *testing.T) {
	m, _ := skillsManager(t)
	repo := newRepo(t)
	j := enqueue(t, m, "FAKE_SLEEP=3", func(r *StartRequest) { r.Kind, r.RepoPath = "fix", repo })
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(repo, ".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# agent-jobs " + j.ID, "\n/.claude/skills/golang\n", "\n/.claude/skills/git-workflows\n"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("info/exclude lacks %q:\n%s", want, b)
		}
	}
	if err := m.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, ".git", "info", "exclude")); strings.Contains(string(b), j.ID) {
		t.Fatalf("cancel kept the block:\n%s", b)
	}
	if _, err := os.Lstat(filepath.Join(m.JobDir(j.ID), "work", ".claude")); !os.IsNotExist(err) {
		t.Fatal("cancel left .claude in the work dir")
	}
}

func TestSkillsCodexSkipped(t *testing.T) {
	m := newCodexManager(t)
	root := skillCatalog(t, map[string]any{"name": "reviewing", "status": "core", "kinds": []string{"review"}})
	m.Cfg.SkillRoots = []string{root}
	j := runToEnd(t, m, enqueue(t, m, "review it", codexReview).ID)
	if j.Status != store.StatusSucceeded {
		t.Fatalf("status %s %q", j.Status, j.Error)
	}
	if !slices.Equal(j.Skills, []string{"reviewing"}) || len(j.SkillsDelivered) != 0 || j.SkillsNote != SkillsCodexNote {
		t.Fatalf("codex skills %v delivered %v note %q", j.Skills, j.SkillsDelivered, j.SkillsNote)
	}
	if _, err := os.Lstat(filepath.Join(m.JobDir(j.ID), "work", ".claude")); !os.IsNotExist(err) {
		t.Fatal("codex job got .claude")
	}
}

func TestSkillGuardRefusesCatalogWrites(t *testing.T) {
	m, root := skillsManager(t)
	j := &store.Job{ID: "x", Kind: "fix", SkillsDelivered: []string{"golang"}}
	work := filepath.Join(m.JobDir(j.ID), "work")
	if err := os.MkdirAll(filepath.Join(work, ".claude", "skills"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "skills", "golang"), filepath.Join(work, ".claude", "skills", "golang")); err != nil {
		t.Fatal(err)
	}
	cfg := guard.Config{Dir: work, Protected: append(m.ProtectedPaths(), m.skillProtected(j)...)}
	for _, cmd := range []string{
		"echo x > .claude/skills/golang/SKILL.md",
		"echo x > " + filepath.Join(root, "index.json"),
		"rm -r .claude/skills/golang/",
		"cp /etc/hostname " + filepath.Join(root, "skills", "golang") + "/",
	} {
		if r := guard.Check(cmd, cfg); !strings.Contains(r, "skill catalog") {
			t.Errorf("%q not refused: %q", cmd, r)
		}
	}
	if r := guard.CheckFileWrite("Edit", ".claude/skills/golang/SKILL.md", cfg); r == "" {
		t.Error("Edit through the link allowed")
	}
	if r := guard.Check("cat .claude/skills/golang/SKILL.md", cfg); r != "" {
		t.Errorf("reading a skill refused: %s", r)
	}
}

func TestSkillListAndRatings(t *testing.T) {
	m, _ := skillsManager(t)
	now := time.Now()
	fin := now.Add(-time.Hour)
	for i, r := range []*store.JobStats{
		{ID: "a", Kind: "fix", Status: store.StatusSucceeded, CostUSD: 1, Repo: "r1", Skills: map[string]int{"golang": 2}, SkillsDelivered: []string{"golang"}},
		{ID: "b", Kind: "fix", Status: store.StatusFailed, CostUSD: 3, Repo: "r2", Skills: map[string]int{"golang": 1}, SkillsDelivered: []string{"golang", "git-workflows"}},
		{ID: "c", Kind: "fix", Status: store.StatusSucceeded, CostUSD: 2, Repo: "r1", Skills: map[string]int{"golang": 1}},
	} {
		r.CreatedAt, r.FinishedAt, r.RecordedAt = fin, &fin, fin
		r.ToolCalls, r.GuardRules = map[string]int{}, map[string]int{}
		_ = i
		if err := m.Store.PutStats(r); err != nil {
			t.Fatal(err)
		}
	}
	out, err := m.SkillList(context.Background(), "", skills.Filter{Kind: "fix"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range out.Skills {
		names = append(names, s.Name)
		if s.Name == "golang" && (s.Uses != 4 || s.Jobs != 3 || s.SuccessRate != 0.667 || s.Stale) {
			t.Fatalf("golang item %+v", s)
		}
	}
	if !slices.Equal(names, []string{"git-workflows", "golang"}) {
		t.Fatalf("list fix %v", names)
	}
	found, err := m.SkillList(context.Background(), "golang", skills.Filter{})
	if err != nil || len(found.Skills) == 0 || found.Skills[0].Name != "golang" || found.Skills[0].Score <= 0 {
		t.Fatalf("search %+v %v", found, err)
	}
	rep, err := m.SkillRatings(context.Background(), now.Add(-30*24*time.Hour), "", 100)
	if err != nil {
		t.Fatal(err)
	}
	g := rep.Skills[0]
	if g.Name != "golang" || g.Used != 3 || g.Delivered != 2 || g.MedianCostUSD != 2 || g.SuccessRate != 0.667 || g.Status != "core" {
		t.Fatalf("rating %+v", g)
	}
	if len(g.ByRepo) != 2 || g.ByRepo[0] != (SkillRepoUse{Repo: "r1", Used: 2, SuccessRate: 1}) ||
		g.ByRepo[1] != (SkillRepoUse{Repo: "r2", Used: 1, SuccessRate: 0}) {
		t.Fatalf("by repo %+v", g.ByRepo)
	}
	if rep.Skills[1].Name != "git-workflows" || rep.Skills[1].Delivered != 1 || rep.Skills[1].Used != 0 {
		t.Fatalf("second %+v", rep.Skills[1])
	}
	r2, _ := m.SkillRatings(context.Background(), now.Add(-30*24*time.Hour), "r2", 100)
	if r2.Skills[0].Name != "golang" || r2.Skills[0].Used != 1 {
		t.Fatalf("repo filter %+v", r2.Skills)
	}
}

func TestSkillStale(t *testing.T) {
	now := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	recent := now.Add(-10 * 24 * time.Hour)
	old := now.Add(-70 * 24 * time.Hour)
	for _, tc := range []struct {
		verified string
		used     *time.Time
		want     bool
	}{
		{"2026-10-01", &recent, false},
		{"2026-10-01", nil, false},
		{"2026-06-01", &recent, true}, // not verified for 90 days
		{"2026-10-01", &old, true},    // not used for 60 days
		{"2026-08-01", nil, true},     // never used, verified 78 days ago
		{"", &recent, true},
	} {
		if got := SkillStale(tc.verified, tc.used, now); got != tc.want {
			t.Errorf("verified %q used %v: stale %t, want %t", tc.verified, tc.used, got, tc.want)
		}
	}
}

func TestSkillWarningsForBadRoots(t *testing.T) {
	m := newManager(t)
	m.Cfg.SkillRoots = []string{filepath.Join(t.TempDir(), "missing"), m.StateDir}
	w := strings.Join(m.SkillWarnings(), "\n")
	if !strings.Contains(w, "missing") || !strings.Contains(w, "agent-jobs state") {
		t.Fatalf("warnings %q", w)
	}
	// Not fatal: a job still starts without skills.
	j := enqueue(t, m, "x", nil)
	if len(j.Skills) != 0 {
		t.Fatalf("skills %v", j.Skills)
	}
}

func TestSkillsConfigLimits(t *testing.T) {
	dir := t.TempDir()
	for body, want := range map[string]string{
		`{"skills_max_per_job": 9}`: "skills_max_per_job",
		`{"skills_max_kb": -1}`:     "skills_max_kb",
	} {
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(dir); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", body, err)
		}
	}
	b := Config{}.SkillBudget()
	if b.MaxCount != 6 || b.MaxKB != 400 {
		t.Fatalf("defaults %+v", b)
	}
}

func TestScheduleCarriesSkills(t *testing.T) {
	m, _ := skillsManager(t)
	if _, err := m.AddSchedule(ScheduleRequest{Cron: "* * * * *", Prompt: "p", Kind: "fix", Model: "sonnet", Skills: []string{"nope"}}); err == nil {
		t.Fatal("unknown skill accepted on a schedule")
	}
	sc, err := m.AddSchedule(ScheduleRequest{Cron: "* * * * *", Prompt: "p", Kind: "fix", Model: "sonnet", Skills: []string{"+extra", "-golang"}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Store.GetSchedule(sc.ID)
	if err != nil || !slices.Equal(got.Skills, []string{"+extra", "-golang"}) {
		t.Fatalf("stored %v %v", got, err)
	}
}

// A stop removes the links (the resume check would refuse a changed .claude otherwise); the resumed
// attempt gets them again.
func TestSkillsRelinkedOnResume(t *testing.T) {
	m, _ := skillsManager(t)
	t.Setenv("FAKE_SESSIONS", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", os.Getenv("FAKE_SESSIONS"))
	repo := newRepo(t)
	st := startAndStop(t, m, func(r *StartRequest) { r.Kind, r.RepoPath = "fix", repo })
	link := filepath.Join(m.JobDir(st.ID), "work", ".claude", "skills", "golang")
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatal("stop left the skill link")
	}
	if _, err := m.Resume(st.ID, "go on FAKE_SLEEP=2", Limits{MaxBudgetUSD: 4}); err != nil {
		t.Fatal(err)
	}
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("resumed attempt has no link: %v", err)
	}
	waitFor(t, "exit_code", 15*time.Second, func() bool {
		_, ok := readExitCode(m.JobDir(st.ID))
		return ok
	})
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if j := get(t, m, st.ID); j.Status != store.StatusSucceeded || j.Attempt != 2 {
		t.Fatalf("after resume %s %q", j.Status, j.Error)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatal("link left after the resumed attempt")
	}
}

// Jobs of one repository update the shared info/exclude concurrently: every block survives.
func TestSkillsExcludeConcurrentUpdates(t *testing.T) {
	repo := newRepo(t)
	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("job-%02d", i)
			errs <- updateExclude(repo, id, []string{"/" + skills.LinkDir + "/" + id})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(filepath.Join(repo, ".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		id := fmt.Sprintf("job-%02d", i)
		if !strings.Contains(string(b), skillExcludeBegin(id)+"\n/.claude/skills/"+id+"\n"+skillExcludeEnd(id)+"\n") {
			t.Fatalf("block of %s lost:\n%s", id, b)
		}
	}
	// Removal under the same lock keeps the other blocks.
	var wg2 sync.WaitGroup
	for i := 0; i < n; i += 2 {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			if err := updateExclude(repo, fmt.Sprintf("job-%02d", i), nil); err != nil {
				t.Error(err)
			}
		}()
	}
	wg2.Wait()
	b, _ = os.ReadFile(filepath.Join(repo, ".git", "info", "exclude"))
	for i := range n {
		if has := strings.Contains(string(b), skillExcludeBegin(fmt.Sprintf("job-%02d", i))); has != (i%2 == 1) {
			t.Fatalf("job-%02d block present=%v after removals:\n%s", i, has, b)
		}
	}
}

// A job that forces its skill link into a commit or the index is refused: its result would carry a
// symlink into the catalog.
func TestSkillLinkInCommitOrIndexRefused(t *testing.T) {
	for _, brief := range []string{"FAKE_LINK_COMMIT", "FAKE_LINK_STAGE"} {
		t.Run(brief, func(t *testing.T) {
			m, _ := skillsManager(t)
			repo := newRepo(t)
			j := runToEnd(t, m, enqueue(t, m, brief, func(r *StartRequest) { r.Kind, r.RepoPath = "fix", repo }).ID)
			if j.Status != store.StatusFailed || !strings.Contains(j.Error, ".claude/skills/golang") {
				t.Fatalf("status %s %q", j.Status, j.Error)
			}
		})
	}
}

// The guard refuses git add -f naming a skill link (an argument that is a protected path); a
// whole-tree add (git add -f -A, git add -f .) names no link and passes, which the check at the end
// of the attempt covers (TestSkillLinkInCommitOrIndexRefused).
func TestSkillGuardAndForcedAdd(t *testing.T) {
	m, root := skillsManager(t)
	j := &store.Job{ID: "x", Kind: "fix", SkillsDelivered: []string{"golang"}}
	work := filepath.Join(m.JobDir(j.ID), "work")
	if err := os.MkdirAll(filepath.Join(work, ".claude", "skills"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "skills", "golang"), filepath.Join(work, ".claude", "skills", "golang")); err != nil {
		t.Fatal(err)
	}
	cfg := guard.Config{Dir: work, Protected: append(m.ProtectedPaths(), m.skillProtected(j)...)}
	for _, cmd := range []string{"git add -f .claude/skills/golang", "git add --force -- .claude/skills/golang"} {
		if r := guard.Check(cmd, cfg); !strings.Contains(r, "skill catalog") {
			t.Errorf("%q not refused: %q", cmd, r)
		}
	}
	for _, cmd := range []string{"git add -f -A", "git add -f ."} {
		if r := guard.Check(cmd, cfg); r != "" {
			t.Errorf("%q refused (%q): update the docs, the end check is no longer the only cover", cmd, r)
		}
	}
}

// A link that cannot be removed (non-writable .claude/skills) stays tracked: its info/exclude line
// stays, the resumed attempt adopts it as delivered instead of taking it for the repository's own,
// and only once a later cleanup really removes it does the exclude block go.
func TestSkillLinkUnlinkFailureTrackedAcrossResume(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	m, _ := skillsManager(t)
	t.Setenv("FAKE_SESSIONS", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", os.Getenv("FAKE_SESSIONS"))
	repo := newRepo(t)
	j := enqueue(t, m, "FAKE_WIP FAKE_SLEEP_ONCE=30", func(r *StartRequest) { r.Kind, r.RepoPath = "fix", repo })
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(m.JobDir(j.ID), "work")
	waitFor(t, "work in progress", 15*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(work, "wip.txt"))
		return err == nil
	})
	skillsDir := filepath.Join(work, ".claude", "skills")
	if err := os.Chmod(skillsDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(skillsDir, 0o755) })
	if _, err := m.Stop(j.ID, "plan changed"); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(skillsDir, "golang")
	exclude := filepath.Join(repo, ".git", "info", "exclude")
	tracked := func(when string) {
		t.Helper()
		if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s: link gone (%v)", when, err)
		}
		if b, _ := os.ReadFile(exclude); !strings.Contains(string(b), skillExcludeBegin(j.ID)+"\n") ||
			!strings.Contains(string(b), "\n/.claude/skills/golang\n") {
			t.Fatalf("%s: exclude block dropped while the link is there:\n%s", when, b)
		}
		if out := gitT(t, work, "status", "--porcelain", "--untracked-files=all"); strings.Contains(out, ".claude") {
			t.Fatalf("%s: git sees the link: %q", when, out)
		}
	}
	tracked("after the stop")
	if _, err := m.Resume(j.ID, "go on FAKE_SLEEP=1", Limits{MaxBudgetUSD: 4}); err != nil {
		t.Fatal(err)
	}
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if r := get(t, m, j.ID); r.Status != store.StatusRunning || !slices.Contains(r.SkillsDelivered, "golang") ||
		strings.Contains(r.SkillsNote, "golang") {
		t.Fatalf("resumed: %s %q delivered %v note %q", r.Status, r.Error, r.SkillsDelivered, r.SkillsNote)
	}
	tracked("while resumed")
	waitFor(t, "exit_code", 15*time.Second, func() bool {
		_, ok := readExitCode(m.JobDir(j.ID))
		return ok
	})
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	r := get(t, m, j.ID)
	if r.Status != store.StatusSucceeded {
		t.Fatalf("after resume %s %q", r.Status, r.Error)
	}
	tracked("after the resumed attempt")
	// Writable again: the next cleanup removes the link, then the block.
	if err := os.Chmod(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.removeSkills(r); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatal("link left after a cleanup that could remove it")
	}
	if b, _ := os.ReadFile(exclude); strings.Contains(string(b), j.ID) {
		t.Fatalf("exclude block kept after the link went:\n%s", b)
	}
}

// .claude unreadable when the attempt ends: removal cannot even inspect the links, so they stay
// tracked (exclude block kept); once readable again a resume adopts the leftover link, also when its
// exclude line went missing in between (git then shows it untracked).
func TestSkillLinkUnreadableParentAdoptedOnResume(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	m, _ := skillsManager(t)
	t.Setenv("FAKE_SESSIONS", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", os.Getenv("FAKE_SESSIONS"))
	repo := newRepo(t)
	j := enqueue(t, m, "FAKE_WIP FAKE_SLEEP_ONCE=30", func(r *StartRequest) { r.Kind, r.RepoPath = "fix", repo })
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(m.JobDir(j.ID), "work")
	waitFor(t, "work in progress", 15*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(work, "wip.txt"))
		return err == nil
	})
	claudeDir := filepath.Join(work, ".claude")
	if err := os.Chmod(claudeDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(claudeDir, 0o755) })
	if _, err := m.Stop(j.ID, "plan changed"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(claudeDir, "skills", "golang")
	exclude := filepath.Join(repo, ".git", "info", "exclude")
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link gone (%v)", err)
	}
	if b, _ := os.ReadFile(exclude); !strings.Contains(string(b), "\n/.claude/skills/golang\n") {
		t.Fatalf("exclude block dropped while the link is there:\n%s", b)
	}
	if r := get(t, m, j.ID); !slices.Contains(r.SkillsDelivered, "golang") {
		t.Fatalf("link no longer tracked: %v", r.SkillsDelivered)
	}
	// The exclude line lost meanwhile: git now shows the leftover link as untracked.
	if err := updateExclude(repo, j.ID, nil); err != nil {
		t.Fatal(err)
	}
	if out := gitT(t, work, "status", "--porcelain", "--untracked-files=all"); !strings.Contains(out, "?? .claude/skills/golang") {
		t.Fatalf("status %q", out)
	}
	if _, err := m.Resume(j.ID, "go on FAKE_SLEEP=1", Limits{MaxBudgetUSD: 4}); err != nil {
		t.Fatal(err)
	}
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if r := get(t, m, j.ID); r.Status != store.StatusRunning || !slices.Contains(r.SkillsDelivered, "golang") ||
		strings.Contains(r.SkillsNote, "golang") {
		t.Fatalf("resumed: %s %q delivered %v note %q", r.Status, r.Error, r.SkillsDelivered, r.SkillsNote)
	}
	waitFor(t, "exit_code", 15*time.Second, func() bool {
		_, ok := readExitCode(m.JobDir(j.ID))
		return ok
	})
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if r := get(t, m, j.ID); r.Status != store.StatusSucceeded {
		t.Fatalf("after resume %s %q", r.Status, r.Error)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatal("link left after the resumed attempt")
	}
	if b, _ := os.ReadFile(exclude); strings.Contains(string(b), j.ID) {
		t.Fatalf("exclude block kept after the link went:\n%s", b)
	}
}
