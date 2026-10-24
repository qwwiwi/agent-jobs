package guard

import (
	"os"
	"path/filepath"
	"testing"
)

// jobFolderSetup is a job folder <state>/jobs/j with its gitconfig and no-hooks protected, the
// state's subagent logs and database too, and the work dir as the call's directory.
func jobFolderSetup(t *testing.T) (gc, hooks string, cfg Config) {
	t.Helper()
	state, _, _, cfg := protectedSetup(t)
	own := filepath.Join(state, "jobs", "j")
	gc, hooks = filepath.Join(own, "gitconfig"), filepath.Join(own, "no-hooks")
	if err := os.WriteFile(gc, []byte("[user]\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(hooks, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(hooks, 0o700) })
	cfg.Protected = append(cfg.Protected, gc, hooks)
	cfg.StateDir, cfg.OwnDir = state, own
	return gc, hooks, cfg
}

func refused(t *testing.T, cfg Config, cmds ...string) {
	t.Helper()
	for _, c := range cmds {
		if r := Check(c, cfg); r == "" {
			t.Errorf("%q allowed", c)
		}
	}
}

func allowed(t *testing.T, cfg Config, cmds ...string) {
	t.Helper()
	for _, c := range cmds {
		if r := Check(c, cfg); r != "" {
			t.Errorf("%q refused: %s", c, r)
		}
	}
}

// REDPROOF 1: a file descriptor is a second name of the file it holds. Opening a protected path in
// any redirection, input too, is refused, and /proc/*/fd, /dev/fd and /dev/std* are no targets of
// a change, a link or a write.
func TestFdAliasesOfProtectedPaths(t *testing.T) {
	gc, hooks, cfg := jobFolderSetup(t)
	q := func(p string) string { return "'" + p + "'" }
	refused(t, cfg,
		"exec 9<"+q(gc)+"; chmod u+w /proc/self/fd/9; printf '[diff]\\n' >> /proc/self/fd/9",
		"exec 9<"+q(gc), "exec 9<>"+q(gc), "cat < "+q(gc), "{ cat; } < "+q(gc), "exec 8<"+q(hooks),
		"ln -L /dev/stdin alias < "+q(gc), "ln -L /dev/stdin alias", "link /dev/stdin alias",
		"chmod u+w /proc/self/fd/9", "chmod u+w /dev/fd/9", "chmod u+w /dev/stdin", "chmod u+w /proc/123/fd/9",
		"printf x >> /proc/self/fd/9", "printf x > /dev/fd/9", "tee -a /proc/self/fd/9", "dd of=/dev/fd/9",
		"cd /proc/self/fd && chmod u+w 9", "cp /tmp/evil /proc/self/fd/9", "truncate -s0 /proc/thread-self/fd/9",
		"touch /proc/self/cwd/x",
	)
	for _, p := range []string{"/proc/self/fd/9", "/dev/fd/9", "/dev/stdin"} {
		if r := CheckFileWrite("Write", p, cfg); r == "" {
			t.Errorf("Write %s allowed", p)
		}
	}
	allowed(t, cfg, "echo x > /dev/stderr", "echo x >/dev/stdout", "echo x | tee /dev/stderr", "cat /dev/stdin",
		"echo hi >&2", "go test ./... 2>/dev/null", "exec 3>&1", "cat "+q(gc), "grep user "+q(gc), "cat < in.txt",
		"sort < in.txt > out.txt")
}

// REDPROOF 2: a hard link of a protected file is the same file; a write through it is refused
// (file tools and Bash), a file with one link and a pair of ordinary links are not.
func TestHardLinkOfProtectedFile(t *testing.T) {
	gc, _, cfg := jobFolderSetup(t)
	alias := filepath.Join(cfg.Dir, "alias")
	if err := os.Link(gc, alias); err != nil {
		t.Skipf("hard links not supported here: %v", err)
	}
	for _, tool := range []string{"Write", "Edit"} {
		if r := CheckFileWrite(tool, alias, cfg); r == "" {
			t.Errorf("%s through a hard link allowed", tool)
		}
	}
	refused(t, cfg, "chmod u+w alias", "echo x >> alias", "sed -i s/a/b/ alias", "cp /tmp/x alias", "truncate -s0 alias")
	allowed(t, cfg, "cat alias")

	plain, other := filepath.Join(cfg.Dir, "a.txt"), filepath.Join(cfg.Dir, "b.txt")
	if err := os.WriteFile(plain, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(plain, other); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{plain, other, filepath.Join(cfg.Dir, "new.txt")} {
		if r := CheckFileWrite("Write", p, cfg); r != "" {
			t.Errorf("Write %s refused: %s", p, r)
		}
	}
	allowed(t, cfg, "echo x >> a.txt", "chmod u+w b.txt")
}

// REDPROOF 3: the wrapper exports GIT_CONFIG_GLOBAL (the job's gitconfig) and GIT_CONFIG_VALUE_0 (its
// no-hooks dir); the variables name those paths in the job's shell and are expanded like $HOME.
func TestExportedGitVarsNameProtectedPaths(t *testing.T) {
	gc, hooks, cfg := jobFolderSetup(t)
	t.Setenv("GIT_CONFIG_GLOBAL", gc)
	t.Setenv("GIT_CONFIG_VALUE_0", hooks)
	refused(t, cfg,
		`chmod u+w "$GIT_CONFIG_GLOBAL"`, `echo '[diff]' >> $GIT_CONFIG_GLOBAL`, `chmod u+w ${GIT_CONFIG_GLOBAL}`,
		`echo x >> "${GIT_CONFIG_GLOBAL:-/tmp/x}"`, `cp /tmp/evil "$GIT_CONFIG_VALUE_0/pre-commit"`,
		`chmod u+w $GIT_CONFIG_VALUE_0`, `dd if=/tmp/x of=$GIT_CONFIG_GLOBAL`,
		`python3 -c "open('$GIT_CONFIG_GLOBAL', 'a').write('x')"`, `exec 9<"$GIT_CONFIG_GLOBAL"`,
	)
	allowed(t, cfg, `cat "$GIT_CONFIG_GLOBAL"`, `echo "$GIT_CONFIG_GLOBAL"`, `echo x > "$UNSET_IN_TEST_ENV/x"`)
}

// REDPROOF 7: file tools write in the job's own folder only: other jobs' files and the state's own
// files (goals, archive) are refused, the job's work tree, progress notes and scratch files are not.
func TestFileToolsStayInOwnFolder(t *testing.T) {
	_, _, cfg := jobFolderSetup(t)
	other := filepath.Join(cfg.StateDir, "jobs", "k")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, other, filepath.Join(cfg.Dir, "peer"))
	for _, p := range []string{filepath.Join(other, "report.md"), filepath.Join(other, "prompt.md"),
		filepath.Join(other, "settings.json"), filepath.Join(cfg.StateDir, "goals.md"),
		filepath.Join(cfg.StateDir, "archive", "x.md"), filepath.Join(cfg.Dir, "peer", "report.md")} {
		if r := CheckFileWrite("Write", p, cfg); r == "" {
			t.Errorf("Write %s allowed", p)
		}
	}
	for _, p := range []string{filepath.Join(cfg.Dir, "main.go"), filepath.Join(cfg.OwnDir, "PROGRESS.md"),
		filepath.Join(cfg.OwnDir, "report.md"), filepath.Join(cfg.OwnDir, "tmp", "x.py"), "notes/a.md"} {
		if r := CheckFileWrite("Write", p, cfg); r != "" {
			t.Errorf("Write %s refused: %s", p, r)
		}
	}
}

// REDPROOF 8: the directory holding a protected path is refused as a destination of programs that
// write into a directory, not as an argument of programs that read or list it.
func TestDirHoldingProtectedReadersAllowed(t *testing.T) {
	_, _, cfg := jobFolderSetup(t)
	own := "'" + cfg.OwnDir + "'"
	allowed(t, cfg, "find ..", "find .. -name '*.md'", "du -sh ..", "tree ..", "ls -la ..", "python3 x.py ..",
		"du -sh "+own, "node scan.js "+own, "bash count.sh ..")
	refused(t, cfg, "cp f "+own+"/", "cp f ..", "mv f ..", "install f ..", "ln -s /tmp/x ..", "rsync -a d/ ..",
		"tar -xf a.tar -C ..", "find .. -delete", "python3 -c 'import sys' ..", "python3 - .. <<'EOF'\nx\nEOF",
		"patch -d .. -p1 < x.diff")
}
