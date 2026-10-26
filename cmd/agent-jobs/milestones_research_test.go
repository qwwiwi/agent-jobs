package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/checklist"
	"github.com/qwwiwi/agent-jobs/internal/mcp"
)

// decisionRow is the DECISIONS.md table row whose bold title starts with title ("" when there is none).
func decisionRow(t *testing.T, title string) string {
	t.Helper()
	for _, line := range strings.Split(readRepoDoc(t, "docs/DECISIONS.md"), "\n") {
		if strings.HasPrefix(line, "| ") && strings.Contains(line, "| **"+title) {
			return line
		}
	}
	return ""
}

// The naming question (rename the checklist to milestones?) is decided and recorded, and the decision
// holds in the code: the checklist keeps its file and tools, the milestones have tools of their own.
func TestMilestoneNamingDecided(t *testing.T) {
	t.Run("[R13] the naming research is recorded: the checklist keeps its name, milestones are stages of work", func(t *testing.T) {
		row := decisionRow(t, "Naming: the checklist keeps its name; stages of work are milestones.")
		if row == "" {
			t.Fatal("DECISIONS.md has no naming row")
		}
		for _, want := range []string{"no rename", "stays the acceptance list", "a stage of work made of tasks", "GUIDE.md#milestones"} {
			if !strings.Contains(row, want) {
				t.Errorf("the naming row lacks %q: %s", want, row)
			}
		}
		if checklist.ChecklistRel != "brief/CHECKLIST.md" || !strings.Contains(row, "`"+checklist.ChecklistRel+"`") {
			t.Errorf("the checklist file is %s; the row must name it: %s", checklist.ChecklistRel, row)
		}
		for _, name := range []string{"checklist_init", "checklist_get", "checklist_set", "milestone_create", "milestone_set",
			"milestone_get", "milestone_list"} {
			if !slices.Contains(mcp.OrchestratorOnly, name) {
				t.Errorf("the server has no tool %s: the decision keeps both the checklist and the milestone tools", name)
			}
		}
	})
}

// The research into how established tools organise milestones is summarised in DECISIONS.md: the job,
// the systems studied and the outcome taken into the build.
func TestMilestoneResearchRecorded(t *testing.T) {
	t.Run("[R14] the big-tech milestone research and its outcome are recorded in DECISIONS", func(t *testing.T) {
		row := decisionRow(t, "Milestones research: how established tools organise milestones")
		if row == "" {
			t.Fatal("DECISIONS.md has no milestones research row")
		}
		for _, want := range []string{"261025-191825-00b4", "GitHub", "GitLab", "Linear", "Jira", "spec-kit", "CrewAI", "task-master",
			"Outcome", "acceptance list", "stages of work"} {
			if !strings.Contains(row, want) {
				t.Errorf("the research row lacks %q: %s", want, row)
			}
		}
		// The research row comes before the approval of the build it fed.
		doc := readRepoDoc(t, "docs/DECISIONS.md")
		if r, a := strings.Index(doc, row), strings.Index(doc, "**Milestones build approved by the owner.**"); a < 0 || r > a {
			t.Errorf("the research row is not recorded before the milestones build approval (%d, %d)", r, a)
		}
	})
}
