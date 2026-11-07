package agentjobs

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Requirements: R05 — a PR title becomes a squash subject, so validate it before publishing.
func TestCheckPRTitle(t *testing.T) {
	for _, tc := range []struct {
		title string
		ok    bool
	}{
		{"Add changelog fragments (#197)", true},
		{"Release v0.7.0 (#202)", true},
		{"Fix user's UTF-8 handling — safely", true},
		{"Добавил проверку лимитов", false},
		{"Fix limits: тело", false},
		{"Release v0.7.0 (#$(basename https://github.com/qwwiwi/agent-jobs/pull/202))", false},
		{"Release `version`", false},
		{"", false},
		{"   ", false},
		{"Add feature\nSecond line", false},
	} {
		t.Run(tc.title, func(t *testing.T) {
			cmd := exec.Command("bash", "scripts/check-pr-title.sh", tc.title)
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.ok {
				t.Fatalf("accepted=%v, want %v: %s", err == nil, tc.ok, out)
			}
		})
	}
}

func TestCheckPRTitleEnvironment(t *testing.T) {
	for _, tc := range []struct {
		env string
		ok  bool
	}{
		{"PR_TITLE=English title", true},
		{"PR_TITLE=Заголовок", false},
		{"PR_TITLE=", false},
	} {
		cmd := exec.Command("bash", "scripts/check-pr-title.sh")
		cmd.Env = append(os.Environ(), tc.env)
		if out, err := cmd.CombinedOutput(); (err == nil) != tc.ok {
			t.Fatalf("%s accepted=%v, want %v: %s", tc.env, err == nil, tc.ok, out)
		}
	}
}

func TestCheckScriptPRTitleGate(t *testing.T) {
	root, env := fakeCheckTree(t, false)
	for _, title := range []string{"Заголовок", "Release $(version)", "Release `version`", ""} {
		out, code, _ := runFakeCheck(t, root, env, "PR_TITLE="+title)
		if code == 0 {
			t.Fatalf("bad PR title passed check.sh: %q\n%s", title, out)
		}
	}
	if out, code, _ := runFakeCheck(t, root, env, "PR_TITLE=English title"); code != 0 {
		t.Fatalf("English title refused: %s", out)
	}
}

func TestCheckPRTitleFunction(t *testing.T) {
	script, err := filepath.Abs("scripts/check-pr-title.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		title string
		ok    bool
	}{{"English title", true}, {"Release $(version)", false}, {"Заголовок", false}} {
		cmd := exec.Command("bash", "-c", `source "$1"; check_pr_title "$2"`, "title-test", script, tc.title)
		if out, err := cmd.CombinedOutput(); (err == nil) != tc.ok {
			t.Fatalf("function accepted=%v, want %v: %s", err == nil, tc.ok, out)
		}
	}
}

func TestCheckScriptChangelogGate(t *testing.T) {
	root, env := fakeCheckTree(t, false)
	writeFile(t, filepath.Join(root, "fakebin/go"), "#!/bin/sh\ncase \"$*\" in\n  'run ./cmd/changelog --validate') echo 'invalid fragment' >&2; exit 1;;\nesac\nexit 0\n", 0o755)
	if out, code, _ := runFakeCheck(t, root, env); code == 0 {
		t.Fatalf("fragment validation was skipped: %s", out)
	}
}
