package mcp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Issue #83: checklist_get and checklist_set say early whether a judge could run its manifest-check:
// config acceptance_allowed and scripts/manifest-check.sh at the work tree's HEAD, named by revision.
// Advisory only: nothing is refused for either.
func TestChecklistJudgeReadiness(t *testing.T) {
	m := newManager(t)
	repo, head := gitRepo(t)
	cs := connect(t, mustServer(t, m))

	var v ChecklistView
	call(t, cs, "checklist_init", map[string]any{"repo": repo, "brief": "I want a login page."}, &v)
	call(t, cs, "checklist_get", map[string]any{"repo": repo}, &v)
	r := v.JudgeReadiness
	if r.AcceptanceAllowed || r.ManifestScript || r.Revision != head || len(r.Notes) != 2 ||
		!strings.Contains(r.Notes[0], "acceptance_allowed is off") ||
		!strings.Contains(r.Notes[1], "scripts/manifest-check.sh is not a regular file at HEAD "+head[:12]) {
		t.Fatalf("missing both: %+v", r)
	}

	// Committed script, acceptance_allowed on: ready, no notes; approval reports the same.
	if err := os.MkdirAll(filepath.Join(repo, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "scripts", "manifest-check.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "scripts/manifest-check.sh"}, {"-c", "user.name=T", "-c", "user.email=t@example.com",
		"commit", "-q", "-m", "manifest-check"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	head = strings.TrimSpace(string(out))
	m.Cfg.AcceptanceAllowed = true
	v = ChecklistView{} // decoding into v would keep the notes the reply omits
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "rows": []map[string]any{
		{"id": "R01", "verbatim": "a login page", "contract": "live", "rationale": "brief"},
	}, "approve": true, "approve_quote": "ok"}, &v)
	if r := v.JudgeReadiness; !r.AcceptanceAllowed || !r.ManifestScript || r.Revision != head || len(r.Notes) != 0 || !v.Approved {
		t.Fatalf("present: approved %v, %+v", v.Approved, r)
	}

	// A script removed only in the work tree still counts: HEAD is what a judge of it would get.
	if err := os.Remove(filepath.Join(repo, "scripts", "manifest-check.sh")); err != nil {
		t.Fatal(err)
	}
	call(t, cs, "checklist_get", map[string]any{"repo": repo}, &v)
	if r := v.JudgeReadiness; !r.ManifestScript || r.Revision != head {
		t.Fatalf("removed in the work tree only: %+v", r)
	}
}
