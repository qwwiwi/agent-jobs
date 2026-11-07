package mcp

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// restackStack makes main <- squash of P, and C stacked on P, in a real repository.
func restackStack(t *testing.T, merged bool) (string, func(args ...string) string) {
	t.Helper()
	repo, _ := gitRepo(t)
	g := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=T", "-c", "user.email=t@example.com"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(file string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, file), []byte(file+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		g("add", "-A")
		g("commit", "-q", "-m", "add "+file)
	}
	g("checkout", "-q", "-b", "P")
	commit("p.txt")
	g("checkout", "-q", "-b", "C")
	commit("c.txt")
	g("checkout", "-q", "main")
	if merged {
		g("merge", "--squash", "P")
		g("commit", "-q", "-m", "squash P")
	}
	return repo, g
}

// branch_restack is orchestrator-only; it creates the restacked branch and returns its name, head and
// the commits carried; a refusal is a tool error with the reason and leaves no ref.
func TestBranchRestackTool(t *testing.T) {
	if !slices.Contains(OrchestratorOnly, "branch_restack") || slices.Contains(JobAllowed, "branch_restack") || Allowed(RoleJob, "branch_restack") {
		t.Fatal("branch_restack must be orchestrator-only")
	}
	m := newManager(t)
	cs := connect(t, mustServer(t, m))

	repo, g := restackStack(t, true)
	var out jobs.RestackResult
	call(t, cs, "branch_restack", map[string]any{"repo": repo, "child": "C", "parent": "P"}, &out)
	if out.Branch != "C-restacked" || out.Commits != 1 || out.Head != g("rev-parse", "C-restacked") {
		t.Fatalf("result: %+v", out)
	}
	if got := g("log", "--format=%s", "main..C-restacked"); got != "add c.txt" {
		t.Fatalf("commits on main: %q", got)
	}
	// The name is taken now: refused with the reason, the branch is where it was.
	if msg := callErr(t, cs, "branch_restack", map[string]any{"repo": repo, "child": "C", "parent": "P"}); !strings.Contains(msg, "branch already exists") {
		t.Fatalf("existing name: %q", msg)
	}
	if g("rev-parse", "C-restacked") != out.Head {
		t.Fatal("existing branch moved")
	}

	repo, g = restackStack(t, false)
	before := g("for-each-ref")
	if msg := callErr(t, cs, "branch_restack", map[string]any{"repo": repo, "child": "C", "parent": "P", "name": "x"}); !strings.Contains(msg, "not contained") {
		t.Fatalf("unmerged parent: %q", msg)
	}
	if g("for-each-ref") != before {
		t.Fatal("a ref appeared on a refusal")
	}
	if msg := callErr(t, cs, "branch_restack", map[string]any{"child": "C", "parent": "P"}); !strings.Contains(msg, "repo") {
		t.Fatalf("missing repo: %q", msg)
	}
}
