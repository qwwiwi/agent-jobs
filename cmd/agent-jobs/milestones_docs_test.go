package main

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/mcp"
)

func readRepoDoc(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../../" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// docSection is the text of doc from the line heading up to the next heading of the same or a higher
// level outside a code fence ("" when doc has no such heading).
func docSection(doc, heading string) string {
	_, rest, ok := strings.Cut(doc, "\n"+heading+"\n")
	if !ok {
		return ""
	}
	level := len(heading) - len(strings.TrimLeft(heading, "#"))
	lines := strings.Split(rest, "\n")
	fenced := false
	for i, line := range lines {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
		}
		n := len(line) - len(strings.TrimLeft(line, "#"))
		if !fenced && n > 0 && n <= level && strings.HasPrefix(line[n:], " ") {
			return strings.Join(lines[:i], "\n")
		}
	}
	return rest
}

// milestoneCommands are the milestone commands of the CLI as its usage text lists them ("milestones",
// "milestone", "milestone create", "milestone set").
func milestoneCommands(usage string) []string {
	var out []string
	for _, line := range strings.Split(usage, "\n") {
		f := strings.Fields(line)
		if !strings.HasPrefix(line, "  milestone") || len(f) == 0 {
			continue
		}
		cmd := f[0]
		if len(f) > 1 && !strings.HasPrefix(f[1], "-") {
			cmd += " " + f[1]
		}
		out = append(out, cmd)
	}
	return out
}

// The milestone docs are English and complete against the code: README, GUIDE, PLAN and the decision
// hold no Cyrillic (the Russian copies are separate files), GUIDE and the skill name every milestone MCP
// tool the server registers, GUIDE every milestone command of the CLI, and the decision keeps «no
// noticeable latency» as a constraint.
func TestMilestoneDocsInEnglish(t *testing.T) {
	t.Run("[R09i] README, GUIDE, PLAN and DECISIONS describe milestones in English, every tool and command included", func(t *testing.T) {
		cyrillic := regexp.MustCompile(`\p{Cyrillic}`)
		decisions := readRepoDoc(t, "docs/DECISIONS.md")
		var approved string
		for _, line := range strings.Split(decisions, "\n") {
			if strings.Contains(line, "**Milestones build approved by the owner.**") {
				approved = line
			}
		}
		if !strings.Contains(approved, "no noticeable latency on job_start or the daemon tick") {
			t.Errorf("DECISIONS.md: the milestones decision lacks the latency constraint: %q", approved)
		}
		guide := docSection(readRepoDoc(t, "docs/GUIDE.md"), "### Milestones")
		cli := docSection(readRepoDoc(t, "docs/GUIDE.md"), "## Install and configure")
		skill := docSection(readRepoDoc(t, "skills/agent-jobs/SKILL.md"), "## Other tools") + docSection(readRepoDoc(t, "skills/agent-jobs/SKILL.md"), "## Milestones")
		sections := []struct{ name, text string }{
			{"README.md ## Milestones", docSection(readRepoDoc(t, "README.md"), "## Milestones: stages, tasks, jobs")},
			{"GUIDE.md ### Milestones", guide},
			{"PLAN.md ## Milestones phase 2", docSection(readRepoDoc(t, "docs/PLAN.md"), "## Milestones phase 2 (approved, not built)")},
			{"DECISIONS.md milestones row", approved},
		}
		for _, c := range sections {
			if !strings.Contains(c.text, "milestone") {
				t.Errorf("%s: no milestones section", c.name)
			}
			if w := cyrillic.FindString(c.text); w != "" {
				t.Errorf("%s: not English (%q)", c.name, w)
			}
		}
		var tools []string
		for _, name := range mcp.OrchestratorOnly {
			if strings.HasPrefix(name, "milestone_") {
				tools = append(tools, name)
			}
		}
		if len(tools) < 4 {
			t.Fatalf("milestone tools of the server: %v", tools)
		}
		for _, name := range tools {
			if !strings.Contains(guide, "`"+name+"`") {
				t.Errorf("GUIDE ### Milestones does not name the tool %s", name)
			}
			if !strings.Contains(skill, "`"+name+"`") {
				t.Errorf("SKILL.md does not name the tool %s", name)
			}
		}
		cmds := milestoneCommands(usage)
		if len(cmds) < 4 {
			t.Fatalf("milestone commands in the usage: %v", cmds)
		}
		for _, cmd := range cmds {
			if !strings.Contains(guide, "`agent-jobs "+cmd+" --agent A") {
				t.Errorf("GUIDE ### Milestones does not document `agent-jobs %s`", cmd)
			}
			if !strings.Contains(cli, "agent-jobs "+cmd+" --agent my-agent") {
				t.Errorf("GUIDE's CLI block has no example of `agent-jobs %s`", cmd)
			}
		}
	})
}
