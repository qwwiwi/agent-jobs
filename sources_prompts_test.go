package agentjobs

import (
	"strings"
	"testing"
)

// G02: research and review jobs study cloned sources locally, never through the GitHub API or website.
func TestKindPromptsReadLocalSources(t *testing.T) {
	t.Run("[G02] research and review briefs say to read the local copy under sources/", func(t *testing.T) {
		for _, name := range []string{"prompts/kind-research.md", "prompts/kind-review.md"} {
			b, err := Templates.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			s := string(b)
			for _, want := range []string{"local copy under sources/", "GitHub API or website", "Never run it on the host"} {
				if !strings.Contains(s, want) {
					t.Errorf("%s lacks %q", name, want)
				}
			}
		}
	})
}
