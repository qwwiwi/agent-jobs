package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #211: a wrapper that changes the working directory (`env -C|--chdir D`, `sudo -D|--chdir D`) is a cd
// for every rule: the command's relative paths resolve against D too, `..` in D the way the kernel
// resolves it, and a wrapper whose directory the guard cannot resolve (`sudo -i`: the target user's
// home) leaves the directory unknown, as `cd $X` does.

// chdirSetup is a job folder (gitconfig protected, the work dir as the call's directory) whose work dir
// also holds a protected brief/BRIEF.md, a symlink up -> brief/inner (so `up/..` is brief for the
// kernel and the work dir lexically) and a plain directory sub.
func chdirSetup(t *testing.T) Config {
	t.Helper()
	_, _, cfg := jobFolderSetup(t)
	for _, d := range []string{"brief/inner", "sub"} {
		if err := os.MkdirAll(filepath.Join(cfg.Dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(cfg.Dir, "brief", "inner"), filepath.Join(cfg.Dir, "up")); err != nil {
		t.Fatal(err)
	}
	cfg.Protected = append(cfg.Protected, filepath.Join(cfg.Dir, "brief", "BRIEF.md"))
	return cfg
}

// chdirForms are the spellings of a directory-changing wrapper in front of a command, %s the directory.
var chdirForms = []string{
	"env -C %s", "env --chdir=%s", "env --chdir %s", "env -C%s", "env -vC %s", "env -u X -C %s",
	"env -C %s --", "env -C . -C %s", "busybox env -C %s", "nice env -C %s", "exec env -C %s",
	"command env --chdir %s", "env env -C %s", "sudo -D %s", "sudo --chdir=%s", "sudo --chdir %s",
	"sudo -D%s", "sudo -u x -D %s", "sudo -bD %s", "sudo --chd=%s", "sudo -n --chdir %s",
}

func wrapIn(form, dir string) string { return strings.ReplaceAll(form, "%s", dir) }

// The three forms of the issue, allowed before, are refused.
func TestChdirWrapperIssueForms(t *testing.T) {
	cfg := chdirSetup(t)
	refused(t, cfg,
		"env -C brief cp a BRIEF.md",
		"env -C brief sh -c 'cat x > BRIEF.md'",
		"env -C .. rm -f gitconfig",
	)
}

// For every protected write, `cd D && CMD` and each wrapper spelling of `D CMD` get the same verdict
// (refused), and so do the matching reads (allowed).
func TestChdirWrapperSameVerdictAsCd(t *testing.T) {
	cfg := chdirSetup(t)
	home := os.Getenv("HOME")
	rows := []struct{ dir, cmd string }{
		{"brief", "cp a BRIEF.md"},
		{"brief", "sh -c 'cat x > BRIEF.md'"},
		{"brief", "bash -c 'echo x >> BRIEF.md'"},
		{"brief", "tee BRIEF.md"},
		{"brief", "touch BRIEF.md"},
		{"brief", "mv x BRIEF.md"},
		{"brief", "rm BRIEF.md"},
		{"brief", "sed -i s/a/b/ BRIEF.md"},
		{"brief", "truncate -s0 BRIEF.md"},
		{"brief", "dd of=BRIEF.md"},
		{"brief", "ln -sf /tmp/x BRIEF.md"},
		{"brief/inner", "cp a ../BRIEF.md"},
		{"sub/../brief", "cp a BRIEF.md"},
		{"up/..", "cp a BRIEF.md"}, // the kernel: brief/inner/.. is brief
		{"..", "rm -f gitconfig"},
		{"..", "chmod u+w gitconfig"},
		{"../work/..", "cp /dev/null gitconfig"},
		{"~/.agent-jobs/thrall/jobs/j", "rm gitconfig"},
		{home + "/.agent-jobs/thrall/jobs", "rm j/gitconfig"},
		{"/", "cp a " + strings.TrimPrefix(filepath.Join(cfg.Dir, "brief", "BRIEF.md"), "/")},
	}
	for _, r := range rows {
		cd := "cd " + r.dir + " && " + r.cmd
		if Check(cd, cfg) == "" {
			t.Fatalf("baseline %q allowed: the row does not name a protected write", cd)
		}
		for _, f := range chdirForms {
			c := wrapIn(f, r.dir) + " " + r.cmd
			if Check(c, cfg) == "" {
				t.Errorf("%q allowed, but %q is refused", c, cd)
			}
		}
	}
	reads := []struct{ dir, cmd string }{
		{"brief", "cat BRIEF.md"},
		{"brief", "grep R01 BRIEF.md"},
		{"..", "cat gitconfig"},
		{"sub", "cp a b"},
	}
	for _, r := range reads {
		cd := "cd " + r.dir + " && " + r.cmd
		if v := Check(cd, cfg); v != "" {
			t.Fatalf("baseline %q refused: %s", cd, v)
		}
		for _, f := range chdirForms {
			if strings.HasPrefix(f, "sudo") {
				continue // sudo is not a read wrapper the guard lets through unchanged in every rule
			}
			c := wrapIn(f, r.dir) + " " + r.cmd
			if v := Check(c, cfg); v != "" {
				t.Errorf("%q refused, but %q is allowed: %s", c, cd, v)
			}
		}
	}
}

// A read through env -C keeps its verdict (allowed, as `cd brief && cat BRIEF.md` is).
func TestChdirWrapperReadAllowed(t *testing.T) {
	cfg := chdirSetup(t)
	allowed(t, cfg, "env -C brief cat BRIEF.md", "env --chdir=brief cat BRIEF.md", "env -C sub cp a b")
}

// A cd by an env -C anywhere in the call counts for every command of it, as a cd does (fail closed).
func TestChdirWrapperCountsForTheCall(t *testing.T) {
	cfg := chdirSetup(t)
	if Check("cd brief; cp b BRIEF.md", cfg) == "" {
		t.Fatal("baseline cd form allowed")
	}
	refused(t, cfg, "env -C brief true; cp b BRIEF.md", "env -C brief true && sh -c 'echo x > BRIEF.md'")
}

// sudo -i / --login runs the command in the target user's home, a directory the guard does not
// resolve: relative paths are compared by their tail, as after `cd $X`.
func TestChdirWrapperSudoLogin(t *testing.T) {
	cfg := chdirSetup(t)
	if Check("cd $X && rm -f .agent-jobs/thrall/jobs/j/gitconfig", cfg) == "" {
		t.Fatal("baseline cd $X form allowed")
	}
	refused(t, cfg,
		"sudo -i rm -f .agent-jobs/thrall/jobs/j/gitconfig",
		"sudo --login rm -f .agent-jobs/thrall/jobs/j/gitconfig",
		"sudo --log cp a .agent-jobs/thrall/jobs/j/gitconfig",
		"sudo -u x -i cp a .agent-jobs/thrall/jobs/j/gitconfig",
		"sudo -ni cp a .agent-jobs/thrall/jobs/j/gitconfig",
		"env -C $D cp a BRIEF.md",
		"sudo -D $D cp a BRIEF.md",
	)
}

// Through the gate the verdict is the direct one (refused).
func TestChdirWrapperGateRun(t *testing.T) {
	cfg := chdirSetup(t)
	refused(t, cfg,
		"agent-jobs gate run --head HEAD -- env -C brief cp a BRIEF.md",
		"agent-jobs gate run --head HEAD -- env -C .. rm -f gitconfig",
	)
}
