// Package agentjobs embeds the prompt and rules templates shipped with the repo.
package agentjobs

import "embed"

// Templates holds prompts/*.md and rules/*.md.
//
//go:embed prompts/*.md rules/*.md
var Templates embed.FS
