package guard

import (
	"os"
	"path/filepath"
	"testing"
)

// Wave F: a job's brief/BRIEF.md and brief/CHECKLIST.md (passed as --protect) are written only by the
// orchestrator's checklist tools: Write, Edit and Bash writes to them are refused, through a relative
// path, a cd or a symlink; reading them and writing other files under brief/ stay allowed.
func TestBriefFilesProtected(t *testing.T) {
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, "brief"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(work, "brief"), filepath.Join(work, "b")); err != nil {
		t.Fatal(err)
	}
	brief, list := filepath.Join(work, "brief", "BRIEF.md"), filepath.Join(work, "brief", "CHECKLIST.md")
	cfg := Config{Protected: []string{brief, list}, Dir: work}
	for _, tool := range []string{"Write", "Edit", "MultiEdit", "NotebookEdit"} {
		for _, p := range []string{brief, list, "brief/BRIEF.md", "brief/CHECKLIST.md", "b/CHECKLIST.md"} {
			if CheckFileWrite(tool, p, cfg) == "" {
				t.Errorf("%s %s allowed", tool, p)
			}
		}
	}
	for _, c := range []string{
		"echo x > brief/BRIEF.md", "printf x >> " + list, "sed -i s/a/b/ brief/CHECKLIST.md", "cp /dev/null brief/BRIEF.md",
		"mv new.md brief/CHECKLIST.md", "rm brief/BRIEF.md", "rm -r brief", "tee brief/CHECKLIST.md", "cd brief && echo x > BRIEF.md",
		"truncate -s0 " + brief, "echo x > b/BRIEF.md",
	} {
		if Check(c, cfg) == "" {
			t.Errorf("%q allowed", c)
		}
	}
	for _, c := range []string{"cat brief/BRIEF.md", "grep R01 brief/CHECKLIST.md", "echo x > brief/notes.md", "go test ./..."} {
		if r := Check(c, cfg); r != "" {
			t.Errorf("%q refused: %s", c, r)
		}
	}
	if CheckFileWrite("Write", filepath.Join(work, "brief", "notes.md"), cfg) != "" {
		t.Error("Write of another file under brief/ refused")
	}
}
