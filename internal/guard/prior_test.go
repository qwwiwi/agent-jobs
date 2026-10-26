package guard

import (
	"os"
	"path/filepath"
	"testing"
)

// Milestones: a job's <jobdir>/prior (the reports of its task's dependencies, passed as --protect) is
// read-only: reading the reports stays allowed, writing, moving or removing them is refused.
func TestPriorFolderReadOnly(t *testing.T) {
	dir := t.TempDir()
	prior := filepath.Join(dir, "prior")
	if err := os.MkdirAll(filepath.Join(prior, "T1"), 0o700); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(prior, "T1", "report.md")
	if err := os.WriteFile(report, []byte("## Summary\nok\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Protected: []string{prior}, Dir: filepath.Join(dir, "work")}
	for _, tool := range []string{"Write", "Edit"} {
		if CheckFileWrite(tool, report, cfg) == "" {
			t.Errorf("%s %s allowed", tool, report)
		}
	}
	for _, c := range []string{"echo x > " + report, "rm -r " + prior, "chmod u+w " + report, "mv " + report + " /tmp/r.md"} {
		if Check(c, cfg) == "" {
			t.Errorf("%q allowed", c)
		}
	}
	for _, c := range []string{"cat " + report, "grep Summary " + report, "ls " + prior} {
		if r := Check(c, cfg); r != "" {
			t.Errorf("%q refused: %s", c, r)
		}
	}
}
