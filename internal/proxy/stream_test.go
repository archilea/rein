package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/archilea/rein/internal/keys"
)

// collectStream drives a streamMeter to completion and returns the captured usage.
func collectStream(t *testing.T, upstream, raw string) (model string, in, out int, clientSaw string) {
	t.Helper()
	var gotModel string
	var gotIn, gotOut int
	sm := newStreamMeter(io.NopCloser(strings.NewReader(raw)), upstream,
		func(m string, i, o int) { gotModel, gotIn, gotOut = m, i, o }, nil)

	var sink bytes.Buffer
	if _, err := io.Copy(&sink, sm); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if err := sm.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return gotModel, gotIn, gotOut, sink.String()
}

func TestStreamMeter_OpenAIExtractsFinalUsage(t *testing.T) {
	raw := `data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"delta":{"content":"hi"}}],"usage":null}

data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"delta":{"content":"!"}}],"usage":null}

data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":34,"total_tokens":46}}

data: [DONE]

`
	model, in, out, passthrough := collectStream(t, keys.UpstreamOpenAI, raw)
	if model != "gpt-4o" || in != 12 || out != 34 {
		t.Errorf("got model=%q in=%d out=%d want gpt-4o 12 34", model, in, out)
	}
	if passthrough != raw {
		t.Errorf("client passthrough mismatch")
	}
}

func TestStreamMeter_AnthropicMessageStartAndDelta(t *testing.T) {
	raw := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"claude-sonnet-4-5","usage":{"input_tokens":25,"output_tokens":1}}}

event: content_block_delta
data: {"type":"content_block_delta","delta":{"text":"hi"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}

event: message_stop
data: {"type":"message_stop"}

`
	model, in, out, _ := collectStream(t, keys.UpstreamAnthropic, raw)
	if model != "claude-sonnet-4-5" || in != 25 || out != 42 {
		t.Errorf("got model=%q in=%d out=%d want claude-sonnet-4-5 25 42", model, in, out)
	}
}

func TestStreamMeter_PartialLineAcrossReads(t *testing.T) {
	// Split the stream mid-line across two separate Read calls by wrapping in
	// a reader that returns at most 20 bytes at a time.
	raw := `data: {"model":"gpt-4o","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}

data: [DONE]

`
	var gotModel string
	var gotIn, gotOut int
	sm := newStreamMeter(
		io.NopCloser(&chunkedReader{src: []byte(raw), step: 20}),
		keys.UpstreamOpenAI,
		func(m string, i, o int) { gotModel, gotIn, gotOut = m, i, o },
		nil,
	)
	var sink bytes.Buffer
	if _, err := io.Copy(&sink, sm); err != nil {
		t.Fatal(err)
	}
	_ = sm.Close()
	if gotModel != "gpt-4o" || gotIn != 5 || gotOut != 7 {
		t.Errorf("got %q/%d/%d want gpt-4o/5/7", gotModel, gotIn, gotOut)
	}
}

func TestStreamMeter_MalformedLineIgnored(t *testing.T) {
	raw := "data: not json\n\ndata: {\"model\":\"gpt-4o\",\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2}}\n\n"
	_, in, out, _ := collectStream(t, keys.UpstreamOpenAI, raw)
	if in != 1 || out != 2 {
		t.Errorf("malformed line should be skipped, got in=%d out=%d", in, out)
	}
}

func TestStreamMeter_NoUsageDoesNotFireCallback(t *testing.T) {
	fired := false
	sm := newStreamMeter(io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		keys.UpstreamOpenAI, func(string, int, int) { fired = true }, nil)
	_, _ = io.Copy(io.Discard, sm)
	_ = sm.Close()
	if fired {
		t.Error("callback should not fire when no usage was captured")
	}
}

// chunkedReader returns at most `step` bytes at a time, simulating a slow TCP
// stream that splits lines across Read boundaries.
type chunkedReader struct {
	src  []byte
	step int
	pos  int
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if c.pos >= len(c.src) {
		return 0, io.EOF
	}
	n := c.step
	if n > len(p) {
		n = len(p)
	}
	if c.pos+n > len(c.src) {
		n = len(c.src) - c.pos
	}
	copy(p, c.src[c.pos:c.pos+n])
	c.pos += n
	return n, nil
}

// --- injectStreamUsage tests ---

func newJSONPost(t *testing.T, body string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	return r
}

func readBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("body not valid JSON: %s", raw)
	}
	return out
}

func TestInjectStreamUsage_AddsWhenMissing(t *testing.T) {
	r := newJSONPost(t, `{"model":"gpt-4o","stream":true,"messages":[]}`)
	injectStreamUsage(r)
	body := readBody(t, r)
	opts, ok := body["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options missing from rewritten body: %+v", body)
	}
	if opts["include_usage"] != true {
		t.Errorf("include_usage: got %v want true", opts["include_usage"])
	}
}

func TestInjectStreamUsage_RespectsExplicitFalse(t *testing.T) {
	r := newJSONPost(t, `{"model":"gpt-4o","stream":true,"stream_options":{"include_usage":false}}`)
	injectStreamUsage(r)
	body := readBody(t, r)
	opts := body["stream_options"].(map[string]any)
	if opts["include_usage"] != false {
		t.Errorf("explicit include_usage:false was overwritten to %v", opts["include_usage"])
	}
}

func TestInjectStreamUsage_NoChangeWhenStreamFalse(t *testing.T) {
	r := newJSONPost(t, `{"model":"gpt-4o","stream":false,"messages":[]}`)
	injectStreamUsage(r)
	body := readBody(t, r)
	if _, present := body["stream_options"]; present {
		t.Error("stream_options should not be injected when stream is false")
	}
}

func TestInjectStreamUsage_MalformedJSONPreservesBody(t *testing.T) {
	r := newJSONPost(t, `this is not json`)
	injectStreamUsage(r)
	raw, _ := io.ReadAll(r.Body)
	if string(raw) != "this is not json" {
		t.Errorf("malformed body should be preserved, got %q", raw)
	}
}

func TestInjectStreamUsage_NonJSONContentTypeSkipped(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`original form body`))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	injectStreamUsage(r)
	raw, _ := io.ReadAll(r.Body)
	if string(raw) != "original form body" {
		t.Errorf("non-json content type should be untouched, got %q", raw)
	}
}
