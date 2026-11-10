package guard

import (
	"encoding/json"
	"strings"
	"testing"
)

func decideWeb(t *testing.T, tool, url string, allow []string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"tool_name": tool, "cwd": t.TempDir(), "tool_input": map[string]any{"url": url}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := Decide(strings.NewReader(string(b)), Config{WebAllow: allow})
	if err != nil {
		t.Fatalf("Decide(%s %s): %v", tool, url, err)
	}
	return r
}

// G07: with an allowlist the guard lets WebFetch reach only an https URL of an exact allowlisted host.
func TestWebFetchAllowlistTable(t *testing.T) {
	allow := []string{"docs.example.com", "api.example.org"}
	cases := []struct {
		name, url string
		ok        bool
	}{
		{"allowlisted host", "https://docs.example.com/guide/start", true},
		{"second allowlisted host with query", "https://api.example.org/v1/spec?x=1#top", true},
		{"scheme and host case", "HTTPS://Docs.Example.COM/a", true},
		{"trailing dot of the host", "https://docs.example.com./a", true},
		{"explicit default port", "https://docs.example.com:443/a", true},
		{"other host", "https://evil.example.net/a", false},
		{"subdomain of an allowlisted host", "https://x.docs.example.com/a", false},
		{"parent of an allowlisted host", "https://example.com/a", false},
		{"allowlisted name as a suffix of another host", "https://docs.example.com.evil.net/a", false},
		{"allowlisted name as a prefix label", "https://evildocs.example.com/a", false},
		{"plain http", "http://docs.example.com/a", false},
		{"no scheme", "docs.example.com/a", false},
		{"ftp", "ftp://docs.example.com/a", false},
		{"file", "file:///etc/passwd", false},
		{"IPv4 literal", "https://93.184.216.34/a", false},
		{"IPv4 literal, decimal form", "https://1572395042/a", false},
		{"loopback", "https://127.0.0.1/a", false},
		{"IPv6 literal", "https://[2606:2800:220:1:248:1893:25c8:1946]/a", false},
		{"IPv6 loopback", "https://[::1]/a", false},
		{"userinfo hiding the real host", "https://docs.example.com@evil.example.net/a", false},
		{"userinfo with the allowlisted host", "https://user:pw@docs.example.com/a", false},
		{"userinfo user only", "https://user@docs.example.com/a", false},
		{"backslash host trick", `https://docs.example.com\@evil.example.net/a`, false},
		{"other port", "https://docs.example.com:8443/a", false},
		{"empty port colon", "https://docs.example.com:/a", false},
		{"empty url", "", false},
		{"blank url", "   ", false},
		{"no host", "https:///a", false},
		{"control character", "https://docs.example.com/a\r\nHost: evil", false},
		{"space inside", "https://docs.example.com/a b", false},
		{"unicode lookalike host", "https://docs.exаmple.com/a", false},
	}
	for _, c := range cases {
		reason := decideWeb(t, "WebFetch", c.url, allow)
		if c.ok && reason != "" {
			t.Errorf("%s: %q refused: %s", c.name, c.url, reason)
		}
		if !c.ok && reason == "" {
			t.Errorf("%s: %q passed, want refused", c.name, c.url)
		}
		if !c.ok && reason != "" && !strings.Contains(reason, "WebFetch") {
			t.Errorf("%s: reason %q does not name the tool", c.name, reason)
		}
	}
}

// G07: an empty list leaves the guard as it was (the settings' deny rule keeps closing WebFetch); a
// WebFetch call without a url is refused when a list is set; WebSearch stays closed under a list.
func TestWebFetchEmptyListAndWebSearch(t *testing.T) {
	if r := decideWeb(t, "WebFetch", "https://anything.example.net/a", nil); r != "" {
		t.Errorf("empty list: the guard must behave as today, got %q", r)
	}
	if r := decideWeb(t, "WebFetch", "", []string{"docs.example.com"}); r == "" {
		t.Error("a WebFetch call with no url passed under a list")
	}
	if r := decideWeb(t, "WebSearch", "", []string{"docs.example.com"}); r == "" || !strings.Contains(r, "WebSearch") {
		t.Errorf("WebSearch must stay closed under a list, got %q", r)
	}
	if r := decideWeb(t, "WebSearch", "", nil); r != "" {
		t.Errorf("WebSearch with no list: as today (the deny rule closes it), got %q", r)
	}
}

// G07: the entries of the list are bare lower-case host names.
func TestNormalizeWebHost(t *testing.T) {
	good := map[string]string{"docs.example.com": "docs.example.com", " Docs.Example.COM ": "docs.example.com", "a.b.c.d.": "a.b.c.d"}
	for in, want := range good {
		got, err := NormalizeWebHost(in)
		if err != nil || got != want {
			t.Errorf("NormalizeWebHost(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "localhost", "https://docs.example.com", "docs.example.com/a", "docs.example.com:443",
		"user@docs.example.com", "*.example.com", ".example.com", "93.184.216.34", "[::1]", "::1", "exаmple.com",
		"a..b.com", "-a.example.com", strings.Repeat("a", 64) + ".com", "ex ample.com"} {
		if got, err := NormalizeWebHost(in); err == nil {
			t.Errorf("NormalizeWebHost(%q) = %q, want an error", in, got)
		}
	}
}

// F1: a non-ASCII host is refused before any case conversion, so a letter whose lower case is ASCII
// (U+0130 İ -> i, U+212A Kelvin sign -> k) never matches an allowlisted ASCII host.
func TestWebFetchNonASCIIFoldsToAllowlisted(t *testing.T) {
	allow := []string{"i.example", "kit.example.com"}
	for _, u := range []string{"https://İ.example/", "https://Kit.example.com/a", "https://KIT.EXAMPLE.COM/a"} {
		if r := decideWeb(t, "WebFetch", u, allow); r == "" {
			t.Errorf("%q passed the allowlist %v, want refused", u, allow)
		}
	}
	for _, h := range []string{"İ.example", "Kit.example.com"} {
		if got, err := NormalizeWebHost(h); err == nil {
			t.Errorf("NormalizeWebHost(%q) = %q, want an error", h, got)
		}
	}
	if r := decideWeb(t, "WebFetch", "https://I.EXAMPLE/", allow); r != "" {
		t.Errorf("ASCII upper case must still match: %s", r)
	}
}

// F2: a last label a URL parser reads as a number (decimal, 0x hex, octal) is an IP address in
// disguise: refused as a config entry and in a URL.
func TestWebHostNumericLastLabel(t *testing.T) {
	for _, h := range []string{"127.0x1", "127.0X1", "0x7f.0x1", "127.0.0.0x1", "0x7f000001.0x", "a.0xff", "127.01",
		"0177.0.0.01", "0x7f.0.0.017", "a.0X"} {
		if got, err := NormalizeWebHost(h); err == nil {
			t.Errorf("NormalizeWebHost(%q) = %q, want an error", h, got)
		}
		if r := decideWeb(t, "WebFetch", "https://"+h+"/", []string{h, "127.0x1"}); r == "" {
			t.Errorf("https://%s/ passed, want refused", h)
		}
	}
	// A last label that only starts like a number stays a name.
	for _, h := range []string{"docs.0xg", "docs.x0x1", "a.b0x1"} {
		if _, err := NormalizeWebHost(h); err != nil {
			t.Errorf("NormalizeWebHost(%q): %v, want a name", h, err)
		}
	}
}

// N1: the guard takes exactly the key "url" of tool_input; another key equal to it ignoring case (which
// Go's decoder would match) is refused, so an allowed "URL" cannot hide an evil "url".
func TestWebFetchURLKeyExactCase(t *testing.T) {
	allow := []string{"docs.example.com"}
	cases := []struct {
		name, input string
		ok          bool
	}{
		{"exact key", `{"url":"https://docs.example.com/a"}`, true},
		{"exact key and prompt", `{"url":"https://docs.example.com/a","prompt":"summarize"}`, true},
		{"evil url, allowed URL", `{"url":"https://evil.example.net/x","URL":"https://docs.example.com/"}`, false},
		{"allowed URL, evil url", `{"URL":"https://docs.example.com/","url":"https://evil.example.net/x"}`, false},
		{"evil url, allowed Url", `{"url":"https://evil.example.net/x","Url":"https://docs.example.com/"}`, false},
		{"only URL", `{"URL":"https://docs.example.com/"}`, false},
		{"url not a string", `{"url":["https://docs.example.com/"]}`, false},
		{"tool_input not an object", `"https://docs.example.com/"`, false},
	}
	for _, c := range cases {
		in := `{"tool_name":"WebFetch","cwd":"` + t.TempDir() + `","tool_input":` + c.input + `}`
		r, err := Decide(strings.NewReader(in), Config{WebAllow: allow})
		refused := err != nil || r != ""
		if c.ok && refused {
			t.Errorf("%s: refused: %q %v", c.name, r, err)
		}
		if !c.ok && !refused {
			t.Errorf("%s: passed, want refused", c.name)
		}
	}
}
