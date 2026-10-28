package agentjobs

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Every `go test` run of scripts/check.sh sets its own -timeout (issue #97): internal/jobs takes longer
// than go's 10-minute default on a busy host, so the default killed the run with a panic before it ended.
func TestCheckScriptGoTestTimeout(t *testing.T) {
	b, err := os.ReadFile("scripts/check.sh")
	if err != nil {
		t.Fatal(err)
	}
	runs := 0
	for _, line := range strings.Split(string(b), "\n") {
		code := strings.TrimSpace(line)
		if strings.HasPrefix(code, "#") || strings.HasPrefix(code, "echo ") || !strings.Contains(code, "go test ") {
			continue
		}
		runs++
		if !strings.Contains(code, ` -timeout "$GO_TEST_TIMEOUT"`) {
			t.Errorf("go test without the explicit timeout: %s", code)
		}
	}
	if runs != 2 {
		t.Fatalf("found %d go test runs in scripts/check.sh, want 2 (the suite and the manifest-check run)", runs)
	}
	m := regexp.MustCompile(`(?m)^GO_TEST_TIMEOUT="\$\{GO_TEST_TIMEOUT:-([0-9]+)m\}"$`).FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal(`scripts/check.sh does not set GO_TEST_TIMEOUT="${GO_TEST_TIMEOUT:-<n>m}"`)
	}
	if n, _ := strconv.Atoi(m[1]); n < 30 {
		t.Fatalf("default go test timeout %sm, want at least 30m (internal/jobs alone takes ~14m)", m[1])
	}
}
