package jobs

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentjobs "github.com/qwwiwi/agent-jobs"
)

// The kind checklists tell a job that touches UI to run <job dir>/tools/ui-check.mjs: prepare puts the
// shipped copy there, whatever repository the job works in.
func TestPrepareWritesUICheckTool(t *testing.T) {
	m := newManager(t)
	j := enqueue(t, m, "do the thing", nil)
	j = runToEnd(t, m, j.ID)
	got, err := os.ReadFile(filepath.Join(m.JobDir(j.ID), UICheckToolRel))
	if err != nil {
		t.Fatal(err)
	}
	want, err := agentjobs.Templates.ReadFile("scripts/ui-check.mjs")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("tools/ui-check.mjs differs from the shipped scripts/ui-check.mjs")
	}
}

func uiCheckTemplate(t *testing.T) []byte {
	t.Helper()
	b, err := agentjobs.Templates.ReadFile("scripts/ui-check.mjs")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The writer runs on a job's first attempt only (a resume goes through prepareResume), but it is
// idempotent for repeated calls: the tool already in place with the shipped content is kept.
func TestWriteUICheckToolIdempotent(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		if err := writeUICheckTool(dir); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(dir, UICheckToolRel))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, uiCheckTemplate(t)) {
		t.Fatal("content differs after repeated calls")
	}
}

// A tool file with other content (the job edited it) is neither kept silently nor truncated.
func TestWriteUICheckToolRefusesDifferentContent(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tools"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, UICheckToolRel)
	if err := os.WriteFile(p, []byte("planted"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := writeUICheckTool(dir)
	if err == nil || !strings.Contains(err.Error(), "different content") {
		t.Fatalf("err = %v, want a different-content refusal", err)
	}
	if got, _ := os.ReadFile(p); string(got) != "planted" {
		t.Fatalf("existing file was rewritten: %q", got)
	}
}

// A symlink at the tool path is refused, and its target outside the job dir is left untouched.
func TestWriteUICheckToolRefusesSymlink(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "victim")
	if err := os.WriteFile(target, []byte("victim"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "tools"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, UICheckToolRel)); err != nil {
		t.Fatal(err)
	}
	err := writeUICheckTool(dir)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err = %v, want a symlink refusal", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "victim" {
		t.Fatalf("symlink target was written: %q", got)
	}
}

// A dangling symlink is refused too: O_CREAT alone would create its target outside the job dir.
func TestWriteUICheckToolRefusesDanglingSymlink(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "created")
	if err := os.MkdirAll(filepath.Join(dir, "tools"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, UICheckToolRel)); err != nil {
		t.Fatal(err)
	}
	if err := writeUICheckTool(dir); err == nil {
		t.Fatal("dangling symlink accepted")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("symlink target was created (lstat err %v)", err)
	}
}

// A hard link to a file outside the job dir is refused even when its content matches, and it is not
// truncated.
func TestWriteUICheckToolRefusesHardLink(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content []byte
	}{{"other content", []byte("victim")}, {"same content", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			dir, outside := t.TempDir(), t.TempDir()
			content := tc.content
			if content == nil {
				content = uiCheckTemplate(t)
			}
			target := filepath.Join(outside, "victim")
			if err := os.WriteFile(target, content, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(dir, "tools"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(target, filepath.Join(dir, UICheckToolRel)); err != nil {
				t.Skipf("hard link not possible here: %v", err)
			}
			err := writeUICheckTool(dir)
			if err == nil || !strings.Contains(err.Error(), "hard link") {
				t.Fatalf("err = %v, want a hard-link refusal", err)
			}
			if got, _ := os.ReadFile(target); !bytes.Equal(got, content) {
				t.Fatal("hard-link target was rewritten")
			}
		})
	}
}
