// Package agentjobs embeds the templates shipped with the repo: prompts, owner rules, the project card
// template and example, and the agent-jobs skill.
package agentjobs

import "embed"

// Templates holds prompts/*.md, rules/*.md, cards/*.md and skills/agent-jobs/SKILL.md.
//
//go:embed prompts/*.md rules/*.md cards/*.md skills/agent-jobs/SKILL.md
var Templates embed.FS
