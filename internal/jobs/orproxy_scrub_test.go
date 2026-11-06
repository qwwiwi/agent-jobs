package jobs

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// orScrubFakeKey is a padded fake key that is not sk-or- shaped: only the exact-key scrub removes it.
var orScrubFakeKey = "fake-openrouter-key-" + strings.Repeat("Q7x9", 16)

// orScrubOtherKey is a different sk-or- shaped token an upstream error might echo.
const orScrubOtherKey = "sk-or-v1-0123456789abcdef0123456789abcdef0123456789abcdef"

// orScrubServe sends one /v1/messages request through a socket-free proxy holding orScrubFakeKey whose
// upstream answers status with body under contentType (and a Content-Length of the raw body).
func orScrubServe(t *testing.T, status int, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	p := &orProxy{key: orScrubFakeKey, token: "tok", slug: orGLM, upstream: "https://upstream.invalid/api",
		provider: json.RawMessage(`{}`), budget: 100, price: ModelPrice{Output: 1}}
	p.client = &http.Client{Transport: orProxyRoutingTransport(func(r *http.Request) (*http.Response, error) {
		_, _ = io.Copy(io.Discard, r.Body)
		h := make(http.Header)
		h.Set("Content-Type", contentType)
		h.Set("Content-Length", strconv.Itoa(len(body)))
		return &http.Response{StatusCode: status, Header: h, ContentLength: int64(len(body)),
			Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	stream := strings.HasPrefix(contentType, "text/event-stream")
	r := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"x","max_tokens":10,"stream":`+strconv.FormatBool(stream)+`,"messages":[]}`))
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	p.inflight.Wait()
	if w.Code != status {
		t.Fatalf("status %d, want %d", w.Code, status)
	}
	return w
}

// orScrubNoFragment fails when body carries any 12-character fragment of key.
func orScrubNoFragment(t *testing.T, what, body, key string) {
	t.Helper()
	for i := 0; i+12 <= len(key); i++ {
		if frag := key[i : i+12]; strings.Contains(body, frag) {
			t.Fatalf("%s carries the key fragment %q:\n%s", what, frag, body)
		}
	}
}

// H1 (fix loop 2): an upstream error body that echoes the configured key (or any sk-or- token) reaches
// claude without it, with a Content-Length that matches what is written.
func TestOpenRouterProxyScrubsUpstreamErrorBodies(t *testing.T) {
	pad := strings.Repeat("a", 130)
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusBadGateway} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			body := `{"error":{"code":` + strconv.Itoa(status) + `,"message":"bad key ` + pad + orScrubFakeKey +
				` or ` + orScrubOtherKey + `","metadata":{"auth":"Bearer ` + orScrubFakeKey + `"}}}`
			w := orScrubServe(t, status, "application/json", body)
			got := w.Body.String()
			orScrubNoFragment(t, "error body", got, orScrubFakeKey)
			orScrubNoFragment(t, "error body", got, orScrubOtherKey)
			if !strings.Contains(got, "bad key "+pad) || !json.Valid(w.Body.Bytes()) {
				t.Fatalf("the error's text is not forwarded as JSON:\n%s", got)
			}
			if cl := w.Header().Get("Content-Length"); cl != strconv.Itoa(len(got)) {
				t.Fatalf("Content-Length %q for a %d-byte body", cl, len(got))
			}
		})
	}
	t.Run("not JSON", func(t *testing.T) {
		got := orScrubServe(t, http.StatusInternalServerError, "text/html",
			"<html>\n<body>key "+orScrubFakeKey+"\n"+orScrubOtherKey+"</body></html>\n").Body.String()
		orScrubNoFragment(t, "html body", got, orScrubFakeKey)
		orScrubNoFragment(t, "html body", got, orScrubOtherKey)
	})
}

// H1 (fix loop 2): an error event inside a 200 stream is scrubbed of the key and of sk-or- tokens; the
// other events pass byte for byte.
func TestOpenRouterProxyScrubsStreamErrorEvents(t *testing.T) {
	start := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n"
	delta := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"an error  sk-or-v1-example\"}}\n\n"
	errEv := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"upstream rejected " +
		orScrubFakeKey + " and " + orScrubOtherKey + "\"}}\n\n"
	for name, tail := range map[string]string{
		"with newline":    errEv,
		"without newline": strings.TrimRight(errEv, "\n"),
	} {
		t.Run(name, func(t *testing.T) {
			got := orScrubServe(t, http.StatusOK, "text/event-stream", start+delta+tail).Body.String()
			orScrubNoFragment(t, "stream", got, orScrubFakeKey)
			orScrubNoFragment(t, "stream", got, orScrubOtherKey)
			if !strings.HasPrefix(got, start+delta) {
				t.Fatalf("the events before the error changed:\n%s", got)
			}
			if !strings.Contains(got, "event: error\ndata: {") || !strings.Contains(got, "upstream rejected [redacted]") {
				t.Fatalf("the error event is not forwarded:\n%s", got)
			}
		})
	}
	// A JSON error object answered with 200 is scrubbed too.
	got := orScrubServe(t, http.StatusOK, "application/json",
		`{"error":{"message":"bad `+orScrubFakeKey+` `+orScrubOtherKey+`"}}`).Body.String()
	orScrubNoFragment(t, "200 JSON error", got, orScrubFakeKey)
	orScrubNoFragment(t, "200 JSON error", got, orScrubOtherKey)
}

// The line scrubber passes a stream unchanged when it carries no key, whatever its chunking, and removes
// the exact key from any line.
func TestOpenRouterLineScrubberChunking(t *testing.T) {
	stream := "data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"error? no\"}}\r\n\r\n: comment\n\ndata: {\"type\":\"message_stop\"}"
	for _, size := range []int{1, 3, 7, len(stream)} {
		var b strings.Builder
		s := &orLineScrubber{w: &b, key: orScrubFakeKey}
		for i := 0; i < len(stream); i += size {
			if err := s.write([]byte(stream[i:min(i+size, len(stream))])); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.end(); err != nil {
			t.Fatal(err)
		}
		if b.String() != stream {
			t.Fatalf("chunk %d: %q, want %q", size, b.String(), stream)
		}
	}
	var b strings.Builder
	s := &orLineScrubber{w: &b, key: orScrubFakeKey}
	_ = s.write([]byte("data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"" + orScrubFakeKey + "\"}}\n"))
	orScrubNoFragment(t, "content line", b.String(), orScrubFakeKey)
}
