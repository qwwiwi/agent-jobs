package httpapi

import (
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

func TestJobViewResultBranch(t *testing.T) {
	j := &store.Job{ID: "job", RequestedBranch: "feature", Branch: "job/123",
		BranchNote: "requested branch feature not moved: not a fast-forward"}
	v := ViewOf(j)
	if v.RequestedBranch != j.RequestedBranch || v.Branch != j.Branch || v.BranchNote != j.BranchNote {
		t.Fatalf("result branch omitted: %+v", v)
	}
}
