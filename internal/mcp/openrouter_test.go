package mcp

import (
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// R7 (issue #136): job_start and job_list show provider and billing; an openrouter job_start shows the
// daily cap's state; claude and codex jobs keep billing subscription.
func TestJobViewProviderOpenRouter(t *testing.T) {
	m := newManager(t)
	capUSD := 20.0
	m.Cfg.Providers.OpenRouter = &jobs.OpenRouterConfig{BaseURL: jobs.DefaultOpenRouterBaseURL, KeyFile: "/k/openrouter.env",
		Models: []string{"z-ai/glm-5.3"}, DailyCapUSD: &capUSD}
	cs := connect(t, mustServer(t, m))
	var or, cl, cx JobView
	call(t, cs, "job_start", startArgs(func(a map[string]any) { a["model"] = "openrouter:z-ai/glm-5.3" }), &or)
	if or.Provider != "openrouter" || or.Billing != "api" || or.Runner != "claude" || or.Model != "openrouter:z-ai/glm-5.3" ||
		or.OpenRouterSpentToday != "$10.00/$20.00" {
		t.Fatalf("openrouter job_start: %+v", or)
	}
	call(t, cs, "job_start", startArgs(nil), &cl)
	if cl.Provider != "anthropic" || cl.Billing != "subscription" || cl.OpenRouterSpentToday != "" {
		t.Fatalf("claude job_start: %+v", cl)
	}
	call(t, cs, "job_start", startArgs(func(a map[string]any) { a["model"] = "gpt-5.5"; a["kind"] = "review" }), &cx)
	if cx.Provider != "openai" || cx.Billing != "subscription" || cx.Runner != "codex" {
		t.Fatalf("codex job_start: %+v", cx)
	}
	var list JobListOutput
	call(t, cs, "job_list", map[string]any{}, &list)
	got := map[string]string{}
	for _, j := range list.Jobs {
		got[j.ID] = j.Provider + "/" + j.Billing + "/" + j.Model
	}
	if got[or.ID] != "openrouter/api/openrouter:z-ai/glm-5.3" || got[cl.ID] != "anthropic/subscription/sonnet" ||
		got[cx.ID] != "openai/subscription/gpt-5.5" {
		t.Fatalf("job_list: %v", got)
	}
}
