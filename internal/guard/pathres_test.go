package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// protectedSetup is the realistic layout of the control review's probe: state under
// $HOME/.agent-jobs/thrall, the call running in <state>/jobs/j/work.
func protectedSetup(t *testing.T) (state, logf, db string, cfg Config) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CDPATH", "")
	state = filepath.Join(home, ".agent-jobs", "thrall")
	logs := filepath.Join(state, "subagents")
	work := filepath.Join(state, "jobs", "j", "work")
	for _, d := range []string{logs, work} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	logf = filepath.Join(logs, "j.log")
	db = filepath.Join(state, "jobs.db")
	for _, f := range []string{logf, db} {
		if err := os.WriteFile(f, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg = Config{Protected: []string{logs, db, db + "-wal", db + "-shm", db + "-journal"}, Dir: work}
	return state, logf, db, cfg
}

// N1-N4: every form the control review found ALLOWED is refused.
func TestProtectedProbeAllowedRowsRefused(t *testing.T) {
	state, logf, db, cfg := protectedSetup(t)
	tl := "~/.agent-jobs/thrall/subagents/j.log"
	tdb := "~/.agent-jobs/thrall/jobs.db"
	for _, c := range []string{
		// N1: tilde
		"rm " + tl,
		"sqlite3 " + tdb + " 'UPDATE jobs SET subagents=0'",
		"dd if=/dev/null of=" + tdb,
		"echo x > " + tl,
		"rm $HOME/.agent-jobs/thrall/subagents/j.log",
		"rm ~thrall/.agent-jobs/thrall/jobs.db",
		// N2: cd tracking
		"cd ~/.agent-jobs/thrall && sqlite3 jobs.db 'UPDATE jobs SET subagents=0'",
		"cd " + state + "; sqlite3 jobs.db 'UPDATE jobs SET subagents=0'",
		"cd " + state + "\nrm subagents/j.log",
		"cd ../../.. && rm subagents/j.log",
		"cd ../.. || true; cd .. && rm subagents/j.log",
		"pushd " + state + " && truncate -s0 subagents/j.log",
		"cd \"$X\" && rm subagents/j.log",
		"cd $(dirname x) && sqlite3 jobs.db .dump",
		"cd - && rm jobs.db",
		"CDPATH=" + state + " cd subagents && rm j.log",
		"cd ~other && rm subagents/j.log",
		"cd " + state + " && python3 -c \"open('jobs.db','w')\"",
		"cd " + state + " && echo > jobs.db",
		// N3: embedded
		"dd if=/dev/null of=" + logf,
		"dd if=/dev/null of=" + db,
		"python3 -c \"open('" + logf + "','w')\"",
		"perl -e \"truncate('" + logf + "',0)\"",
		"node -e \"require('fs').writeFileSync('" + logf + "','')\"",
		"python3 -c \"open('" + tl + "','w')\"",
		"sqlite3 --cmd=x --file=" + db,
		"python3 - <<'EOF'\nopen('" + logf + "','w')\nEOF",
		// N4: ancestors, xargs, ln
		"find " + state + " -name j.log -delete",
		"find " + state + " -name j.log -exec rm {} +",
		"rm -r " + state,
		"rm -rf ~/.agent-jobs",
		"mv " + state + " /tmp/zz",
		"chmod -R 000 " + state,
		"tar -C " + state + " -xf a.tar",
		"rsync -a --delete empty/ " + state + "/",
		"echo " + logf + " | xargs rm",
		"find " + state + " -name j.log | xargs rm",
		"ln -s " + state + " S; rm S/subagents/j.log",
		"cp evil " + state,
		"cp evil " + state + "/",
		"cd $X && rm -r .agent-jobs",
	} {
		if Check(c, cfg) == "" {
			t.Errorf("%q allowed", c)
		}
	}
}

// The rows the control review found blocked stay blocked; plain readers and ordinary job work stay allowed.
func TestProtectedProbeBlockedAndReaders(t *testing.T) {
	state, logf, db, cfg := protectedSetup(t)
	if err := os.Symlink(state, filepath.Join(cfg.Dir, "S")); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{
		"rm ../../../subagents/j.log", "bash -c 'rm " + logf + "'", "sh -c \": > " + logf + "\"",
		"exec 3>" + logf, "sqlite3 " + db + " 'UPDATE jobs SET subagents=0'",
		"find " + state + "/subagents -name j.log -delete", "env rm " + logf, "command rm " + logf,
		"nohup rm " + logf, "install /dev/null " + logf, "tee " + logf, "sed -i d " + logf,
		"truncate -s0 " + logf, "cp -t " + state + "/subagents x", "rm S/subagents/j.log",
		"cd " + state + "/subagents && rm j.log",
	} {
		if Check(c, cfg) == "" {
			t.Errorf("%q allowed", c)
		}
	}
	for _, c := range []string{
		"cat " + logf, "wc -l " + logf, "head -1 " + logf, "grep x " + logf, "cat ~/.agent-jobs/thrall/subagents/j.log",
		"cd " + state + " && cat jobs.db", "cd " + state + " && wc -l subagents/j.log",
		"echo hi > out.txt", "go test ./...", "rm -rf build", "rm -rf ../work/tmp", "cd sub && rm x",
		"cd $X && git status", "find build -name x.o -delete", "cp a b", "mv a b && ls",
		"git commit -m 'fix jobs dir'", "ls " + state, "cd ~ && ls", "cd ~/project && rm -r out",
		"printf 'a\\n' | xargs rm", "cat list | xargs rm",
	} {
		if r := Check(c, cfg); r != "" {
			t.Errorf("%q refused: %s", c, r)
		}
	}
}

func TestCouldReach(t *testing.T) {
	s := &pathScope{protected: []string{"/h/.agent-jobs/thrall/subagents", "/h/.agent-jobs/thrall/jobs.db"}}
	for _, c := range []struct {
		rel        string
		tree, want bool
	}{
		{"subagents/j.log", false, true}, {"thrall/jobs.db", false, true}, {"jobs.db", false, true},
		{"status", false, false}, {"x/jobs.db", false, false}, {"../x", false, true},
		{"thrall", false, false}, {"thrall", true, true}, {".", true, true}, {".", false, false},
		{".agent-jobs", true, true}, {"work", true, false},
	} {
		if got := s.couldReach(c.rel, c.tree); got != c.want {
			t.Errorf("couldReach(%q, %v) = %v, want %v", c.rel, c.tree, got, c.want)
		}
	}
}

func TestExpandAllHomes(t *testing.T) {
	s := &pathScope{home: "/h"}
	got := s.expandAllHomes("of=~/a x '~/b' a~/c $HOME/d")
	if want := "of=/h/a x '/h/b' a~/c /h/d"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if !strings.HasPrefix(s.expandAllHomes("~/x"), "/h/") {
		t.Fatal("leading ~/ not expanded")
	}
}
