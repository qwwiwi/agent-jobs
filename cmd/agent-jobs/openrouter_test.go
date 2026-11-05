package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func endpointsFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "jobs", "testdata", "openrouter", "z-ai_glm-5.3.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// R9 (issue #136): `openrouter endpoints <slug>` prints the endpoints ranked by the routing policy with
// the pick marked, from a mocked endpoints API (the agent's base_url and policy) or a saved file.
func TestOpenRouterEndpointsCommand(t *testing.T) {
	fixture := endpointsFixture(t)
	var gotAuth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		if r.URL.Path != "/api/v1/models/z-ai/glm-5.3/endpoints" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()
	home := t.TempDir()
	t.Setenv("AGENT_JOBS_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "or"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `{"providers":{"openrouter":{"base_url":"` + srv.URL + `/api","key_file":"/k/or.env","models":["z-ai/glm-5.3"],
		"daily_cap_usd":5,"routing":{"order":["novita"]}}}}`
	if err := os.WriteFile(filepath.Join(home, "or", "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := cmdOpenRouter([]string{"endpoints", "z-ai/glm-5.3", "--agent", "or"}, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out.String(), "\n")
	if !strings.Contains(lines[0], "sort=price min_quantization=fp8 data_collection=deny") || !strings.Contains(lines[0], "order=novita") ||
		!strings.HasPrefix(lines[2], "* novita/fp8") || !strings.Contains(lines[2], "PICKED") || !strings.HasPrefix(lines[3], "  baidu/fp8") {
		t.Fatalf("agent policy table:\n%s", out.String())
	}
	if len(gotAuth) != 1 || gotAuth[0] != "" {
		t.Fatalf("the endpoints API must be called once without a key: %q", gotAuth)
	}
	// Defaults from a saved file, as JSON.
	file := filepath.Join(t.TempDir(), "glm.json")
	if err := os.WriteFile(file, fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	t.Setenv("AGENT_JOBS_AGENT", "")
	if err := cmdOpenRouter([]string{"endpoints", "--file", file, "--json", "z-ai/glm-5.3"}, &out); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Endpoints []struct {
			Tag    string `json:"tag"`
			Picked bool   `json:"picked"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil || len(doc.Endpoints) < 10 || doc.Endpoints[0].Tag != "baidu/fp8" ||
		!doc.Endpoints[0].Picked {
		t.Fatalf("json: %v %s", err, out.String())
	}
	if err := cmdOpenRouter([]string{"models"}, &out); err == nil {
		t.Fatal("unknown subcommand accepted")
	}
}
