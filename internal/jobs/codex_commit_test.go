package jobs

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

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

// A codex write job's commands get the network by default (issue #92: `npm ci` failed with EAI_AGAIN):
// workspace-write gets CodexNetworkOverride, read-only and full-access never; network false drops it.
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

// codex_write_network: absent = on, false = off; at launch a false on disk revokes it from an active
// config that has it on (like codex_write_allowed), and a true on disk does not grant it to one that has
// it off.
func TestCodexWriteNetworkConfig(t *testing.T) {
	if !(Config{}).CodexWriteNetworkOn() {
		t.Fatal("codex_write_network absent is not on")
	}
	for body, want := range map[string]bool{`{}`: true, `{"codex_write_network":true}`: true, `{"codex_write_network":false}`: false} {
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
		{nil, `{}`, true},
		{nil, `{"codex_write_network":false}`, false},
		{&off, `{"codex_write_network":true}`, false},
		{&on, `{}`, true},
	} {
		m.Cfg.CodexWriteNetwork = c.active
		writeReloadConfig(t, m, c.disk)
		cfg, err := m.authorizedLaunchConfig()
		if err != nil || cfg.CodexWriteNetworkOn() != c.want {
			t.Fatalf("active %v, disk %s: on=%v err=%v, want %v", c.active, c.disk, cfg.CodexWriteNetworkOn(), err, c.want)
		}
	}
}

// The daemon launches a codex fix job with the network override and a review job without; with
// codex_write_network false the fix job runs offline.
func TestCodexWriteJobNetworkLaunch(t *testing.T) {
	for _, c := range []struct {
		name, config string
		start        func(*StartRequest)
		want         bool
	}{
		{"fix", `{"codex_write_allowed":true}`, func(r *StartRequest) { r.Kind, r.Model = "fix", "gpt-6.1-sol" }, true},
		{"fix offline", `{"codex_write_allowed":true,"codex_write_network":false}`,
			func(r *StartRequest) { r.Kind, r.Model = "fix", "gpt-6.1-sol" }, false},
		{"review", `{"codex_write_allowed":true}`, codexReview, false},
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
