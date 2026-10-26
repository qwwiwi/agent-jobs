package jobs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

func TestSourceURLs(t *testing.T) {
	t.Run("[G02] only plain https git URLs are taken", func(t *testing.T) {
		for _, u := range []string{
			"http://github.com/a/b",
			"ssh://git@github.com/a/b.git",
			"git@github.com:a/b.git",
			"git://github.com/a/b",
			"file:///tmp/repo",
			"/home/openclaw/repo",
			"./repo",
			"https://user:pass@github.com/a/b.git",
			"https://token@github.com/a/b.git",
			"https://github.com/a/b?ref=main",
			"https://github.com/a/b#main",
			"https://github.com:8443/a/b",
			"https://127.0.0.1/a/b",
			"https://[::1]/a/b",
			"https://localhost/a/b",
			"https://github.com",
			"https://github.com/",
			"https://github.com/a/../b",
			"https://github.com/a/%2e%2e/b",
			"https://github.com/a/b c",
			"ext::sh -c touch% /tmp/x",
			"HTTPS://github.com/a/b",
			"https://github.com/a/b\n",
			"",
		} {
			if _, err := ValidateSourceURL(u); err == nil {
				t.Errorf("%q: accepted", u)
			}
		}
		for _, u := range []string{"https://github.com/qwwiwi/agent-jobs", "https://github.com/qwwiwi/agent-jobs.git",
			"https://gitlab.example.org/group/sub/project.git"} {
			if _, err := ValidateSourceURL(u); err != nil {
				t.Errorf("%q: %v", u, err)
			}
		}
	})
	t.Run("[G02] the folder name is the last path segment, sanitized", func(t *testing.T) {
		for u, want := range map[string]string{
			"https://github.com/qwwiwi/agent-jobs":           "agent-jobs",
			"https://github.com/qwwiwi/agent-jobs.git":       "agent-jobs",
			"https://gitlab.example.org/group/sub/proj.git/": "proj",
			"https://github.com/a/.hidden":                   "hidden",
			"https://github.com/a/name_with.dots":            "name_with.dots",
		} {
			if got := SourceName(u); got != want {
				t.Errorf("%s: name %q, want %q", u, got, want)
			}
		}
		if _, err := ValidateSources([]string{"https://github.com/a/lib", "https://github.com/b/lib.git"}); err == nil ||
			!strings.Contains(err.Error(), "lib") {
			t.Errorf("two sources with one folder name: %v", err)
		}
		if _, err := ValidateSources(make([]string, MaxJobSources+1)); err == nil {
			t.Error("too many sources accepted")
		}
	})
}

// bareRepo makes a bare repository with one commit holding a file, an executable script, a symlink to a
// host file and a .gitattributes with a filter, and returns its path.
func bareRepo(t *testing.T, big int) string {
	t.Helper()
	src := t.TempDir()
	gitT(t, src, "init", "-q", "-b", "main")
	files := map[string]string{"README.md": "hello\n", "install.sh": "#!/bin/sh\necho pwned > /tmp/pwned\n",
		".gitattributes": "*.md filter=evil\n"}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if big > 0 {
		b := make([]byte, big)
		for i := range b {
			b[i] = byte(i*7919 + i/13)
		}
		if err := os.WriteFile(filepath.Join(src, "blob.bin"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(src, "passwd-link")); err != nil {
		t.Fatal(err)
	}
	gitT(t, src, "add", "-A")
	gitT(t, src, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "one")
	bare := filepath.Join(t.TempDir(), "lib.git")
	gitT(t, t.TempDir(), "clone", "-q", "--bare", src, bare)
	return bare
}

// withLocalRemote maps https URLs to local bare repositories for the test (the file transport is allowed
// only here); production stays https only.
func withLocalRemote(t *testing.T, remotes map[string]string) {
	t.Helper()
	old := sourceRemote
	sourceRemote = func(u string) (string, string) {
		if p, ok := remotes[u]; ok {
			return "file://" + p, "file"
		}
		return u, "https"
	}
	t.Cleanup(func() { sourceRemote = old })
}

func TestSourcesCloneIntoJobDir(t *testing.T) {
	m := newManager(t)
	// The clones are read-only: the job folders are removed the way prune does before TempDir's cleanup.
	t.Cleanup(func() { _ = removeTree(filepath.Join(m.StateDir, "jobs")) })
	bare := bareRepo(t, 0)
	const url = "https://git.example.test/org/lib.git"
	withLocalRemote(t, map[string]string{url: bare, "https://git.example.test/org/big.git": bareRepo(t, 3<<20)})
	t.Run("[G02] the daemon clones each source read-only under sources/<name> before the job starts", func(t *testing.T) {
		j := enqueue(t, m, "look at lib", func(r *StartRequest) {
			r.Kind = "research"
			r.Sources = []string{url, "https://git.example.test/org/missing.git"}
		})
		if !slices.Equal(get(t, m, j.ID).Sources, []string{url, "https://git.example.test/org/missing.git"}) {
			t.Fatalf("sources not stored: %+v", get(t, m, j.ID).Sources)
		}
		end := runToEnd(t, m, j.ID)
		if end.Status != store.StatusSucceeded && end.Status != store.StatusFailed {
			t.Fatalf("job did not run: %s %s", end.Status, end.Error)
		}
		dir := filepath.Join(m.JobDir(j.ID), SourcesDir, "lib")
		b, err := os.ReadFile(filepath.Join(dir, "README.md"))
		if err != nil || string(b) != "hello\n" {
			t.Fatalf("README: %q %v", b, err)
		}
		// No checkout filter ran, the symlink is a plain file, no hooks, read-only.
		if fi, err := os.Lstat(filepath.Join(dir, "passwd-link")); err != nil || fi.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("symlink checked out as a link: %v %v", fi, err)
		}
		if ents, _ := os.ReadDir(filepath.Join(dir, ".git", "hooks")); len(ents) != 0 {
			t.Fatalf("hooks in the clone: %v", ents)
		}
		for _, p := range []string{dir, filepath.Join(dir, "install.sh"), filepath.Join(dir, "README.md")} {
			fi, err := os.Stat(p)
			if err != nil || fi.Mode().Perm()&0o222 != 0 {
				t.Fatalf("%s writable: %v %v", p, fi.Mode(), err)
			}
		}
		if fi, _ := os.Stat(filepath.Join(dir, "install.sh")); fi.Mode().Perm() != 0o555 {
			t.Fatalf("install.sh: mode %v, want r-xr-xr-x (no write bits, exec bit kept for the sandbox copy)", fi.Mode())
		}
		if out, err := git(dir, "rev-list", "--count", "HEAD"); err != nil || out != "1" {
			t.Fatalf("not a shallow clone of one commit: %q %v", out, err)
		}
		prompt, _ := os.ReadFile(filepath.Join(m.JobDir(j.ID), "prompt.md"))
		for _, want := range []string{"## Sources", url + " → " + dir, "missing.git", "not cloned",
			"do not study their code through the GitHub API or website", "docker run --rm --name aj-" + j.ID + "-<suffix> --memory 2g --pids-limit 512 --cpus 2 --network none --user"} {
			if !strings.Contains(string(prompt), want) {
				t.Errorf("prompt lacks %q:\n%s", want, prompt)
			}
		}
		// The guard is told the sources and the sandbox; docker is no longer denied by prefix for this job.
		s, _ := os.ReadFile(filepath.Join(m.JobDir(j.ID), "settings.json"))
		var set struct {
			Permissions struct{ Deny []string } `json:"permissions"`
		}
		if err := json.Unmarshal(s, &set); err != nil {
			t.Fatal(err)
		}
		if slices.Contains(set.Permissions.Deny, "Bash(docker:*)") || !strings.Contains(string(s), "--sources") ||
			!strings.Contains(string(s), "--sandbox") || !slices.Contains(set.Permissions.Deny, "Write(/"+filepath.Join(m.JobDir(j.ID), SourcesDir)+"/**)") {
			t.Fatalf("settings: %s", s)
		}
	})
	t.Run("[G02] a clone past the size cap is aborted and the job told", func(t *testing.T) {
		m.Cfg.SourcesMaxMB = 1
		j := enqueue(t, m, "look at big", func(r *StartRequest) {
			r.Kind = "research"
			r.Sources = []string{"https://git.example.test/org/big.git"}
		})
		runToEnd(t, m, j.ID)
		if _, err := os.Stat(filepath.Join(m.JobDir(j.ID), SourcesDir, "big")); !os.IsNotExist(err) {
			t.Fatalf("oversized clone left behind: %v", err)
		}
		prompt, _ := os.ReadFile(filepath.Join(m.JobDir(j.ID), "prompt.md"))
		if !strings.Contains(string(prompt), "size cap") {
			t.Fatalf("prompt does not say the clone hit the cap:\n%s", prompt)
		}
	})
	t.Run("[G02] the production path refuses a non-https source", func(t *testing.T) {
		r := StartRequest{Title: "x", Kind: "research", Brief: "x", Model: "sonnet", Sources: []string{"file://" + bare}}
		if _, err := m.Enqueue(r); err == nil || !strings.Contains(err.Error(), "https") {
			t.Fatalf("file source accepted: %v", err)
		}
		sourceRemote = defaultSourceRemote
		res := cloneSource(context.Background(), "file://"+bare, filepath.Join(t.TempDir(), "x"), 1<<20, t.TempDir())
		if res.Err == "" {
			t.Fatal("the production remote cloned a file:// URL")
		}
	})
}

func TestMilestoneTaskSources(t *testing.T) {
	m := newManager(t)
	ctx := context.Background()
	t.Run("[G02] sources are planned per task and shown in the tree; a task job inherits them", func(t *testing.T) {
		v, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "S", Tasks: []MilestoneTaskInput{
			{Key: "T1", Title: "study", Sources: deps("https://github.com/a/lib")}, {Key: "T2", Title: "other"},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if got := taskOf(t, v, "T1").Sources; !slices.Equal(got, []string{"https://github.com/a/lib"}) {
			t.Fatalf("tree sources: %v", got)
		}
		if got := taskOf(t, v, "T2").Sources; got == nil || len(got) != 0 {
			t.Fatalf("T2 sources: %#v, want []", got)
		}
		b, _ := json.Marshal(taskOf(t, v, "T2"))
		if !strings.Contains(string(b), `"sources":[]`) {
			t.Fatalf("tree JSON: %s", b)
		}
		if _, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "S", Tasks: []MilestoneTaskInput{
			{Key: "T1", Title: "x", Sources: deps("git@github.com:a/b.git")}}}); err == nil {
			t.Fatal("ssh source planned")
		}
		// An edit that does not name sources keeps them; an empty list clears them.
		v, err = m.SetMilestone(ctx, MilestoneSet{ID: v.ID, Tasks: []MilestoneTaskInput{{Key: "T1", Title: "study more"}}})
		if err != nil || !slices.Equal(taskOf(t, v, "T1").Sources, []string{"https://github.com/a/lib"}) {
			t.Fatalf("title edit lost sources: %v %+v", err, v)
		}
		j := enqueue(t, m, "x", func(r *StartRequest) { r.Milestone, r.Task, r.Kind = v.ID, "T1", "research" })
		if !slices.Equal(j.Sources, []string{"https://github.com/a/lib"}) {
			t.Fatalf("job on T1 sources: %v", j.Sources)
		}
		j = enqueue(t, m, "x", func(r *StartRequest) {
			r.Milestone, r.Task, r.Kind, r.Sources = v.ID, "T1", "research", []string{}
		})
		if len(j.Sources) != 0 {
			t.Fatalf("explicit empty sources: %v", j.Sources)
		}
		v, err = m.SetMilestone(ctx, MilestoneSet{ID: v.ID, Tasks: []MilestoneTaskInput{{Key: "T1", Sources: deps()}}})
		if err != nil || len(taskOf(t, v, "T1").Sources) != 0 {
			t.Fatalf("clear: %v %+v", err, taskOf(t, v, "T1"))
		}
	})
}
