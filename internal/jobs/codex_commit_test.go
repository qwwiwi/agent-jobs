package jobs

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// newCodexWriteManager is a daemon-locked codex manager whose config.json (body) is also its active
// config, as a daemon started with that file has it.
func newCodexWriteManager(t *testing.T, body string) *Manager {
	t.Helper()
	m := newCodexManager(t)
	writeReloadConfig(t, m, body)
	cfg, err := LoadConfig(m.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	off := 0
	cfg.CodexJobsPerDay = &off
	m.Cfg = cfg
	unlock, err := m.LockDaemon()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)
	m.WatchDaemonConfig()
	return m
}

// codexFix starts a codex fix job in repo.
func codexFix(repo string) func(*StartRequest) {
	return func(r *StartRequest) { r.Kind, r.Model, r.RepoPath = "fix", "gpt-6.1-sol", repo }
}

// jobCommits are the commits of job j past its base, newest first ("<subject>\x1f<author email>" each),
// read from its worktree or, once cleanup removed that, from refs/agent-jobs/jobs/<id>.
func jobCommits(t *testing.T, repo string, m *Manager, j *store.Job) []string {
	t.Helper()
	dir, rev := filepath.Join(m.JobDir(j.ID), "work"), "HEAD"
	if _, err := os.Stat(dir); err != nil {
		dir, rev = repo, "refs/agent-jobs/jobs/"+j.ID
	}
	out := gitT(t, dir, "log", "--format=%s%x1f%ae", j.BaseCommit+".."+rev)
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// With network (codex_write_network true) a codex write job's commands can reach it (issue #92: `npm ci`
// failed with EAI_AGAIN): workspace-write gets CodexNetworkOverride, read-only and full-access never;
// network false drops it.
func TestCodexArgsWriteNetwork(t *testing.T) {
	jobDir := filepath.Join(t.TempDir(), "jobs", "j1")
	for _, c := range []struct {
		kind, sandbox string
		network, want bool
	}{
		{"fix", CodexSandboxWrite, true, true},
		{"general", CodexSandboxWrite, true, true},
		{"fix", CodexSandboxWrite, false, false},
		{"review", CodexSandboxReadOnly, true, false},
		{"research", CodexSandboxReadOnly, true, false},
		{"fix", CodexSandboxFullAccess, true, false},
	} {
		args := CodexArgs(&store.Job{Kind: c.kind, Model: "gpt-6.1-sol"}, "high", c.sandbox, jobDir, c.network)
		i := slices.Index(args, CodexNetworkOverride)
		if got := i > 0 && args[i-1] == "-c"; got != c.want {
			t.Fatalf("%s/%s network=%v: override %v, want %v (argv %q)", c.kind, c.sandbox, c.network, got, c.want, args)
		}
		if i >= 0 && i > slices.Index(args, "-") {
			t.Fatalf("override after the prompt: %q", args)
		}
		ca, err := codexArgvSandbox(append([]string{"codex"}, args...))
		if err != nil || ca.network != c.want || ca.sandbox != c.sandbox {
			t.Fatalf("%s/%s: wrapper reads %+v, %v", c.kind, c.sandbox, ca, err)
		}
	}
}

// codex_write_network: absent = off (an owner opt-in: with network the job can reach anything), true = on,
// false = off; at launch a false or absent key on disk revokes it from an active config that has it on
// (like codex_write_allowed), and a true on disk does not grant it to one that has it off or absent.
func TestCodexWriteNetworkConfig(t *testing.T) {
	if (Config{}).CodexWriteNetworkOn() {
		t.Fatal("codex_write_network absent is on")
	}
	for body, want := range map[string]bool{`{}`: false, `{"codex_write_network":true}`: true, `{"codex_write_network":false}`: false} {
		c, err := LoadConfig(writeConfigDir(t, body))
		if err != nil || c.CodexWriteNetworkOn() != want {
			t.Fatalf("%s: on=%v err=%v, want %v", body, c.CodexWriteNetworkOn(), err, want)
		}
	}
	m := newCodexManager(t)
	off, on := false, true
	for _, c := range []struct {
		active *bool
		disk   string
		want   bool
	}{
		{nil, `{}`, false},
		{nil, `{"codex_write_network":true}`, false},
		{&off, `{"codex_write_network":true}`, false},
		{&on, `{}`, false},
		{&on, `{"codex_write_network":false}`, false},
		{&on, `{"codex_write_network":true}`, true},
	} {
		m.Cfg.CodexWriteNetwork = c.active
		writeReloadConfig(t, m, c.disk)
		cfg, err := m.authorizedLaunchConfig()
		if err != nil || cfg.CodexWriteNetworkOn() != c.want {
			t.Fatalf("active %v, disk %s: on=%v err=%v, want %v", c.active, c.disk, cfg.CodexWriteNetworkOn(), err, c.want)
		}
	}
}

// The daemon launches a codex fix job without the network override unless codex_write_network is
// explicitly true: an absent key and false both keep it offline, and a review job never gets it.
func TestCodexWriteJobNetworkLaunch(t *testing.T) {
	fix := func(r *StartRequest) { r.Kind, r.Model = "fix", "gpt-6.1-sol" }
	for _, c := range []struct {
		name, config string
		start        func(*StartRequest)
		want         bool
	}{
		{"fix absent key", `{"codex_write_allowed":true}`, fix, false},
		{"fix network true", `{"codex_write_allowed":true,"codex_write_network":true}`, fix, true},
		{"fix network false", `{"codex_write_allowed":true,"codex_write_network":false}`, fix, false},
		{"review", `{"codex_write_allowed":true,"codex_write_network":true}`, codexReview, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := newCodexWriteManager(t, c.config)
			j := runToEnd(t, m, enqueue(t, m, "work", c.start).ID)
			if j.Status != store.StatusSucceeded {
				t.Fatalf("%s %q", j.Status, j.Error)
			}
			argv, err := os.ReadFile(filepath.Join(m.JobDir(j.ID), "argv.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(argv), "-c\n"+CodexNetworkOverride+"\n"); got != c.want {
				t.Fatalf("network override %v, want %v:\n%s", got, c.want, argv)
			}
		})
	}
}

// The wrapper admits the network override only as the daemon builds it: once, exactly, on a write job's
// workspace-write sandbox whose launch recorded network. Any other sandbox key stays refused.
func TestWrapperChecksCodexNetwork(t *testing.T) {
	for _, c := range []struct {
		name    string
		kind    string
		network bool     // recorded in the launch authorization
		extra   []string // replaces the daemon's network override
		ok      bool
	}{
		{"daemon argv", "fix", true, nil, true},
		{"offline daemon argv", "fix", false, []string{}, true},
		{"not recorded", "fix", false, []string{"-c", CodexNetworkOverride}, false},
		{"twice", "fix", true, []string{"-c", CodexNetworkOverride, "-c", CodexNetworkOverride}, false},
		{"attached", "fix", true, []string{"--config=" + CodexNetworkOverride}, true},
		{"false value", "fix", true, []string{"-c", "sandbox_workspace_write.network_access=false"}, false},
		{"quoted key", "fix", true, []string{"-c", `"sandbox_workspace_write".network_access=true`}, false},
		{"writable roots", "fix", true, []string{"-c", `sandbox_workspace_write.writable_roots=["/"]`}, false},
		{"review", "review", true, []string{"-c", CodexNetworkOverride}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := newCodexManager(t)
			m.Cfg.CodexWriteAllowed = true
			j := enqueue(t, m, "work", func(r *StartRequest) { r.Kind, r.Model = c.kind, "gpt-6.1-sol" })
			const token = "current-token"
			if err := m.Store.Claim(j.ID, token, m.Now()); err != nil {
				t.Fatal(err)
			}
			j = get(t, m, j.ID)
			if err := m.Store.SetLaunchAuthorization(j.ID, token, j.Attempt,
				store.LaunchAuthorization{CodexWriteAllowed: true, CodexSandbox: CodexSandbox(c.kind), CodexNetwork: c.network}); err != nil {
				t.Fatal(err)
			}
			dir := m.JobDir(j.ID)
			if err := m.prepare(j, token, dir); err != nil {
				t.Fatal(err)
			}
			args := CodexArgs(j, "high", CodexSandbox(j.Kind), dir, c.network)
			if c.extra != nil {
				var kept []string
				for i := 0; i < len(args); i++ {
					if args[i] == "-c" && args[i+1] == CodexNetworkOverride {
						i++
						continue
					}
					kept = append(kept, args[i])
				}
				args = append(append(kept[:len(kept)-1:len(kept)-1], c.extra...), "-")
			}
			argv := []string{"run-wrapper", dir, "--token", token, "--runner", store.RunnerCodex, "--", m.Cfg.codexBin()}
			cmd := exec.Command(selfBin, append(argv, args...)...)
			cmd.Dir, cmd.Env = filepath.Join(dir, "work"), m.spawnEnv("")
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			out, err := cmd.CombinedOutput()
			_, ranErr := os.Stat(filepath.Join(dir, "argv.txt"))
			if c.ok {
				if err != nil || ranErr != nil {
					t.Fatalf("daemon-built launch refused: %v %v\n%s", err, ranErr, out)
				}
				return
			}
			if err == nil || !strings.Contains(string(out), "was not authorized by the launching daemon") {
				t.Fatalf("wrapper not refused: %v\n%s", err, out)
			}
			if !os.IsNotExist(ranErr) {
				t.Fatalf("codex ran with an unauthorized network override: %v", ranErr)
			}
		})
	}
}

// A codex write job cannot commit inside its sandbox, so the wrapper commits what it left in work/ after
// the run (issue #92): one commit on the base, with the job's message from progress/COMMIT_MSG and the
// job's git identity; without that file a default message. A read-only codex job gets no commit.
func TestCodexWriteJobCommittedByWrapper(t *testing.T) {
	const cfg = `{"codex_write_allowed":true,"git_name":"Job Bot","git_email":"job@example.com"}`
	t.Run("message file", func(t *testing.T) {
		m := newCodexWriteManager(t, cfg)
		repo := newRepo(t)
		j := runToEnd(t, m, enqueue(t, m, "FAKE_CODEX_WRITE FAKE_CODEX_COMMITMSG", codexFix(repo)).ID)
		if j.Status != store.StatusSucceeded {
			t.Fatalf("%s %q", j.Status, j.Error)
		}
		if got := jobCommits(t, repo, m, j); !slices.Equal(got, []string{"Add codex-wrote.txt\x1fjob@example.com"}) {
			t.Fatalf("job commits %q", got)
		}
		log, _ := os.ReadFile(filepath.Join(m.JobDir(j.ID), "stderr.log"))
		if !strings.Contains(string(log), "run-wrapper: codex commit: committed the job's changes as ") {
			t.Fatalf("stderr.log:\n%s", log)
		}
	})
	t.Run("default message", func(t *testing.T) {
		m := newCodexWriteManager(t, cfg)
		repo := newRepo(t)
		j := runToEnd(t, m, enqueue(t, m, "FAKE_CODEX_WRITE", codexFix(repo)).ID)
		if j.Status != store.StatusSucceeded {
			t.Fatalf("%s %q", j.Status, j.Error)
		}
		want := "Codex job " + j.ID + ": changes committed by the wrapper\x1fjob@example.com"
		if got := jobCommits(t, repo, m, j); !slices.Equal(got, []string{want}) {
			t.Fatalf("job commits %q, want %q", got, want)
		}
	})
	t.Run("no changes", func(t *testing.T) {
		m := newCodexWriteManager(t, cfg)
		repo := newRepo(t)
		j := runToEnd(t, m, enqueue(t, m, "nothing to change", codexFix(repo)).ID)
		if got := jobCommits(t, repo, m, j); j.Status != store.StatusSucceeded || got != nil {
			t.Fatalf("%s %q, commits %q", j.Status, j.Error, got)
		}
	})
	t.Run("read-only job", func(t *testing.T) {
		m := newCodexWriteManager(t, cfg)
		repo := newRepo(t)
		j := runToEnd(t, m, enqueue(t, m, "FAKE_CODEX_WRITE", func(r *StartRequest) { codexReview(r); r.RepoPath = repo }).ID)
		if got := jobCommits(t, repo, m, j); got != nil {
			t.Fatalf("read-only codex job got commits %q", got)
		}
		if log, _ := os.ReadFile(filepath.Join(m.JobDir(j.ID), "stderr.log")); strings.Contains(string(log), "codex commit") {
			t.Fatalf("wrapper tried to commit a read-only job:\n%s", log)
		}
	})
	t.Run("filter driver", func(t *testing.T) {
		m := newCodexWriteManager(t, cfg)
		repo := newRepo(t)
		marker := filepath.Join(t.TempDir(), "driver-ran")
		gitT(t, repo, "config", "filter.x.clean", "touch "+marker+"; cat")
		j := runToEnd(t, m, enqueue(t, m, "FAKE_CODEX_WRITE", codexFix(repo)).ID)
		log, _ := os.ReadFile(filepath.Join(m.JobDir(j.ID), "stderr.log"))
		if !strings.Contains(string(log), "codex commit skipped: the repository config names filter/diff drivers (filter.x.clean)") {
			t.Fatalf("stderr.log:\n%s", log)
		}
		if got := jobCommits(t, repo, m, j); got != nil {
			t.Fatalf("commits %q", got)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("a filter driver ran: %v", err)
		}
	})
}

// A nested repository the job lays out in work/ never makes the wrapper's git recurse into it: git runs a
// submodule's status with the submodule's own config and without the command-level overrides, so a filter
// driver there would run outside the sandbox. The wrapper finds the nested .git before any git command and
// commits nothing, both for a new nested repository and for one in a submodule path of the repository.
func TestCodexCommitSkipsNestedRepository(t *testing.T) {
	for _, c := range []struct {
		name string
		sub  bool // the repository has a submodule at sub/ (its worktree has an empty sub/)
	}{
		{"new nested repository", false},
		{"submodule path", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := newCodexManager(t)
			m.Cfg.CodexWriteAllowed = true
			repo := newRepo(t)
			if c.sub {
				subRepo := newRepo(t)
				head := gitT(t, subRepo, "rev-parse", "HEAD")
				gitT(t, repo, "update-index", "--add", "--cacheinfo", "160000,"+head+",sub")
				gitT(t, repo, "commit", "-qm", "add submodule")
			}
			j := enqueue(t, m, "work", codexFix(repo))
			j.BaseCommit = gitT(t, repo, "rev-parse", "HEAD")
			const token = "tok"
			if err := m.Store.Claim(j.ID, token, m.Now()); err != nil {
				t.Fatal(err)
			}
			j = get(t, m, j.ID)
			dir := m.JobDir(j.ID)
			if err := m.prepare(j, token, dir); err != nil {
				t.Fatal(err)
			}
			work := filepath.Join(dir, "work")
			// What the job does in its sandbox: a repository at sub/ whose config names a clean filter for
			// every file, a file in it, and a changed file in the outer worktree.
			nested := filepath.Join(work, "sub")
			if err := os.MkdirAll(nested, 0o755); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "driver-ran")
			if err := os.WriteFile(filepath.Join(nested, "f.txt"), []byte("nested\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"init", "-q"}, {"add", "f.txt"},
				{"-c", "user.name=J", "-c", "user.email=j@example.com", "commit", "-qm", "nested"}} {
				cmd := exec.Command("git", append([]string{"-C", nested}, args...)...)
				cmd.Env = daemonGitEnv()
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v %s", args, err, out)
				}
			}
			// The filter after the commit; f.txt then only stat-dirty, so a status in sub/ cleans it.
			f, err := os.OpenFile(filepath.Join(nested, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.WriteString("[filter \"x\"]\n\tclean = touch " + marker + "; cat\n")
			_ = f.Close()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(nested, ".git", "info", "attributes"), []byte("* filter=x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			later := time.Now().Add(time.Minute)
			if err := os.Chtimes(filepath.Join(nested, "f.txt"), later, later); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(work, "tracked.txt"), []byte("v2\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			line := commitCodexWork(dir, j.ID)
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Errorf("the nested repository's filter ran in the wrapper: %v", err)
			}
			if !strings.Contains(line, "codex commit skipped: the worktree holds a nested git repository (sub/.git)") {
				t.Fatalf("commit line %q", line)
			}
			if got := gitT(t, work, "rev-parse", "HEAD"); got != j.BaseCommit {
				t.Fatalf("HEAD moved to %s", got)
			}
		})
	}
}

// The commit message is the job's file unless it is a link, empty, names an owner or the owner names are
// unknown; control characters are dropped.
func TestCodexCommitMessage(t *testing.T) {
	jobDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(jobDir, CodexProgressDir), 0o700); err != nil {
		t.Fatal(err)
	}
	path := CodexCommitMessagePath(jobDir)
	def := codexCommitMessage(jobDir, "j1", nil, true)
	if !strings.HasPrefix(def, "Codex job j1: changes committed by the wrapper\n") {
		t.Fatalf("default %q", def)
	}
	write := func(s string) {
		_ = os.Remove(path)
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("Fix it\x1b[31m\n\n\tbody\x00\n")
	if got := codexCommitMessage(jobDir, "j1", []string{"Owner"}, true); got != "Fix it[31m\n\n\tbody\n" {
		t.Fatalf("message %q", got)
	}
	if got := codexCommitMessage(jobDir, "j1", nil, false); got != def {
		t.Fatalf("unknown owner names: %q", got)
	}
	write("Fix for OWNER's repo")
	if got := codexCommitMessage(jobDir, "j1", []string{" owner "}, true); got != def {
		t.Fatalf("owner name kept: %q", got)
	}
	write(" \n\t\n")
	if got := codexCommitMessage(jobDir, "j1", nil, true); got != def {
		t.Fatalf("blank message kept: %q", got)
	}
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(path)
	if err := os.Symlink(secret, path); err != nil {
		t.Fatal(err)
	}
	if got := codexCommitMessage(jobDir, "j1", nil, true); got != def {
		t.Fatalf("symlinked message followed: %q", got)
	}
}

// preparedCodexFix is a codex fix job on repo, claimed and prepared (worktree and job git files) as the
// launch leaves it, with m's git identity; it returns the job and its dir.
func preparedCodexFix(t *testing.T, m *Manager, repo string) (*store.Job, string) {
	t.Helper()
	j := enqueue(t, m, "work", codexFix(repo))
	j.BaseCommit = gitT(t, repo, "rev-parse", "HEAD")
	const token = "tok"
	if err := m.Store.Claim(j.ID, token, m.Now()); err != nil {
		t.Fatal(err)
	}
	j = get(t, m, j.ID)
	dir := m.JobDir(j.ID)
	if err := m.prepare(j, token, dir); err != nil {
		t.Fatal(err)
	}
	return j, dir
}

// writeSized writes n bytes to work/name, or with sparse a file of that size and no data.
func writeSized(t *testing.T, work, name string, n int64, sparse bool) {
	t.Helper()
	p := filepath.Join(work, name)
	if sparse {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(p, n); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.WriteFile(p, []byte(strings.Repeat("x", int(n))), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The wrapper stages a codex job's changes into the object database shared with the owner's repository
// only within limits (sol review of #92): too many changed paths, one file too large (by its size, so a
// sparse file counts in full) or too many bytes in all give no commit, a reason in the line, nothing
// staged and no blob of the job's files written. Within the limits the commit is made.
func TestCodexCommitLimits(t *testing.T) {
	for _, c := range []struct {
		name   string
		limits codexCommitLimits
		files  map[string]int64
		sparse bool
		reason string // "" = committed
	}{
		{"within limits", codexCommitLimits{files: 3, fileBytes: 100, totalBytes: 300},
			map[string]int64{"a.txt": 100, "b.txt": 100, "tracked.txt": 100}, false, ""},
		{"file count", codexCommitLimits{files: 3, fileBytes: 100, totalBytes: 1000},
			map[string]int64{"a.txt": 1, "b.txt": 1, "c.txt": 1, "tracked.txt": 1}, false,
			"codex commit skipped: 4 changed paths, over the limit of 3"},
		{"file size", codexCommitLimits{files: 3, fileBytes: 100, totalBytes: 1000},
			map[string]int64{"a.txt": 10, "big.bin": 101}, false,
			"codex commit skipped: big.bin has 101 bytes, over the limit of 100 per file"},
		{"sparse file size", codexCommitMax, map[string]int64{"sparse.bin": codexCommitMax.fileBytes + 1}, true,
			"codex commit skipped: sparse.bin has 20971521 bytes, over the limit of 20971520 per file"},
		{"aggregate size", codexCommitLimits{files: 10, fileBytes: 100, totalBytes: 250},
			map[string]int64{"a.txt": 100, "b.txt": 100, "c.txt": 100}, false,
			"codex commit skipped: the changed files have 300 bytes, over the limit of 250 in all"},
	} {
		t.Run(c.name, func(t *testing.T) {
			prev := codexCommitMax
			codexCommitMax = c.limits
			t.Cleanup(func() { codexCommitMax = prev })
			m := newCodexManager(t)
			m.Cfg.CodexWriteAllowed, m.Cfg.GitName, m.Cfg.GitEmail = true, "Job Bot", "job@example.com"
			repo := newRepo(t)
			j, dir := preparedCodexFix(t, m, repo)
			work := filepath.Join(dir, "work")
			for name, n := range c.files {
				writeSized(t, work, name, n, c.sparse)
			}
			line := commitCodexWork(dir, j.ID)
			head := gitT(t, work, "rev-parse", "HEAD")
			if c.reason == "" {
				if !strings.HasPrefix(line, "codex commit: committed the job's changes as ") || head == j.BaseCommit {
					t.Fatalf("commit line %q, HEAD %s", line, head)
				}
				return
			}
			if line != c.reason {
				t.Fatalf("commit line %q, want %q", line, c.reason)
			}
			if head != j.BaseCommit {
				t.Fatalf("HEAD moved to %s", head)
			}
			if staged := gitT(t, work, "diff", "--cached", "--name-only"); staged != "" {
				t.Fatalf("staged: %q", staged)
			}
			for name := range c.files {
				blob := gitT(t, work, "hash-object", name)
				if _, err := git(work, "cat-file", "-e", blob); err == nil {
					t.Fatalf("blob of %s written to the object database", name)
				}
			}
		})
	}
}

// The commit step has one deadline for all of it: past it, no commit and a line saying so.
func TestCodexCommitDeadline(t *testing.T) {
	prev := codexCommitTimeout
	codexCommitTimeout = time.Nanosecond
	t.Cleanup(func() { codexCommitTimeout = prev })
	m := newCodexManager(t)
	m.Cfg.CodexWriteAllowed, m.Cfg.GitName, m.Cfg.GitEmail = true, "Job Bot", "job@example.com"
	repo := newRepo(t)
	j, dir := preparedCodexFix(t, m, repo)
	work := filepath.Join(dir, "work")
	writeSized(t, work, "a.txt", 10, false)
	if line := commitCodexWork(dir, j.ID); line != "codex commit skipped: the commit step passed its 1ns deadline" {
		t.Fatalf("commit line %q", line)
	}
	if head := gitT(t, work, "rev-parse", "HEAD"); head != j.BaseCommit {
		t.Fatalf("HEAD moved to %s", head)
	}
}

// statusPaths reads every porcelain v2 entry kind, a rename's source included, paths with spaces kept.
func TestCodexStatusPaths(t *testing.T) {
	out := "1 .M N... 100644 100644 100644 aaaa bbbb a dir/file one.txt\x00" +
		"2 R. N... 100644 100644 100644 aaaa bbbb R100 new name.txt\x00old name.txt\x00" +
		"u UU N... 100644 100644 100644 100644 aaaa bbbb cccc conflict.txt\x00" +
		"? untracked file\x00"
	got, err := statusPaths(out)
	want := []string{"a dir/file one.txt", "new name.txt", "old name.txt", "conflict.txt", "untracked file"}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("paths %q, %v; want %q", got, err, want)
	}
	for _, bad := range []string{"x what\x00", "1 .M N...\x00", "2 R. N... 100644 100644 100644 aaaa bbbb R100 new.txt"} {
		if got, err := statusPaths(bad); err == nil {
			t.Fatalf("%q: no error, paths %q", bad, got)
		}
	}
}

// The wrapper commits under the job identity of the launch (<jobdir>/gitconfig, written at prepare from
// git_name / git_email) as author and committer, over a repository-local user.* and whatever config.json
// says by then; without both fields of that identity, or without the file, it commits nothing (sol review
// of #92: the owner's repository-local identity must never sign the job's commit).
func TestCodexCommitIdentity(t *testing.T) {
	for _, c := range []struct {
		name, gitName, gitEmail string
		removeGitConfig         bool
		want                    string // author and committer "<name> <email>"; "" = no commit
	}{
		{"job identity", "Job Bot", "job@example.com", false, "Job Bot <job@example.com>|Job Bot <job@example.com>"},
		{"name missing", "", "job@example.com", false, ""},
		{"email missing", "Job Bot", "", false, ""},
		{"gitconfig missing", "Job Bot", "job@example.com", true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := newCodexManager(t)
			m.Cfg.CodexWriteAllowed, m.Cfg.GitName, m.Cfg.GitEmail = true, c.gitName, c.gitEmail
			repo := newRepo(t)
			gitT(t, repo, "config", "user.name", "Owner Person")
			gitT(t, repo, "config", "user.email", "owner@example.com")
			j, dir := preparedCodexFix(t, m, repo)
			// config.json changed after the launch does not change the identity of the commit.
			writeReloadConfig(t, m, `{"git_name":"Later Name","git_email":"later@example.com"}`)
			if c.removeGitConfig {
				if err := os.Remove(filepath.Join(dir, gitConfigFile)); err != nil {
					t.Fatal(err)
				}
			}
			work := filepath.Join(dir, "work")
			writeSized(t, work, "a.txt", 10, false)
			line := commitCodexWork(dir, j.ID)
			head := gitT(t, work, "rev-parse", "HEAD")
			if c.want == "" {
				if !strings.HasPrefix(line, "codex commit skipped: ") || !strings.Contains(line, "identity") {
					t.Fatalf("commit line %q", line)
				}
				if head != j.BaseCommit {
					t.Fatalf("HEAD moved to %s: %s", head, gitT(t, work, "log", "-1", "--format=%an <%ae>|%cn <%ce>"))
				}
				if staged := gitT(t, work, "diff", "--cached", "--name-only"); staged != "" {
					t.Fatalf("staged: %q", staged)
				}
				return
			}
			if head == j.BaseCommit {
				t.Fatalf("no commit: %q", line)
			}
			if got := gitT(t, work, "log", "-1", "--format=%an <%ae>|%cn <%ce>"); got != c.want {
				t.Fatalf("author|committer %q, want %q", got, c.want)
			}
		})
	}
}
