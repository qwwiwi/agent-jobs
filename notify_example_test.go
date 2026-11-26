package agentjobs

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The example notify.command (examples/notify/loopback-webhook.sh) posts its stdin, byte for byte,
// to a loopback webhook with the token of the token file, and refuses a URL that is not loopback.
func TestNotifyExampleLoopbackWebhook(t *testing.T) {
	for _, bin := range []string{"bash", "curl"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
	script, err := filepath.Abs("examples/notify/loopback-webhook.sh")
	if err != nil {
		t.Fatal(err)
	}
	var auth, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		auth, body = r.Header.Get("Authorization"), string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("example-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const mark = "aj-example-pwned-marker"
	line := "[jobs:job-done] job x $(touch " + mark + ") `touch " + mark + "` \"q\" 'x'\n"
	run := func(url, tokenFile string) (string, error) {
		cmd := exec.Command(script, url, tokenFile)
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader(line)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run(srv.URL+"/hook", token); err != nil {
		t.Fatalf("loopback delivery: %v\n%s", err, out)
	}
	if body != line || auth != "Bearer example-token" {
		t.Fatalf("webhook got auth %q body %q", auth, body)
	}
	if _, err := os.Stat(filepath.Join(dir, mark)); err == nil {
		t.Fatal("shell syntax of the line was executed")
	}
	body = ""
	if out, err := run("http://example.com/hook", token); err == nil || body != "" {
		t.Fatalf("non-loopback URL accepted: %s", out)
	}
	if out, err := run(srv.URL+"/hook", filepath.Join(dir, "missing")); err == nil || body != "" {
		t.Fatalf("missing token file accepted: %s", out)
	}
}
