package mcp

import (
	"context"
	"slices"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/brain"
)

// fakeAdvisor records the call and answers like the daemon's proxy.
type fakeAdvisor struct{ question, background string }

func (f *fakeAdvisor) AdvisorAsk(_ context.Context, question, background string) (brain.Object, error) {
	f.question, f.background = question, background
	return brain.Object{"advice": "take B"}, nil
}

// advisor_ask is listed only in a crew job's server (JobAdvisor set) and goes through it.
func TestAdvisorAskOnlyInCrewJobServer(t *testing.T) {
	plain, err := NewServer(RoleJob, Deps{JobID: "261106-000000-aaaa"})
	if err != nil {
		t.Fatal(err)
	}
	if got := toolNames(t, connect(t, plain)); slices.Contains(got, "advisor_ask") {
		t.Fatalf("plain job lists advisor_ask: %v", got)
	}
	f := &fakeAdvisor{}
	crew, err := NewServer(RoleJob, Deps{JobID: "261106-000000-aaaa", JobAdvisor: f})
	if err != nil {
		t.Fatal(err)
	}
	cs := connect(t, crew)
	if got := toolNames(t, cs); len(got) != len(JobAllowed) || !slices.Contains(got, "advisor_ask") {
		t.Fatalf("crew job lists %v", got)
	}
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "advisor_ask",
		Arguments: map[string]any{"question": "A or B?", "context": "ctx"}})
	if err != nil || res.IsError {
		t.Fatalf("advisor_ask: %+v %v", res, err)
	}
	if f.question != "A or B?" || f.background != "ctx" || !strings.Contains(text(res), "take B") {
		t.Fatalf("advisor_ask went through as %+v, answered %s", f, text(res))
	}
}
