package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// job_start takes size (stored, shown by job_status); large needs covers of the repo's approved
// checklist; a value other than small or large is refused.
func TestJobStartSize(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))
	repo := coverRepo(t)
	approveCovers(t, m, repo)
	var v JobView
	call(t, cs, "job_start", startArgs(func(a map[string]any) { a["kind"] = "fix"; a["size"] = "small" }), &v)
	var st JobView
	call(t, cs, "job_status", map[string]any{"id": v.ID}, &st)
	if v.Size != "small" || st.Size != "small" {
		t.Fatalf("size not stored: %q %q", v.Size, st.Size)
	}
	call(t, cs, "job_start", startArgs(func(a map[string]any) {
		a["size"] = "large"
		a["repo"] = repo
		a["covers"] = []string{"R01"}
	}), &v)
	if v.Size != "large" {
		t.Fatalf("large: %q", v.Size)
	}
	if msg := callErr(t, cs, "job_start", startArgs(func(a map[string]any) { a["size"] = "large"; a["repo"] = repo })); !strings.Contains(msg, "size large needs covers") {
		t.Fatalf("large without covers: %q", msg)
	}
	if msg := callErr(t, cs, "job_start", startArgs(func(a map[string]any) { a["size"] = "huge" })); !strings.Contains(msg, "must be small or large") {
		t.Fatalf("bad size: %q", msg)
	}
	// Without size: allowed while size_required is off; inferred from covers (owner decision 2026-10-25).
	var none, covered JobView
	call(t, cs, "job_start", startArgs(nil), &none)
	if none.ID == "" || none.Size != "small" {
		t.Fatalf("no size: %+v", none)
	}
	call(t, cs, "job_start", startArgs(func(a map[string]any) { a["repo"] = repo; a["covers"] = []string{"R01"} }), &covered)
	if covered.Size != "large" {
		t.Fatalf("no size, covers: %+v", covered)
	}
}

// size_required is a hot key: written to config.json, the next job_start refuses a general job without
// size, and a review job is still exempt.
func TestJobStartSizeRequired(t *testing.T) {
	m := newManager(t)
	s, err := NewServer(RoleOrchestrator, Deps{Manager: m, Watch: NewServerWatch(m)})
	if err != nil {
		t.Fatal(err)
	}
	cs := connect(t, s)
	var v JobView
	call(t, cs, "job_start", startArgs(nil), &v)
	if err := os.WriteFile(filepath.Join(m.StateDir, "config.json"), []byte(`{"size_required": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if msg := callErr(t, cs, "job_start", startArgs(nil)); !strings.Contains(msg, "size is required for a general job") {
		t.Fatalf("size_required on: %q", msg)
	}
	call(t, cs, "job_start", startArgs(func(a map[string]any) { a["kind"] = "review" }), &v)
	call(t, cs, "job_start", startArgs(func(a map[string]any) { a["size"] = "small" }), &v)
}
