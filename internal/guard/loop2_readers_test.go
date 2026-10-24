package guard

import (
	"os"
	"path/filepath"
	"testing"
)

// homeProtected adds a protected skill root directly in $HOME and a protected session transcript
// dir in ~/.claude/projects, as a job's real configuration has them.
func homeProtected(t *testing.T, cfg *Config) {
	t.Helper()
	home := os.Getenv("HOME")
	for _, d := range []string{filepath.Join(home, "skills"), filepath.Join(home, ".claude", "projects", "-sess")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		cfg.Protected = append(cfg.Protected, d)
	}
}

// Fix loop 2, item 1: an interpreter running a script or a module is no directory reader: the
// directory holding a protected path stays a destination for it (`python3 -m zipfile --extract
// evil.zip ..` writes into it), and the archive and package modules of python write whole trees.
func TestInterpretersAreNoDirReaders(t *testing.T) {
	_, _, cfg := jobFolderSetup(t)
	homeProtected(t, &cfg)
	// A skill link inside the work tree is protected, as in a job's real configuration.
	skill := filepath.Join(cfg.Dir, ".claude", "skills", "s")
	if err := os.MkdirAll(skill, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.Protected = append(cfg.Protected, skill)
	refused(t, cfg,
		"python3 -m zipfile --extract evil.zip ..", "python3 -m zipfile --extract evil.zip ~",
		`python3 -m zipfile --extract evil.zip "$HOME"`, "python3 -m tarfile --extract evil.tar ~/.claude/projects",
		"python3 -m pip install --target ~ x", "python3 -m pip download -d ~/.claude/projects x",
		"python3 -m zipfile -e evil.zip ~/.agent-jobs", "python3 -mtarfile -e evil.tar ~/.claude",
		"python3 -m pip install --target ~/.agent-jobs x", `python3 -m pip install --target=$HOME/.agent-jobs x`,
		`python3 -m pip install -t$HOME/.agent-jobs x`, "python3 -m pip install --target . x",
		"python3 x.py ..", "node scan.js '"+cfg.OwnDir+"'", "bash count.sh ..", "ruby x.rb ~",
	)
	allowed(t, cfg, "python3 -m zipfile --extract evil.zip out", "python3 -m pip install --target ./vendor x",
		"python3 x.py src", "node scan.js .", "python3 -m pytest tests", "python3 -m pip install -e .",
		"python3 -m pip install -r requirements.txt")
}

// Normal job work stays allowed under the fixes of loop 2: writes in the work tree and the job's
// folder, go test, git add/commit, and readers in the job's folder.
func TestNormalJobWorkAllowed(t *testing.T) {
	_, _, cfg := jobFolderSetup(t)
	homeProtected(t, &cfg)
	skill := filepath.Join(cfg.Dir, ".claude", "skills", "s")
	if err := os.MkdirAll(skill, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.Protected = append(cfg.Protected, skill)
	own := "'" + cfg.OwnDir + "'"
	allowed(t, cfg, "exec 3<in.txt", "python3 -c 'print(1)' < in.txt", "python3 -m pip install -e .",
		"cat > "+own+"/PROGRESS.md <<'EOF'\n# progress\n- step 1 done\nEOF", "go test ./...", "go vet ./... 2>&1 | tail -5",
		"git add -A && git commit -m 'Fix guard'", "find . -name '*.go'", "find "+own+" -maxdepth 1", "du -sh "+own,
		"ls -la ..", "ls ~", "mkdir -p internal/x && cp a.go internal/x/", "chmod -R u+w build", "gofmt -l .",
		"TMPDIR=/tmp/wt GOTMPDIR=/tmp/wt go test ./internal/guard/", "bash scripts/check.sh",
	)
	for _, p := range []string{filepath.Join(cfg.Dir, "main.go"), filepath.Join(cfg.OwnDir, "PROGRESS.md"),
		filepath.Join(cfg.OwnDir, "report.md")} {
		if r := CheckFileWrite("Write", p, cfg); r != "" {
			t.Errorf("Write %s refused: %s", p, r)
		}
	}
}
