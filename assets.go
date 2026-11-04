// Package agentjobs embeds the templates shipped with the repo: prompts, owner rules, the project card
// template and example, the agent-jobs skill and the browser check script jobs get a copy of.
package agentjobs

import "embed"

// Templates holds prompts/*.md, rules/*.md, cards/*.md, skills/agent-jobs/SKILL.md and scripts/ui-check.mjs.
//
//go:embed prompts/*.md rules/*.md cards/*.md skills/agent-jobs/SKILL.md scripts/ui-check.mjs
var Templates embed.FS
