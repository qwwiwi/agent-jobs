package agentjobs

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Owner requirement 2026-11-03: the orchestrator never accepts or deploys a UI job without browser
// evidence, runs the check itself before merge or deploy, and again on the live URL after a frontend deploy.
func TestOrchestratorUIRule(t *testing.T) {
	for _, name := range []string{"prompts/orchestrator.md", "skills/agent-jobs/SKILL.md"} {
		b, err := Templates.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		for _, want := range []string{
			"UI job", "never accept or deploy", "`BROWSER CHECK NOT RUN`", "browser evidence",
			"ui-check.mjs", "before merge or deploy", "live URL", "after a frontend deploy",
		} {
			if !strings.Contains(s, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
	}
}

// The script a job runs is the one shipped with the binary (copied to <job dir>/tools/ui-check.mjs).
func TestUICheckScriptEmbedded(t *testing.T) {
	disk, err := os.ReadFile("scripts/ui-check.mjs")
	if err != nil {
		t.Fatal(err)
	}
	emb, err := Templates.ReadFile("scripts/ui-check.mjs")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(disk, emb) {
		t.Fatal("embedded scripts/ui-check.mjs differs from the file")
	}
}

// scripts/ui-check.mjs parses, and its pure logic (argument parsing, word segmentation, line grouping of
// a word's client rects, the CLS session window, the stick-out rule, the in-page probe it builds) passes
// scripts/ui-check.test.mjs under node --test. Static: no browser starts here (check.sh runs without one;
// Playwright is resolved only by a real run). Skipped when node is not installed.
func TestUICheckScriptNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	for _, args := range [][]string{
		{"--check", "scripts/ui-check.mjs"},
		{"--test", "scripts/ui-check.test.mjs"},
	} {
		cmd := exec.Command(node, args...)
		cmd.Env = append(os.Environ(), "NODE_OPTIONS=")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("node %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
}
