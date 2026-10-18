package brain

import (
	"strings"
	"testing"
)

// The real brain answers a note write with {"result": "<word>: <path>"}; a path field also works;
// anything else, or a path that leaves the vault, is refused.
func TestParseWriteResult(t *testing.T) {
	for _, tc := range []struct {
		in   Object
		verb string
		path string
	}{
		{Object{"result": "created: 80-error-patterns/2026-10-10-infra-risks-flagged-20261010.md"}, "created", "80-error-patterns/2026-10-10-infra-risks-flagged-20261010.md"},
		{Object{"result": "exists: 30-decisions/a.md"}, "exists", "30-decisions/a.md"},
		{Object{"result": " updated: 70-runbooks/b_c.md\n"}, "updated", "70-runbooks/b_c.md"},
		{Object{"path": "30-decisions/a.md", "title": "x"}, "", "30-decisions/a.md"},
	} {
		r, err := ParseWriteResult(tc.in)
		if err != nil || r.Verb != tc.verb || r.Path != tc.path {
			t.Fatalf("%v: %+v %v", tc.in, r, err)
		}
	}
	for _, in := range []Object{
		nil,
		{},
		{"result": "ok"},
		{"result": 42.0},
		{"result": "created 30-decisions/a.md"},
		{"result": "Created: 30-decisions/a.md"},
		{"result": "created: 30-decisions/a.txt"},
		{"result": "created: ../secrets/a.md"},
		{"result": "created: 30-decisions/../../a.md"},
		{"result": "created: /etc/a.md"},
		{"result": "created: a b.md"},
		{"result": "created: 30-decisions//a.md"},
		{"result": "created: 30-decisions/a.md; rm"},
		{"result": "created: " + strings.Repeat("a", 600) + ".md"},
		{"path": "../a.md"},
		{"path": ""},
		{"path": 3.0, "result": "nonsense"},
	} {
		if r, err := ParseWriteResult(in); err == nil {
			t.Fatalf("%v accepted: %+v", in, r)
		}
	}
}
