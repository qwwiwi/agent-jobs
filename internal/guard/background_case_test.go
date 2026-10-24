package guard

import (
	"encoding/json"
	"strings"
	"testing"
)

// encoding/json matches struct fields ignoring case and keeps the last key, while claude reads the
// exact key: a case variant set to false after run_in_background must not hide a background call.
func TestGateRefusesBackgroundBashCaseVariants(t *testing.T) {
	l := newReadAllowLayout(t)
	decideRaw := func(cfg Config, input string) string {
		t.Helper()
		cwd, _ := json.Marshal(l.work)
		payload := `{"tool_name":"Bash","cwd":` + string(cwd) + `,"tool_input":` + input + `}`
		r, err := Decide(strings.NewReader(payload), cfg)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, input := range []string{
		`{"command":"go test ./...","run_in_background":true,"Run_In_Background":false}`,
		`{"command":"go test ./...","run_in_background":true,"RUN_IN_BACKGROUND":null}`,
		`{"command":"go test ./...","Run_In_Background":true,"run_in_background":false}`,
		`{"command":"go test ./...","run_in_BACKGROUND":"yes"}`,
		`{"command":"go test ./...","run_in_background":1}`,
	} {
		if r := decideRaw(l.cfg, input); !strings.Contains(r, "run_in_background") {
			t.Errorf("gate job, %s: %q, want the background refusal", input, r)
		}
	}
	for _, input := range []string{
		`{"command":"go test ./..."}`,
		`{"command":"go test ./...","run_in_background":false,"Run_In_Background":null}`,
		`{"command":"go test ./...","run_in_background": false }`,
	} {
		if r := decideRaw(l.cfg, input); r != "" {
			t.Errorf("gate job, foreground %s: %s", input, r)
		}
	}
	open := l.cfg
	open.ReadAllow = nil
	if r := decideRaw(open, `{"command":"go test ./...","run_in_background":true,"Run_In_Background":false}`); r != "" {
		t.Errorf("other kinds, background Bash: %s", r)
	}
}
