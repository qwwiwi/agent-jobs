package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/skills"
)

// SkillListInput is skill_list's input.
type SkillListInput struct {
	Query  string `json:"query,omitempty" jsonschema:"words to search for in the skills' names, descriptions, kinds, projects and SKILL.md text (any language); returns the best 10 by score. Omit to list"`
	Kind   string `json:"kind,omitempty" jsonschema:"a job kind (review | fix | research | general) or a catalog kind such as code"`
	Repo   string `json:"repo,omitempty" jsonschema:"repository path or folder name: only skills whose projects include it"`
	Status string `json:"status,omitempty" jsonschema:"core | active | draft | deprecated; default: core and active (a search never returns deprecated)"`
}

func addSkillList(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "skill_list",
		Description: "The skill catalog (config skill_roots) for choosing a job's skills: name, status, one-line " +
			"description, kinds, size_kb, risk, last_verified, uses and success rate from job stats, and a stale flag " +
			"(not verified for 90 days or not used for 60). With query: a local content search (no model call), the best " +
			"10 with their score. A job gets the core skills of its kind and the repository's project skills by default; " +
			"pass job_start skills [\"+name\", \"-name\"] to change that. warnings name catalog roots or entries that did not load.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in SkillListInput) (*sdk.CallToolResult, jobs.SkillListOutput, error) {
		out, err := d.Manager.SkillList(ctx, in.Query, skills.Filter{Kind: in.Kind, Repo: in.Repo, Status: in.Status})
		return nil, out, err
	})
}
