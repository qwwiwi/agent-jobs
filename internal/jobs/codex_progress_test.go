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

// argAddDirs are the `--add-dir` values of a codex command line.
func argAddDirs(args []string) []string {
	var dirs []string
	for i, a := range args {
		if a == "--add-dir" && i+1 < len(args) {
			dirs = append(dirs, args[i+1])
		}
	}
	return dirs
}

// A codex write job (fix, general) gets exactly its job dir's progress folder as an extra writable
// root, so it can keep its progress file (issue #92); read-only kinds and the full-access mode get none.
func TestCodexArgsProgressDirWritable(t *testing.T) {
	jobDir := filepath.Join(t.TempDir(), "jobs", "j1")
	for _, c := range []struct {
		kind, sandbox string
		want          []string
	}{
		{"fix", CodexSandboxWrite, []string{filepath.Join(jobDir, CodexProgressDir)}},
		{"general", CodexSandboxWrite, []string{filepath.Join(jobDir, CodexProgressDir)}},
		{"review", CodexSandboxReadOnly, nil},
		{"research", CodexSandboxReadOnly, nil},
		{KindJudge, CodexSandboxReadOnly, nil},
		{KindSlicing, CodexSandboxReadOnly, nil},
		{"fix", CodexSandboxFullAccess, nil},
	} {
		if got := CodexSandbox(c.kind); c.sandbox != CodexSandboxFullAccess && got != c.sandbox {
			t.Fatalf("%s: sandbox %q, want %q", c.kind, got, c.sandbox)
		}
		args := CodexArgs(&store.Job{Kind: c.kind, Model: "gpt-6.1-sol"}, "high", c.sandbox, jobDir, false)
		if got := argAddDirs(args); !slices.Equal(got, c.want) {
			t.Fatalf("%s/%s: --add-dir %q, want %q (argv %q)", c.kind, c.sandbox, got, c.want, args)
		}
		// The extra root is an option of exec, before the stdin prompt.
		if i := slices.Index(args, "--add-dir"); i >= 0 && i > slices.Index(args, "-") {
			t.Fatalf("--add-dir after the prompt: %q", args)
		}
	}
	if p := CodexProgressPath(jobDir); p != filepath.Join(jobDir, CodexProgressDir, ProgressFile) {
		t.Fatalf("progress path %q", p)
	}
}

// The prompt of a codex write job names the writable progress path; a read-only codex job is told it
// keeps no progress file; a claude job keeps the preamble's job-dir progress file (issue #92).
func TestCodexPromptNamesProgressPath(t *testing.T) {
	m := newCodexManager(t)
	m.Cfg.CodexWriteAllowed = true
	const claudeRule = "Keep `PROGRESS.md` in the job directory up to date"
	for _, c := range []struct {
		name string
		opt  func(*StartRequest)
		want []string
		not  []string
	}{
		{"codex fix", func(r *StartRequest) { r.Kind, r.Model = "fix", "gpt-6.1-sol" }, nil, []string{claudeRule}},
		{"codex general", func(r *StartRequest) { r.Kind, r.Model = "general", "gpt-6.1-sol" }, nil, []string{claudeRule}},
		{"codex review", codexReview, []string{"Your sandbox is read-only: keep no progress file"},
			[]string{claudeRule, "/" + CodexProgressDir + "/"}},
		{"claude fix", func(r *StartRequest) { r.Kind = "fix" }, []string{claudeRule}, []string{"/" + CodexProgressDir + "/"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			j := enqueue(t, m, "work", c.opt)
			p, err := m.BuildPrompt(j, "rules", nil, MemoryNotes{})
			if err != nil {
				t.Fatal(err)
			}
			want := c.want
			if j.Runner == store.RunnerCodex && CodexSandbox(j.Kind) == CodexSandboxWrite {
				path := CodexProgressPath(m.JobDir(j.ID))
				want = []string{"`" + path + "`", "cat > " + path + " <<'EOF'",
					"You cannot commit", "`" + CodexCommitMessagePath(m.JobDir(j.ID)) + "`"}
			}
			for _, s := range want {
				if !strings.Contains(p, s) {
					t.Fatalf("prompt lacks %q:\n%s", s, p)
				}
			}
			for _, s := range c.not {
				if strings.Contains(p, s) {
					t.Fatalf("prompt has %q:\n%s", s, p)
				}
			}
		})
	}
}

// prepare creates the progress folder of a codex write job (and only of one), and a codex write job
// launched by the daemon writes its progress file there through the wrapper's argv check.
func TestCodexWriteJobProgressDirLaunch(t *testing.T) {
	m := newCodexManager(t)
	writeReloadConfig(t, m, `{"codex_write_allowed":true}`)
	var err error
	if m.Cfg, err = LoadConfig(m.StateDir); err != nil {
		t.Fatal(err)
	}
	unlock, err := m.LockDaemon()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	m.WatchDaemonConfig()
	j := runToEnd(t, m, enqueue(t, m, "fix it", func(r *StartRequest) { r.Kind, r.Model = "fix", "gpt-6.1-sol" }).ID)
	if j.Status != store.StatusSucceeded {
		t.Fatalf("codex fix job: %s %q", j.Status, j.Error)
	}
	if fi, err := os.Lstat(filepath.Join(m.JobDir(j.ID), CodexProgressDir)); err != nil || !fi.IsDir() {
		t.Fatalf("progress dir of a codex write job: %v", err)
	}
	argv, _ := os.ReadFile(filepath.Join(m.JobDir(j.ID), "argv.txt"))
	if !strings.Contains(string(argv), "--add-dir\n"+filepath.Join(m.JobDir(j.ID), CodexProgressDir)+"\n") {
		t.Fatalf("codex argv without the progress dir:\n%s", argv)
	}

	r := runToEnd(t, m, enqueue(t, m, "review it", codexReview).ID)
	if r.Status != store.StatusSucceeded {
		t.Fatalf("codex review job: %s %q", r.Status, r.Error)
	}
	if _, err := os.Lstat(filepath.Join(m.JobDir(r.ID), CodexProgressDir)); !os.IsNotExist(err) {
		t.Fatalf("read-only codex job got a progress dir: %v", err)
	}
	if argv, _ := os.ReadFile(filepath.Join(m.JobDir(r.ID), "argv.txt")); strings.Contains(string(argv), "--add-dir") {
		t.Fatalf("read-only codex job got a writable root:\n%s", argv)
	}
}

// The wrapper admits `--add-dir` only as the daemon builds it: once, the job's own progress folder (a
// real directory), on a write job's workspace-write sandbox. Any other root, or one on a read-only job,
// is refused before codex runs.
func TestWrapperChecksCodexAddDir(t *testing.T) {
	for _, c := range []struct {
		name  string
		kind  string
		extra func(jobDir string) []string // replaces the daemon's --add-dir pair
		setup func(t *testing.T, jobDir string)
		ok    bool
	}{
		{"daemon argv", "fix", nil, nil, true},
		{"no add-dir", "fix", func(string) []string { return nil }, nil, true},
		{"root", "fix", func(string) []string { return []string{"--add-dir", "/"} }, nil, false},
		{"whole job dir", "fix", func(d string) []string { return []string{"--add-dir", d} }, nil, false},
		{"attached job dir", "fix", func(d string) []string { return []string{"--add-dir=" + d} }, nil, false},
		{"twice", "fix", func(d string) []string {
			p := filepath.Join(d, CodexProgressDir)
			return []string{"--add-dir", p, "--add-dir", p}
		}, nil, false},
		// The wrapper runs in work/, where ../progress is the progress folder, but codex resolves a
		// relative root against its own working dir: only the absolute path is admitted.
		{"relative", "fix", func(string) []string { return []string{"--add-dir", "../" + CodexProgressDir} }, nil, false},
		{"dotdot", "fix", func(d string) []string {
			return []string{"--add-dir", filepath.Join(d, CodexProgressDir) + "/.."}
		}, nil, false},
		{"symlinked progress dir", "fix", nil, func(t *testing.T, d string) {
			p := filepath.Join(d, CodexProgressDir)
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), p); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"review with progress dir", "review", func(d string) []string {
			return []string{"--add-dir", filepath.Join(d, CodexProgressDir)}
		}, func(t *testing.T, d string) {
			if err := os.Mkdir(filepath.Join(d, CodexProgressDir), 0o700); err != nil {
				t.Fatal(err)
			}
		}, false},
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
				store.LaunchAuthorization{CodexWriteAllowed: true}); err != nil {
				t.Fatal(err)
			}
			dir := m.JobDir(j.ID)
			if err := m.prepare(j, token, dir); err != nil {
				t.Fatal(err)
			}
			if c.setup != nil {
				c.setup(t, dir)
			}
			args := CodexArgs(j, "high", CodexSandbox(j.Kind), dir, false)
			if c.extra != nil {
				var kept []string
				for i := 0; i < len(args); i++ {
					if args[i] == "--add-dir" {
						i++
						continue
					}
					kept = append(kept, args[i])
				}
				args = append(append(kept[:len(kept)-1:len(kept)-1], c.extra(dir)...), "-")
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
				t.Fatalf("codex ran with an unauthorized writable root: %v", ranErr)
			}
		})
	}
}
