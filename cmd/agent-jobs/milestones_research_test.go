package main

import (
	"regexp"
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

// tableRow is the Markdown table row of doc whose first cell is exactly cell ("" when there is none).
func tableRow(doc, cell string) string {
	for _, line := range strings.Split(doc, "\n") {
		first, _, ok := strings.Cut(strings.TrimPrefix(line, "|"), "|")
		if strings.HasPrefix(line, "|") && ok && strings.TrimSpace(first) == cell {
			return line
		}
	}
	return ""
}

// The research into how established tools organise milestones is summarised in docs/research/milestones.md
// and linked from its DECISIONS.md row: a concrete finding per system studied, the conclusions, the
// outcome taken into the build, what was not verified and the sources.
func TestMilestoneResearchRecorded(t *testing.T) {
	t.Run("[R14] the big-tech milestone research, its findings and its outcome are recorded in docs", func(t *testing.T) {
		row := decisionRow(t, "Milestones research: how established tools organise milestones")
		if row == "" {
			t.Fatal("DECISIONS.md has no milestones research row")
		}
		for _, want := range []string{"261025-191825-00b4", "GitHub", "GitLab", "Linear", "Jira", "spec-kit", "CrewAI", "task-master",
			"Outcome", "acceptance list", "stages of work", "](research/milestones.md)"} {
			if !strings.Contains(row, want) {
				t.Errorf("the research row lacks %q: %s", want, row)
			}
		}
		// The research row comes before the approval of the build it fed.
		doc := readRepoDoc(t, "docs/DECISIONS.md")
		if r, a := strings.Index(doc, row), strings.Index(doc, "**Milestones build approved by the owner.**"); a < 0 || r > a {
			t.Errorf("the research row is not recorded before the milestones build approval (%d, %d)", r, a)
		}

		research := readRepoDoc(t, "docs/research/milestones.md")
		if w := regexp.MustCompile(`\p{Cyrillic}`).FindString(research); w != "" {
			t.Errorf("docs/research/milestones.md is not English (%q)", w)
		}
		// Each system studied has a table row with the concrete finding the research made about it.
		findings := []struct {
			system string
			want   []string
		}{
			{"GitHub", []string{"group of issues and pull requests", "blocked-by / blocking", "shown but not enforced", "formula not documented"}},
			{"GitLab", []string{"timebox", "closed / total", "weight is not counted", "warning only"}},
			{"Linear", []string{"stage in a project's lifecycle", "a started issue counts partially", "shown in red"}},
			{"Jira", []string{"Fix version", "Epic", "acceptance criteria sit on the story", "To Do / In Progress / Done"}},
			{"GitHub spec-kit", []string{"`[P]`", "different files and no dependencies", "checkpoint at every phase"}},
			{"CrewAI", []string{"`context=[tasks]`", "full raw output", "guardrail"}},
			{"claude-task-master", []string{"explicit dependencies with a cycle check", "`next` returns one ready task", "no parallelism"}},
		}
		for _, f := range findings {
			line := tableRow(research, f.system)
			if line == "" {
				t.Errorf("docs/research/milestones.md has no table row for %s", f.system)
				continue
			}
			for _, want := range f.want {
				if !strings.Contains(line, want) {
					t.Errorf("the %s row lacks the finding %q: %s", f.system, want, line)
				}
			}
		}
		sections := []struct {
			heading string
			want    []string
		}{
			{"## Mainstream trackers (verified against sources)", []string{"Acceptance criteria are not a milestone",
				"conditions that a ... user story ... must satisfy to be complete", "edges between tasks", "share of closed tasks"}},
			{"## OSS agent orchestrators (verified against README and code)", []string{"only in spec-kit",
				"a short summary in the brief, the full report as a file"}},
			{"## Outcome and recommendation [inference]", []string{"does not rename the checklist", "acceptance list",
				"stage of work under the brief", "`depends_on`", "`covers`", "short summary", "full report", "`prior/<key>/report.md`"}},
			{"## Not verified", []string{"GitHub progress formula", "Jira", "Google", "TodoWrite"}},
			{"## Sources", []string{"docs.github.com", "docs.gitlab.com/user/project/milestones/", "linear.app/docs/project-milestones",
				"support.atlassian.com", "docs.crewai.com/en/concepts/tasks", "github/spec-kit", "task-master"}},
		}
		for _, s := range sections {
			text := docSection(research, s.heading)
			if text == "" {
				t.Errorf("docs/research/milestones.md has no section %q", s.heading)
				continue
			}
			for _, want := range s.want {
				if !strings.Contains(text, want) {
					t.Errorf("section %q lacks %q", s.heading, want)
				}
			}
		}
	})
}
