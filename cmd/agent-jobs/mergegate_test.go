package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/checklist"
	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// bashEvent is a PreToolUse hook input for a Bash call.
func bashEvent(cwd, command string) string {
	b, _ := json.Marshal(map[string]any{"session_id": "s1", "hook_event_name": "PreToolUse", "cwd": cwd, "tool_name": "Bash",
		"tool_input": map[string]string{"command": command}})
	return string(b)
}

// gatedRepo is a git repository with an approved checklist (stored for agent test) and a release_check pass
// on its head; it returns the repository and the head sha.
func gatedRepo(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.name", "T"}, {"config", "user.email", "t@example.com"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	repo, _ = filepath.EvalSymlinks(repo)
	if err := os.Mkdir(filepath.Join(repo, "brief"), 0o755); err != nil {
		t.Fatal(err)
	}
	brief := "# Brief\n\nI want a login page.\n"
	list := "# Checklist\n\n- id: demo\n- brief: brief/BRIEF.md\n- version: 2\n- approved: no\n\n" + checklist.TableHeader +
		"\n|---|---|---|---|\n| R01 | a login page | live | brief |\n"
	if err := os.WriteFile(checklist.BriefPath(repo), []byte(brief), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(checklist.ChecklistPath(repo), []byte(list), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, repo, "add", ".")
	gitOut(t, repo, "commit", "-qm", "brief")
	head := strings.TrimSpace(gitOut(t, repo, "rev-parse", "HEAD"))
	c, err := checklist.Parse(list)
	if err != nil {
		t.Fatal(err)
	}
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	hash := checklist.Hash(c.Rows, []byte(brief))
	if err := m.Store.PutApproval(store.ChecklistApproval{Repo: repo, ChecklistID: c.ID, Version: c.Version, ChecklistSHA256: hash,
		BriefSHA256: checklist.BriefPrefixSHA256([]byte(brief)), Quote: "yes", ApprovedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.PutReleasePass(store.ReleasePass{Repo: repo, SHA: head, ChecklistID: c.ID, Version: c.Version,
		ChecklistSHA256: hash, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return repo, head
}

// `hook merge-gate` exits 0 to allow a Bash call and 2 to block it with one line on stderr: a merge of
// the passed sha passes, another sha or none is blocked; a call that runs no merge passes before the
// store is opened (even misconfigured); a merge it cannot check is blocked; a merge it cannot read runs
// with a warning as hook JSON on stdout (exit 0).
func TestHookMergeGate(t *testing.T) {
	t.Setenv("AGENT_JOBS_HOME", t.TempDir())
	t.Setenv("GH_REPO", "")
	repo, head := gatedRepo(t)
	gate := []string{"merge-gate", "--agent", "test"}
	run := func(args []string, stdin string) (int, string, string) {
		var out, errb bytes.Buffer
		code := runHook(args, strings.NewReader(stdin), &out, &errb, open)
		return code, out.String(), errb.String()
	}
	other := strings.Repeat("b", 40)
	for _, tc := range []struct {
		name  string
		args  []string
		stdin string
		code  int
		err   string
	}{
		{"passed sha", gate, bashEvent(repo, "gh pr merge 5 --squash --match-head-commit "+head), 0, ""},
		{"other sha", gate, bashEvent(repo, "gh pr merge 5 --squash --match-head-commit "+other), 2, "release_check has not passed on " + other},
		{"no flag", gate, bashEvent(repo, "git fetch && gh pr merge 5"), 2, "without --match-head-commit"},
		{"no merge", gate, bashEvent(repo, "ls"), 0, ""},
		{"another tool", gate, `{"hook_event_name":"PreToolUse","tool_name":"Read","tool_input":{"file_path":"/x"}}`, 0, ""},
		{"no repo", gate, bashEvent(t.TempDir(), "gh pr merge 5"), 0, ""},
		{"no merge, wrong agent", []string{"merge-gate", "--agent", "nope"}, bashEvent(repo, "ls"), 0, ""},
		{"merge, wrong agent", []string{"merge-gate", "--agent", "nope"}, bashEvent(repo, "gh pr merge 5"), 2, "cannot check the merge"},
		{"merge, no agent", []string{"merge-gate"}, bashEvent(repo, "gh pr merge 5"), 2, "--agent is required"},
		{"merge, positional", []string{"merge-gate", "--agent", "test", "x"}, bashEvent(repo, "gh pr merge 5"), 2, "misconfigured"},
		{"bad json", gate, "{", 2, "unreadable hook input"},
	} {
		code, out, errs := run(tc.args, tc.stdin)
		if code != tc.code || out != "" || (tc.err == "") != (errs == "") || !strings.Contains(errs, tc.err) ||
			strings.Count(errs, "\n") > 1 {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", tc.name, code, out, errs)
		}
		if errs != "" && !strings.HasPrefix(errs, jobs.MergeGatePrefix) {
			t.Errorf("%s: stderr %q", tc.name, errs)
		}
	}
	// An unreadable merge runs with its warning as hook JSON on stdout (TestHookMergeGateWarningIsHookJSON).
	if code, out, errs := run(gate, bashEvent(repo, "sh -c 'gh pr merge 5'")); code != 0 || errs != "" ||
		!strings.Contains(out, `"additionalContext":"`+jobs.MergeGateWarnPrefix+"cannot tell whether") {
		t.Errorf("unreadable merge: exit %d, stdout %q, stderr %q", code, out, errs)
	}
}

// Inside a job (the job role) `hook merge-gate` is a no-op: exit 0, no output, no state opened.
func TestHookMergeGateInsideJob(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "agent-jobs")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	home := t.TempDir()
	cmd := exec.Command(bin, "hook", "merge-gate", "--agent", "test")
	cmd.Env = append(os.Environ(), "AGENT_JOBS_HOME="+home, jobs.JobEnvVar+"=review-probe")
	cmd.Stdin = strings.NewReader(bashEvent("/", "gh pr merge 5"))
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatal(err)
		}
		t.Fatalf("exit %d, stderr %q", ee.ExitCode(), errb.String())
	}
	if out.Len() != 0 || errb.Len() != 0 {
		t.Fatalf("stdout %q, stderr %q", out.String(), errb.String())
	}
	if left, err := os.ReadDir(home); err != nil || len(left) != 0 {
		t.Fatalf("state opened inside a job: %v %v", left, err)
	}
}
